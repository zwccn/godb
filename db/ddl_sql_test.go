package db

import (
	"fmt"
	"strings"
	"testing"

	"godb/sql"
)

// sqlCopyFor 造一个 COPY 语句对象，方便直接调 db 层的导入导出。
func sqlCopyFor(table string, to bool) *sql.Copy {
	return &sql.Copy{Table: table, To: to}
}

// string_agg / unnest：psql 的 \d 依赖这两个（拼列名数组、再把数组摊成行）。
func TestStringAggAndUnnest(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sa (g text, v text)`)
	run(t, d, s, `INSERT INTO sa VALUES ('a','x'), ('a','y'), ('b','z'), ('a',NULL)`)

	// 分组拼接，NULL 要跳过（PostgreSQL 行为）
	res := run(t, d, s, `SELECT g, string_agg(v, ',') AS cols FROM sa GROUP BY g ORDER BY g`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应有 2 组，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "x,y" {
		t.Fatalf("a 组拼接 = %q，期望 x,y", got)
	}
	if got := fmt.Sprint(res[0].Rows[1][1]); got != "z" {
		t.Fatalf("b 组拼接 = %q，期望 z", got)
	}

	// 不分组的聚合
	res = run(t, d, s, `SELECT string_agg(g, '|') FROM sa`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "a|a|b|a" {
		t.Fatalf("全表拼接 = %q", got)
	}

	// unnest：数组文本摊成多行
	res = run(t, d, s, `SELECT u FROM unnest('{a,b,c}') u`)
	if len(res[0].Rows) != 3 {
		t.Fatalf("unnest 应出 3 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[1][0]); got != "b" {
		t.Fatalf("第二行 = %q，期望 b", got)
	}

	// 空数组
	res = run(t, d, s, `SELECT u FROM unnest('{}') u`)
	if len(res[0].Rows) != 0 {
		t.Fatalf("空数组应出 0 行，实际 %d", len(res[0].Rows))
	}

	// 组合用法（psql \d 的形状）：数组 → 行 → 再拼回去
	res = run(t, d, s, `SELECT string_agg(u, ',') FROM unnest('{p,q}') u`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "p,q" {
		t.Fatalf("往返拼接 = %q，期望 p,q", got)
	}
}

// CREATE TABLE AS SELECT
func TestCreateTableAs(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE src (id int4 PRIMARY KEY, name text, qty int4)`)
	run(t, d, s, `INSERT INTO src VALUES (1,'a',10), (2,'b',20), (3,'c',30)`)

	res := run(t, d, s, `CREATE TABLE dst AS SELECT id, qty * 2 AS double_qty FROM src WHERE qty > 10`)
	if got := res[0].Tag; got != "SELECT 2" {
		t.Fatalf("tag = %q，期望 SELECT 2", got)
	}

	// 结构来自查询：列名与类型都要对
	res = run(t, d, s, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'dst' ORDER BY ordinal_position`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("dst 应有 2 列，实际 %d", len(res[0].Rows))
	}
	if fmt.Sprint(res[0].Rows[0][0]) != "id" || fmt.Sprint(res[0].Rows[1][0]) != "double_qty" {
		t.Fatalf("列名不对: %v", res[0].Rows)
	}

	// 数据要在
	res = run(t, d, s, `SELECT id, double_qty FROM dst ORDER BY id`)
	if len(res[0].Rows) != 2 || fmt.Sprint(res[0].Rows[0][1]) != "40" {
		t.Fatalf("dst 数据不对: %v", res[0].Rows)
	}

	// 重名列自动加序号（SELECT 允许重名，表不允许）
	run(t, d, s, `CREATE TABLE dst2 AS SELECT 1 AS x, 2 AS x`)
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM dst2`)[0].Rows[0][0]); got != "1" {
		t.Fatalf("dst2 应有 1 行，实际 %s", got)
	}

	// 表已存在要报错
	if err := runErr(t, d, s, `CREATE TABLE dst AS SELECT 1`); err == nil {
		t.Fatal("重名建表应该报错")
	}
}

// psql 的 \d 发的就是这种查询：pg_attribute + format_type + string_agg(unnest(...))
func TestDescribeTableShape(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dt (id int4 NOT NULL, name varchar(10), flag bool DEFAULT true)`)

	res := run(t, d, s, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod) AS type, a.attnotnull
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'dt' AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attname`)
	if len(res[0].Rows) != 3 {
		t.Fatalf("应描述出 3 列，实际 %d 行（%v）", len(res[0].Rows), res[0].Rows)
	}
	var names []string
	for _, r := range res[0].Rows {
		names = append(names, fmt.Sprint(r[0]))
	}
	if strings.Join(names, ",") != "flag,id,name" {
		t.Fatalf("列名 = %v", names)
	}
}

// WITH（CTE）：物化一次、按名字引用，可以被子查询和后续 CTE 复用。
func TestCommonTableExpression(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ord (id int4 PRIMARY KEY, cust text, amt int4)`)
	run(t, d, s, `INSERT INTO ord VALUES (1,'a',100), (2,'a',50), (3,'b',300), (4,'b',20)`)

	// 基本 CTE + 聚合
	res := run(t, d, s, `
		WITH big AS (SELECT cust, amt FROM ord WHERE amt >= 100)
		SELECT cust, sum(amt) AS total FROM big GROUP BY cust ORDER BY cust`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应有 2 组，实际 %d", len(res[0].Rows))
	}
	if fmt.Sprint(res[0].Rows[0][1]) != "100" || fmt.Sprint(res[0].Rows[1][1]) != "300" {
		t.Fatalf("聚合结果不对: %v", res[0].Rows)
	}

	// 引用两次（物化后复用，不会算两遍）
	res = run(t, d, s, `
		WITH t AS (SELECT count(*) AS n FROM ord)
		SELECT a.n, b.n FROM t a JOIN t b ON a.n = b.n`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "4" {
		t.Fatalf("CTE 复用结果 = %s，期望 4", got)
	}

	// 列名列表：WITH t(x) AS (...)
	res = run(t, d, s, `WITH t(x) AS (SELECT amt FROM ord) SELECT sum(x) FROM t`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "470" {
		t.Fatalf("WITH t(x) 求和 = %s，期望 470", got)
	}

	// 多个 CTE，后面的能引用前面的
	res = run(t, d, s, `
		WITH a AS (SELECT cust, amt FROM ord),
		     b AS (SELECT cust, sum(amt) AS total FROM a GROUP BY cust)
		SELECT cust FROM b WHERE total > 200`)
	if len(res[0].Rows) != 1 || fmt.Sprint(res[0].Rows[0][0]) != "b" {
		t.Fatalf("链式 CTE 结果不对: %v", res[0].Rows)
	}

	// CTE 里的集合运算
	res = run(t, d, s, `
		WITH u AS (SELECT cust FROM ord WHERE amt > 200 UNION SELECT 'z')
		SELECT count(*) FROM u`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("CTE 内 UNION 结果 = %s，期望 2", got)
	}

	// 引用不存在的 CTE 要报 relation does not exist
	if err := runErr(t, d, s, `WITH a AS (SELECT 1) SELECT * FROM nope`); err == nil {
		t.Fatal("引用不存在的表应该报错")
	}
	// 递归 CTE 明确不支持
	if err := runErr(t, d, s, `WITH RECURSIVE r AS (SELECT 1) SELECT * FROM r`); err == nil {
		t.Fatal("WITH RECURSIVE 应该报不支持")
	}
}

// 不带括号的关键字常量（current_user 等）：psql 与客户端都会这么写。
func TestBareKeywordFunctions(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	for _, q := range []string{
		`SELECT current_user AS u`,
		`SELECT current_schema AS s`,
		`SELECT current_date AS d`,
		`SELECT session_user AS u`,
	} {
		res := run(t, d, s, q)
		if len(res[0].Rows) != 1 || res[0].Rows[0][0] == nil {
			t.Fatalf("%s 应返回一行非 NULL，实际 %v", q, res[0].Rows)
		}
	}
}

// COPY 文本协议：tab 分隔、\N 为 NULL、转义往返一致，且约束照常生效。
func TestCopyInOut(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE cpx (id int4 PRIMARY KEY, name text, qty int4)`)

	data := []byte("1\talpha\t10\n2\tbeta\t20\n3\t\\N\t30\n")
	n, err := d.CopyIn(d.Session(s), sqlCopyFor("cpx", false), data)
	if err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	if n != 3 {
		t.Fatalf("导入 %d 行，期望 3", n)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT name FROM cpx WHERE id = 3`)[0].Rows[0][0]); got != "<nil>" {
		// 输出可能是 nil 的字符串形式，这里只确认它是 NULL
		if run(t, d, s, `SELECT name FROM cpx WHERE id = 3`)[0].Rows[0][0] != nil {
			t.Fatalf("id=3 的 name 应该是 NULL，实际 %q", got)
		}
	}

	lines, cnt, err := d.CopyOut(d.Session(s), sqlCopyFor("cpx", true))
	if err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	if cnt != 3 || len(lines) != 3 {
		t.Fatalf("导出 %d 行 / %d 条，期望 3", cnt, len(lines))
	}
	// NULL 要写成 \N
	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, "3\t") && strings.Contains(l, `\N`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("导出的 NULL 应为 \\N，实际: %v", lines)
	}

	// 值里带 tab / 换行 / 反斜杠时，导出要转义，再导入要还原
	if _, err := d.CopyIn(d.Session(s), sqlCopyFor("cpx", false), []byte("9\ta\\tb\t1\n")); err != nil {
		t.Fatalf("导入带转义的值: %v", err)
	}
	lines, _, err = d.CopyOut(d.Session(s), sqlCopyFor("cpx", true))
	if err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	escaped := false
	for _, l := range lines {
		if strings.HasPrefix(l, "9\t") && strings.Contains(l, `\t`) {
			escaped = true
		}
	}
	if !escaped {
		t.Fatalf("值里的 tab 应转义成 \\t，实际: %v", lines)
	}

	// 主键冲突要被拦住
	if _, err := d.CopyIn(d.Session(s), sqlCopyFor("cpx", false), []byte("1\tdup\t1\n")); err == nil {
		t.Fatal("COPY 也要做唯一性校验")
	}
}

// CREATE INDEX / DROP INDEX：登记式索引——能建能删、系统目录能看到，
// 但不加速查询（只有主键走哈希索引）。
func TestCreateDropIndex(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ix_t (id int4 PRIMARY KEY, name text, qty int4)`)

	res := run(t, d, s, `CREATE INDEX ix_t_name ON ix_t (name)`)
	if res[0].Tag != "CREATE INDEX" {
		t.Fatalf("tag = %q", res[0].Tag)
	}
	// 结果里要明确说明不加速，避免误解
	if !strings.Contains(res[0].Message, "不会用它加速") {
		t.Fatalf("应提示不加速，实际 %q", res[0].Message)
	}

	// 系统目录里能看到这条索引
	res = run(t, d, s, `
		SELECT i.indexrelid, c.relname, i.indisunique, i.indisprimary, i.indkey
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
		WHERE t.relname = 'ix_t' AND NOT i.indisprimary`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应有 1 条用户索引，实际 %d", len(res[0].Rows))
	}
	r := res[0].Rows[0]
	if fmt.Sprint(r[1]) != "ix_t_name" || fmt.Sprint(r[2]) != "false" || fmt.Sprint(r[3]) != "false" {
		t.Fatalf("索引元数据不对: %v", r)
	}
	if fmt.Sprint(r[4]) != "2" {
		t.Fatalf("indkey 应为 2（name 是第 2 列），实际 %v", r[4])
	}

	// 用户索引不该在 pg_constraint 里凭空出现
	res = run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'ix_t' AND c.conname = 'ix_t_name'`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("用户索引不该有约束行，实际 %s", got)
	}

	// UNIQUE 索引要标出来
	run(t, d, s, `CREATE UNIQUE INDEX ix_t_qty ON ix_t (qty)`)
	res = run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'ix_t_qty' AND i.indisunique`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1" {
		t.Fatalf("UNIQUE 索引应标记 indisunique")
	}

	// 重名 / 不存在的列 / 不存在的表都要报错
	if err := runErr(t, d, s, `CREATE INDEX ix_t_name ON ix_t (qty)`); err == nil {
		t.Fatal("索引重名应该报错")
	}
	if err := runErr(t, d, s, `CREATE INDEX bad ON ix_t (nope)`); err == nil {
		t.Fatal("不存在的列应该报错")
	}
	if err := runErr(t, d, s, `CREATE INDEX bad2 ON nope (id)`); err == nil {
		t.Fatal("不存在的表应该报错")
	}

	// 删除
	run(t, d, s, `DROP INDEX ix_t_name`)
	res = run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'ix_t_name'`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("索引应已删除")
	}
	run(t, d, s, `DROP INDEX IF EXISTS not_there`)
	if err := runErr(t, d, s, `DROP INDEX not_there`); err == nil {
		t.Fatal("删不存在的索引应该报错")
	}
}

// 索引定义要能持久化（catalog.json）。
func TestIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	for _, q := range []string{
		`CREATE TABLE ixs (id int4 PRIMARY KEY, name text)`,
		`CREATE INDEX ixs_name ON ixs (name)`,
	} {
		if _, err := d.Exec(sess, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	s2 := d2.Session("s")
	res, err := d2.Exec(s2, `
		SELECT count(*) FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'ixs_name'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1" {
		t.Fatalf("重启后索引应还在，实际 count=%s", got)
	}
}
