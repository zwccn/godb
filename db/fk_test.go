package db

import (
	"fmt"
	"strings"
	"testing"
)

// 外键：插入/更新要校验父行存在，删除/删表要挡住仍被引用的父行。
func TestForeignKey(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dept (id int4 PRIMARY KEY, name text)`)
	run(t, d, s, `CREATE TABLE emp (id int4 PRIMARY KEY, dept_id int4 REFERENCES dept(id), note text)`)
	run(t, d, s, `INSERT INTO dept VALUES (1,'eng'), (2,'sales')`)

	// 有父行 → 可以插
	run(t, d, s, `INSERT INTO emp VALUES (10, 1, 'a'), (11, 2, 'b')`)
	// 没有父行 → 拦住
	err := runErr(t, d, s, `INSERT INTO emp VALUES (12, 99, 'c')`)
	if err == nil || !strings.Contains(err.Error(), "foreign key constraint") {
		t.Fatalf("期望外键错误，实际 %v", err)
	}
	// NULL 外键不校验
	run(t, d, s, `INSERT INTO emp VALUES (13, NULL, 'd')`)

	// 更新成不存在的父行也要拦
	if err := runErr(t, d, s, `UPDATE emp SET dept_id = 42 WHERE id = 10`); err == nil {
		t.Fatal("UPDATE 到不存在的父行应该报错")
	}
	// 更新成存在的父行可以
	run(t, d, s, `UPDATE emp SET dept_id = 2 WHERE id = 10`)

	// 删除仍被引用的父行 → 拦
	err = runErr(t, d, s, `DELETE FROM dept WHERE id = 2`)
	if err == nil || !strings.Contains(err.Error(), "foreign key constraint") {
		t.Fatalf("期望删除被拒，实际 %v", err)
	}
	// 没有引用时可以删
	run(t, d, s, `DELETE FROM dept WHERE id = 1`)

	// 删被引用的表 → 拦
	if err := runErr(t, d, s, `DROP TABLE dept`); err == nil {
		t.Fatal("删被外键引用的表应该报错")
	}
}

// ON DELETE CASCADE：删父行时连带删子行（含多级）。
func TestForeignKeyCascade(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE a (id int4 PRIMARY KEY)`)
	run(t, d, s, `CREATE TABLE b (id int4 PRIMARY KEY, a_id int4 REFERENCES a(id) ON DELETE CASCADE)`)
	run(t, d, s, `CREATE TABLE c (id int4 PRIMARY KEY, b_id int4 REFERENCES b(id) ON DELETE CASCADE)`)
	run(t, d, s, `INSERT INTO a VALUES (1), (2)`)
	run(t, d, s, `INSERT INTO b VALUES (10,1), (11,1), (12,2)`)
	run(t, d, s, `INSERT INTO c VALUES (100,10), (101,11), (102,12)`)

	run(t, d, s, `DELETE FROM a WHERE id = 1`)

	// a=1 的两个 b 没了，它们的 c 也没了；a=2 那条链还在
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM b`)[0].Rows[0][0]); got != "1" {
		t.Fatalf("b 应剩 1 行，实际 %s", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM c`)[0].Rows[0][0]); got != "1" {
		t.Fatalf("c 应剩 1 行（级联要递归），实际 %s", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT id FROM c`)[0].Rows[0][0]); got != "102" {
		t.Fatalf("留下的应是 102，实际 %s", got)
	}
}

// 表级复合外键：FOREIGN KEY (a,b) REFERENCES p(x,y)
func TestCompositeForeignKey(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE p (x int4, y int4, PRIMARY KEY (x, y))`)
	run(t, d, s, `CREATE TABLE ch (id int4 PRIMARY KEY, x int4, y int4, FOREIGN KEY (x, y) REFERENCES p(x, y))`)
	run(t, d, s, `INSERT INTO p VALUES (1,1), (1,2)`)

	run(t, d, s, `INSERT INTO ch VALUES (1, 1, 2)`)
	if err := runErr(t, d, s, `INSERT INTO ch VALUES (2, 1, 9)`); err == nil {
		t.Fatal("(1,9) 在父表不存在，应该被拦")
	}

	// 引用列没有唯一约束时，建表就该失败
	if err := runErr(t, d, s, `CREATE TABLE bad (v int4 REFERENCES p(x))`); err == nil {
		t.Fatal("引用非唯一列建表应该失败")
	}
	// 引用不存在的表
	if err := runErr(t, d, s, `CREATE TABLE bad2 (v int4 REFERENCES nope(id))`); err == nil {
		t.Fatal("引用不存在的表应该失败")
	}
}

// 外键定义要能持久化（catalog.json），重启后仍然生效。
func TestForeignKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	for _, q := range []string{
		`CREATE TABLE p2 (id int4 PRIMARY KEY)`,
		`CREATE TABLE c2 (id int4 PRIMARY KEY, p_id int4 REFERENCES p2(id))`,
		`INSERT INTO p2 VALUES (1)`,
		`INSERT INTO c2 VALUES (1,1)`,
	} {
		if _, err := d.Exec(sess, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	s2 := d2.Session("s")
	if _, err := d2.Exec(s2, `INSERT INTO c2 VALUES (2, 99)`); err == nil {
		t.Fatal("重启后外键应该还在（拒绝不存在的父行）")
	}
	if _, err := d2.Exec(s2, `INSERT INTO c2 VALUES (3, 1)`); err != nil {
		t.Fatalf("合法插入应通过: %v", err)
	}
}

// 加载路径必须重建主键索引：否则重启后第一次写入检测不到重复主键。
func TestDuplicateKeyAfterReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	if _, err := d.Exec(sess, `CREATE TABLE dk (id int4 PRIMARY KEY, v text)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Exec(sess, `INSERT INTO dk VALUES (1,'a'), (2,'b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	d.Close()

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	s2 := d2.Session("s")
	// 第一条写之前，这张表是懒加载的：索引必须在加载时建好
	if _, err := d2.Exec(s2, `INSERT INTO dk VALUES (1,'dup')`); err == nil {
		t.Fatal("重启后重复主键应该被拦住（索引没在加载时重建）")
	}
	if _, err := d2.Exec(s2, `INSERT INTO dk VALUES (3,'c')`); err != nil {
		t.Fatalf("新主键应能插入: %v", err)
	}
}

// 客户端看外键读的是 pg_constraint（contype='f'），必须能看到。
func TestForeignKeyInCatalog(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE fp (x int4, y int4, PRIMARY KEY (x, y))`)
	run(t, d, s, `CREATE TABLE fc (id int4 PRIMARY KEY, x int4, y int4, FOREIGN KEY (x, y) REFERENCES fp(x, y) ON DELETE CASCADE)`)

	res := run(t, d, s, `
		SELECT c.conname, c.contype, c.conkey, c.confkey, c.confdeltype,
		       p.relname AS parent
		FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class t ON t.oid = c.conrelid
		LEFT JOIN pg_catalog.pg_class p ON p.oid = c.confrelid
		WHERE t.relname = 'fc' AND c.contype = 'f'`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应有 1 条外键约束，实际 %d", len(res[0].Rows))
	}
	r := res[0].Rows[0]
	// 子表 fc 的 x,y 是第 2、3 列；父表 fp 的 x,y 是第 1、2 列
	if fmt.Sprint(r[2]) != "{2,3}" || fmt.Sprint(r[3]) != "{1,2}" {
		t.Fatalf("conkey/confkey 不对: %v / %v", r[2], r[3])
	}
	if fmt.Sprint(r[4]) != "c" {
		t.Fatalf("ON DELETE CASCADE 应报 'c'，实际 %v", r[4])
	}
	if fmt.Sprint(r[5]) != "fp" {
		t.Fatalf("父表应是 fp，实际 %v", r[5])
	}
}
