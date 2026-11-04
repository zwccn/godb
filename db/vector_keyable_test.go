package db

import (
	"fmt"
	"strings"
	"testing"
)

// 向量被拿来当「等值/分组/排序键」时，必须在规划期就报错。
//
// 这条约束的理由在 exec/vector_guard.go 里写得很细，这里只钉住行为：
// 之前 DISTINCT / GROUP BY / COUNT(DISTINCT) 用的是行里的 .vec 槽位号当身份，
// 槽位每次写入都会换 —— 结果「看着是答案，其实取决于物理时机」；
// 而在向量列上建唯一索引更糟：所有向量被编码成同一个键，插入任何新向量都报重复。
//
// 这里同时钉住「空表也要报」：运行时才发现的话，同一句 SQL 会表现为
// 「有数据报错、没数据返回空」。
func TestVectorCannotBeUsedAsKey(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE vk (id int4 PRIMARY KEY, v vector(3))`)
	run(t, d, s, `INSERT INTO vk VALUES (1,'[1,2,3]'), (2,'[1,2,3]'), (3,'[2,2,3]')`)
	run(t, d, s, `CREATE TABLE vk_empty (id int4 PRIMARY KEY, v vector(3))`)

	cases := []struct {
		stmt string
		want string
	}{
		// 相等 / 分组 / 去重
		{`SELECT DISTINCT v FROM vk`, "SELECT DISTINCT"},
		{`SELECT v, COUNT(*) FROM vk GROUP BY v`, "GROUP BY"},
		{`SELECT COUNT(DISTINCT v) FROM vk`, "DISTINCT"},
		{`SELECT * FROM vk UNION SELECT * FROM vk`, "UNION"},
		{`SELECT * FROM vk INTERSECT SELECT * FROM vk`, "INTERSECT"},
		{`SELECT * FROM vk EXCEPT SELECT * FROM vk`, "EXCEPT"},
		{`SELECT id FROM vk a JOIN vk b ON a.v = b.v`, "JOIN"},
		{`SELECT id FROM vk WHERE v IN (SELECT v FROM vk)`, "vector"},
		// 顺序
		{`SELECT id FROM vk ORDER BY v`, "ORDER BY"},
		{`SELECT MIN(v) FROM vk`, "MIN()"},
		{`SELECT MAX(v) FROM vk`, "MAX()"},
		// 文本化：存盘的向量没有文本形式，硬转会漏出 [vector:0]
		{`SELECT lower(v) FROM vk`, "lower()"},
		{`SELECT length(v) FROM vk`, "length()"},
		{`SELECT concat(v) FROM vk`, "concat()"},
		{`SELECT v || 'x' FROM vk`, "||"},
		{`SELECT string_agg(v, ',') FROM vk`, "STRING_AGG()"},
		// 空表也必须在规划期报错，不能等第一行出现
		{`SELECT id FROM vk_empty ORDER BY v`, "ORDER BY"},
		{`SELECT DISTINCT v FROM vk_empty`, "SELECT DISTINCT"},
	}
	for _, c := range cases {
		err := runErr(t, d, s, c.stmt)
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q 的报错里没有 %q：%v", c.stmt, c.want, err)
		}
	}

	// UNION ALL 不去重，向量列可以照常出现。
	res := run(t, d, s, `SELECT v FROM vk UNION ALL SELECT v FROM vk`)
	if got := len(rowsOf(t, res)); got != 6 {
		t.Fatalf("UNION ALL 应返回 6 行，实际 %d", got)
	}
	// 距离排序是向量的正确用法，不能被这条约束误伤。
	res = run(t, d, s, `SELECT id FROM vk ORDER BY v <-> '[1,2,3]' LIMIT 1`)
	if got := len(rowsOf(t, res)); got != 1 {
		t.Fatalf("ORDER BY 距离应返回 1 行，实际 %d", got)
	}
	// count(v) 只数非 NULL，不涉及顺序/文本，必须照常可用。
	res = run(t, d, s, `SELECT count(v) FROM vk`)
	if got := fmt.Sprint(rowsOf(t, res)[0][0]); got != "3" {
		t.Fatalf("count(v) 应为 3，实际 %s", got)
	}
	// 单独取列能拿到文本（这是「要文本」时的正确做法）。
	res = run(t, d, s, `SELECT v FROM vk ORDER BY id LIMIT 1`)
	if got := fmt.Sprint(rowsOf(t, res)[0][0]); got != "[1,2,3]" {
		t.Fatalf("SELECT v 应为 [1,2,3]，实际 %s", got)
	}
}

// 向量列上不能建索引（含主键/唯一约束）：索引键是按列值编码的，而行里只有槽位号。
func TestVectorCannotBeIndexed(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE vi (id int4 PRIMARY KEY, v vector(3))`)

	for _, stmt := range []string{
		`CREATE INDEX vi_v_idx ON vi (v)`,
		`CREATE UNIQUE INDEX vi_v_uidx ON vi (v)`,
	} {
		err := runErr(t, d, s, stmt)
		if !strings.Contains(err.Error(), "no default operator class") {
			t.Errorf("%q 的报错应说明向量没有 btree 操作符类：%v", stmt, err)
		}
	}

	// 复合索引里带一列向量同样拒绝：键编码不区分「哪一列是向量」。
	if err := runErr(t, d, s, `CREATE INDEX vi_mix_idx ON vi (id, v)`); !strings.Contains(err.Error(), "no default operator class") {
		t.Errorf("复合索引的报错不对：%v", err)
	}

	// 列级约束在建表阶段就拒绝，而不是等第一次插入才出现「重复键」。
	if err := runErr(t, d, s, `CREATE TABLE vipk (v vector(3) PRIMARY KEY)`); !strings.Contains(err.Error(), "PRIMARY KEY") {
		t.Errorf("向量主键的报错不对：%v", err)
	}
	if err := runErr(t, d, s, `CREATE TABLE viuq (v vector(3) UNIQUE)`); !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("向量唯一约束的报错不对：%v", err)
	}

	// 把已有索引列改成 vector 也要拦住（否则索引会在原地变成错的东西）。
	run(t, d, s, `CREATE TABLE vt (id int4 PRIMARY KEY, n int4)`)
	run(t, d, s, `CREATE INDEX vt_n_idx ON vt (n)`)
	if err := runErr(t, d, s, `ALTER TABLE vt ALTER COLUMN n TYPE vector(3)`); !strings.Contains(err.Error(), "vector") {
		t.Errorf("ALTER TYPE 的报错不对：%v", err)
	}
	run(t, d, s, `CREATE TABLE vt2 (id int4 PRIMARY KEY, n int4)`)
	run(t, d, s, `ALTER TABLE vt2 ALTER COLUMN n TYPE vector(3)`)
}
