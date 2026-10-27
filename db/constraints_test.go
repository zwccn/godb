package db

import (
	"fmt"
	"strings"
	"testing"
)

// CHECK 约束：列级与表级都要生效，且 NULL 不能拦（SQL 里 CHECK 只拦 false）。
func TestCheckConstraint(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ck (id int4 PRIMARY KEY, qty int4 CHECK (qty > 0), note text)`)
	// 表级 CHECK 可以引用多列
	run(t, d, s, `CREATE TABLE ck2 (id int4 PRIMARY KEY, lo int4, hi int4, CHECK (lo <= hi))`)

	// 合规的行能进
	run(t, d, s, `INSERT INTO ck VALUES (1, 5, 'ok')`)

	// 违规的行要报错，且库里不能留下脏数据
	err := runErr(t, d, s, `INSERT INTO ck VALUES (2, 0, 'bad')`)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("期望 check constraint 错误，实际 %v", err)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM ck`)[0].Rows[0][0]); got != "1" {
		t.Fatalf("被拒绝的行不该留下，ck 行数 = %s", got)
	}

	// NULL 通过（三值逻辑：结果未知不算违反）
	run(t, d, s, `INSERT INTO ck VALUES (3, NULL, 'null ok')`)

	// UPDATE 也要校验
	err = runErr(t, d, s, `UPDATE ck SET qty = -1 WHERE id = 1`)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("UPDATE 应被 CHECK 拦住，实际 %v", err)
	}
	// 原值必须没被改动
	if got := fmt.Sprint(run(t, d, s, `SELECT qty FROM ck WHERE id = 1`)[0].Rows[0][0]); got != "5" {
		t.Fatalf("被拒绝的 UPDATE 不该改数据，qty = %s", got)
	}

	// 多列条件
	run(t, d, s, `INSERT INTO ck2 VALUES (1, 1, 2)`)
	if err := runErr(t, d, s, `INSERT INTO ck2 VALUES (2, 5, 1)`); err == nil {
		t.Fatal("lo > hi 应该被表级 CHECK 拦住")
	}
}

// CHECK 的表达式以文本存进 catalog.json，重启后必须还在。
func TestCheckConstraintSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	if _, err := d.Exec(sess, `CREATE TABLE ckr (id int4 PRIMARY KEY, qty int4 CHECK (qty > 0))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	sess2 := d2.Session("s")
	_, err = d2.Exec(sess2, `INSERT INTO ckr VALUES (1, -5)`)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("重启后 CHECK 丢了，实际错误 %v", err)
	}
	if _, err := d2.Exec(sess2, `INSERT INTO ckr VALUES (2, 5)`); err != nil {
		t.Fatalf("合规数据应能插入: %v", err)
	}
}

// IN (SELECT ...) 与 EXISTS：之前一律报语法错误。
func TestInAndExistsSubquery(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE emp (id int4 PRIMARY KEY, dept int4, name text)`)
	run(t, d, s, `CREATE TABLE dept (id int4 PRIMARY KEY, name text)`)
	run(t, d, s, `INSERT INTO dept VALUES (1,'eng'), (2,'sales')`)
	run(t, d, s, `INSERT INTO emp VALUES (1,1,'a'), (2,1,'b'), (3,2,'c'), (4,9,'orphan')`)

	// 基本 IN
	res := run(t, d, s, `SELECT count(*) FROM emp WHERE dept IN (SELECT id FROM dept)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "3" {
		t.Fatalf("dept IN (子查询) 应命中 3 行，实际 %s", got)
	}

	// NOT IN
	res = run(t, d, s, `SELECT name FROM emp WHERE dept NOT IN (SELECT id FROM dept)`)
	if len(res[0].Rows) != 1 || fmt.Sprint(res[0].Rows[0][0]) != "orphan" {
		t.Fatalf("NOT IN 应只返回 orphan，实际 %v", res[0].Rows)
	}

	// 相关子查询（引用外层 emp 的列）
	res = run(t, d, s, `SELECT count(*) FROM emp e WHERE e.dept IN (SELECT d.id FROM dept d WHERE d.id = e.dept)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "3" {
		t.Fatalf("相关 IN 子查询应命中 3 行，实际 %s", got)
	}

	// EXISTS / NOT EXISTS
	res = run(t, d, s, `SELECT count(*) FROM dept d WHERE EXISTS (SELECT 1 FROM emp e WHERE e.dept = d.id)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("EXISTS 应命中 2 个部门，实际 %s", got)
	}
	res = run(t, d, s, `SELECT count(*) FROM dept d WHERE NOT EXISTS (SELECT 1 FROM emp e WHERE e.dept = d.id)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("NOT EXISTS 应为 0，实际 %s", got)
	}

	// IN 的集合里有 NULL 且没匹配上 → 结果是 NULL（不是 false）
	run(t, d, s, `CREATE TABLE n (v int4)`)
	run(t, d, s, `INSERT INTO n VALUES (1), (NULL)`)
	res = run(t, d, s, `SELECT count(*) FROM n WHERE v IN (SELECT v FROM n)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1" {
		t.Fatalf("IN 遇 NULL 的三值逻辑不对，count = %s", got)
	}

	// 子查询返回多列要报错
	if err := runErr(t, d, s, `SELECT * FROM emp WHERE dept IN (SELECT id, name FROM dept)`); err == nil {
		t.Fatal("IN 子查询返回多列应该报错")
	}
}

// 复合主键：以前直接报「不支持」。关键是按「整组」判重——单列重复是合法的。
func TestCompositePrimaryKey(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE cp (a int4, b int4, note text, PRIMARY KEY (a, b))`)

	// a 单独重复必须允许（这正是复合主键的意义）
	run(t, d, s, `INSERT INTO cp VALUES (1, 1, 'x'), (1, 2, 'y'), (2, 1, 'z')`)
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM cp`)[0].Rows[0][0]); got != "3" {
		t.Fatalf("应有 3 行，实际 %s", got)
	}

	// 整组重复要拦
	if err := runErr(t, d, s, `INSERT INTO cp VALUES (1, 1, 'dup')`); err == nil {
		t.Fatal("(1,1) 重复应该报唯一约束错误")
	}
	// 同一语句内部重复也要拦
	if err := runErr(t, d, s, `INSERT INTO cp VALUES (3, 3, 'a'), (3, 3, 'b')`); err == nil {
		t.Fatal("同一语句内的重复应该报错")
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT count(*) FROM cp`)[0].Rows[0][0]); got != "3" {
		t.Fatalf("失败的插入不该留下数据，实际 %s 行", got)
	}

	// UPDATE 改成已存在的组合要拦
	if err := runErr(t, d, s, `UPDATE cp SET b = 2 WHERE a = 1 AND b = 1`); err == nil {
		t.Fatal("UPDATE 撞上 (1,2) 应该报错")
	}
	// 改成不冲突的组合可以
	run(t, d, s, `UPDATE cp SET b = 9 WHERE a = 1 AND b = 1`)
	if got := fmt.Sprint(run(t, d, s, `SELECT b FROM cp WHERE a = 1 AND b = 9`)[0].Rows[0][0]); got != "9" {
		t.Fatalf("更新成功后应能查到 b=9，实际 %s", got)
	}

	// 复合主键的每一列都是 NOT NULL
	if err := runErr(t, d, s, `INSERT INTO cp VALUES (NULL, 5, 'n')`); err == nil {
		t.Fatal("主键列不允许 NULL")
	}

	// 系统目录要能看到两列：
	// conkey = {1,2}（int2[]），indkey = "1 2"（int2vector，空格分隔无括号）
	res := run(t, d, s, `
		SELECT c.conkey FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'cp' AND c.contype = 'p'`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "{1,2}" {
		t.Fatalf("conkey = %q，期望 {1,2}", got)
	}
	res = run(t, d, s, `
		SELECT i.indkey, i.indclass FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
		WHERE t.relname = 'cp'`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "1 2" {
		t.Fatalf("indkey = %q，期望 \"1 2\"", got)
	}
	// 两列都是 int4，操作符类应该是 1978 1978
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "1978 1978" {
		t.Fatalf("indclass = %q，期望 \"1978 1978\"", got)
	}
}

// 复合主键写入后重启，索引重建必须按整组来，否则重启后判重会回到「单列」。
func TestCompositePrimaryKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	for _, q := range []string{
		`CREATE TABLE cpr (a int4, b int4, PRIMARY KEY (a, b))`,
		`INSERT INTO cpr VALUES (1,1), (1,2)`,
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
	// 仍允许 a 重复（说明索引按整组重建了）
	if _, err := d2.Exec(s2, `INSERT INTO cpr VALUES (1,3)`); err != nil {
		t.Fatalf("重启后 (1,3) 应该能插入: %v", err)
	}
	// 整组重复仍然要拦
	if _, err := d2.Exec(s2, `INSERT INTO cpr VALUES (1,2)`); err == nil {
		t.Fatal("重启后 (1,2) 重复应该被拦")
	}
}
