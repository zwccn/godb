package db

import (
	"fmt"
	"strings"
	"testing"
)

// 标量子查询是客户端（Navicat / pgAdmin）刷对象树的刚需：
// pg_class 的查询里有 (SELECT count(*) FROM pg_inherits WHERE inhparent = c.oid)。
func TestScalarSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sq (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, s, `INSERT INTO sq VALUES (1, 10), (2, 20), (3, 30)`)

	// 不相关子查询
	assertRows(t, run(t, d, s, `SELECT (SELECT count(*) FROM sq) AS n`), []string{"3"})
	assertRows(t, run(t, d, s, `SELECT (SELECT max(v) FROM sq) - (SELECT min(v) FROM sq) AS spread`), []string{"20"})

	// 空集 → NULL
	assertRows(t, run(t, d, s, `SELECT (SELECT v FROM sq WHERE id = 999) IS NULL AS empty`), []string{"true"})
}

// 相关子查询：子查询引用外层列。
func TestCorrelatedSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dept (id INTEGER PRIMARY KEY, name TEXT)`)
	run(t, d, s, `CREATE TABLE emp (id INTEGER PRIMARY KEY, dept_id INTEGER, name TEXT)`)
	run(t, d, s, `INSERT INTO dept VALUES (1,'A'),(2,'B'),(3,'C')`)
	run(t, d, s, `INSERT INTO emp VALUES (1,1,'x'),(2,1,'y'),(3,2,'z')`)

	// 每个部门有几个人（C 部门 0 个）
	assertRows(t, run(t, d, s, `
		SELECT d.name, (SELECT count(*) FROM emp e WHERE e.dept_id = d.id) AS n
		FROM dept d ORDER BY d.id`), []string{"A|2", "B|1", "C|0"})

	// 相关子查询 + 比较：等价于 EXISTS 的常见写法
	assertRows(t, run(t, d, s, `
		SELECT d.name FROM dept d
		WHERE (SELECT count(*) FROM emp e WHERE e.dept_id = d.id) > 0
		ORDER BY d.id`), []string{"A", "B"})
}

// 标量子查询返回多行必须报错（与 PostgreSQL 一致），不能静默取第一行。
func TestScalarSubqueryMultipleRows(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sq2 (id INTEGER PRIMARY KEY)`)
	run(t, d, s, `INSERT INTO sq2 VALUES (1),(2)`)

	_, err := d.Exec(d.Session(s), `SELECT (SELECT id FROM sq2) AS x`)
	if err == nil {
		t.Fatal("多行子查询应该报错")
	}
	if !strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("错误信息不对: %v", err)
	}
}

// 客户端那条真实的刷表查询（简化自 Navicat），必须能跑通并列出用户表。
func TestCatalogQueryWithSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE mytable (id INTEGER PRIMARY KEY)`)

	res := run(t, d, s, `
		SELECT c.relname, ((SELECT count(*) FROM pg_inherits i WHERE i.inhparent = c.oid) > 0) AS inhtable
		FROM pg_catalog.pg_class c
		LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r'::"char" AND n.nspname = 'public'
		ORDER BY c.relname`)
	names := make([]string, 0, len(res[0].Rows))
	for _, r := range res[0].Rows {
		names = append(names, fmt.Sprint(r[0]))
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mytable") {
		t.Fatalf("应该列出 mytable，实际: %s", joined)
	}
	// 我们不做表继承，inhtable 恒为 false
	for _, r := range res[0].Rows {
		if fmt.Sprint(r[1]) != "false" {
			t.Fatalf("inhtable 应为 false，实际 %v", r[1])
		}
	}
}
