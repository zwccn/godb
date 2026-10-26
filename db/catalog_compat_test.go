package db

import "testing"

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
