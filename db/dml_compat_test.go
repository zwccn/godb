package db

import (
	"fmt"
	"testing"
)

// RETURNING 是应用层拿自增主键的标准做法，也是 ORM 的刚需。
func TestInsertReturning(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE r (id serial PRIMARY KEY, name text NOT NULL, qty int4)`)

	// 单行：返回自增 id 与默认推导出来的值
	res := run(t, d, s, `INSERT INTO r (name, qty) VALUES ('a', 5) RETURNING id, name`)
	if len(res) != 1 {
		t.Fatalf("应该只有 1 条结果，实际 %d", len(res))
	}
	r := res[0]
	if len(r.Columns) != 2 || r.Columns[0].Name != "id" || r.Columns[1].Name != "name" {
		t.Fatalf("返回列不对: %+v", r.Columns)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("应该返回 1 行，实际 %d", len(r.Rows))
	}
	if got := fmt.Sprint(r.Rows[0][0]); got != "1" {
		t.Fatalf("RETURNING id = %s，期望 1", got)
	}
	if got := fmt.Sprint(r.Rows[0][1]); got != "a" {
		t.Fatalf("RETURNING name = %s，期望 a", got)
	}
	// 标签仍是 INSERT 0 n，不能因为 RETURNING 变成 SELECT
	if r.Tag != "INSERT 0 1" {
		t.Fatalf("tag = %q，期望 INSERT 0 1", r.Tag)
	}

	// 多行 RETURNING：每插一行返回一行
	res = run(t, d, s, `INSERT INTO r (name, qty) VALUES ('b', 1), ('c', 2) RETURNING id`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("多行插入应返回 2 行，实际 %d", len(res[0].Rows))
	}

	// RETURNING * 展开成全部列
	res = run(t, d, s, `INSERT INTO r (name) VALUES ('d') RETURNING *`)
	if got := len(res[0].Columns); got != 3 {
		t.Fatalf("RETURNING * 应展开 3 列，实际 %d", got)
	}

	// 表达式与别名：RETURNING 里可以算
	res = run(t, d, s, `INSERT INTO r (name, qty) VALUES ('e', 10) RETURNING qty * 2 AS doubled`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "20" {
		t.Fatalf("RETURNING qty*2 = %s，期望 20", got)
	}
	if got := res[0].Columns[0].Name; got != "doubled" {
		t.Fatalf("列别名 = %s，期望 doubled", got)
	}

	// 不带 RETURNING 时不能凭空产生结果集
	res = run(t, d, s, `INSERT INTO r (name) VALUES ('f')`)
	if len(res[0].Columns) != 0 || len(res[0].Rows) != 0 {
		t.Fatalf("普通 INSERT 不该有结果集: %+v", res[0])
	}
}

// INSERT ... SELECT：数据搬运的基础能力，之前直接报「不支持」。
func TestInsertSelect(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE src (id int4 PRIMARY KEY, v text)`)
	run(t, d, s, `CREATE TABLE dst (id int4 PRIMARY KEY, v text)`)
	run(t, d, s, `INSERT INTO src VALUES (1,'a'), (2,'b'), (3,'c')`)

	// 全表搬运
	res := run(t, d, s, `INSERT INTO dst SELECT id, v FROM src`)
	if got := res[len(res)-1].Affected; got != 3 {
		t.Fatalf("INSERT ... SELECT 影响 %d 行，期望 3", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM dst`)[0].Rows[0][0]); got != "3" {
		t.Fatalf("dst 行数 = %s，期望 3", got)
	}

	// 带 WHERE 与投影表达式
	run(t, d, s, `INSERT INTO dst SELECT id + 10, upper(v) FROM src WHERE id >= 2`)
	res = run(t, d, s, `SELECT v FROM dst WHERE id = 12`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "B" {
		t.Fatalf("id=12 的 v = %s，期望 B", got)
	}

	// 指定列：未提供的列走默认值
	run(t, d, s, `CREATE TABLE dst2 (id int4 PRIMARY KEY, v text DEFAULT 'dflt')`)
	run(t, d, s, `INSERT INTO dst2 (id) SELECT id FROM src`)
	if got := fmt.Sprint(run(t, d, s, `SELECT v FROM dst2 WHERE id = 1`)[0].Rows[0][0]); got != "dflt" {
		t.Fatalf("未提供的列应取默认值，实际 %s", got)
	}

	// 列数不匹配要报错，而不是插进去
	if err := runErr(t, d, s, `INSERT INTO dst2 (id) SELECT id, v FROM src`); err == nil {
		t.Fatal("列数不匹配应该报错")
	}

	// 唯一约束仍然生效
	if err := runErr(t, d, s, `INSERT INTO dst SELECT id, v FROM src`); err == nil {
		t.Fatal("主键冲突应该报错")
	}

	// RETURNING 与 INSERT ... SELECT 组合
	res = run(t, d, s, `INSERT INTO dst SELECT id + 100, v FROM src RETURNING id`)
	if len(res[0].Rows) != 3 {
		t.Fatalf("应返回 3 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "101" {
		t.Fatalf("第一行 returning id = %s，期望 101", got)
	}
}

// UPDATE / DELETE 的 RETURNING：DELETE ... RETURNING * 是 ORM 的常用写法。
func TestUpdateDeleteReturning(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ur (id int4 PRIMARY KEY, qty int4, note text)`)
	run(t, d, s, `INSERT INTO ur VALUES (1, 10, 'a'), (2, 20, 'b'), (3, 30, 'c')`)

	// UPDATE 返回改之后的值
	res := run(t, d, s, `UPDATE ur SET qty = qty + 5 WHERE id = 1 RETURNING id, qty`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应返回 1 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "15" {
		t.Fatalf("RETURNING qty = %s，期望 15（改后的值）", got)
	}
	if res[0].Tag != "UPDATE 1" {
		t.Fatalf("tag = %q，期望 UPDATE 1", res[0].Tag)
	}

	// 多行更新，多行返回
	res = run(t, d, s, `UPDATE ur SET note = upper(note) WHERE id >= 2 RETURNING id, note`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应返回 2 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "B" {
		t.Fatalf("note = %s，期望 B", got)
	}

	// DELETE 返回被删掉的行
	res = run(t, d, s, `DELETE FROM ur WHERE id = 3 RETURNING id, qty`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应返回 1 行，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "30" {
		t.Fatalf("被删行的 qty = %s，期望 30", got)
	}
	if res[0].Tag != "DELETE 1" {
		t.Fatalf("tag = %q，期望 DELETE 1", res[0].Tag)
	}

	// 没匹配到行时，RETURNING 返回空结果集而不是报错
	res = run(t, d, s, `DELETE FROM ur WHERE id = 999 RETURNING id`)
	if len(res[0].Rows) != 0 {
		t.Fatalf("未命中应返回 0 行，实际 %d", len(res[0].Rows))
	}

	// RETURNING * 展开全部列
	res = run(t, d, s, `DELETE FROM ur WHERE id = 1 RETURNING *`)
	if got := len(res[0].Columns); got != 3 {
		t.Fatalf("RETURNING * 应展开 3 列，实际 %d", got)
	}
}
