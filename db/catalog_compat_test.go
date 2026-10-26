package db

import (
	"fmt"
	"testing"
)

// Navicat 的「设计表」会 SELECT pg_constraint.consrc（PG 12 已移除的列）
// 以及 information_schema.columns 的类型名。缺列或类型名错都会让它打不开表。
func TestCatalogColumnsForTableDesigner(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dc (id int4 PRIMARY KEY, name varchar(255))`)

	// consrc 必须存在（值恒为 NULL），否则整条约束查询失败
	res := run(t, d, s, `
		SELECT c.conname, c.contype, c.consrc, c.conindid, c.connoinherit
		FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'dc'`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应该有 1 个主键约束，实际 %d", len(res[0].Rows))
	}
	if got := res[0].Rows[0][1]; got != "p" {
		t.Fatalf("contype = %v，期望 p", got)
	}

	// information_schema.columns 的类型名要按列自己记录的类型走，
	// 不能因为内部统一用 int64 就一律报 bigint。
	type row struct{ dataType, udt string }
	got := map[string]row{}
	for _, r := range run(t, d, s, `
		SELECT column_name, data_type, udt_name
		FROM information_schema.columns WHERE table_name = 'dc'`)[0].Rows {
		got[r[0].(string)] = row{dataType: r[1].(string), udt: r[2].(string)}
	}
	if got["id"] != (row{"integer", "int4"}) {
		t.Fatalf("id 的类型显示不对: %+v", got["id"])
	}
	if got["name"] != (row{"character varying", "varchar"}) {
		t.Fatalf("name 的类型显示不对: %+v", got["name"])
	}

	// 定长/变长长度要能读出来，否则设计表里长度列是空的
	run(t, d, s, `CREATE TABLE dc2 (a bigint, b double precision, c boolean, d timestamp)`)
	for _, r := range run(t, d, s, `
		SELECT column_name, data_type, udt_name
		FROM information_schema.columns WHERE table_name = 'dc2'`)[0].Rows {
		name := r[0].(string)
		want := map[string]row{
			"a": {"bigint", "int8"},
			"b": {"double precision", "float8"},
			"c": {"boolean", "bool"},
			"d": {"timestamp without time zone", "timestamp"},
		}[name]
		if r[1].(string) != want.dataType || r[2].(string) != want.udt {
			t.Fatalf("%s: 得到 %v/%v，期望 %v/%v", name, r[1], r[2], want.dataType, want.udt)
		}
	}
}

// 系统列（tableoid / ctid / xmin…）在 PostgreSQL 里每张表都隐含存在，
// 客户端会用它们做 join 条件。它们不在列定义里，也不能出现在 SELECT * 中。
func TestSystemColumns(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sysc (id INTEGER PRIMARY KEY)`)

	// 显式引用不报错
	res := run(t, d, s, `SELECT c.relname, c.tableoid FROM pg_catalog.pg_class c LIMIT 3`)
	if len(res[0].Rows) == 0 {
		t.Fatal("应该有行")
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "0" {
		t.Fatalf("tableoid 应恒为 0，实际 %s", got)
	}

	// 客户端索引页的 join：dep.classid = ci.tableoid
	run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_index i
		LEFT JOIN pg_catalog.pg_class ci ON ci.oid = i.indexrelid
		LEFT JOIN pg_catalog.pg_depend dep ON dep.classid = ci.tableoid AND dep.objid = ci.oid`)

	// SELECT * 里不应该多出系统列
	res = run(t, d, s, `SELECT * FROM pg_catalog.pg_namespace LIMIT 1`)
	for _, c := range res[0].Columns {
		if c.Name == "tableoid" || c.Name == "ctid" {
			t.Fatalf("SELECT * 不应该包含系统列 %s", c.Name)
		}
	}
}

// pg_proc.proisagg / proiswindow 在 PG 11 被 prokind 取代，但客户端函数列表
// 仍在 SELECT 它们 —— 和 consrc、relhasoids 是同一类问题。
func TestProcLegacyColumns(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	res := run(t, d, s, `
		SELECT p.proname, p.prokind, p.proisagg, p.proiswindow
		FROM pg_catalog.pg_proc p LIMIT 3`)
	if len(res[0].Rows) == 0 {
		t.Fatal("pg_proc 应该有内置函数行")
	}
	if got := fmt.Sprint(res[0].Rows[0][2]); got != "false" {
		t.Fatalf("proisagg = %s，期望 false", got)
	}
	for _, c := range res[0].Columns {
		if c.Name == "proisagg" || c.Name == "proiswindow" {
			return
		}
	}
	t.Fatal("结果里缺 proisagg / proiswindow 列")
}
