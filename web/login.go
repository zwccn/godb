// 网页控制台的登录保护。
//
// 策略：**本机访问永远免登录，远程访问必须登录**。
//
// 为什么不做成「永远登录」：能坐在本机的人本来就能直接读数据目录里的文件，
// 让他每次开控制台都输一遍密码，只是给自己的日常加一道毫无收益的仪式。
// 而远程是另一回事 —— `httpRemote=true` 时控制台绑到 0.0.0.0，局域网里任何人
// 都能 POST /api/query 执行任意 SQL（含 DROP DATABASE），这才是真实的口子。
//
// 所以登录只挂在「远程」这一侧，开关是配置里的 `auth.requirePassword`：
// 远程访问必须先设密码（`config.Validate` 里强制），设了之后远程就必须登录。
//
// 会话是**签名 cookie**，服务端不存状态：
//   - 重启服务不会被踢下线（用户能接受重启，不能接受重启就重登）；
//   - 改密码会让所有旧会话失效（签名密钥由 PasswordHash 派生，天然如此）。
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"godb/config"
)

const (
	// authCookie 是登录状态 cookie 名。和 godb_sid（会话/事务）不是一回事：
	// 那个是数据库会话 id，这个是「这个人有没有通过登录」。
	authCookie = "godb_auth"

	// authTTL 是登录有效期。本机工具，30 天足够长；改密码会立刻全失效。
	authTTL = 30 * 24 * time.Hour

	// authKeySalt 参与签名密钥派生，换掉它等于让所有旧 cookie 失效。
	authKeySalt = "godb-web-auth-v1|"
)

// sessionKey 由 PasswordHash 派生签名密钥：改密码 → 旧 cookie 全部作废。
func (s *Server) sessionKey() []byte {
	sum := sha256.Sum256([]byte(authKeySalt + s.Config.Get().Auth.PasswordHash))
	return sum[:]
}

// signSession 生成 cookie 值：base64(用户\n过期时间) + "." + hex(HMAC)。
func (s *Server) signSession(user string, exp time.Time) string {
	payload := user + "\n" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		hex.EncodeToString(mac.Sum(nil))
}

// checkSession 校验请求上的 cookie：签名对、没过期、用户还是当前配置的那个。
func (s *Server) checkSession(r *http.Request) bool {
	if s.Config == nil {
		return false
	}
	c, err := r.Cookie(authCookie)
	if err != nil || c.Value == "" {
		return false
	}
	dot := strings.LastIndex(c.Value, ".")
	if dot <= 0 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), "\n", 2)
	if len(parts) != 2 {
		return false
	}
	// 先验签再解析内容：签名不对的话，里面的过期时间也不值得信任。
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write(raw)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(c.Value[dot+1:])) {
		return false
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return false
	}
	return parts[0] == strings.TrimSpace(s.Config.Get().Auth.User)
}

// needLogin 判断这个请求是否需要登录才能继续。
//
// 三个条件同时成立才要登录：启用了密码 + 请求不是本机 + 配置了用户名。
// 「不是本机」用 isLocalRequest，和「配置只能本机改」用的是同一个判断。
func (s *Server) needLogin(r *http.Request) bool {
	if s.Config == nil {
		return false
	}
	c := s.Config.Get()
	if !c.Auth.RequirePassword || c.Auth.PasswordHash == "" {
		return false
	}
	if isLocalRequest(r) {
		return false
	}
	return true
}

// withAuth 把登录检查套在所有路由外面。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.needLogin(r) || s.checkSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		// 登录页和登录接口本身当然不能要求登录
		if p == "/login" || p == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			// 前端看到 401 + login 字段就去跳登录页，不用猜
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "需要登录后才能访问控制台",
				"login": "/login",
			})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

/* ---------------- 登录接口 ---------------- */

// loginLimiter 是按 IP 的失败次数限制：控制台一旦开了远程，就是暴露在局域网上的
// 密码框，不能让脚本无限次试。
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failEntry
}

type failEntry struct {
	n     int
	until time.Time
}

const (
	loginMaxFails  = 10
	loginFailGrace = 5 * time.Minute
)

var limiter = &loginLimiter{fails: map[string]*failEntry{}}

// allow 返回这个 IP 现在能不能试。顺手清掉过期的记录，不用定时器。
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.fails[ip]
	if e == nil {
		return true
	}
	if time.Now().After(e.until) {
		delete(l.fails, ip)
		return true
	}
	return e.n < loginMaxFails
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.fails[ip]
	if e == nil {
		e = &failEntry{}
		l.fails[ip] = e
	}
	e.n++
	e.until = time.Now().Add(loginFailGrace)
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// handleLogin 处理 GET /login（页面）与 POST /login（表单提交兜底）。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.serveLoginPage(w, r)
		return
	}
	s.postLogin(w, r)
}

// handleAPILogin 是前端登录页调用的 JSON 接口。
func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.postLogin(w, r)
}

// handleLogout 清掉登录 cookie。GET 和 POST 都接受（页面上的链接直接跳）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !limiter.allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "尝试次数太多，请 5 分钟后再试",
		})
		return
	}
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析请求体"})
		return
	}
	cfg := s.Config.Get()
	user := strings.TrimSpace(req.User)
	if user == "" {
		user = cfg.Auth.User
	}
	ok := user == cfg.Auth.User && cfg.Auth.PasswordHash != "" &&
		config.VerifyPassword(cfg.Auth.PasswordHash, req.Password)
	if !ok {
		limiter.fail(ip)
		// 不区分「用户不对」和「密码不对」：省一个探测面
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码不对"})
		return
	}
	limiter.reset(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    s.signSession(user, time.Now().Add(authTTL)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 没有 HTTPS 时不设 Secure，否则 cookie 根本存不下来；
		// 远程 + 明文 HTTP 的风险在设置页里明说了。
		MaxAge: int(authTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "user": user})
}

// serveLoginPage 送出登录页（embed 里的 assets/login.html）。
func (s *Server) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	b, err := loginPage()
	if err != nil {
		http.Error(w, "登录页缺失："+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Write(b)
}

// loginPage 读内嵌的登录页。放进 assets/ 里一起 embed，不需要额外打包步骤。
func loginPage() ([]byte, error) {
	return fs.ReadFile(Assets(), "login.html")
}
