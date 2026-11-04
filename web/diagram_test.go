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
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	body := `{"version":2,
		"groups":[{"id":"g1","name":"主链","color":3},{"id":"g2","name":"空组","color":-1}],
		"tables":{"` + a + `":{"group":"g1","x":100,"y":60},
		          "` + b + `":{"group":"g1"}},
		"links":[{"id":"m1","child":` + b + `,"columns":["a_id"],
		          "parent":` + a + `,"refColumns":["id"],"card":"11"}],
		"overrides":{"fk|` + b + `|a_id":"NM"}}`
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
	if doc.Tables[b].Group != "g1" {
		t.Fatalf("b 的归属没读回来: %+v", doc.Tables[b])
	}
	if len(doc.Groups) != 2 || doc.Groups[0].Name != "主链" || doc.Groups[0].Color != 3 {
		t.Fatalf("分组没读回来: %+v", doc.Groups)
	}
	if len(doc.Links) != 1 || doc.Links[0].Card != "11" || doc.Links[0].Columns[0] != "a_id" {
		t.Fatalf("逻辑关系没读回来: %+v", doc.Links)
	}
	if doc.Overrides["fk|"+b+"|a_id"] != "NM" {
		t.Fatalf("基数标注没读回来: %+v", doc.Overrides)
	}
}

// v1 的「组拥有表」要能读成 v2 的「表拥有组」，而且迁移结果要写回文件 ——
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
	if doc.Version != 2 {
		t.Fatalf("version = %d, want 2", doc.Version)
	}
	for _, k := range []string{a, b} {
		if doc.Tables[k].Group != "g1" {
			t.Fatalf("表 %s 没迁到 g1: %+v", k, doc.Tables[k])
		}
		if _, _, ok := coordOf(doc.Tables[k]); !ok {
			t.Fatalf("表 %s 的坐标在迁移中丢了: %+v", k, doc.Tables[k])
		}
	}

	// 写回过了：文件里应该已经是 v2 的 tables 字段。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["tables"]; !ok {
		t.Fatalf("迁移没写回，文件里还是旧字段: %s", raw)
	}
	if v, _ := onDisk["version"].(float64); int(v) != 2 {
		t.Fatalf("文件版本没更新: %s", raw)
	}
}

// 生命周期剪枝：表被 DROP 之后，它的布局、它参与的逻辑关系、它的基数标注
// 都要消失，而且改动要写回文件。空分组保留 —— 用户建了组还没放表，那也是设计意图。
func TestDiagramPrunesDroppedTables(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d, "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))")
	a, b := itoa(ids["a"]), itoa(ids["b"])

	body := `{"version":2,
		"groups":[{"id":"g1","color":1},{"id":"g2","color":2}],
		"tables":{"` + a + `":{"group":"g1","x":10,"y":20},
		          "` + b + `":{"group":"g1","x":30,"y":400}},
		"links":[{"id":"m1","child":` + b + `,"columns":["a_id"],
		          "parent":` + a + `,"refColumns":["id"],"card":"1N"}],
		"overrides":{"fk|` + b + `|a_id":"11"}}`
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
		t.Fatalf("b 参与了那条逻辑关系，应被剪掉: %+v", doc.Links)
	}
	if len(doc.Overrides) != 0 {
		t.Fatalf("b 的外键没了，基数标注应被剪掉: %+v", doc.Overrides)
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

// 坏数据进门就归一化：超范围坐标丢掉、半个坐标（只有 x）整条丢掉、
// 重复的 link id 只留第一条、非法基数退回 1N、非法的基数标注清掉。
// 这些数据直接决定 DOM 的 left/top，放进去就是白屏。
func TestDiagramNormalizesInput(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))",
		"CREATE TABLE c (id INTEGER PRIMARY KEY)")
	a, b, c := itoa(ids["a"]), itoa(ids["b"]), itoa(ids["c"])

	body := `{"version":2,
		"groups":[{"id":"g1","color":9},{"id":"g1","color":1}],
		"tables":{"` + a + `":{"x":1e12,"y":0},
		          "` + b + `":{"x":50.4,"y":60.6},
		          "` + c + `":{"group":"nope"},
		          "abc":{"x":1,"y":2},
		          "999":{"x":1,"y":2}},
		"links":[{"id":"m1","child":` + b + `,"columns":["a_id"],"parent":` + a + `,"refColumns":["id"],"card":"xx"},
		         {"id":"m1","child":` + b + `,"columns":["a_id"],"parent":` + a + `,"refColumns":["id"],"card":"1N"},
		         {"id":"m2","child":` + b + `,"columns":["a_id","z"],"parent":` + a + `,"refColumns":["id"],"card":"1N"},
		         {"id":"m3","child":0,"columns":["x"],"parent":` + a + `,"refColumns":["id"],"card":"1N"}],
		"overrides":{"fk|` + b + `|a_id":"zz","nope":"1N","fk|` + a + `|id":"11"}}`
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

	if len(doc.Links) != 1 {
		t.Fatalf("重复 id / 列数不匹配 / child=0 的关系都该被丢掉: %+v", doc.Links)
	}
	if doc.Links[0].Card != "1N" {
		t.Fatalf("非法基数应退回 1N: %+v", doc.Links[0])
	}

	// 非法值、非法键、以及「键形状对但库里没有这条外键」全部清掉。
	if len(doc.Overrides) != 0 {
		t.Fatalf("非法标注应被清空: %+v", doc.Overrides)
	}
}

// 表只能属于一个组，归属由 tables[].group 决定，不由组里列了谁决定。
// 空组必须保留 —— 用户建了组还没放表，那也是设计意图。
func TestDiagramKeepsEmptyGroups(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	a := itoa(ids["a"])
	body := `{"version":2,
		"groups":[{"id":"g1","color":1},{"id":"g2","color":2}],
		"tables":{"` + a + `":{"group":"g2","x":1,"y":2}}}`
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

// 指向不存在的组：表保留，归属归零（不能凭空造出一个组）。
func TestDiagramDropsUnknownGroupRef(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	a := itoa(ids["a"])
	body := `{"version":2,"tables":{"` + a + `":{"group":"ghost","x":1,"y":2}}}`
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

// 逻辑层可以否认一条边。这里验三件事：
//   - 形状认得、库里对得上的键要存下来（外键边 fk|… 和折叠出来的 j|…）；
//   - 形状不认得、或者指向已经不存在的外键/表的键，进门就清掉；
//   - 被隐藏的那条外键真的没了（DROP 之后）时，隐藏记录跟着失效并写回文件，
//     否则它会一直留在文件里，某天恰好和另一条边同键就开始乱隐藏。
func TestDiagramHidesEdges(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	d := mgr.Default()
	ids := makeTables(t, d,
		"CREATE TABLE a (id INTEGER PRIMARY KEY)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))",
		"CREATE TABLE t (a_id INTEGER REFERENCES a(id), b_id INTEGER REFERENCES b(id), PRIMARY KEY (a_id, b_id))",
		"CREATE TABLE c (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id))")
	a, b, c := itoa(ids["a"]), itoa(ids["b"]), itoa(ids["c"])
	tid := itoa(ids["t"])

	body := `{"version":2,"hidden":[
		"fk|` + b + `|a_id",
		"j|` + tid + `",
		"fk|` + c + `|a_id",
		"nope|1",
		"fk|` + a + `|id",
		"j|999"]}`
	code, doc := diagramCall(t, s, http.MethodPost, body)
	if code != http.StatusOK {
		t.Fatalf("post code = %d", code)
	}
	got := map[string]bool{}
	for _, k := range doc.Hidden {
		got[k] = true
	}
	if len(got) != 3 {
		t.Fatalf("应留下三条隐藏记录，实际 %+v", doc.Hidden)
	}
	for _, k := range []string{"fk|" + b + "|a_id", "j|" + tid, "fk|" + c + "|a_id"} {
		if !got[k] {
			t.Fatalf("合法隐藏记录被丢了: %s in %+v", k, doc.Hidden)
		}
	}
	for _, k := range []string{"nope|1", "fk|" + a + "|id", "j|999"} {
		if got[k] {
			t.Fatalf("非法/失效的隐藏记录应被清掉: %s", k)
		}
	}

	// 把 c 删掉：它的外键没了，那条隐藏记录也该跟着消失。
	if _, err := d.Exec(d.Session("drop"), "DROP TABLE c"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, doc = diagramCall(t, s, http.MethodGet, "")
	if len(doc.Hidden) != 2 {
		t.Fatalf("外键没了，隐藏记录应被剪掉: %+v", doc.Hidden)
	}
	for _, k := range doc.Hidden {
		if k == "fk|"+c+"|a_id" {
			t.Fatalf("失效的隐藏记录没剪掉: %+v", doc.Hidden)
		}
	}

	raw, err := os.ReadFile(filepath.Join(d.Dir(), diagramFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk diagramDoc
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Hidden) != 2 {
		t.Fatalf("剪枝结果没写回文件: %s", raw)
	}
}
