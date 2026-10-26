package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"godb/db"
	"godb/exec"
	"godb/hrtime"
	"godb/lock"
	"godb/sql"
	"godb/txn"
)

// Server 提供 HTTP API 与内嵌前端。
type Server struct {
	DB *db.DB
}

// New 创建服务器。
func New(d *db.DB) *Server { return &Server{DB: d} }

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
	return mux
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "fallback-session"
	}
	return hex.EncodeToString(b)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) *txn.Session {
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
	return s.DB.Session(id)
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
	sess := s.session(w, r)
	if strings.TrimSpace(req.SQL) == "" {
		writeJSON(w, http.StatusOK, queryResp{Results: []*exec.Result{}, Tx: s.DB.TxState(sess)})
		return
	}

	start := hrtime.Now()
	results, err := s.DB.Exec(sess, req.SQL)
	resp := queryResp{
		Results:     results,
		ElapsedNs:   hrtime.Since(start),
		ClockGranNs: s.DB.ClockGranNs,
		Tx:          s.DB.TxState(sess),
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
	switch {
	case errors.As(err, &te):
		e.Kind = "lock_timeout"
		e.RolledBack = true
	case errors.As(err, &de):
		e.Kind = "deadlock"
		e.RolledBack = true
	}
	if pe, ok := err.(*sql.Error); ok {
		e.Line = pe.Line
		e.Col = pe.Col
	}
	return e
}

func (s *Server) handleLocks(w http.ResponseWriter, r *http.Request) {
	snap := s.DB.LockSnapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r)
	writeJSON(w, http.StatusOK, schemaResp{Tables: s.DB.Tables(), Tx: s.DB.TxState(sess), ClockGranNs: s.DB.ClockGranNs})
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
	sess := s.session(w, r)
	start := hrtime.Now()
	results, err := s.DB.Exec(sess, stmt)
	resp := queryResp{Results: results, ElapsedNs: hrtime.Since(start), Tx: s.DB.TxState(sess), ClockGranNs: s.DB.ClockGranNs}
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
	sess := s.session(w, r)
	if sess.InTx() {
		sess.ForceRollback()
	}
	if err := s.DB.DropAll(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "tables": s.DB.Tables()})
}

func (s *Server) handleSample(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := s.session(w, r)
	if sess.InTx() {
		sess.ForceRollback()
	}
	if err := s.DB.LoadSample(sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "tables": s.DB.Tables()})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
