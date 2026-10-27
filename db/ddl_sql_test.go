package db

import (
	"fmt"
	"strings"
	"testing"
)

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
