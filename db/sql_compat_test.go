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

// 数组类型后缀：Navicat 统计对象数时会写 c.relkind = ANY ('{r,v,m}'::char[])。
// 类型名后面跟 [] 以前会被当成语法错误（就在 "]" 上报）。
func TestArrayTypeCast(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE atc (id INTEGER PRIMARY KEY)`)

	res := run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_class c
		WHERE c.relkind = ANY ('{r,v,m}'::char[])`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(res[0].Rows))
	}
	// 至少要数到我们自己建的表
	if got := fmt.Sprint(res[0].Rows[0][0]); got == "0" {
		t.Fatal("ANY ('{r,v,m}'::char[]) 没匹配到任何普通表")
	}

	// 带长度参数的数组类型也要能解析
	res = run(t, d, s, `SELECT ARRAY['a','b']::varchar(10)[] AS a`)
	if res[0].Rows[0][0] == nil {
		t.Fatal("varchar(10)[] 转换结果不该是 NULL")
	}
}

// numeric 转文本必须给定点表示。PG 的 numeric 从不用科学计数法，
// 而 godb 内部把 numeric 存成 float64：不管的话 1234567890123.45 会从
// `::text` / CAST 这条路漏成 1.23456789012345e+12，客户端的 decimal 解析直接失败。
// 协议层（pgwire.textValueAs）按列类型也挡了一次，两条路都要有测试。
func TestNumericToTextIsPlainDecimal(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `CREATE TABLE nt (id int primary key, amount numeric)`)
	run(t, d, s, `INSERT INTO nt VALUES (1, 1234567890123.45), (2, -0.001), (3, 0)`)

	cases := []struct{ sql, want string }{
		{`SELECT 1234567890123.45::numeric::text`, "1234567890123.45"},
		{`SELECT amount::text FROM nt WHERE id = 1`, "1234567890123.45"},
		{`SELECT CAST(amount AS text) FROM nt WHERE id = 2`, "-0.001"},
		{`SELECT amount::text FROM nt WHERE id = 3`, "0"},
		// float8 保持 'g'（大数用指数形式，PG 也是这样），别把这条规则扩大化
		{`SELECT (1e20)::float8::text`, "1e+20"},
	}
	for _, c := range cases {
		got := fmt.Sprint(run(t, d, s, c.sql)[0].Rows[0][0])
		if got != c.want {
			t.Fatalf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

// date 转文本只出年月日：内部是 KTime，TextForm 会带 " 00:00:00"，
// 而 PG 的 date 文本就是 2024-01-01。timestamp 仍要保留时间部分。
func TestDateToTextHasNoTimePart(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `CREATE TABLE dt (id int primary key, day date, ts timestamp)`)
	run(t, d, s, `INSERT INTO dt VALUES (1, '2024-01-01', '2024-01-01 08:09:10')`)

	cases := []struct{ sql, want string }{
		{`SELECT day::text FROM dt WHERE id = 1`, "2024-01-01"},
		{`SELECT CAST(day AS text) FROM dt WHERE id = 1`, "2024-01-01"},
		{`SELECT ts::text FROM dt WHERE id = 1`, "2024-01-01 08:09:10"},
		// 不是 date 列就不要截，避免把 timestamp 也砍成日期
		{`SELECT ('2024-01-01 08:09:10'::timestamp)::text`, "2024-01-01 08:09:10"},
		// 控制台走 JSON，不经过 ::text，也要只出年月日
		{`SELECT day FROM dt WHERE id = 1`, "2024-01-01"},
	}
	for _, c := range cases {
		got := fmt.Sprint(run(t, d, s, c.sql)[0].Rows[0][0])
		if got != c.want {
			t.Fatalf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

// 带类型前缀的字面量：DATE '2024-03-05' / TIMESTAMP '…' / TIME '…'。
// 这是 PG 的标准写法，ORM 生成的 SQL（`WHERE ts > TIMESTAMP '2019-01-01'`）常见，
// 之前直接报 syntax error。注意它只在「类型名紧跟字符串」时生效。
func TestTypedStringLiteral(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `CREATE TABLE tl (id int primary key, day date, ts timestamp)`)

	cases := []struct{ sql, want string }{
		{`SELECT DATE '2024-03-05'::text`, "2024-03-05"},
		{`SELECT TIMESTAMP '2024-03-05 06:07:08'::text`, "2024-03-05 06:07:08"},
		{`SELECT TIME '02:03:04'::text`, "02:03:04"},
		// 用在 WHERE 里（最常见的形态）
		{`SELECT count(*) AS n FROM tl WHERE ts > TIMESTAMP '2019-01-01 00:00:00'`, "0"},
		{`SELECT extract(year from DATE '2024-03-05')`, "2024"},
	}
	for _, c := range cases {
		got := fmt.Sprint(run(t, d, s, c.sql)[0].Rows[0][0])
		if got != c.want {
			t.Fatalf("%s = %q, want %q", c.sql, got, c.want)
		}
	}

	// date 仍能当列名用（不能被当成类型前缀吃掉）
	run(t, d, s, `CREATE TABLE tl2 (date text)`)
	run(t, d, s, `INSERT INTO tl2 VALUES ('x')`)
	if got := fmt.Sprint(run(t, d, s, `SELECT date FROM tl2`)[0].Rows[0][0]); got != "x" {
		t.Fatalf("列名 date 被误当类型前缀：%q", got)
	}
}

// DML 里的子查询：`WHERE id IN (SELECT …)` / `SET c = (SELECT …)` / `VALUES ((SELECT …))`。
// 这些位置以前一律报 "subqueries are not supported in this context"（编译上下文没带 planner），
// 而 PG 是允许的 —— EF Core 的 ExecuteDelete/ExecuteUpdate 就靠这个，手写的数据清理更常用。
func TestDMLAllowsSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `CREATE TABLE dq_a (id int primary key, v text)`)
	run(t, d, s, `CREATE TABLE dq_b (id int primary key, v text)`)
	run(t, d, s, `INSERT INTO dq_a VALUES (1,'a'),(2,'b'),(3,'c')`)
	run(t, d, s, `INSERT INTO dq_b VALUES (1,'B1'),(3,'B3')`)

	val := func(sql string) string {
		res := run(t, d, s, sql)
		if len(res[0].Rows) == 0 {
			return "(no rows)"
		}
		return fmt.Sprint(res[0].Rows[0][0])
	}

	// ① UPDATE 的 WHERE 用 IN (子查询)：只改 dq_b 里存在的 id
	if n := run(t, d, s, `UPDATE dq_a SET v = 'hit' WHERE id IN (SELECT id FROM dq_b)`)[0].Affected; n != 2 {
		t.Fatalf("UPDATE … WHERE id IN (子查询) 影响了 %d 行，期望 2", n)
	}
	if got := val(`SELECT v FROM dq_a WHERE id = 2`); got != "b" {
		t.Fatalf("id=2 不该被改到，得到 %q", got)
	}

	// ② UPDATE 的 SET 值是相关子查询（引用本行的列）
	run(t, d, s, `UPDATE dq_a SET v = (SELECT b.v FROM dq_b b WHERE b.id = dq_a.id) WHERE id = 3`)
	if got := val(`SELECT v FROM dq_a WHERE id = 3`); got != "B3" {
		t.Fatalf("SET 标量子查询结果 = %q，期望 B3", got)
	}

	// ③ DELETE 的 WHERE 用 NOT IN (子查询)
	if n := run(t, d, s, `DELETE FROM dq_a WHERE id NOT IN (SELECT id FROM dq_b)`)[0].Affected; n != 1 {
		t.Fatalf("DELETE … NOT IN (子查询) 删了 %d 行，期望 1", n)
	}
	if got := val(`SELECT count(*) FROM dq_a`); got != "2" {
		t.Fatalf("删完应剩 2 行，实际 %q", got)
	}

	// ④ DELETE 的 WHERE 用 EXISTS
	run(t, d, s, `INSERT INTO dq_a VALUES (9,'gone')`)
	if n := run(t, d, s, `DELETE FROM dq_a WHERE EXISTS (SELECT 1 FROM dq_b b WHERE b.id = dq_a.id AND b.v = 'B1')`)[0].Affected; n != 1 {
		t.Fatalf("DELETE … WHERE EXISTS 删了 %d 行，期望 1", n)
	}

	// ⑤ INSERT ... VALUES 里的标量子查询
	run(t, d, s, `INSERT INTO dq_b VALUES ((SELECT max(id) + 1 FROM dq_b), 'B4')`)
	if got := val(`SELECT v FROM dq_b WHERE id = 4`); got != "B4" {
		t.Fatalf("INSERT VALUES (子查询) 插入的 v = %q，期望 B4", got)
	}

	// ⑥ 子查询读的是**本语句要改的表**时，也必须是「改之前」的视角：
	//    按 v='a' 的行去改 v，不能边改边读到新值。
	run(t, d, s, `CREATE TABLE dq_c (id int primary key, v text)`)
	run(t, d, s, `INSERT INTO dq_c VALUES (1,'a'),(2,'a'),(3,'z')`)
	if n := run(t, d, s, `UPDATE dq_c SET v = 'b' WHERE id IN (SELECT id FROM dq_c WHERE v = 'a')`)[0].Affected; n != 2 {
		t.Fatalf("自引用子查询的 UPDATE 影响了 %d 行，期望 2", n)
	}
}
