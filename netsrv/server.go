// Package netsrv 管理 godb 的两个网络监听：内嵌 Web 控制台（HTTP）
// 和 PostgreSQL 协议端口，并支持在运行中热切换地址。
//
// 热切换的语义：先关旧 listener（正在处理的请求不受影响，只拒绝新连接），
// 再绑定新地址。绑定失败时自动回滚到旧地址并把错误返回给调用方 ——
// 所以「保存配置」失败不会让服务失去监听。
package netsrv

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"godb/config"
	"godb/db"
	"godb/pgwire"
)

// State 是当前的监听状态（实际生效的地址，不是配置里写的期望值）。
type State struct {
	HTTPAddr  string `json:"httpAddr"`
	PGAddr    string `json:"pgAddr"`
	PGEnabled bool   `json:"pgEnabled"`
}

// Manager 持有两个 listener 的生命周期。
type Manager struct {
	cfg      *config.Store
	db       *db.DB
	handler  http.Handler
	logf     func(format string, args ...interface{})
	queryLog io.Writer

	mu       sync.Mutex
	httpSrv  *http.Server
	httpLns  []net.Listener
	httpAddr string
	pgSrv    *pgwire.Server
	pgLns    []net.Listener
	pgAddr   string
	// tlsCert 是当前 PG 监听用的证书；nil 表示不加密
	tlsCert *tls.Certificate
}

// New 创建监听管理器。handler 是 Web 控制台的路由，queryLog 可为 nil。
func New(cfg *config.Store, d *db.DB, handler http.Handler,
	logf func(string, ...interface{}), queryLog io.Writer) *Manager {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	return &Manager{cfg: cfg, db: d, handler: handler, logf: logf, queryLog: queryLog}
}

// Start 按当前配置启动两个监听。HTTP 失败会返回错误（控制台是必需品），
// PG 失败只记日志（不该因为 PG 端口被占就整个服务起不来）。
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := m.cfg.Get().Network
	if err := m.rebindHTTP(n.HTTPAddr()); err != nil {
		return fmt.Errorf("HTTP 端口 %s 无法监听: %w", n.HTTPAddr(), err)
	}
	if err := m.rebindPG(n.PGAddr()); err != nil {
		m.logf("PostgreSQL 协议监听失败（%s）: %v —— 其余功能正常，可在设置页改端口", n.PGAddr(), err)
	}
	return nil
}

// Apply 把新的网络配置生效，失败时回滚到旧地址。
func (m *Manager) Apply(n config.Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wantHTTP, wantPG := n.HTTPAddr(), n.PGAddr()
	oldHTTP, oldPG := m.httpAddr, m.pgAddr

	// 证书变了要重绑 PG 监听（地址没变也一样），否则开关 SSL 不生效
	cert, err := loadTLS(n)
	if err != nil {
		return err
	}
	tlsChanged := (cert == nil) != (m.tlsCert == nil)
	m.tlsCert = cert

	if wantHTTP != oldHTTP {
		if err := m.rebindHTTP(wantHTTP); err != nil {
			// 回滚：旧地址通常还空着（刚才已经关掉了旧 listener）
			if oldHTTP != "" {
				if err2 := m.rebindHTTP(oldHTTP); err2 != nil {
					m.logf("回滚 HTTP 监听到 %s 也失败了: %v", oldHTTP, err2)
				}
			}
			return fmt.Errorf("HTTP 端口 %s 无法监听: %w（已回滚到 %s）", wantHTTP, err, oldHTTP)
		}
	}

	if wantPG != oldPG || tlsChanged {
		if err := m.rebindPG(wantPG); err != nil {
			if oldPG != "" {
				if err2 := m.rebindPG(oldPG); err2 != nil {
					m.logf("回滚 PG 监听到 %s 也失败了: %v", oldPG, err2)
				}
			}
			return fmt.Errorf("PostgreSQL 端口 %s 无法监听: %w（已回滚到 %s）", wantPG, err, oldPG)
		}
	}
	return nil
}

// State 返回实际生效的监听地址。
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{HTTPAddr: m.httpAddr, PGAddr: m.pgAddr, PGEnabled: m.pgAddr != ""}
}

// Close 关掉两个监听（进程退出时用）。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	closeAll(m.httpLns)
	m.httpLns = nil
	closeAll(m.pgLns)
	m.pgLns = nil
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		ln.Close()
	}
}

// ---------- 内部 ----------

// listenLoopback 在 addr 上监听；如果 addr 是本机回环地址，就把 IPv4 和
// IPv6 两个回环都占上。
//
// 为什么必须双栈：客户端（Navicat、psql -h localhost）会把 localhost 解析成
// ::1 先试。只监听 127.0.0.1 时，这次尝试会先失败再回落到 IPv4 —— 在 Windows
// 上要等约 2 秒。客户端每个操作都新开连接，于是「每个操作都慢 2~3 秒」，
// 而服务端日志里每条 SQL 都只有 1~5 毫秒，根本看不出问题。
func listenLoopback(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 端口写成纯数字之类，交给 net.Listen 自己报错
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return nil, lerr
		}
		return []net.Listener{ln}, nil
	}

	primary, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	lns := []net.Listener{primary}

	if !isLoopbackHost(host) {
		return lns, nil
	}

	// 补上另一族回环；失败不影响主监听（有些机器关了 IPv6）
	other := "::1"
	if strings.Contains(host, ":") {
		other = "127.0.0.1"
	}
	if second, serr := net.Listen("tcp", net.JoinHostPort(other, port)); serr == nil {
		lns = append(lns, second)
	}
	return lns, nil
}

// isLoopbackHost 判断监听地址是不是「本机回环」。
//
// 空主机（":8080" / "0.0.0.0"）表示所有网卡，Go 的 tcp 监听本身就是双栈的，
// 不需要也不应该再补一个。
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// rebindHTTP 关闭旧 listener 并在 addr 上重建。
//
// 注意这里**不调用 Shutdown**：Shutdown 会掐断正在处理的请求，
// 而配置保存这个请求本身就跑在这个 server 上。关 listener 只影响新连接，
// 已经建立的连接会自然结束。
func (m *Manager) rebindHTTP(addr string) error {
	closeAll(m.httpLns)
	m.httpLns = nil
	m.httpAddr = ""
	if addr == "" {
		return nil
	}
	lns, err := listenLoopback(addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: m.handler, ReadHeaderTimeout: 10 * time.Second}
	m.httpSrv, m.httpLns, m.httpAddr = srv, lns, addr
	for _, ln := range lns {
		ln := ln
		go func() {
			if err := srv.Serve(ln); err != nil &&
				!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				m.logf("HTTP 服务结束: %v", err)
			}
		}()
	}
	return nil
}

// rebindPG 关闭旧 PG listener 并在 addr 上重建；addr 为空表示关闭 PG 协议。
func (m *Manager) rebindPG(addr string) error {
	closeAll(m.pgLns)
	m.pgLns = nil
	m.pgAddr = ""
	if addr == "" {
		return nil
	}
	lns, err := listenLoopback(addr)
	if err != nil {
		return err
	}
	srv := &pgwire.Server{
		DB:       m.db,
		Addr:     addr,
		Logf:     m.logf,
		QueryLog: m.queryLog,
		// 认证开关走配置，改完立即生效（不需要重建 listener）
		AuthRequired: func() bool { return m.cfg.Get().Auth.RequirePassword },
		TLSCert:      m.tlsCert,
		AuthScram: func(user string) (string, bool) {
			c := m.cfg.Get()
			if !c.Auth.RequirePassword {
				return "", false
			}
			if c.Auth.User != "" && user != c.Auth.User {
				return "", false
			}
			return c.Auth.ScramVerifier, c.Auth.ScramVerifier != ""
		},
		AuthMD5: func(user string) (string, bool) {
			c := m.cfg.Get()
			if !c.Auth.RequirePassword {
				return "", false
			}
			if c.Auth.User != "" && user != c.Auth.User {
				return "", false
			}
			return c.Auth.PasswordMD5, c.Auth.PasswordMD5 != ""
		},
		AuthVerify: func(user, password string) bool {
			c := m.cfg.Get()
			if !c.Auth.RequirePassword {
				return true
			}
			if c.Auth.User != "" && user != c.Auth.User {
				return false
			}
			return config.VerifyPassword(c.Auth.PasswordHash, password)
		},
	}
	m.pgSrv, m.pgLns, m.pgAddr = srv, lns, addr
	for _, ln := range lns {
		ln := ln
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
				m.logf("PostgreSQL 协议服务结束: %v", err)
			}
		}()
	}
	return nil
}
