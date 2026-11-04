package db

import (
	"fmt"
	"strings"
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

// CTE 里的向量列要能读回真实数据。
//
// Value 里只有槽位号，投影层靠列上的 TableID/ColID 才能定位 .vec 文件。
// CTE 物化时如果不透传这两个，`WITH c AS (SELECT v FROM t) SELECT v FROM c`
// 会原样漏出内部占位 `[vector:0]`（实测过 —— 看着像有值，其实是引用）。
func TestVectorColumnThroughCTE(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE t_ctevec (id INTEGER, v vector(3))`)
	run(t, d, s, `INSERT INTO t_ctevec VALUES (1,'[1,2,3]'),(2,'[4,5,6]')`)

	res := run(t, d, s, `WITH c AS (SELECT id, v FROM t_ctevec) SELECT v FROM c ORDER BY id`)
	if len(res) == 0 || len(res[0].Rows) != 2 {
		t.Fatalf("CTE 应返回 2 行: %+v", res)
	}
	got := res[0].Rows
	if fmt.Sprint(got[0][0]) != "[1,2,3]" || fmt.Sprint(got[1][0]) != "[4,5,6]" {
		t.Fatalf("CTE 里的向量 = %v / %v，期望 [1,2,3] / [4,5,6]", got[0][0], got[1][0])
	}

	// 列改名也要能读回（WITH c(x) AS ...）
	res = run(t, d, s, `WITH c(i, x) AS (SELECT id, v FROM t_ctevec) SELECT x FROM c ORDER BY i`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "[1,2,3]" {
		t.Fatalf("改名后的向量 = %s，期望 [1,2,3]", got)
	}
	if strings.Contains(fmt.Sprint(res[0].Rows[0][0]), "vector:") {
		t.Fatalf("向量列漏出了内部引用: %v", res[0].Rows[0][0])
	}
}

// MIN/MAX 作用在向量列上一律拒绝，而且是在规划期就拒绝。
//
// 历史：这里曾经能建向量索引，索引键存的是槽位号，取端点 = 按插入顺序挑一行，
// 于是 MIN(v) 会「成功」并返回 [vector:0] —— 有索引比没索引更容易给出错答案。
// 现在两道门：规划期（buildAggSpec）直接拒绝 MIN/MAX(vector)；向量列也不允许
// 建索引（键是按列值编码的，槽位号编码出来的键没有任何语义）。
func TestVectorMinMaxRefusedWithIndex(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE t_mmvec (id INTEGER, v vector(3))`)
	run(t, d, s, `INSERT INTO t_mmvec VALUES (1,'[1,2,3]'),(2,'[4,5,6]')`)

	// 向量列上建不了索引，快路径的入口就被堵死了。
	if err := runErr(t, d, s, `CREATE INDEX t_mmvec_v ON t_mmvec (v)`); !strings.Contains(err.Error(), "no default operator class") {
		t.Fatalf("向量索引应被拒绝: %v", err)
	}

	for _, fn := range []string{"MIN", "MAX"} {
		err := runErr(t, d, s, `SELECT `+fn+`(v) FROM t_mmvec`)
		if err == nil {
			t.Fatalf("%s(v) 应该报错（向量没有可比顺序），却成功了", fn)
		}
		if !strings.Contains(err.Error(), fn+"()") {
			t.Fatalf("%s(v) 的报错不对: %v", fn, err)
		}
	}

	// 数字列的 min/max 仍然走快路径（别为了修向量把正常的也关了）
	res := run(t, d, s, `SELECT MIN(id) FROM t_mmvec`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1" {
		t.Fatalf("MIN(id) = %s，期望 1", got)
	}
}
