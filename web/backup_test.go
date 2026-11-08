package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 备份 → 恢复的**往返**：导出的 .sql 灌回一个空库，数据和序列都要对得上。
//
// 这条以前完全没有：只验过「导出的文本里有 CREATE TABLE / INSERT / setval」，
// 但没验过「拿它灌回去真的能灌、灌完和原来一样」。而这是整站唯一一个
// 「出事了还能救回来」的功能 —— 文本对不对和能不能恢复是两件事。
func TestBackupRestoreRoundTrip(t *testing.T) {
	s, mgr, _ := newDBServer(t)
	h := s.Routes()
	local := "127.0.0.1:5000"

	run := func(sql string, ck *http.Cookie) *queryResp {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"sql": sql})
		rec := call(t, h, http.MethodPost, "/api/query", local, string(b), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q = %d %s", sql, rec.Code, rec.Body.String())
		}
		var out queryResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("query %q 响应解析失败: %v", sql, err)
		}
		return &out
	}

	// 1) 源库：自增主键（靠序列）、唯一约束、DEFAULT、文本、以及中文
	for _, q := range []string{
		`CREATE TABLE bkr_users (id SERIAL, email TEXT UNIQUE, amount NUMERIC(12,2) DEFAULT 0)`,
		`INSERT INTO bkr_users (email, amount) VALUES ('a@x', 12.50)`,
		`INSERT INTO bkr_users (email) VALUES ('b@x')`,
		`CREATE TABLE bkr_logs (id INT4 PRIMARY KEY, body TEXT)`,
		`INSERT INTO bkr_logs VALUES (1, 'hello'), (2, '中文，带逗号和引号 ''x''')`,
	} {
		if r := run(q, nil); r.Error != nil {
			t.Fatalf("造数据 %q 失败: %s", q, r.Error.Message)
		}
	}

	// 2) 导出
	rec := call(t, h, http.MethodGet, "/api/backup?name=bk", local, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup = %d %s", rec.Code, rec.Body.String())
	}
	dump := rec.Body.String()
	for _, want := range []string{"CREATE TABLE", "bkr_users", "bkr_logs", "INSERT INTO", "setval"} {
		if !strings.Contains(dump, want) {
			t.Fatalf("备份里缺 %q：\n%s", want, head(dump, 400))
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "sql") {
		t.Fatalf("Content-Type 应为 sql，实际 %q", ct)
	}

	// 3) 空库 + 切过去（cookie 就是前端切库用的那个）
	if _, err := mgr.Create("bkr2"); err != nil {
		t.Fatalf("create bkr2: %v", err)
	}
	ck := &http.Cookie{Name: "godb_db", Value: "bkr2"}

	// 4) 把备份原样灌回去 —— 这就是前端「从备份恢复」做的事（整段丢给 /api/query）
	if r := run(dump, ck); r.Error != nil {
		t.Fatalf("恢复失败: %s（第 %d 条语句）\n%s", r.Error.Message, r.Error.Stmt, head(dump, 400))
	}

	// 5) 数据一致：行数、值、默认值
	got := run(`SELECT id, email, amount FROM bkr_users ORDER BY id`, ck)
	if got.Error != nil {
		t.Fatalf("查恢复后的表: %s", got.Error.Message)
	}
	rows := rowsOf(t, got)
	if len(rows) != 2 {
		t.Fatalf("恢复后应有 2 行，实际 %d：%v", len(rows), rows)
	}
	if rows[0][1] != "a@x" || !strings.HasPrefix(asStr(rows[0][2]), "12.5") {
		t.Fatalf("第一行内容不对: %v", rows[0])
	}
	// DEFAULT 0 的那行：备份里 INSERT 只给了 email
	if asStr(rows[1][2]) != "0.00" && asStr(rows[1][2]) != "0" {
		t.Fatalf("默认值应恢复成 0，实际 %v", rows[1][2])
	}

	// 6) 文本原样（中文 + 逗号 + 引号，最容易在导出时被转义搞坏）
	logs := run(`SELECT body FROM bkr_logs ORDER BY id`, ck)
	lr := rowsOf(t, logs)
	if len(lr) != 2 || !strings.Contains(asStr(lr[1][0]), "中文") || !strings.Contains(asStr(lr[1][0]), "'x'") {
		t.Fatalf("文本没原样恢复: %v", lr)
	}

	// 7) 序列：setval 没写的话，新插入会撞主键（1 已被占用）
	ins := run(`INSERT INTO bkr_users (email) VALUES ('c@x')`, ck)
	if ins.Error != nil {
		t.Fatalf("恢复后插入失败（序列没对齐？）: %s", ins.Error.Message)
	}
	next := run(`SELECT max(id) FROM bkr_users`, ck)
	nr := rowsOf(t, next)
	if len(nr) != 1 || asStr(nr[0][0]) != "3" {
		t.Fatalf("序列应从 3 继续，实际 %v", nr)
	}
}

// 备份要能拒绝非 GET：它是下载接口，只读。
func TestBackupMethodAndEmpty(t *testing.T) {
	s, _, _ := newDBServer(t)
	h := s.Routes()
	if rec := call(t, h, http.MethodPost, "/api/backup", "127.0.0.1:5000", "{}", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/backup 应 405，实际 %d", rec.Code)
	}
	// 空库也要能导出（只有头注释，没有表）——恢复空库是合法场景
	rec := call(t, h, http.MethodGet, "/api/backup", "127.0.0.1:5000", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "godb") {
		t.Fatalf("空库导出 = %d %q", rec.Code, head(rec.Body.String(), 120))
	}
}

func rowsOf(t *testing.T, r *queryResp) [][]interface{} {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("查询失败: %s", r.Error.Message)
	}
	if len(r.Results) == 0 {
		t.Fatalf("没有结果集")
	}
	return r.Results[len(r.Results)-1].Rows
}

func asStr(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = httptest.NewRecorder
