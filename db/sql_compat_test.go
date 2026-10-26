package db

import (
	"fmt"
	"testing"
)

// Navicat 打开「设计表」的触发器页会发一条极长的 information_schema 查询，
// 里面用到了我们最初不支持的一串语法：
//
//	(n.nspname)::information_schema.sql_identifier      带 schema 的类型名
//	((t.tgtype)::integer & 1) = 1                        位运算
//	E'.{35,} WHEN \((.+)\) EXECUTE PROCEDURE'            转义字符串
//	(SELECT rm.m[1] FROM regexp_matches(...) rm(m))      集合函数 + 数组下标
//	(((SELECT 4,'INSERT' UNION ALL …) …)) em(num, text)  集合运算 + 列别名
//	information_schema.triggered_update_columns          缺这张视图整条查询就挂
//
// 这些用例逐条钉住上面的能力，避免以后改解析器时悄悄退化。
func TestSetOperationAndFromSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	// UNION ALL 保留重复，UNION 去重，EXCEPT 做差
	res := run(t, d, s, `SELECT 1 AS x UNION ALL SELECT 1 UNION ALL SELECT 2`)
	if len(res[0].Rows) != 3 {
		t.Fatalf("UNION ALL 应返回 3 行，实际 %d", len(res[0].Rows))
	}
	res = run(t, d, s, `SELECT 1 AS x UNION SELECT 1 UNION SELECT 2`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("UNION 去重后应返回 2 行，实际 %d", len(res[0].Rows))
	}
	res = run(t, d, s, `SELECT 1 AS x UNION ALL SELECT 2 EXCEPT SELECT 1`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("EXCEPT 后应剩 1 行，实际 %d", len(res[0].Rows))
	}

	// FROM 子查询 + 列别名（触发器查询里的 em(num, text) 就是这个写法）
	res = run(t, d, s, `
		SELECT em.num, em.ev
		FROM (SELECT 4, 'INSERT' UNION ALL SELECT 8, 'DELETE') em(num, ev)
		ORDER BY em.num`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应返回 2 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "INSERT" {
		t.Fatalf("第一行第二列 = %s，期望 INSERT", got)
	}
	// 列名要按别名表里的名字暴露，否则 em.num 解析不出来
	names := []string{res[0].Columns[0].Name, res[0].Columns[1].Name}
	if names[0] != "num" || names[1] != "ev" {
		t.Fatalf("列名 = %v，期望 [num ev]", names)
	}
}

func TestTableFunctionsAndSubscript(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	res := run(t, d, s, `SELECT g FROM generate_series(1, 3) g`)
	if len(res[0].Rows) != 3 {
		t.Fatalf("generate_series 应返回 3 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[2][0]); got != "3" {
		t.Fatalf("第三行 = %s，期望 3", got)
	}

	// 集合函数 + 列定义列表 + 数组下标（触发器查询的写法）
	res = run(t, d, s, `
		SELECT m[1] FROM regexp_matches('abc WHEN (x>0) EXECUTE PROCEDURE f()',
			E'.{3,4} WHEN \\((.+)\\) EXECUTE PROCEDURE') rm(m)`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("regexp_matches 应返回 1 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "x>0" {
		t.Fatalf("捕获组 = %s，期望 x>0", got)
	}
}

func TestBitwiseAndFunctions(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	res := run(t, d, s, `SELECT 6 & 4 AS a, 4 | 1 AS b, 5 # 1 AS c, 1 + 2 & 1 AS d`)
	row := res[0].Rows[0]
	for i, want := range []string{"4", "5", "4", "1"} {
		if got := fmt.Sprint(row[i]); got != want {
			t.Fatalf("第 %d 列 = %s，期望 %s", i+1, got, want)
		}
	}

	// 1 + 2 & 1 必须按 (1+2) & 1 算：位运算优先级低于加减
	res = run(t, d, s, `SELECT position('WHEN', 'abc WHEN (x)') AS p, length('abcd') AS n`)
	row = res[0].Rows[0]
	if fmt.Sprint(row[0]) != "5" || fmt.Sprint(row[1]) != "4" {
		t.Fatalf("position/length = %v/%v，期望 5/4", row[0], row[1])
	}
}

// 带 schema 的类型名、information_schema 的域类型、缺省的视图 —— 三者缺一
// 客户端就会在「设计表」里报 syntax error 或 type does not exist。
func TestInformationSchemaCompat(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ist (id int4 PRIMARY KEY, name varchar(20))`)

	res := run(t, d, s, `
		SELECT (n.nspname)::information_schema.sql_identifier AS schema_name,
		       (1)::pg_catalog.int4 AS v
		FROM pg_catalog.pg_namespace n
		WHERE (n.nspname)::information_schema.sql_identifier = 'public'`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应命中 public 一行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "public" {
		t.Fatalf("schema = %s，期望 public", got)
	}

	// 这张视图必须存在（恒空），客户端的触发器查询 LEFT JOIN 它
	res = run(t, d, s, `SELECT count(*) FROM information_schema.triggered_update_columns`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("triggered_update_columns 应为空，实际 %s", got)
	}

	// E'...' 转义字符串：反斜杠要保留给正则用
	res = run(t, d, s, `SELECT E'a\\(b\\)' AS s`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != `a\(b\)` {
		t.Fatalf("E 字符串 = %s，期望 a\\(b\\)", got)
	}
}
