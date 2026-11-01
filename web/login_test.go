package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"godb/config"
)

// 登录测试的核心是三个身份：
//
//	本机（127.0.0.1）→ 永远放行，不管有没有密码、有没有 cookie
//	远程 + 没登录   → 401（API）/ 303 跳登录页（页面）
//	远程 + 已登录   → 放行
//
// 之所以要区分本机，是因为「能坐在这台机器前的人」本来就能直接读数据目录，
// 给他加一道登录只是添乱；真正的边界在远程那一侧。

const (
	localAddr  = "127.0.0.1:5000"
	remoteAddr = "192.168.1.9:5000"
	pwPlain    = "s3cret-pw"
)

// loginServer 造一个「设了密码」的服务：这是远程访问的前置条件。
func loginServer(t *testing.T) *Server {
	t.Helper()
	s, store, _ := newConfigServer(t)
	c := store.Get()
	h, err := config.HashPassword(pwPlain)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c.Auth.RequirePassword = true
	c.Auth.User = "godb"
	c.Auth.PasswordHash = h
	if _, err := store.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	return s
}

func call(t *testing.T, h http.Handler, method, path, remote, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = remote
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func loginCookie(t *testing.T, h http.Handler, password string) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/api/login", remoteAddr,
		`{"user":"godb","password":"`+password+`"}`, nil)
	if rec.Code != http.StatusOK {
		return nil, rec
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookie {
			return c, rec
		}
	}
	return nil, rec
}

func TestLocalNeverNeedsLogin(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()

	// 有密码、没 cookie，但来自本机 → 正常返回
	rec := call(t, h, http.MethodGet, "/api/schema", localAddr, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机访问被拦了：%d %s", rec.Code, rec.Body.String())
	}
	// 页面也一样（不是 303 跳登录页）
	rec = call(t, h, http.MethodGet, "/", localAddr, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机打开首页被拦了：%d", rec.Code)
	}
}

func TestRemoteBlockedWithoutLogin(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()

	rec := call(t, h, http.MethodPost, "/api/query", remoteAddr, `{"sql":"SELECT 1"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("远程未登录执行 SQL 应该 401，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/login") {
		t.Fatalf("401 里应该带上登录地址，方便前端跳转：%s", rec.Body.String())
	}

	// 页面请求 → 303 到登录页
	rec = call(t, h, http.MethodGet, "/", remoteAddr, "", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("远程打开首页应该跳登录页，实际 %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// 登录页本身必须能打开，否则死循环
	rec = call(t, h, http.MethodGet, "/login", remoteAddr, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "godb 控制台") {
		t.Fatalf("登录页打不开：%d %s", rec.Code, rec.Body.String())
	}
}

func TestLoginFlow(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()

	// 密码错：401，且不下发 cookie
	bad := call(t, h, http.MethodPost, "/api/login", remoteAddr,
		`{"user":"godb","password":"wrong"}`, nil)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应该 401，实际 %d", bad.Code)
	}
	for _, c := range bad.Result().Cookies() {
		if c.Name == authCookie && c.Value != "" {
			t.Fatal("登录失败却下发了 cookie")
		}
	}

	// 密码对：拿到 cookie，之后远程请求放行
	ck, rec := loginCookie(t, h, pwPlain)
	if ck == nil {
		t.Fatalf("登录成功却没拿到 cookie：%d %s", rec.Code, rec.Body.String())
	}
	if !ck.HttpOnly {
		t.Fatal("登录 cookie 必须 HttpOnly")
	}
	ok := call(t, h, http.MethodPost, "/api/query", remoteAddr, `{"sql":"SELECT 1"}`, ck)
	if ok.Code != http.StatusOK {
		t.Fatalf("带 cookie 的远程请求应该放行，实际 %d %s", ok.Code, ok.Body.String())
	}

	// 用户名字段留空时用配置里的用户名：单用户工具，不该逼人记住账号
	rec = call(t, h, http.MethodPost, "/api/login", remoteAddr,
		`{"password":"`+pwPlain+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("留空用户名应该按配置的用户名走，实际 %d %s", rec.Code, rec.Body.String())
	}
}

func TestSessionCookieTamperAndRotate(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()
	ck, _ := loginCookie(t, h, pwPlain)
	if ck == nil {
		t.Fatal("没能登录")
	}

	// 改一位签名 → 无效
	forged := &http.Cookie{Name: authCookie, Value: ck.Value[:len(ck.Value)-1] + "0"}
	if forged.Value == ck.Value {
		forged.Value = ck.Value[:len(ck.Value)-1] + "1"
	}
	if rec := call(t, h, http.MethodGet, "/api/schema", remoteAddr, "", forged); rec.Code != http.StatusUnauthorized {
		t.Fatalf("篡改过的 cookie 应该被拒，实际 %d", rec.Code)
	}

	// 改密码 → 旧 cookie 立刻失效（签名密钥由 PasswordHash 派生）
	c := s.Config.Get()
	nh, err := config.HashPassword("another-pw")
	if err != nil {
		t.Fatal(err)
	}
	c.Auth.PasswordHash = nh
	if _, err := s.Config.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	if rec := call(t, h, http.MethodGet, "/api/schema", remoteAddr, "", ck); rec.Code != http.StatusUnauthorized {
		t.Fatalf("改密码后旧 cookie 应该失效，实际 %d", rec.Code)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()
	ck, _ := loginCookie(t, h, pwPlain)
	if ck == nil {
		t.Fatal("没能登录")
	}
	rec := call(t, h, http.MethodPost, "/api/logout", remoteAddr, "", ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应该 200，实际 %d", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("登出没有清掉 cookie")
	}
}

func TestLoginRateLimited(t *testing.T) {
	s := loginServer(t)
	h := s.Handler()
	limiter.reset(remoteAddr)
	t.Cleanup(func() { limiter.reset(remoteAddr) })

	var last int
	for i := 0; i < loginMaxFails+2; i++ {
		rec := call(t, h, http.MethodPost, "/api/login", remoteAddr,
			`{"user":"godb","password":"nope"}`, nil)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("连续失败 %d 次后应该 429，实际 %d", loginMaxFails+2, last)
	}
}
