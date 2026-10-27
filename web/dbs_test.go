package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"godb/db"
)

func newDBServer(t *testing.T) (*Server, *db.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	mgr, warns, err := db.OpenManager(dir, db.Options{})
	if err != nil {
		t.Fatalf("open manager: %v", err)
	}
	for _, w := range warns {
		t.Logf("warn: %v", w)
	}
	t.Cleanup(func() { mgr.Close() })
	s := New(mgr.Default())
	s.DBs = mgr
	return s, mgr, dir
}

// dbsReq 发一个 /api/dbs 请求，返回状态码和解出来的 JSON。
func dbsReq(t *testing.T, s *Server, method, remote, body string, cookie *http.Cookie) (int, map[string]interface{}) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/dbs", nil)
	} else {
		r = httptest.NewRequest(method, "/api/dbs", strings.NewReader(body))
	}
	r.RemoteAddr = remote
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)
	var out map[string]interface{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// 列表要带每个库的概况：默认库排第一、标 default，当前库标 current。
func TestDBsList(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	if _, err := mgr.Create("shop"); err != nil {
		t.Fatalf("create: %v", err)
	}

	code, out := dbsReq(t, s, http.MethodGet, "127.0.0.1:5000", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, out)
	}
	if out["current"] != "godb" || out["default"] != "godb" {
		t.Fatalf("current/default 不对: %v", out)
	}
	list, _ := out["dbs"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("应有 2 个库，实际 %d：%v", len(list), list)
	}
	first, _ := list[0].(map[string]interface{})
	if first["name"] != "godb" || first["default"] != true || first["current"] != true {
		t.Fatalf("默认库应排第一且标记 current，实际 %v", first)
	}
	second, _ := list[1].(map[string]interface{})
	if second["name"] != "shop" || second["current"] != false {
		t.Fatalf("第二个库不对：%v", second)
	}
	// 统计字段必须齐全（前端直接读，缺了会显示 undefined）
	for _, k := range []string{"tables", "rows", "bytes", "walBytes"} {
		if _, ok := second[k]; !ok {
			t.Fatalf("缺少字段 %q：%v", k, second)
		}
	}
}

// 新建：成功后列表里能看到，且目录真的建出来了；重名报 400。
func TestDBsCreate(t *testing.T) {
	s, _, dir := newDBServer(t)

	code, out := dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"create","name":"Report"}`, nil)
	if code != http.StatusOK || out["name"] != "report" {
		t.Fatalf("create 失败: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "db", "report", "catalog.json")); err != nil {
		t.Fatalf("库目录应有 catalog.json: %v", err)
	}

	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"create","name":"report"}`, nil)
	if code != http.StatusBadRequest || !strings.Contains(str(out["error"]), "already exists") {
		t.Fatalf("重名应报 400，实际 %d %v", code, out)
	}

	// 非法名字（大写会被规范化，带路径的必须拒）
	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"create","name":"../evil"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("路径穿越应被拒，实际 %d %v", code, out)
	}
}

// 切换：写 cookie，之后 GET 的 current 就变了。
func TestDBsSwitchSetsCookie(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	if _, err := mgr.Create("shop"); err != nil {
		t.Fatalf("create: %v", err)
	}

	code, out := dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"switch","db":"shop"}`, nil)
	if code != http.StatusOK || out["current"] != "shop" {
		t.Fatalf("switch 失败: %d %v", code, out)
	}
	cookie := &http.Cookie{Name: "godb_db", Value: "shop"}
	code, out = dbsReq(t, s, http.MethodGet, "127.0.0.1:5000", "", cookie)
	if code != http.StatusOK || out["current"] != "shop" {
		t.Fatalf("切库后 current 应为 shop，实际 %d %v", code, out)
	}

	// 不存在的库：400，且不能把 cookie 设成它
	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"switch","db":"nope"}`, nil)
	if code != http.StatusBadRequest || !strings.Contains(str(out["error"]), "does not exist") {
		t.Fatalf("切到不存在的库应 400，实际 %d %v", code, out)
	}
}

// 删除：默认库和当前库都拒；切走之后能删，且目录被清掉。
func TestDBsDropRules(t *testing.T) {
	s, mgr, dir := newDBServer(t)
	if _, err := mgr.Create("shop"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 默认库
	code, out := dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"drop","name":"godb"}`, nil)
	if code != http.StatusBadRequest || !strings.Contains(str(out["error"]), "默认库") {
		t.Fatalf("删默认库应被拒，实际 %d %v", code, out)
	}

	// 当前库（cookie 指向 shop）
	shopCookie := &http.Cookie{Name: "godb_db", Value: "shop"}
	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"drop","name":"shop"}`, shopCookie)
	if code != http.StatusBadRequest || !strings.Contains(str(out["error"]), "当前正在使用") {
		t.Fatalf("删当前库应被拒，实际 %d %v", code, out)
	}

	// 不是当前库 → 能删
	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"drop","name":"shop"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("应能删除：%d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "db", "shop")); !os.IsNotExist(err) {
		t.Fatalf("目录应被删除，stat err = %v", err)
	}

	// 删完再删：400（库里已经没有它）
	code, out = dbsReq(t, s, http.MethodPost, "127.0.0.1:5000",
		`{"action":"drop","name":"shop"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("重复删除应 400，实际 %d %v", code, out)
	}
}

// 建库/删库会动磁盘，只允许本机。远程请求统一 403。
func TestDBsWriteOnlyLocal(t *testing.T) {
	s, mgr, _ := newDBServer(t)

	for _, body := range []string{
		`{"action":"create","name":"x1"}`,
		`{"action":"drop","name":"godb"}`,
		`{"action":"switch","db":"godb"}`,
	} {
		code, out := dbsReq(t, s, http.MethodPost, "10.0.0.9:5000", body, nil)
		if code != http.StatusForbidden {
			t.Fatalf("远程 %s 应 403，实际 %d %v", body, code, out)
		}
	}
	if mgr.Exists("x1") {
		t.Fatal("远程不该能建库")
	}
	// 读接口对远程开放（只是看列表）
	code, _ := dbsReq(t, s, http.MethodGet, "10.0.0.9:5000", "", nil)
	if code != http.StatusOK {
		t.Fatalf("远程读库列表应 200，实际 %d", code)
	}
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}
