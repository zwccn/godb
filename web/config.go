package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"godb/config"
	"godb/netsrv"
	"godb/storage"
)

// authView 是给前端看的认证状态：只有「有没有密码」，没有密码本身，
// 也没有哈希。前端无法读回密码，只能重设。
type authView struct {
	RequirePassword bool   `json:"requirePassword"`
	User            string `json:"user"`
	HasPassword     bool   `json:"hasPassword"`
}

// checkpointView 是落盘状态的展示视图。
type checkpointView struct {
	Commits      int    `json:"commits"`
	Seconds      int    `json:"seconds"`
	WALMB        int    `json:"walMB"`
	CommitsSince int64  `json:"commitsSince"`
	Runs         int64  `json:"runs"`
	LastAt       string `json:"lastAt"`
	SecondsAgo   int64  `json:"secondsAgo"`
	WALBytes     int64  `json:"walBytes"`
}

type runtimeView struct {
	HTTPAddr   string         `json:"httpAddr"`
	PGAddr     string         `json:"pgAddr"`
	PGEnabled  bool           `json:"pgEnabled"`
	ClientIP   string         `json:"clientIp"`
	CanEdit    bool           `json:"canEdit"`
	Checkpoint checkpointView `json:"checkpoint"`
	Tables     int            `json:"tables"`
	Rows       int            `json:"rows"`
	Loaded     int            `json:"loaded"`
	DataBytes  int64          `json:"dataBytes"`
	WALBytes   int64          `json:"walBytes"`
	WALSyncs   int64          `json:"walSyncs"`
}

type configResp struct {
	Path     string         `json:"path"`
	Storage  config.Storage `json:"storage"`
	Network  config.Network `json:"network"`
	Auth     authView       `json:"auth"`
	Runtime  runtimeView    `json:"runtime"`
	Warnings []string       `json:"warnings"`
}

// configReq 是保存请求。密码单独用明文传入（不回显），
// ClearPassword 用来显式清掉已有密码。
type configReq struct {
	Storage config.Storage `json:"storage"`
	Network config.Network `json:"network"`
	Auth    struct {
		RequirePassword bool   `json:"requirePassword"`
		User            string `json:"user"`
		Password        string `json:"password"`
		ClearPassword   bool   `json:"clearPassword"`
	} `json:"auth"`
}

// handleConfig 提供设置页的读写接口。
//
// 写操作只接受来自本机的请求：Web 控制台本身不登录，如果放开远程写，
// 任何能访问控制台的人都可以把 PG 密码关掉。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.Config == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "当前实例没有启用配置文件（用 godb.exe 启动才会有）",
		})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.configView(r))
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// saveConfig 保存 + 热应用配置。
//
// 顺序：校验 → 写文件 → 应用监听 → 应用落盘策略。
// 监听应用失败时把文件回滚成旧配置，保证「页面报错 = 什么都没变」。
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": config.ErrReadOnly.Error()})
		return
	}
	var req configReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}

	cur := s.Config.Get()
	next := cur
	next.Storage = req.Storage
	next.Network = req.Network
	next.Auth.RequirePassword = req.Auth.RequirePassword
	next.Auth.User = req.Auth.User
	if req.Auth.ClearPassword {
		next.Auth.PasswordHash = ""
	}
	if pw := req.Auth.Password; pw != "" {
		h, err := config.HashPassword(pw)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "无法生成密码哈希: " + err.Error()})
			return
		}
		next.Auth.PasswordHash = h
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	saved, err := s.Config.Save(next)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// 监听是唯一可能失败的一步（端口被占、权限不足），失败要整体回滚。
	if s.Net != nil {
		if err := s.Net.Apply(saved.Network); err != nil {
			if _, err2 := s.Config.Save(cur); err2 != nil {
				s.lastRollbackErr = err2.Error()
			}
			_ = s.Net.Apply(cur.Network)
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
	}

	s.DB.Store.SetCheckpointPolicy(toPolicy(saved.Storage))
	writeJSON(w, http.StatusOK, s.configView(r))
}

// configView 组装「配置 + 实际运行状态」。
func (s *Server) configView(r *http.Request) configResp {
	c := s.Config.Get()
	resp := configResp{
		Path:    s.Config.Path(),
		Storage: c.Storage,
		Network: c.Network,
		Auth: authView{
			RequirePassword: c.Auth.RequirePassword,
			User:            c.Auth.User,
			HasPassword:     c.Auth.PasswordHash != "",
		},
		Warnings: nonNil(c.StorageWarnings()),
	}

	// 单元测试里可能没有监听管理器，这时用配置里的期望值兜底。
	st := netsrv.State{HTTPAddr: c.Network.HTTPAddr(), PGAddr: c.Network.PGAddr(), PGEnabled: c.Network.PGEnabled}
	if s.Net != nil {
		st = s.Net.State()
	}
	resp.Runtime = runtimeView{
		HTTPAddr:  st.HTTPAddr,
		PGAddr:    st.PGAddr,
		PGEnabled: st.PGEnabled,
		ClientIP:  clientIP(r),
		CanEdit:   isLocalRequest(r),
	}

	cp := s.DB.Store.CheckpointStatsNow()
	resp.Runtime.Checkpoint = checkpointView{
		Commits:      cp.Policy.Commits,
		Seconds:      cp.Policy.Seconds,
		WALMB:        int(cp.Policy.WALBytes / (1 << 20)),
		CommitsSince: cp.CommitsSince,
		Runs:         cp.Runs,
		WALBytes:     cp.WALBytes,
	}
	if !cp.LastAt.IsZero() {
		resp.Runtime.Checkpoint.LastAt = cp.LastAt.Format("2006-01-02 15:04:05")
		resp.Runtime.Checkpoint.SecondsAgo = int64(time.Since(cp.LastAt).Seconds())
	}

	info := s.DB.Store.Info()
	resp.Runtime.Tables = info.Tables
	resp.Runtime.Rows = info.Rows
	resp.Runtime.Loaded = info.Loaded
	resp.Runtime.DataBytes = info.Bytes
	resp.Runtime.WALBytes = info.WALBytes
	resp.Runtime.WALSyncs = info.WALSyncs
	return resp
}

// nonNil 让空切片序列化成 [] 而不是 null，前端就不用额外判空。
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func toPolicy(s config.Storage) storage.CheckpointPolicy {
	return storage.CheckpointPolicy{
		Commits:  s.CheckpointCommits,
		Seconds:  s.CheckpointSeconds,
		WALBytes: int64(s.CheckpointWALMB) << 20,
	}
}

// clientIP 取出对端 IP（去掉端口）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLocalRequest 判断请求是否来自本机回环地址。
func isLocalRequest(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && ip.IsLoopback()
}

// DescribeNetwork 给启动日志用的一句话说明。
func DescribeNetwork(c config.Network) string {
	var b strings.Builder
	fmt.Fprintf(&b, "控制台 %s", c.HTTPAddr())
	if c.PGAddr() == "" {
		b.WriteString("；PG 协议未启用")
	} else {
		fmt.Fprintf(&b, "；PG 协议 %s", c.PGAddr())
	}
	return b.String()
}
