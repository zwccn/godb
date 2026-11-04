package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"godb/db"
)

func diagramCall(t *testing.T, s *Server, method, body string) (int, diagramDoc) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/diagram", nil)
	} else {
		r = httptest.NewRequest(method, "/api/diagram", strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)
	var out diagramDoc
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func makeTables(t *testing.T, d *db.DB, stmts ...string) map[string]int64 {
	t.Helper()
	sess := d.Session("diagram-test")
	for _, st := range stmts {
		if _, err := d.Exec(sess, st); err != nil {
			t.Fatalf("exec %q: %v", st, err)
		}
	}
	ids := map[string]int64{}
	for _, ti := range d.Tables() {
		ids[ti.Name] = ti.ID
	}
	return ids
}

// 没有任何布局时，接口要返回一份空文档而不是 404 —— 前端打开关系图
// 第一件事就是拉它，404 会被当成「关系图坏了」。
func TestDiagramEmpty(t *testing.T) {
	s, _, _ := newDBServer(t)
	code, doc := diagramCall(t, s, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if doc.Version != diagramVersion || len(doc.Cards) != 0 || len(doc.Groups) != 0 {
		t.Fatalf("unexpected doc: %+v", doc)
	}
}

// 存进去再读回来要一模一样，并且真的落在数据目录里。
func TestDiagramRoundTrip(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))")

	body := `{"version":1,
		"cards":{"` + itoa(ids["a"]) + `":{"x":100,"y":60},
		          "` + itoa(ids["b"]) + `":{"x":100,"y":300}},
		"groups":[{"id":"g1","name":"主链","color":3,
		            "tables":[` + itoa(ids["a"]) + `,` + itoa(ids["b"]) + `]}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(mgr.Default().Dir(), diagramFile)); err != nil {
		t.Fatalf("侧车文件没落盘: %v", err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if doc.Cards[itoa(ids["a"])].X != 100 || doc.Cards[itoa(ids["b"])].Y != 300 {
		t.Fatalf("坐标没读回来: %+v", doc.Cards)
	}
	if len(doc.Groups) != 1 || doc.Groups[0].Name != "主链" || doc.Groups[0].Color != 3 {
		t.Fatalf("分组没读回来: %+v", doc.Groups)
	}
}

// 生命周期剪枝：表被 DROP 之后，布局里对应那张表的坐标和成员都要消失，
// 成员被剪空的组整组删掉，而且改动要写回文件（不能每次打开都重算一遍）。
func TestDiagramPrunesDroppedTables(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d, "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY)")
	body := `{"version":1,
		"cards":{"` + itoa(ids["a"]) + `":{"x":10,"y":20},"` + itoa(ids["b"]) + `":{"x":30,"y":40}},
		"groups":[{"id":"g1","color":1,"tables":[` + itoa(ids["a"]) + `,` + itoa(ids["b"]) + `]},
		          {"id":"g2","color":2,"tables":[` + itoa(ids["b"]) + `]}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}

	if _, err := d.Exec(d.Session("drop"), "DROP TABLE b"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if len(doc.Cards) != 1 {
		t.Fatalf("应只剩 a 的坐标，实际 %+v", doc.Cards)
	}
	if _, ok := doc.Cards[itoa(ids["b"])]; ok {
		t.Fatalf("b 已 DROP，坐标还在")
	}
	// g1 只剩 a；g2 的成员全没了，整组应被删掉。
	if len(doc.Groups) != 1 || len(doc.Groups[0].Tables) != 1 {
		t.Fatalf("剪枝结果不对: %+v", doc.Groups)
	}

	// 写回过了：直接读文件也应该是剪过的样子。
	b, err := os.ReadFile(filepath.Join(d.Dir(), diagramFile))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var onDisk diagramDoc
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(onDisk.Cards) != 1 || len(onDisk.Groups) != 1 {
		t.Fatalf("文件里没剪: %+v", onDisk)
	}
}

// 坏数据进门就归一化：NaN/超范围坐标丢掉、重复认领的表只留第一个、色号夹回 0..7。
// 这些数据直接决定 DOM 的 left/top，放进去就是白屏。
func TestDiagramNormalizesInput(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY)")
	body := `{"version":1,
		"cards":{"` + itoa(ids["a"]) + `":{"x":1e12,"y":0},
		          "` + itoa(ids["b"]) + `":{"x":50.4,"y":60.6},
		          "abc":{"x":1,"y":2},
		          "999":{"x":1,"y":2}},
		"groups":[{"id":"g1","color":9,"tables":[` + itoa(ids["a"]) + `,` + itoa(ids["b"]) + `]},
		          {"id":"g2","color":1,"tables":[` + itoa(ids["b"]) + `]},
		          {"id":"g1","color":1,"tables":[` + itoa(ids["a"]) + `]}]}`
	code, doc := diagramCall(t, s, http.MethodPost, body)
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if len(doc.Cards) != 1 {
		t.Fatalf("只该留下 b 的坐标: %+v", doc.Cards)
	}
	if c := doc.Cards[itoa(ids["b"])]; c.X != 50 || c.Y != 61 {
		t.Fatalf("坐标没四舍五入: %+v", c)
	}
	if len(doc.Groups) != 1 {
		t.Fatalf("重复 id / 重复认领的组应被丢掉: %+v", doc.Groups)
	}
	if doc.Groups[0].Color != -1 {
		t.Fatalf("越界色号应归成 -1: %+v", doc.Groups[0])
	}
	if len(doc.Groups[0].Tables) != 2 {
		t.Fatalf("第一个组应保留两张表: %+v", doc.Groups[0])
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
