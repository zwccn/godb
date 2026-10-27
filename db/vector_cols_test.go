package db

import (
	"fmt"
	"testing"
)

// `'表名'::regclass` 在 PostgreSQL 里解析成 pg_class.oid（整数）。
// 我们以前把它当文本，于是 `conrelid = 't'::regclass` 直接报
// operator does not exist: integer = text —— psql 和客户端都常用这种写法。
func TestRegclassCast(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE rgc (id int4 PRIMARY KEY, name text)`)

	res := run(t, d, s, `
		SELECT 'rgc'::regclass = (SELECT oid FROM pg_class WHERE relname = 'rgc') AS same,
		       'int4'::regtype AS typoid`)
	row := res[0].Rows[0]
	if got := fmt.Sprint(row[0]); got != "true" {
		t.Fatalf("regclass 与 pg_class.oid 不一致（%s）", got)
	}
	if got := fmt.Sprint(row[1]); got != "23" {
		t.Fatalf("'int4'::regtype = %s，期望 23", got)
	}

	// 直接用 regclass 做等值条件（这就是报错的那个写法）
	res = run(t, d, s, `SELECT conname FROM pg_constraint WHERE conrelid = 'rgc'::regclass`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应命中 1 个约束，实际 %d", len(res[0].Rows))
	}

	// 带 schema 前缀、带引号也要能解析
	res = run(t, d, s, `SELECT 'public.rgc'::regclass = '"public"."rgc"'::regclass AS same`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "true" {
		t.Fatalf("带 schema/引号解析不一致（%s）", got)
	}
}

// pg_constraint.conkey 在 PostgreSQL 里是 int2[]，文本形式带花括号 {1}；
// pg_index.indclass 是 oidvector（空格分隔、不带括号），指向 pg_opclass。
// 以前 conkey 少了花括号、indclass 恒为 0 —— 客户端「约束/索引」页会读不出来。
func TestConstraintIndexVectorColumns(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE vc (id int4 PRIMARY KEY)`)

	res := run(t, d, s, `SELECT conkey FROM pg_constraint WHERE conrelid = 'vc'::regclass`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "{1}" {
		t.Fatalf("conkey = %s，期望 {1}（int2[] 的字面量形式）", got)
	}

	res = run(t, d, s, `SELECT indkey, indclass, indoption FROM pg_index WHERE indrelid = 'vc'::regclass`)
	row := res[0].Rows[0]
	if got := fmt.Sprint(row[0]); got != "1" {
		t.Fatalf("indkey = %s，期望 1（int2vector，空格分隔）", got)
	}
	if got := fmt.Sprint(row[1]); got != "1978" {
		t.Fatalf("indclass = %s，期望 1978（int4_ops）", got)
	}
	if got := fmt.Sprint(row[2]); got != "0" {
		t.Fatalf("indoption = %s，期望 0", got)
	}

	// 操作符类表必须有内容，否则客户端的下拉框是空的
	res = run(t, d, s, `SELECT opcname FROM pg_opclass WHERE oid = 1978`)
	if len(res[0].Rows) != 1 || fmt.Sprint(res[0].Rows[0][0]) != "int4_ops" {
		t.Fatalf("pg_opclass 里没有 int4_ops：%v", res[0].Rows)
	}
}
