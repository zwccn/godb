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

// 客户端「设计表」预填类型下拉靠的就是这几个字段，取值必须和真 PG 一致：
// numeric 不能被兜底成 double precision，整数要有 numeric_precision，
// 文本列要挂默认排序规则，pg_type 的数组类型名要是 _int4 这种写法。
// 任何一项对不上，Navicat 都会把列类型降级显示成 (Type) 对象类型。
func TestCatalogTypeMetadataForDesigner(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE tmeta (i int4, n numeric(12,2), v varchar(255), txt text)`)

	type want struct {
		dataType, udt string
		prec, scale   interface{} // nil 表示应为 NULL
		collation     interface{}
	}
	expect := map[string]want{
		"i":   {"integer", "int4", int64(32), int64(0), nil},
		"n":   {"numeric", "numeric", int64(12), int64(2), nil},
		"v":   {"character varying", "varchar", nil, nil, "default"},
		"txt": {"text", "text", nil, nil, "default"},
	}
	for _, r := range run(t, d, s, `
		SELECT column_name, data_type, udt_name, numeric_precision, numeric_scale, collation_name
		FROM information_schema.columns WHERE table_name = 'tmeta'`)[0].Rows {
		name := r[0].(string)
		w := expect[name]
		if r[1] != w.dataType || r[2] != w.udt {
			t.Fatalf("%s 类型显示不对: %v/%v，期望 %v/%v", name, r[1], r[2], w.dataType, w.udt)
		}
		if r[3] != w.prec || r[4] != w.scale {
			t.Fatalf("%s 精度不对: %v/%v，期望 %v/%v", name, r[3], r[4], w.prec, w.scale)
		}
		if r[5] != w.collation {
			t.Fatalf("%s 排序规则不对: %v，期望 %v", name, r[5], w.collation)
		}
	}

	// pg_attribute：文本列挂默认排序规则 100、存储方式是 extended，
	// 整数是 plain。Navicat 的「排序规则」下拉和列存储估计读的是这里。
	for _, r := range run(t, d, s, `
		SELECT a.attname, a.attcollation, a.attstorage
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'tmeta' AND a.attnum > 0 ORDER BY a.attnum`)[0].Rows {
		name := r[0].(string)
		switch name {
		case "txt", "v":
			if fmt.Sprint(r[1]) != "100" || r[2] != "x" {
				t.Fatalf("%s: attcollation/attstorage = %v/%v，期望 100/x", name, r[1], r[2])
			}
		case "i":
			if fmt.Sprint(r[1]) != "0" || r[2] != "p" {
				t.Fatalf("%s: attcollation/attstorage = %v/%v，期望 0/p", name, r[1], r[2])
			}
		}
	}

	// pg_type：数组类型必须是 _int4 这种名字，且 typarray 指过去的 OID 一定存在。
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM pg_type WHERE typname = '_int4' AND typelem = 23`)[0].Rows[0][0]); got != "1" {
		t.Fatalf("pg_type 缺少 _int4（typelem=23），实际 %s 行", got)
	}
	if got := fmt.Sprint(run(t, d, s, `
		SELECT count(*) FROM pg_type t LEFT JOIN pg_type u ON u.oid = t.typarray
		WHERE t.typarray <> 0 AND u.oid IS NULL`)[0].Rows[0][0]); got != "0" {
		t.Fatalf("有 %s 个类型的 typarray 指向不存在的类型", got)
	}

	// pg_settings：Navicat 打开设计表会问 block_size 来算存储。
	if got := fmt.Sprint(run(t, d, s, `SELECT setting FROM pg_settings WHERE name = 'block_size'`)[0].Rows[0][0]); got != "8192" {
		t.Fatalf("pg_settings.block_size = %s，期望 8192", got)
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
