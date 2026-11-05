package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modelReq 直接打路由，这样路径切分（/api/models/{id}/revisions/{n}/restore）
// 也一起被覆盖 —— 手工解析路径最容易在这儿切错。
func modelReq(t *testing.T, s *Server, method, path, body string) (int, []byte) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)
	return rec.Code, rec.Body.Bytes()
}

func mustModel(t *testing.T, s *Server, path string) modelDoc {
	t.Helper()
	code, b := modelReq(t, s, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, code, b)
	}
	var m modelDoc
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return m
}

func mustModels(t *testing.T, s *Server) []modelSummary {
	t.Helper()
	code, b := modelReq(t, s, http.MethodGet, "/api/models", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/models = %d %s", code, b)
	}
	var out struct {
		Models []modelSummary `json:"models"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out.Models
}

func TestModelsCRUD(t *testing.T) {
	s, _, dir := newDBServer(t)

	if list := mustModels(t, s); len(list) != 0 {
		t.Fatalf("新数据目录不该有模型: %+v", list)
	}

	code, b := modelReq(t, s, http.MethodPost, "/api/models", `{"name":"订单模型"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d %s", code, b)
	}
	m := mustModel(t, s, "/api/models/"+mustFirstID(t, s))
	if m.Name != "订单模型" || len(m.Tables) != 0 {
		t.Fatalf("新建的模型应是空的: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "models", m.ID, "model.json")); err != nil {
		t.Fatalf("模型应落在 <data>/models/<id>/model.json: %v", err)
	}

	body := `{"name":"订单模型","tables":[
		{"id":"t1","name":"orders","columns":[
			{"name":"id","type":"INTEGER","pk":true,"notNull":true},
			{"name":"amount","type":"NUMERIC(12,2)"}]},
		{"id":"t2","name":"items","columns":[
			{"name":"id","type":"INTEGER","pk":true},
			{"name":"order_id","type":"INTEGER"}]}],
		"links":[{"id":"l1","from":{"table":"t1","columns":["id"]},
		          "to":{"table":"t2","columns":["order_id"]},"card":"1N"}]}`
	if code, b := modelReq(t, s, http.MethodPost, "/api/models/"+m.ID, body); code != http.StatusOK {
		t.Fatalf("save = %d %s", code, b)
	}
	back := mustModel(t, s, "/api/models/"+m.ID)
	if len(back.Tables) != 2 || len(back.Tables[0].Columns) != 2 || len(back.Links) != 1 {
		t.Fatalf("保存回来的结构不对: %+v", back)
	}
	if back.UpdatedAt == "" {
		t.Fatalf("updatedAt 应被填上")
	}

	if code, _ := modelReq(t, s, http.MethodDelete, "/api/models/"+m.ID, ""); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if list := mustModels(t, s); len(list) != 0 {
		t.Fatalf("删掉之后列表应为空: %+v", list)
	}
}

func mustFirstID(t *testing.T, s *Server) string {
	t.Helper()
	list := mustModels(t, s)
	if len(list) == 0 {
		t.Fatalf("列表是空的")
	}
	return list[0].ID
}

// 模型会直接变成 DDL，所以脏数据必须在进门前洗掉：重名表、重名列、悬空关系、
// 越界坐标都不该落盘。
func TestModelNormalizesInput(t *testing.T) {
	s, _, _ := newDBServer(t)
	id := createModel(t, s, "脏数据")
	body := `{"name":"脏数据","groups":[{"id":"g1","color":9},{"id":"g1"}],
		"tables":[
			{"id":"t1","name":" orders ","x":1e12,"y":3,"group":"nope","columns":[
				{"name":"id","type":"INTEGER"},
				{"name":"ID","type":"BIGINT"},
				{"name":"  ","type":"TEXT"}]},
			{"id":"t2","name":"Orders","columns":[{"name":"id"}]},
			{"id":"t1","name":"third","columns":[{"name":"id","type":""}]}],
		"links":[{"id":"l1","from":{"table":"t1","columns":["id"]},"to":{"table":"ghost","columns":["id"]},"card":"zz"},
		         {"id":"l2","from":{"table":"t1","columns":["nope"]},"to":{"table":"t1","columns":["id"]},"card":"1N"}]}`
	code, b := modelReq(t, s, http.MethodPost, "/api/models/"+id, body)
	if code != http.StatusOK {
		t.Fatalf("save = %d %s", code, b)
	}
	m := mustModel(t, s, "/api/models/"+id)

	if len(m.Tables) != 2 {
		t.Fatalf("表名重复（大小写不敏感）应只留第一张: %+v", m.Tables)
	}
	if m.Tables[0].Name != "orders" {
		t.Fatalf("表名没 trim: %q", m.Tables[0].Name)
	}
	if len(m.Tables[0].Columns) != 1 {
		t.Fatalf("列名重复/空名应被丢掉: %+v", m.Tables[0].Columns)
	}
	if m.Tables[0].X != nil {
		t.Fatalf("越界坐标应被丢掉: %+v", m.Tables[0].X)
	}
	if m.Tables[0].Group != "" {
		t.Fatalf("指向不存在的组应清空: %q", m.Tables[0].Group)
	}
	if len(m.Links) != 0 {
		t.Fatalf("悬空端点的关系应整条丢掉: %+v", m.Links)
	}
	if len(m.Groups) != 1 || m.Groups[0].Color != -1 {
		t.Fatalf("重复组 id 丢掉、越界色号归 -1: %+v", m.Groups)
	}
}

func createModel(t *testing.T, s *Server, name string) string {
	t.Helper()
	code, b := modelReq(t, s, http.MethodPost, "/api/models", `{"name":"`+name+`"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d %s", code, b)
	}
	return mustFirstID(t, s)
}

// 定版 = 把当前副本整份复制成快照；历史版本可以单独删，删了不影响别的版本。
func TestModelRevisions(t *testing.T) {
	s, _, _ := newDBServer(t)
	id := createModel(t, s, "版本")

	save := func(cols string) {
		body := `{"name":"版本","tables":[{"id":"t1","name":"t","columns":[` + cols + `]}]}`
		if code, b := modelReq(t, s, http.MethodPost, "/api/models/"+id, body); code != http.StatusOK {
			t.Fatalf("save = %d %s", code, b)
		}
	}
	revs := func() []modelRev {
		code, b := modelReq(t, s, http.MethodGet, "/api/models/"+id+"/revisions", "")
		if code != http.StatusOK {
			t.Fatalf("list revisions = %d %s", code, b)
		}
		var out struct {
			Revisions []modelRev `json:"revisions"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode revisions: %v", err)
		}
		return out.Revisions
	}

	save(`{"name":"id","type":"INTEGER"}`)
	if code, b := modelReq(t, s, http.MethodPost, "/api/models/"+id+"/revisions", `{"note":"第一版"}`); code != http.StatusOK {
		t.Fatalf("定版 = %d %s", code, b)
	}
	save(`{"name":"id","type":"INTEGER"},{"name":"n","type":"TEXT"}`)
	if code, _ := modelReq(t, s, http.MethodPost, "/api/models/"+id+"/revisions", `{"note":"加了 n"}`); code != http.StatusOK {
		t.Fatalf("第二次定版失败")
	}

	list := revs()
	if len(list) != 2 || list[0].Seq != 2 || list[1].Seq != 1 {
		t.Fatalf("版本应新的在前: %+v", list)
	}
	if list[0].Note != "加了 n" || list[1].Note != "第一版" {
		t.Fatalf("备注没存住: %+v", list)
	}
	if list[1].Tables != 1 {
		t.Fatalf("版本摘要应带表数: %+v", list[1])
	}

	// 取某一版：内容必须是一版时的样子（1 列），不是现在的 2 列
	rev1 := mustModel(t, s, "/api/models/"+id+"/revisions/1")
	if len(rev1.Tables) != 1 || len(rev1.Tables[0].Columns) != 1 {
		t.Fatalf("第 1 版应该是 1 列: %+v", rev1.Tables)
	}

	// 回滚：工作副本变回 1 列，但两个快照都还在
	if code, b := modelReq(t, s, http.MethodPost, "/api/models/"+id+"/revisions/1/restore", ""); code != http.StatusOK {
		t.Fatalf("回滚 = %d %s", code, b)
	}
	cur := mustModel(t, s, "/api/models/"+id)
	if len(cur.Tables[0].Columns) != 1 {
		t.Fatalf("回滚之后应只剩 1 列: %+v", cur.Tables)
	}
	if len(revs()) != 2 {
		t.Fatalf("回滚不该删掉历史版本")
	}

	// 删某一版
	if code, _ := modelReq(t, s, http.MethodDelete, "/api/models/"+id+"/revisions/2", ""); code != http.StatusOK {
		t.Fatalf("删版本失败")
	}
	if left := revs(); len(left) != 1 || left[0].Seq != 1 {
		t.Fatalf("删掉第 2 版后只剩第 1 版: %+v", left)
	}
}

// 从库导入：表、列、主键都要带过来；老侧车里的布局和关系也要跟着搬。
func TestModelsImportFromDB(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	ids := makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER)")
	a, b := ids["a"], ids["b"]

	if code, _ := diagramCall(t, s, http.MethodGet, ""); code != http.StatusOK {
		t.Fatalf("初始化侧车失败")
	}
	layout := `{"version":3,"groups":[{"id":"g1","name":"交易","color":2}],
		"tables":{"` + itoa(a) + `":{"x":40,"y":50,"group":"g1"}},
		"links":[{"id":"m1","from":{"table":` + itoa(a) + `,"columns":["id"]},
		          "to":{"table":` + itoa(b) + `,"columns":["a_id"]},"card":"1N"}]}`
	if code, _ := diagramCall(t, s, http.MethodPost, layout); code != http.StatusOK {
		t.Fatalf("写侧车失败")
	}

	code, resp := modelReq(t, s, http.MethodPost, "/api/models", `{"name":"导入","fromDB":true}`)
	if code != http.StatusOK {
		t.Fatalf("import = %d %s", code, resp)
	}
	var created modelDoc
	if err := json.Unmarshal(resp, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := mustModel(t, s, "/api/models/"+created.ID)

	if len(m.Tables) != 2 {
		t.Fatalf("应有 2 张表: %+v", m.Tables)
	}
	var ta *modelTable
	for i := range m.Tables {
		if m.Tables[i].Name == "a" {
			ta = &m.Tables[i]
		}
	}
	if ta == nil || len(ta.Columns) != 2 {
		t.Fatalf("a 表的两列应带过来: %+v", ta)
	}
	if !ta.Columns[0].PK || ta.Columns[0].Name != "id" {
		t.Fatalf("主键信息丢了: %+v", ta.Columns)
	}
	if ta.X == nil || *ta.X != 40 {
		t.Fatalf("老布局应跟着搬过来: %+v", ta.X)
	}
	if ta.Group != "g1" || len(m.Groups) != 1 {
		t.Fatalf("分组应跟着搬过来: %+v / %+v", ta.Group, m.Groups)
	}
	if len(m.Links) != 1 || m.Links[0].From.Table != ta.ID {
		t.Fatalf("手画的关系应跟着搬过来: %+v", m.Links)
	}
}

// 老用户第一次打开模型列表：把按库的 diagram.json 自动变成一个模型。
func TestModelsMigrateLegacyDiagram(t *testing.T) {
	s, mgr, dir := newDBServer(t)
	makeTables(t, mgr.Default(), "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT)")

	if code, _ := diagramCall(t, s, http.MethodPost,
		`{"version":3,"tables":{}}`); code != http.StatusOK {
		t.Fatalf("写侧车失败")
	}

	list := mustModels(t, s) // 触发迁移
	if len(list) != 1 {
		t.Fatalf("应该迁移出一个模型: %+v", list)
	}
	if list[0].Name != "默认模型" || list[0].Tables != 1 {
		t.Fatalf("迁移结果不对: %+v", list[0])
	}
	// 幂等：再列一次还是 1 个，不会每次都新建一个
	if again := mustModels(t, s); len(again) != 1 || again[0].ID != list[0].ID {
		t.Fatalf("迁移必须幂等: %+v", again)
	}
	if _, err := os.Stat(filepath.Join(dir, "models")); err != nil {
		t.Fatalf("models 目录应已创建: %v", err)
	}
}

// 没摆过图（没有 diagram.json）就别凭空造模型。
func TestModelsNoMigrationWithoutDiagram(t *testing.T) {
	s, _, _ := newDBServer(t)
	if list := mustModels(t, s); len(list) != 0 {
		t.Fatalf("没有 diagram.json 时不该造模型: %+v", list)
	}
}

// 路径里的 id 会拼进文件路径，必须挡住 `../` 这种。
func TestModelRejectsBadID(t *testing.T) {
	s, _, _ := newDBServer(t)
	for _, bad := range []string{"..%2f..%2fetc", "ABC", "a-b", "x"} {
		code, _ := modelReq(t, s, http.MethodGet, "/api/models/"+bad, "")
		if code == http.StatusOK {
			t.Fatalf("非法 id %q 不该返回 200", bad)
		}
	}
}
