package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"godb/config"
	"godb/db"
	"godb/storage"
)

// 本文件是监控面板的 HTTP 接口。
//
// 四个动作，对应文档里「每个数字对应一个动作」的原则：
//   - GET  /api/monitor          看：水位、三层状态、锁明细、慢SQL/慢事务
//   - POST /api/monitor/detail   取一个结构的执行明细（点开某一行时）
//   - POST /api/monitor/kill     按会话 Kill（回滚事务、放弃等锁/排队）
//   - POST /api/monitor/timeouts 改四个超时开关（写 config.json）

// handleMonitor 返回当前库的监控快照。
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	d := s.currentDB(r)
	snap := d.Monitor(d.Timeouts())
	snap.ClockGranNs = d.ClockGranNs
	writeJSON(w, http.StatusOK, snap)
}

// handleSpace 返回空间构成；带 ?table= 时下钻到单张表的页级构成。
//
// 两档代价差一个量级，所以分开：
//   - 不给表名：只 stat 文件 + 读 meta，毫秒级，可以跟着轮询；
//   - 给表名：要逐页扫（8KB×页数），按需点开才算，不放进每秒轮询。
func (s *Server) handleSpace(w http.ResponseWriter, r *http.Request) {
	d := s.currentDB(r)
	name := strings.TrimSpace(r.URL.Query().Get("table"))
	if name == "" {
		tables := d.Store.TableSizes()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"space":  storage.SumSpace(tables, d.Store.WalBytes()),
			"tables": tables,
		})
		return
	}
	det, err := d.Store.SpaceDetail(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, det)
}

// handleMonitorKill 取消一个会话当前的活。
//
// 语义按「保守版」约定：空闲事务立刻回滚；等锁/排队的请求立刻退出；
// 正在执行的语句没法打断，标记为取消，语句跑完后自动回滚。
func (s *Server) handleMonitorKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	id := strings.TrimSpace(req.Session)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少会话 ID"})
		return
	}
	// 注意：Kill 会回滚事务，但在本机管理界面里这是合法操作，不做额外鉴权
	// （配置写入那种「能改就永久生效」的操作才限本机）。
	if err := s.currentDB(r).KillSession(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "session": id})
}

// handleMonitorDetail 取一个结构的执行明细。
//
// 列表接口只给统计，明细单独按需取：一个结构最多 100 条明细，
// 100 个结构全给就是 400KB，而用户一次只看得到 10 行。
func (s *Server) handleMonitorDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind string `json:"kind"` // sql / tx
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	d := s.currentDB(r)
	if req.Kind == "tx" {
		det, ok := d.TxDetail(req.Key)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "这个事务结构已经不在统计里了（可能已被淘汰或库已重启）"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"kind": "tx", "details": det})
		return
	}
	det, plan, ok := d.StmtDetail(req.Key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "这个语句结构已经不在统计里了（可能已被淘汰或库已重启）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"kind": "sql", "details": det, "plan": plan})
}

// handleMonitorTimeouts 改四个超时开关并持久化。
//
// 只接受本机请求，理由和配置页一样：这几个开关能让任意人的事务被回滚，
// 开放到公网等于送人一把「踢人」的按钮。
func (s *Server) handleMonitorTimeouts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": config.ErrReadOnly.Error()})
		return
	}
	var req db.TimeoutStatus
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	// 没有配置文件时（单元测试 / 单库模式）只改运行时，不落盘。
	if s.Config == nil {
		s.currentDB(r).SetTimeouts(req)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "persisted": false, "timeouts": req})
		return
	}
	cur := s.Config.Get()
	next := cur
	next.Timeouts = config.Timeouts{
		SlowSQLSec:    req.SlowSQLSec,
		SlowTxIdleSec: req.SlowTxIdleSec,
		LockWaitSec:   req.LockWaitSec,
		QueueWaitSec:  req.QueueWaitSec,
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.Config.Save(next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.currentDB(r).SetTimeouts(req)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "persisted": true, "timeouts": req})
}
