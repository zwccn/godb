package db

import (
	"strings"
	"testing"
)

// ORDER BY 里一个裸名字同时像「输出列名」和「来源列」时，PG 按输出列名解释
// （ORDER BY 文档原话）。Npgsql 加载类型目录的最后一条就吃这条规则：
//
//	SELECT pg_type.oid, enumlabel FROM pg_enum JOIN pg_type ON pg_type.oid = enumtypid
//	ORDER BY oid, enumsortorder
//
// 两张表都有 oid，但输出里那个 oid 是第一列，所以不该报歧义。
// 以前只认显式 Alias，没别名的输出列一律落到来源列解析，于是 Npgsql 连不上。
func setupOrderNameTable(t *testing.T) *DB {
	t.Helper()
	d := newTestDB(t)
	run(t, d, "s", `CREATE TABLE lefty (id int4 PRIMARY KEY, v text);
		INSERT INTO lefty VALUES (1, 'b'), (2, 'a');`)
	run(t, d, "s", `CREATE TABLE righty (id int4 PRIMARY KEY, v text);
		INSERT INTO righty VALUES (10, 'x'), (20, 'y');`)
	return d
}

// 两个来源表都有 id，ORDER BY id 应按输出第 1 列（a.id）排，而不是报歧义。
func TestOrderByPrefersOutputColumnName(t *testing.T) {
	d := setupOrderNameTable(t)
	assertRows(t, run(t, d, "s",
		`SELECT a.id, b.v FROM lefty a CROSS JOIN righty b ORDER BY id DESC, b.v`),
		[]string{"2|x", "2|y", "1|x", "1|y"})
}

// 别名也要优先于来源列：输出里 v 被改名成 id，ORDER BY id 就该按 v 排。
func TestOrderByAliasBeatsSourceColumn(t *testing.T) {
	d := setupOrderNameTable(t)
	assertRows(t, run(t, d, "s", `SELECT id AS k, v AS id FROM lefty ORDER BY id`),
		[]string{"2|a", "1|b"})
}

// 输出里有两个同名列时 PG 报歧义；这里也得报，不能默默取第一个。
func TestOrderByAmbiguousOutputName(t *testing.T) {
	d := setupOrderNameTable(t)
	err := runErr(t, d, "s", `SELECT a.id, b.id FROM lefty a CROSS JOIN righty b ORDER BY id`)
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("错误信息 = %q, want 含 ambiguous", err)
	}
}

// 序号仍是第一优先级，不受影响。
func TestOrderByPositionStillWorks(t *testing.T) {
	d := setupOrderNameTable(t)
	assertRows(t, run(t, d, "s",
		`SELECT a.id, b.v FROM lefty a CROSS JOIN righty b ORDER BY 2, 1`),
		[]string{"1|x", "2|x", "1|y", "2|y"})
}

// 名字出现在表达式里就不算「输出列名」，继续按来源列解析。
func TestOrderByExpressionDoesNotUseOutputName(t *testing.T) {
	d := setupOrderNameTable(t)
	assertRows(t, run(t, d, "s", `SELECT v AS x FROM lefty ORDER BY v || 'z'`),
		[]string{"a", "b"})
}

// 带表限定的名字（ORDER BY a.id）继续走来源列，不当输出列名。
func TestOrderByQualifiedNameUsesSourceColumn(t *testing.T) {
	d := setupOrderNameTable(t)
	assertRows(t, run(t, d, "s",
		`SELECT b.v, a.id FROM lefty a CROSS JOIN righty b ORDER BY a.id DESC, b.v`),
		[]string{"x|2", "y|2", "x|1", "y|1"})
}
