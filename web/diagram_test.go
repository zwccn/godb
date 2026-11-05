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

func coordOf(c diagramCard) (float64, float64, bool) {
	if c.X == nil || c.Y == nil {
		return 0, 0, false
	}
	return *c.X, *c.Y, true
}

// 没有任何布局时，接口要返回一份空文档而不是 404 —— 前端打开模型图
// 第一件事就是拉它，404 会被当成「模型图坏了」。
func TestDiagramEmpty(t *testing.T) {
	s, _, _ := newDBServer(t)
	code, doc := diagramCall(t, s, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if doc.Version != diagramVersion {
		t.Fatalf("version = %d, want %d", doc.Version, diagramVersion)
	}
	if len(doc.Tables) != 0 || len(doc.Groups) != 0 || len(doc.Links) != 0 {
		t.Fatalf("unexpected doc: %+v", doc)
	}
}

// 存进去再读回来要一模一样，并且真的落在数据目录里。
// 这里同时覆盖「有归属没坐标」（移到分组但没拖过）—— 坐标必须能是空的，
// 用 0 代替会把卡片摞到画布左上角。
func TestDiagramRoundTrip(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER)")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	body := `{"version":3,
		"groups":[{"id":"g1","name":"主链","color":3},{"id":"g2","name":"空组","color":-1}],
		"tables":{"` + a + `":{"group":"g1","x":100,"y":60},
		          "` + b + `":{"group":"g1","hidden":true}},
		"links":[{"id":"m1",
		          "from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},
		          "card":"1N"}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(mgr.Default().Dir(), diagramFile)); err != nil {
		t.Fatalf("侧车文件没落盘: %v", err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	x, y, ok := coordOf(doc.Tables[a])
	if !ok || x != 100 || y != 60 {
		t.Fatalf("a 的坐标没读回来: %+v", doc.Tables[a])
	}
	if _, _, ok := coordOf(doc.Tables[b]); ok {
		t.Fatalf("b 没有坐标，不该读出一个位置: %+v", doc.Tables[b])
	}
	if doc.Tables[b].Group != "g1" || !doc.Tables[b].Hidden {
		t.Fatalf("b 的归属/收起标记没读回来: %+v", doc.Tables[b])
	}
	if len(doc.Groups) != 2 || doc.Groups[0].Name != "主链" || doc.Groups[0].Color != 3 {
		t.Fatalf("分组没读回来: %+v", doc.Groups)
	}
	if len(doc.Links) != 1 {
		t.Fatalf("关系没读回来: %+v", doc.Links)
	}
	ln := doc.Links[0]
	if ln.Card != "1N" || ln.From.Table != ids["a"] || ln.To.Table != ids["b"] ||
		ln.From.Columns[0] != "id" || ln.To.Columns[0] != "a_id" {
		t.Fatalf("关系两端没读回来: %+v", ln)
	}
}

// v1 的「组拥有表」要能读成「表拥有组」，而且迁移结果要写回文件 ——
// 不然每次打开都要重新折腾一遍旧格式。
func TestDiagramMigratesV1(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d, "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY)")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	v1 := `{"version":1,
		"cards":{"` + a + `":{"x":10,"y":20},"` + b + `":{"x":10,"y":260}},
		"groups":[{"id":"g1","name":"主链","color":3,"tables":[` + a + `,` + b + `]}]}`
	path := filepath.Join(d.Dir(), diagramFile)
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if doc.Version != diagramVersion {
		t.Fatalf("version = %d, want %d", doc.Version, diagramVersion)
	}
	for _, k := range []string{a, b} {
		if doc.Tables[k].Group != "g1" {
			t.Fatalf("表 %s 没迁到 g1: %+v", k, doc.Tables[k])
		}
		if _, _, ok := coordOf(doc.Tables[k]); !ok {
			t.Fatalf("表 %s 的坐标在迁移中丢了: %+v", k, doc.Tables[k])
		}
	}

	// 写回过了：文件里应该已经是 v3 的 tables 字段。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["cards"]; ok {
		t.Fatalf("迁移没写回，文件里还是旧字段: %s", raw)
	}
	if v, _ := onDisk["version"].(float64); int(v) != diagramVersion {
		t.Fatalf("文件版本没更新: %s", raw)
	}
}

// v2 的边是「child 指向 parent」，card 说的是**子端**多不多；
// v3 的边是「From 看向 To」，card 说的是 **From 端**是不是一。
// 这条用例专门盯那个方向翻转：迁错了，每一对 1:N 都会画反。
func TestDiagramMigratesV2Links(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d, "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER, x_id INTEGER)")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	v2 := `{"version":2,
		"tables":{"` + b + `":{"x":10,"y":20}},
		"links":[{"id":"m1","child":` + b + `,"columns":["a_id"],
		          "parent":` + a + `,"refColumns":["id"],"card":"1N"},
		         {"id":"m2","child":` + b + `,"columns":["x_id"],
		          "parent":` + a + `,"refColumns":["id"],"card":"11"}],
		"overrides":{"fk|` + b + `|a_id":"NM"},
		"hidden":["fk|` + b + `|a_id"]}`
	path := filepath.Join(d.Dir(), diagramFile)
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if len(doc.Links) != 2 {
		t.Fatalf("两条手画的关系都该迁过来: %+v", doc.Links)
	}
	byID := map[string]diagramLink{}
	for _, ln := range doc.Links {
		byID[ln.ID] = ln
	}
	// 1:N 的子端是多，v3 的 From 必须是「一」那一端 —— 也就是 a。
	if ln := byID["m1"]; ln.From.Table != ids["a"] || ln.To.Table != ids["b"] ||
		ln.From.Columns[0] != "id" || ln.To.Columns[0] != "a_id" || ln.Card != "1N" {
		t.Fatalf("1:N 的端点没翻对: %+v", ln)
	}
	// 1:1 两端对称，方向保持 v2 的子→父。
	if ln := byID["m2"]; ln.From.Table != ids["b"] || ln.To.Table != ids["a"] || ln.Card != "11" {
		t.Fatalf("1:1 的端点不该被翻: %+v", ln)
	}

	// v2 的基数标注和隐藏记录没有对应概念，迁移时丢掉，不该留在文件里。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["overrides"]; ok {
		t.Fatalf("v2 的 overrides 应被丢弃: %s", raw)
	}
	if _, ok := onDisk["hidden"]; ok {
		t.Fatalf("v2 的 hidden 应被丢弃: %s", raw)
	}
}

// 生命周期剪枝：表被 DROP 之后，它的布局和它参与的关系都要消失，
// 而且改动要写回文件。空分组保留 —— 用户建了组还没放表，那也是设计意图。
func TestDiagramPrunesDroppedTables(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d, "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER)")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	body := `{"version":3,
		"groups":[{"id":"g1","color":1},{"id":"g2","color":2}],
		"tables":{"` + a + `":{"group":"g1","x":10,"y":20},
		          "` + b + `":{"group":"g1","x":30,"y":400}},
		"links":[{"id":"m1","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"1N"}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}

	if _, err := d.Exec(d.Session("drop"), "DROP TABLE b"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if len(doc.Tables) != 1 {
		t.Fatalf("应只剩 a 的布局，实际 %+v", doc.Tables)
	}
	if _, ok := doc.Tables[b]; ok {
		t.Fatalf("b 已 DROP，布局还在")
	}
	if len(doc.Links) != 0 {
		t.Fatalf("b 参与了那条关系，应被剪掉: %+v", doc.Links)
	}
	// 两个组都还在：g1 只剩 a，g2 本来就是空的。
	if len(doc.Groups) != 2 {
		t.Fatalf("分组不该因为空掉就消失: %+v", doc.Groups)
	}

	raw, err := os.ReadFile(filepath.Join(d.Dir(), diagramFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk diagramDoc
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Tables) != 1 || len(onDisk.Links) != 0 || len(onDisk.Groups) != 2 {
		t.Fatalf("文件里没剪干净: %+v", onDisk)
	}
}

// 关系指向的列被 DROP 掉之后，这条关系已经无所指：留着它，前端会把线
// 画到卡片外面去。整条剪掉，而不是留一半。
func TestDiagramPrunesDroppedColumns(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d,
		"CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER, keep_id INTEGER)")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	body := `{"version":3,
		"links":[{"id":"gone","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"1N"},
		         {"id":"keep","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["keep_id"]},"card":"1N"}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	if _, err := d.Exec(d.Session("drop"), "ALTER TABLE b DROP COLUMN a_id"); err != nil {
		t.Fatalf("drop column: %v", err)
	}

	_, doc := diagramCall(t, s, http.MethodGet, "")
	if len(doc.Links) != 1 || doc.Links[0].ID != "keep" {
		t.Fatalf("指向已删列的关系应被剪掉，另一条要留: %+v", doc.Links)
	}
}

// 坏数据进门就归一化：超范围坐标丢掉、半个坐标（只有 x）整条丢掉、
// 重复的 link id 只留第一条、非法基数退回 1N、列数不等的端点整条丢掉。
// 这些数据直接决定 DOM 的 left/top 和连线的落点，放进去就是白屏或飞线。
func TestDiagramNormalizesInput(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER, c_id INTEGER)",
		"CREATE TABLE c (id INTEGER PRIMARY KEY)")
	a, b, c := itoa(ids["a"]), itoa(ids["b"]), itoa(ids["c"])

	body := `{"version":3,
		"groups":[{"id":"g1","color":9},{"id":"g1","color":1}],
		"tables":{"` + a + `":{"x":1e12,"y":0},
		          "` + b + `":{"x":50.4,"y":60.6},
		          "` + c + `":{"group":"nope"},
		          "abc":{"x":1,"y":2},
		          "999":{"x":1,"y":2}},
		"links":[{"id":"m1","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"xx"},
		         {"id":"m1","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"NM"},
		         {"id":"m2","from":{"table":` + a + `,"columns":["id","id"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"1N"},
		         {"id":"m3","from":{"table":0,"columns":["x"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"1N"},
		         {"id":"m4","from":{"table":` + a + `,"columns":["id"]},
		          "to":{"table":` + a + `,"columns":["id"]},"card":"1N"},
		         {"id":"m5","from":{"table":` + a + `,"columns":["ghost"]},
		          "to":{"table":` + b + `,"columns":["a_id"]},"card":"1N"}]}`
	code, doc := diagramCall(t, s, http.MethodPost, body)
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}

	if len(doc.Groups) != 1 {
		t.Fatalf("重复 id 的组应被丢掉: %+v", doc.Groups)
	}
	if doc.Groups[0].Color != -1 {
		t.Fatalf("越界色号应归成 -1: %+v", doc.Groups[0])
	}

	if _, _, ok := coordOf(doc.Tables[a]); ok {
		t.Fatalf("超范围坐标应被丢掉: %+v", doc.Tables[a])
	}
	x, y, ok := coordOf(doc.Tables[b])
	if !ok || x != 50 || y != 61 {
		t.Fatalf("坐标没四舍五入: %+v", doc.Tables[b])
	}
	if _, _, ok := coordOf(doc.Tables[c]); ok {
		t.Fatalf("没写过坐标的表不该有位置: %+v", doc.Tables[c])
	}
	if doc.Tables[c].Group != "" {
		t.Fatalf("指向不存在的组应归成未分组: %+v", doc.Tables[c])
	}
	if _, ok := doc.Tables["abc"]; ok {
		t.Fatalf("非数字键应被丢掉")
	}
	if _, ok := doc.Tables["999"]; ok {
		t.Fatalf("不存在的表 ID 应被剪掉")
	}

	// 只剩 m1：m2 列数不等、m3 一端没有表、m4 同表同列自连、
	// m5 指向不存在的列（剪枝阶段）。
	if len(doc.Links) != 1 || doc.Links[0].ID != "m1" {
		t.Fatalf("只有 m1 该留下: %+v", doc.Links)
	}
	if doc.Links[0].Card != "1N" {
		t.Fatalf("重复 id 应留第一条，那条的非法基数退回 1N: %+v", doc.Links[0])
	}
}

// 同表不同列是合法的自引用（employees.manager_id → employees.id），
// 剪枝和归一化都不能把它当自环删掉。
func TestDiagramKeepsSelfReference(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(),
		"CREATE TABLE emp (id INTEGER PRIMARY KEY, manager_id INTEGER)")
	e := itoa(ids["emp"])
	body := `{"version":3,
		"links":[{"id":"m1","from":{"table":` + e + `,"columns":["manager_id"]},
		          "to":{"table":` + e + `,"columns":["id"]},"card":"1N"}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	_, doc := diagramCall(t, s, http.MethodGet, "")
	if len(doc.Links) != 1 {
		t.Fatalf("自引用关系被误删了: %+v", doc.Links)
	}
}

// 空组必须保留 —— 用户建了组还没放表，那也是设计意图。
func TestDiagramKeepsEmptyGroups(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	a := itoa(ids["a"])
	body := `{"version":3,
		"groups":[{"id":"g1","color":1},{"id":"g2","color":2}],
		"tables":{"` + a + `":{"group":"g2","hiden":false,"x":1,"y":2}}}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	_, doc := diagramCall(t, s, http.MethodGet, "")
	if doc.Tables[a].Group != "g2" {
		t.Fatalf("归属应听 tables[].group: %+v", doc.Tables[a])
	}
	if len(doc.Groups) != 2 {
		t.Fatalf("两个组都该保留（空组不删）: %+v", doc.Groups)
	}
}

// 空组也要有落脚点：刚建的组里一张表都没有，没有坐标就拖不动 ——
// 拖完一渲染框又回到自动布局的位置，表现成「这个组拖不走」。
// 规则和卡片坐标一样：半个坐标（只有 x）整条丢掉，越界的丢掉，其余四舍五入。
func TestDiagramKeepsEmptyGroupPosition(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	body := `{"version":3,"groups":[
		{"id":"g1","color":1,"x":320.4,"y":-40.6},
		{"id":"g2","color":2,"x":10},
		{"id":"g3","color":3,"x":1e12,"y":5}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	code, doc := diagramCall(t, s, http.MethodGet, "")
	if code != http.StatusOK || len(doc.Groups) != 3 {
		t.Fatalf("三个组都该留下: code=%d %+v", code, doc.Groups)
	}
	if doc.Groups[0].X == nil || doc.Groups[0].Y == nil ||
		*doc.Groups[0].X != 320 || *doc.Groups[0].Y != -41 {
		t.Fatalf("组坐标没四舍五入或丢了: %+v", doc.Groups[0])
	}
	for _, g := range doc.Groups[1:] {
		if g.X != nil || g.Y != nil {
			t.Fatalf("半个坐标 / 越界坐标应整条丢掉: %+v", g)
		}
	}
}

// 指向不存在的组：表保留，归属归零（不能凭空造出一个组）。
func TestDiagramDropsUnknownGroupRef(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	a := itoa(ids["a"])
	body := `{"version":3,"tables":{"` + a + `":{"group":"ghost","x":1,"y":2}}}`
	if code, _ := diagramCall(t, s, http.MethodPost, body); code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	_, doc := diagramCall(t, s, http.MethodGet, "")
	if doc.Tables[a].Group != "" {
		t.Fatalf("不存在的组应归成未分组: %+v", doc.Tables[a])
	}
	if len(doc.Groups) != 0 {
		t.Fatalf("不该凭空造出一个组: %+v", doc.Groups)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
