package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"godb/config"
	"godb/db"
	"godb/exec"
	"godb/hrtime"
	"godb/lock"
	"godb/netsrv"
	"godb/sql"
	"godb/storage"
	"godb/txn"
)

// Server 提供 HTTP API 与内嵌前端。
type Server struct {
	// DB 是默认库（配置页、落盘统计这类全局东西用它）。
	// DBs 非空时，业务请求按 cookie godb_db 选库。
	DB  *db.DB
	DBs *db.Manager

	// Config 与 Net 只在「带配置文件启动」时非 nil；
	// 单元测试里直接 New(d) 即可，设置页会返回 501。
	Config *config.Store
	Net    *netsrv.Manager

	// lastRollbackErr 记录配置回滚失败（正常情况下永远是空）。
	lastRollbackErr string
}

// New 创建服务器。
func New(d *db.DB) *Server { return &Server{DB: d} }

// WithConfig 挂上配置存储与监听管理器，启用设置页。
func (s *Server) WithConfig(cfg *config.Store, net *netsrv.Manager) *Server {
	s.Config = cfg
	s.Net = net
	return s
}

type apiError struct {
	Message    string `json:"message"`
	Line       int    `json:"line,omitempty"`
	Col        int    `json:"col,omitempty"`
	Stmt       int    `json:"stmt"`
	Kind       string `json:"kind,omitempty"`       // lock_timeout | deadlock | sql
	RolledBack bool   `json:"rolledBack,omitempty"` // 事务是否已因锁冲突自动回滚
}

type queryResp struct {
	Results     []*exec.Result `json:"results"`
	Error       *apiError      `json:"error,omitempty"`
	Tx          db.TxState     `json:"tx"`
	ElapsedNs   int64          `json:"elapsedNs"`
	ClockGranNs int64          `json:"clockGranNs"`
	Statements  int            `json:"statements"`
}

type schemaResp struct {
	Tables      []db.TableInfo `json:"tables"`
	Tx          db.TxState     `json:"tx"`
	ClockGranNs int64          `json:"clockGranNs"`
}

// Routes 注册所有路由。
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(Assets())))
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/schema", s.handleSchema)
	mux.HandleFunc("/api/tx", s.handleTx)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/sample", s.handleSample)
	mux.HandleFunc("/api/locks", s.handleLocks)
	mux.HandleFunc("/api/monitor", s.handleMonitor)
	mux.HandleFunc("/api/space", s.handleSpace)
	mux.HandleFunc("/api/pages", s.handlePages)
	mux.HandleFunc("/api/page", s.handlePage)
	mux.HandleFunc("/api/monitor/detail", s.handleMonitorDetail)
	mux.HandleFunc("/api/monitor/kill", s.handleMonitorKill)
	mux.HandleFunc("/api/monitor/timeouts", s.handleMonitorTimeouts)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/dbs", s.handleDBs)
	return mux
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "fallback-session"
	}
	return hex.EncodeToString(b)
}

// currentDB 选本次请求要操作的库：cookie godb_db 优先，取不到就用默认库。
//
// 用 cookie 而不是每个接口都加 ?db= 参数：前端切库只写一次 cookie，
// 之后所有既有接口（查询/表结构/事务/导出）都不用改 URL。
func (s *Server) currentDB(r *http.Request) *db.DB {
	if s.DBs == nil {
		return s.DB
	}
	name := ""
	if c, err := r.Cookie("godb_db"); err == nil {
		name = c.Value
	}
	if name == "" {
		return s.DBs.Default()
	}
	d, err := s.DBs.Get(name)
	if err != nil {
		return s.DBs.Default() // 库被删掉了：默默回落到默认库，不打断操作
	}
	return d
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) *txn.Session {
	return s.sessionOf(w, r, s.currentDB(r))
}

// sessionOf 在指定库上取会话。会话表是每库独立的，所以同一个浏览器
// 在两个库里各有一个事务上下文，切库不会串。
func (s *Server) sessionOf(w http.ResponseWriter, r *http.Request, d *db.DB) *txn.Session {
	id := ""
	if c, err := r.Cookie("godb_sid"); err == nil {
		id = c.Value
	}
	if id == "" {
		id = newID()
		http.SetCookie(w, &http.Cookie{
			Name:     "godb_sid",
			Value:    id,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return d.Session(id)
}

// handleDBs 是库列表与库管理接口。
//
//	GET  → { current, default, dbs: [{name, default, tables, rows, bytes, walBytes}] }
//	POST { action: "switch", db }   切换当前库（写 cookie）
//	POST { action: "create", name } 新建空库
//	POST { action: "drop",  name }  删除（默认库和当前库会被拒）
//
// 不写 action 且带 db 的旧请求按 switch 处理，避免前端缓存版本对不上。
func (s *Server) handleDBs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.postDBs(w, r)
		return
	}
	if s.DBs == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"current": s.DB.Name,
			"default": s.DB.Name,
			"dbs": []map[string]interface{}{{
				"name": s.DB.Name, "default": true,
			}},
		})
		return
	}
	cur := s.currentDB(r)
	stats := s.DBs.List()
	dbs := make([]map[string]interface{}, 0, len(stats))
	for _, st := range stats {
		dbs = append(dbs, map[string]interface{}{
			"name":     st.Name,
			"default":  st.Default,
			"current":  st.Name == cur.Name,
			"tables":   st.Tables,
			"rows":     st.Rows,
			"bytes":    st.Bytes,
			"walBytes": st.WALBytes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current": cur.Name,
		"default": s.DBs.Default().Name,
		"dbs":     dbs,
	})
}

func (s *Server) postDBs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		DB     string `json:"db"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	if s.DBs == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "当前是单库模式，不支持多库操作"})
		return
	}
	// 库管理会建目录、删目录，只允许本机操作 —— 和控制台改配置同一个理由：
	// 放开给远程等于把「删库」这个按钮摆到公网上。
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "多库管理只允许在本机操作"})
		return
	}
	switch req.Action {
	case "", "switch":
		d, err := s.DBs.Get(req.DB)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "godb_db", Value: d.Name, Path: "/", SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "current": d.Name})
	case "create":
		d, err := s.DBs.Create(req.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": d.Name})
	case "drop":
		cur := s.currentDB(r)
		name := strings.ToLower(strings.TrimSpace(req.Name))
		// 先判默认库：它同时也是当前库时，「默认库不能删」这个理由是更准确的那个。
		if name == strings.ToLower(s.DBs.Default().Name) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "默认库不能删除（它就是数据目录根上的那一套，删了服务下次起不来）",
			})
			return
		}
		if name == cur.Name {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "不能删除当前正在使用的库，请先切换到别的库",
			})
			return
		}
		if err := s.DBs.Drop(req.Name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知操作：" + req.Action})
	}
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	d := s.currentDB(r)
	sess := s.sessionOf(w, r, d)
	if strings.TrimSpace(req.SQL) == "" {
		writeJSON(w, http.StatusOK, queryResp{Results: []*exec.Result{}, Tx: d.TxState(sess)})
		return
	}

	start := hrtime.Now()
	results, err := d.Exec(sess, req.SQL)
	resp := queryResp{
		Results:     results,
		ElapsedNs:   hrtime.Since(start),
		ClockGranNs: d.ClockGranNs,
		Tx:          d.TxState(sess),
		Statements:  len(results),
	}
	if resp.Results == nil {
		resp.Results = []*exec.Result{}
	}
	if err != nil {
		resp.Error = classifyError(err, len(results))
	}
	writeJSON(w, http.StatusOK, resp)
}

// classifyError 把错误分成 SQL 错误与锁冲突，方便前端区别展示。
func classifyError(err error, stmt int) *apiError {
	e := &apiError{Message: err.Error(), Stmt: stmt, Kind: "sql"}
	var te *lock.TimeoutError
	var de *lock.DeadlockError
	var pe *sql.Error
	switch {
	case errors.As(err, &te):
		e.Kind = "lock_timeout"
		e.RolledBack = true
	case errors.As(err, &de):
		e.Kind = "deadlock"
		e.RolledBack = true
	}
	if errors.As(err, &pe) {
		e.Line = pe.Line
		e.Col = pe.Col
	}
	return e
}

func (s *Server) handleLocks(w http.ResponseWriter, r *http.Request) {
	snap := s.currentDB(r).LockSnapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	d := s.currentDB(r)
	sess := s.sessionOf(w, r, d)
	writeJSON(w, http.StatusOK, schemaResp{Tables: d.Tables(), Tx: d.TxState(sess), ClockGranNs: d.ClockGranNs})
}

func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	var stmt string
	switch strings.ToLower(req.Action) {
	case "begin":
		stmt = "BEGIN"
	case "commit":
		stmt = "COMMIT"
	case "rollback":
		stmt = "ROLLBACK"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知事务动作"})
		return
	}
	d := s.currentDB(r)
	sess := s.sessionOf(w, r, d)
	start := hrtime.Now()
	results, err := d.Exec(sess, stmt)
	resp := queryResp{Results: results, ElapsedNs: hrtime.Since(start), Tx: d.TxState(sess), ClockGranNs: d.ClockGranNs}
	if resp.Results == nil {
		resp.Results = []*exec.Result{}
	}
	if err != nil {
		resp.Error = classifyError(err, len(results))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d := s.currentDB(r)
	sess := s.sessionOf(w, r, d)
	if sess.InTx() {
		sess.ForceRollback()
	}
	if err := d.DropAll(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "tables": d.Tables(), "trash": newestTrash(d)})
}

func (s *Server) handleSample(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d := s.currentDB(r)
	sess := s.sessionOf(w, r, d)
	if sess.InTx() {
		sess.ForceRollback()
	}
	if err := d.LoadSample(sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "tables": d.Tables(), "trash": newestTrash(d)})
}

// newestTrash 返回最近一次回收站备份的目录名（没有备份时返回空串）。
// 两个「一键清库」接口都把它带回去，调用方至少知道东西被备份到哪儿了。
func newestTrash(d *db.DB) string {
	ents, err := storage.TrashEntries(d.Dir())
	if err != nil || len(ents) == 0 {
		return ""
	}
	return ents[0]
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
