package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"godb/config"
	"godb/db"
)

func newConfigServer(t *testing.T) (*Server, *config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir, db.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	dataDir := filepath.Join(dir, "data")
	cfg, err := config.Load(dataDir, config.Default(), nil)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return New(d).WithConfig(cfg, nil), cfg, dataDir
}

func do(t *testing.T, s *Server, method, remote, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/config", nil)
	} else {
		r = httptest.NewRequest(method, "/api/config", strings.NewReader(body))
	}
	r.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)

	var out map[string]interface{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestConfigGet(t *testing.T) {
	s, _, dataDir := newConfigServer(t)
	rec, out := do(t, s, http.MethodGet, "127.0.0.1:5000", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if out["path"] != filepath.Join(dataDir, config.FileName) {
		t.Fatalf("path = %v", out["path"])
	}
	rt := out["runtime"].(map[string]interface{})
	if rt["canEdit"] != true {
		t.Fatal("本机请求应该可编辑")
	}
	if rt["clientIp"] != "127.0.0.1" {
		t.Fatalf("clientIp = %v", rt["clientIp"])
	}
	// 密码哈希不能出现在响应里
	if strings.Contains(rec.Body.String(), "passwordHash") {
		t.Fatal("响应里不应该有 passwordHash")
	}
}

func TestConfigSaveFromLocalhost(t *testing.T) {
	s, cfg, _ := newConfigServer(t)
	body := `{
		"storage": {"checkpointCommits": 5, "checkpointSeconds": 30, "checkpointWalMB": 8},
		"network": {"httpPort": 18080, "httpRemote": true, "pgEnabled": true, "pgPort": 16432, "pgRemote": false},
		"auth": {"requirePassword": true, "user": "admin", "password": "hunter2", "clearPassword": false}
	}`
	rec, out := do(t, s, http.MethodPost, "127.0.0.1:5000", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if out["error"] != nil {
		t.Fatalf("unexpected error: %v", out["error"])
	}

	c := cfg.Get()
	if c.Storage.CheckpointCommits != 5 || c.Storage.CheckpointSeconds != 30 {
		t.Fatalf("落盘策略没保存: %+v", c.Storage)
	}
	if c.Network.HTTPPort != 18080 || !c.Network.HTTPRemote {
		t.Fatalf("网络配置没保存: %+v", c.Network)
	}
	if !c.Auth.RequirePassword || c.Auth.User != "admin" {
		t.Fatalf("认证配置没保存: %+v", c.Auth)
	}
	if !config.VerifyPassword(c.Auth.PasswordHash, "hunter2") {
		t.Fatal("密码没正确哈希")
	}
	// 落盘策略要真的应用到存储层
	if got := s.DB.Store.CheckpointPolicyNow(); got.Commits != 5 || got.Seconds != 30 {
		t.Fatalf("存储层策略没生效: %+v", got)
	}

	auth := out["auth"].(map[string]interface{})
	if auth["hasPassword"] != true {
		t.Fatal("hasPassword 应该为 true")
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "passwordHash") {
		t.Fatal("响应里不应该出现密码或哈希")
	}
}

// 打开远程访问后，远程请求只能看，不能改 —— 否则任何人都能关掉密码。
func TestConfigSaveFromRemoteRejected(t *testing.T) {
	s, cfg, _ := newConfigServer(t)
	before := cfg.Get()

	body := `{
		"storage": {"checkpointCommits": 1, "checkpointSeconds": 1, "checkpointWalMB": 1},
		"network": {"httpPort": 8080, "httpRemote": true, "pgEnabled": false, "pgPort": 6432, "pgRemote": false},
		"auth": {"requirePassword": false, "user": "godb", "password": "", "clearPassword": true}
	}`
	rec, out := do(t, s, http.MethodPost, "192.168.1.20:5000", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("远程写应该 403，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if out["error"] == nil {
		t.Fatal("应该返回错误信息")
	}
	if after := cfg.Get(); after.Auth != before.Auth || after.Storage != before.Storage {
		t.Fatal("被拒的请求不应该改动配置")
	}

	// 但远程读是允许的
	rec, out = do(t, s, http.MethodGet, "192.168.1.20:5000", "")
	if rec.Code != 200 {
		t.Fatalf("远程读应该允许，得到 %d", rec.Code)
	}
	rt := out["runtime"].(map[string]interface{})
	if rt["canEdit"] != false {
		t.Fatal("远程请求的 canEdit 应该为 false")
	}
}

func TestConfigSaveRejectsInvalid(t *testing.T) {
	s, cfg, _ := newConfigServer(t)
	before := cfg.Get()
	body := `{
		"storage": {"checkpointCommits": 128, "checkpointSeconds": 60, "checkpointWalMB": 64},
		"network": {"httpPort": 0, "httpRemote": false, "pgEnabled": false, "pgPort": 6432, "pgRemote": false},
		"auth": {"requirePassword": false, "user": "godb", "password": "", "clearPassword": false}
	}`
	rec, out := do(t, s, http.MethodPost, "127.0.0.1:5000", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法端口应该 400，得到 %d", rec.Code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "HTTP 端口") {
		t.Fatalf("错误信息不对: %q", msg)
	}
	if cfg.Get().Network.HTTPPort != before.Network.HTTPPort {
		t.Fatal("非法配置不应该落盘")
	}
}

// 启用密码但没有密码（且没提供新密码）应该被拒。
func TestConfigRequirePasswordWithoutPassword(t *testing.T) {
	s, _, _ := newConfigServer(t)
	body := `{
		"storage": {"checkpointCommits": 128, "checkpointSeconds": 60, "checkpointWalMB": 64},
		"network": {"httpPort": 8080, "httpRemote": false, "pgEnabled": false, "pgPort": 6432, "pgRemote": false},
		"auth": {"requirePassword": true, "user": "godb", "password": "", "clearPassword": false}
	}`
	rec, out := do(t, s, http.MethodPost, "127.0.0.1:5000", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应该 400，得到 %d", rec.Code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "必须设置密码") {
		t.Fatalf("错误信息不对: %q", msg)
	}
}

// 没有配置文件（单元测试里 New(d) 的用法）时设置页要明确返回 501。
func TestConfigNotAvailable(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(dir, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := New(d)
	rec, _ := do(t, s, http.MethodGet, "127.0.0.1:5000", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestConfigFileWritten(t *testing.T) {
	s, _, dataDir := newConfigServer(t)
	body := `{
		"storage": {"checkpointCommits": 9, "checkpointSeconds": 0, "checkpointWalMB": 0},
		"network": {"httpPort": 8080, "httpRemote": false, "pgEnabled": false, "pgPort": 6432, "pgRemote": false},
		"auth": {"requirePassword": false, "user": "godb", "password": "", "clearPassword": false}
	}`
	if rec, _ := do(t, s, http.MethodPost, "127.0.0.1:5000", body); rec.Code != 200 {
		t.Fatalf("save failed: %s", rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"checkpointCommits": 9`) {
		t.Fatalf("配置文件内容不对:\n%s", raw)
	}
}
