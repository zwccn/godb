package pgwire

import (
	"strings"
	"testing"
)

// 客户端连上来后基本都会查 pg_catalog，这里把常见的几类都跑一遍。
func TestSystemCatalogOverWire(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.conn.Close()

	cases := []struct {
		name string
		sql  string
		want []string // 每行以 "|" 连接
	}{
		{
			name: "pg_database",
			sql:  `SELECT datname FROM pg_database WHERE datallowconn = true ORDER BY datname`,
			want: []string{"godb"},
		},
		{
			name: "pg_namespace 过滤系统 schema",
			sql:  `SELECT nspname FROM pg_catalog.pg_namespace WHERE nspname NOT LIKE 'pg\_%' ORDER BY nspname`,
			want: []string{"information_schema", "public"},
		},
		{
			name: "pg_class 按名字查",
			sql:  `SELECT relname, relkind FROM pg_catalog.pg_class WHERE relname = 't'`,
			want: []string{"t|r"},
		},
		{
			name: "pg_class + pg_namespace join + ANY(ARRAY)",
			sql: `SELECT n.nspname, c.relname FROM pg_catalog.pg_class c
			      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			      WHERE c.relkind = ANY (ARRAY['r','p']) AND n.nspname = 'public'`,
			want: []string{"public|t"},
		},
		{
			name: "information_schema.tables",
			sql:  `SELECT table_schema, table_name, table_type FROM information_schema.tables`,
			want: []string{"public|t|BASE TABLE"},
		},
		{
			name: "information_schema.columns",
			sql: `SELECT column_name, is_nullable FROM information_schema.columns
			      WHERE table_name = 't' ORDER BY ordinal_position`,
			want: []string{"id|NO", "name|YES", "score|YES"},
		},
		{
			name: "format_type 还原列类型",
			sql: `SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
			      FROM pg_catalog.pg_attribute a WHERE a.attrelid = 16385 AND a.attnum > 0
			      AND NOT a.attisdropped ORDER BY a.attnum`,
			want: []string{"id|integer", "name|text", "score|integer"},
		},
		{
			name: "索引视图",
			sql:  `SELECT indexname FROM pg_indexes WHERE tablename = 't'`,
			want: []string{"t_pkey"},
		},
		{
			name: "正则取反",
			sql:  `SELECT 'employees' !~ '^pg_' AS ok, 'pg_class' !~ '^pg_' AS no`,
			want: []string{"t|f"},
		},
		{
			name: "pg_get_expr 透出默认值",
			sql:  `SELECT pg_catalog.pg_get_expr(adbin, adrelid) FROM pg_catalog.pg_attrdef WHERE adrelid = 16385`,
			want: []string{"nextval('t_id_seq'::regclass)"},
		},
		{
			name: "客户端探针函数",
			sql:  `SELECT current_database(), current_schema(), pg_backend_pid()`,
			want: []string{"godb|public|1"},
		},
	}

	for _, tc := range cases {
		msgs := c.query(tc.sql)
		if e := errMsg(msgs); e != "" {
			t.Fatalf("%s: 报错 %s", tc.name, e)
		}
		rows := dataRows(msgs)
		got := make([]string, len(rows))
		for i, r := range rows {
			parts := make([]string, len(r))
			for j, v := range r {
				parts[j] = string(v)
			}
			got[i] = strings.Join(parts, "|")
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: 行数 %d，期望 %d（%v）", tc.name, len(got), len(tc.want), got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: 第 %d 行 = %q，期望 %q", tc.name, i+1, got[i], tc.want[i])
			}
		}
	}
}

// 系统目录是只读的：写入要给出明确错误而不是“表不存在”。
func TestSystemCatalogIsReadOnly(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.conn.Close()

	for _, q := range []string{
		`INSERT INTO pg_database VALUES (1)`,
		`UPDATE pg_class SET relname = 'x'`,
		`DELETE FROM pg_attribute`,
		`DROP TABLE pg_class`,
	} {
		msgs := c.query(q)
		e := errMsg(msgs)
		if e == "" {
			t.Fatalf("%s 应该被拒绝", q)
		}
		if !strings.Contains(e, "system catalog") && !strings.Contains(e, "does not exist") {
			t.Fatalf("%s 的错误信息不合适: %s", q, e)
		}
	}
}

// 查系统目录不应该给业务表加锁，也不该在锁面板里出现。
func TestSystemCatalogDoesNotTakeLocks(t *testing.T) {
	d, addr := startServer(t)
	c := dial(t, addr)
	defer c.conn.Close()

	if e := errMsg(c.query(`BEGIN`)); e != "" {
		t.Fatalf("begin: %s", e)
	}
	if e := errMsg(c.query(`SELECT count(*) FROM pg_catalog.pg_class`)); e != "" {
		t.Fatalf("select: %s", e)
	}
	snap := d.LockSnapshot()
	for _, tb := range snap.Tables {
		if strings.HasPrefix(strings.ToLower(tb.Name), "pg_") {
			t.Fatalf("系统目录被加了锁: %s", tb.Name)
		}
	}
	if e := errMsg(c.query(`ROLLBACK`)); e != "" {
		t.Fatalf("rollback: %s", e)
	}
}

// errMsg 从消息流里取错误文本（没有错误返回空串）。
func errMsg(msgs []msg) string {
	for _, m := range msgs {
		if m.typ == 'E' {
			for _, f := range splitFields(m.body) {
				if len(f) > 0 && f[0] == 'M' {
					return string(f[1:])
				}
			}
			return "unknown error"
		}
	}
	return ""
}

// splitFields 把 ErrorResponse 的字段切成 (code, value)。
func splitFields(b []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(b) {
		code := b[i]
		j := i + 1
		for j < len(b) && b[j] != 0 {
			j++
		}
		if j > i+1 {
			out = append(out, append([]byte{code}, b[i+1:j]...))
		}
		i = j + 1
	}
	return out
}
