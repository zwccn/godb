package db

import (
	"fmt"
	"testing"
)

// Navicat 导出的建表脚本长得很有特点：类型带 pg_catalog 前缀、每列都带
// COLLATE "pg_catalog"."default"、标识符全用双引号。少认一个 COLLATE，
// 整条 CREATE 就失败 —— 而这时前面的 DROP TABLE 已经生效了，表就没了。
func TestNavicatCreateTableScript(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `
		DROP TABLE IF EXISTS "public"."emp_export";
		CREATE TABLE "public"."emp_export" (
			"id" "pg_catalog"."int4" NOT NULL DEFAULT 0,
			"name" "pg_catalog"."text" COLLATE "pg_catalog"."default" NOT NULL,
			"dept_id" "pg_catalog"."int4",
			"salary" "pg_catalog"."numeric" NOT NULL DEFAULT 0,
			"hired" "pg_catalog"."timestamp",
			"active" "pg_catalog"."bool" DEFAULT TRUE
		)`)

	res := run(t, d, s, `
		SELECT column_name, data_type, is_nullable, coalesce(column_default, '-')
		FROM information_schema.columns
		WHERE table_name = 'emp_export' ORDER BY ordinal_position`)
	if len(res[0].Rows) != 6 {
		t.Fatalf("应有 6 列，实际 %d", len(res[0].Rows))
	}

	want := []struct{ name, typ, nullable, dflt string }{
		{"id", "integer", "NO", "0"},
		{"name", "text", "NO", "-"},
		{"dept_id", "integer", "YES", "-"},
		{"salary", "numeric", "NO", "0"},
		{"hired", "timestamp without time zone", "YES", "-"},
		{"active", "boolean", "YES", "TRUE"},
	}
	for i, w := range want {
		row := res[0].Rows[i]
		got := []string{
			fmt.Sprint(row[0]), fmt.Sprint(row[1]),
			fmt.Sprint(row[2]), fmt.Sprint(row[3]),
		}
		if got[0] != w.name || got[1] != w.typ || got[2] != w.nullable || got[3] != w.dflt {
			t.Fatalf("第 %d 列 = %v，期望 %s/%s/%s/%s", i+1, got, w.name, w.typ, w.nullable, w.dflt)
		}
	}

	// COLLATE 出现在表达式位置时也不能报错
	run(t, d, s, `SELECT "name" FROM "public"."emp_export" ORDER BY "name" COLLATE "C"`)
}

// psql 的 \d 和 pg_dump 用的是 SQL 标准写法，和普通函数调用不一样。
func TestPostgresClientSyntax(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE pcs (id INTEGER PRIMARY KEY, tag varchar(20))`)
	run(t, d, s, `INSERT INTO pcs VALUES (1, 'alpha'), (2, 'beta')`)

	// substring(s FOR n) / substring(s FROM a FOR b)
	res := run(t, d, s, `SELECT substring('abcdef' for 3) AS a, substring('abcdef' from 2 for 2) AS b`)
	row := res[0].Rows[0]
	if fmt.Sprint(row[0]) != "abc" || fmt.Sprint(row[1]) != "bc" {
		t.Fatalf("substring 标准写法 = %v/%v，期望 abc/bc", row[0], row[1])
	}

	// OPERATOR(schema.op)：psql 用它做正则匹配
	res = run(t, d, s, `SELECT count(*) FROM pcs WHERE tag OPERATOR(pg_catalog.~) '^a'`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1" {
		t.Fatalf("OPERATOR(pg_catalog.~) 命中 %s 行，期望 1", got)
	}

	// ARRAY(SELECT ...)：把子查询的每一行收成 '{a,b}' 数组文本
	res = run(t, d, s, `SELECT ARRAY(SELECT tag FROM pcs ORDER BY id) AS arr`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "{alpha,beta}" {
		t.Fatalf("ARRAY(SELECT ...) = %s，期望 {alpha,beta}", got)
	}
	// 空结果给空数组，不是 NULL
	res = run(t, d, s, `SELECT ARRAY(SELECT tag FROM pcs WHERE id > 100) AS arr`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "{}" {
		t.Fatalf("空 ARRAY(SELECT ...) = %s，期望 {}", got)
	}
}
