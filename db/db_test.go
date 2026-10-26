package db

import (
	"fmt"
	"testing"
	"time"

	"godb/exec"
	"godb/lock"
)

// testOptions 用较短的锁超时，让并发用例跑得快。
func testOptions() Options {
	return Options{Lock: lock.Options{
		Timeout:        800 * time.Millisecond,
		Retry:          10 * time.Millisecond,
		DeadlockDetect: true,
		DeadlockAfter:  150 * time.Millisecond,
	}}
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func run(t *testing.T, d *DB, sess string, sqlText string) []*exec.Result {
	t.Helper()
	res, err := d.Exec(d.Session(sess), sqlText)
	if err != nil {
		t.Fatalf("exec %q: %v", sqlText, err)
	}
	return res
}

func runErr(t *testing.T, d *DB, sess string, sqlText string) error {
	t.Helper()
	_, err := d.Exec(d.Session(sess), sqlText)
	if err == nil {
		t.Fatalf("expect error for %q, got nil", sqlText)
	}
	return err
}

func rowsOf(t *testing.T, res []*exec.Result) [][]interface{} {
	t.Helper()
	if len(res) == 0 {
		t.Fatalf("no result")
	}
	return res[len(res)-1].Rows
}

func fmtRow(r []interface{}) string {
	out := ""
	for i, v := range r {
		if i > 0 {
			out += "|"
		}
		out += fmt.Sprint(v)
	}
	return out
}

func assertRows(t *testing.T, res []*exec.Result, want []string) {
	t.Helper()
	got := rowsOf(t, res)
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if fmtRow(got[i]) != want[i] {
			t.Fatalf("row %d = %q, want %q", i, fmtRow(got[i]), want[i])
		}
	}
}

func TestBasicCRUD(t *testing.T) {
	d := newTestDB(t)
	s := "s1"
	run(t, d, s, `CREATE TABLE t (id SERIAL PRIMARY KEY, name TEXT NOT NULL, score NUMERIC(6,2), ok BOOLEAN DEFAULT TRUE)`)
	run(t, d, s, `INSERT INTO t (name, score) VALUES ('alice', 90.5), ('bob', 75), ('carol', NULL)`)
	assertRows(t, run(t, d, s, `SELECT id, name, score, ok FROM t ORDER BY id`), []string{
		"1|alice|90.5|true",
		"2|bob|75|true",
		"3|carol|<nil>|true",
	})

	assertRows(t, run(t, d, s, `SELECT name FROM t WHERE score > 80`), []string{"alice"})
	assertRows(t, run(t, d, s, `SELECT name FROM t WHERE score IS NULL`), []string{"carol"})
	assertRows(t, run(t, d, s, `SELECT count(*), sum(score), avg(score) FROM t`), []string{"3|165.5|82.75"})
	assertRows(t, run(t, d, s, `SELECT name FROM t ORDER BY score DESC NULLS LAST`), []string{"alice", "bob", "carol"})
	assertRows(t, run(t, d, s, `SELECT name FROM t ORDER BY score ASC`), []string{"bob", "alice", "carol"})
	assertRows(t, run(t, d, s, `SELECT name FROM t ORDER BY name LIMIT 2 OFFSET 1`), []string{"bob", "carol"})

	run(t, d, s, `UPDATE t SET score = score + 10 WHERE name = 'bob'`)
	assertRows(t, run(t, d, s, `SELECT score FROM t WHERE name = 'bob'`), []string{"85"})
	run(t, d, s, `DELETE FROM t WHERE name = 'carol'`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM t`), []string{"2"})

	// 主键冲突
	runErr(t, d, s, `INSERT INTO t (id, name) VALUES (1, 'dup')`)
	// NOT NULL
	runErr(t, d, s, `INSERT INTO t (name) VALUES (NULL)`)
}

func TestGroupByJoin(t *testing.T) {
	d := newTestDB(t)
	s := "s1"
	run(t, d, s, `CREATE TABLE dept (id SERIAL PRIMARY KEY, name TEXT NOT NULL)`)
	run(t, d, s, `CREATE TABLE emp (id SERIAL PRIMARY KEY, name TEXT, dept_id INTEGER, salary INTEGER)`)
	run(t, d, s, `INSERT INTO dept (name) VALUES ('研发'), ('产品'), ('空部门')`)
	run(t, d, s, `INSERT INTO emp (name, dept_id, salary) VALUES
		('a', 1, 100), ('b', 1, 200), ('c', 2, 300), ('d', NULL, 400)`)

	assertRows(t, run(t, d, s, `
		SELECT d.name, count(*) AS cnt, sum(e.salary) AS total
		FROM emp e JOIN dept d ON e.dept_id = d.id
		GROUP BY d.name
		ORDER BY total DESC`), []string{"研发|2|300", "产品|1|300"})

	assertRows(t, run(t, d, s, `
		SELECT d.name, count(e.id) AS cnt FROM dept d LEFT JOIN emp e ON e.dept_id = d.id
		GROUP BY d.name ORDER BY d.name`), []string{"产品|1", "研发|2", "空部门|0"})

	assertRows(t, run(t, d, s, `
		SELECT d.name, count(*) AS cnt FROM emp e JOIN dept d ON e.dept_id = d.id
		GROUP BY d.name HAVING count(*) > 1`), []string{"研发|2"})

	// 非等值连接走嵌套循环
	assertRows(t, run(t, d, s, `SELECT count(*) FROM emp a JOIN emp b ON a.salary < b.salary`), []string{"6"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM emp a CROSS JOIN dept b`), []string{"12"})
}

func TestTransaction(t *testing.T) {
	d := newTestDB(t)
	s1, s2 := "s1", "s2"
	run(t, d, s1, `CREATE TABLE acct (id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)`)
	run(t, d, s1, `INSERT INTO acct VALUES (1, 100), (2, 100)`)

	run(t, d, s1, `BEGIN`)
	run(t, d, s1, `UPDATE acct SET bal = bal - 30 WHERE id = 1`)
	// 事务内可见自己的修改
	assertRows(t, run(t, d, s1, `SELECT bal FROM acct WHERE id = 1`), []string{"70"})
	run(t, d, s1, `ROLLBACK`)
	assertRows(t, run(t, d, s1, `SELECT bal FROM acct WHERE id = 1`), []string{"100"})

	run(t, d, s1, `BEGIN`)
	run(t, d, s1, `UPDATE acct SET bal = bal - 30 WHERE id = 1`)
	run(t, d, s1, `UPDATE acct SET bal = bal + 30 WHERE id = 2`)
	run(t, d, s1, `COMMIT`)
	assertRows(t, run(t, d, s1, `SELECT sum(bal) FROM acct`), []string{"200"})

	// 事务内语句失败 → 事务中毒，必须 ROLLBACK
	run(t, d, s1, `BEGIN`)
	runErr(t, d, s1, `INSERT INTO acct VALUES (1, 0)`)
	if _, err := d.Exec(d.Session(s1), `SELECT 1`); err == nil {
		t.Fatalf("expect aborted transaction error")
	}
	run(t, d, s1, `ROLLBACK`)
	assertRows(t, run(t, d, s1, `SELECT count(*) FROM acct`), []string{"2"})

	// 表级锁：s1 读了 acct（持 S），s2 写 acct（要 X）必须等待并超时
	run(t, d, s1, `BEGIN`)
	run(t, d, s1, `SELECT * FROM acct WHERE id = 1`)
	if _, err := d.Exec(d.Session(s2), `INSERT INTO acct VALUES (9, 9)`); err == nil {
		t.Fatalf("expect lock conflict for s2")
	}
	run(t, d, s1, `ROLLBACK`)
	// 释放后 s2 可以写
	run(t, d, s2, `INSERT INTO acct VALUES (9, 9)`)
	assertRows(t, run(t, d, s2, `SELECT count(*) FROM acct`), []string{"3"})
}

func TestPersist(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	run(t, d, "s", `CREATE TABLE p (id SERIAL PRIMARY KEY, txt TEXT)`)
	run(t, d, "s", `INSERT INTO p (txt) VALUES ('x'), ('y')`)
	run(t, d, "s", `UPDATE p SET txt = 'z' WHERE id = 2`)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT id, txt FROM p ORDER BY id`), []string{"1|x", "2|z"})
	run(t, d2, "s", `INSERT INTO p (txt) VALUES ('w')`)
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM p`), []string{"3"})
}

func TestAlter(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT)`)
	run(t, d, s, `INSERT INTO a VALUES (1, 'one')`)
	run(t, d, s, `ALTER TABLE a ADD COLUMN y INTEGER DEFAULT 7`)
	assertRows(t, run(t, d, s, `SELECT id, x, y FROM a`), []string{"1|one|7"})
	run(t, d, s, `ALTER TABLE a RENAME COLUMN x TO name`)
	assertRows(t, run(t, d, s, `SELECT name, y FROM a`), []string{"one|7"})
	run(t, d, s, `ALTER TABLE a DROP COLUMN y`)
	assertRows(t, run(t, d, s, `SELECT * FROM a`), []string{"1|one"})
	run(t, d, s, `ALTER TABLE a RENAME TO b`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM b`), []string{"1"})
}

func TestExpressions(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	assertRows(t, run(t, d, s, `SELECT 1 + 2 * 3, 'a' || 'b', 7 / 2`), []string{"7|ab|3"})
	assertRows(t, run(t, d, s, `SELECT upper('abc'), length('中文abc'), coalesce(NULL, 5)`), []string{"ABC|5|5"})
	assertRows(t, run(t, d, s, `SELECT CASE WHEN 1 > 2 THEN 'yes' ELSE 'no' END`), []string{"no"})
	assertRows(t, run(t, d, s, `SELECT CAST('12' AS INTEGER) + 1, 3.7::INTEGER`), []string{"13|4"})
	assertRows(t, run(t, d, s, `SELECT 'abc' LIKE 'a%', 'abc' LIKE 'b%'`), []string{"true|false"})
	assertRows(t, run(t, d, s, `SELECT 5 BETWEEN 1 AND 10, 3 IN (1, 2, 3)`), []string{"true|true"})
	assertRows(t, run(t, d, s, `SELECT NULL = NULL IS NULL`), []string{"true"})
}

func TestWALRecovery(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	run(t, d, "s", `CREATE TABLE w (id SERIAL PRIMARY KEY, v TEXT)`)
	run(t, d, "s", `INSERT INTO w (v) VALUES ('a'), ('b'), ('c')`)
	run(t, d, "s", `UPDATE w SET v = 'B' WHERE id = 2`)
	run(t, d, "s", `DELETE FROM w WHERE id = 3`)
	// 模拟崩溃：只关闭 WAL 文件，不写快照
	if err := d.Store.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT id, v FROM w ORDER BY id`), []string{"1|a", "2|B"})
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM w`), []string{"2"})
}

func TestDDLInTxRejected(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s", `BEGIN`)
	runErr(t, d, "s", `CREATE TABLE nope (id INTEGER)`)
	run(t, d, "s", `ROLLBACK`)
}

func TestErrors(t *testing.T) {
	d := newTestDB(t)
	runErr(t, d, "s", `SELECT * FROM missing_table`)
	run(t, d, "s", `CREATE TABLE e (id INTEGER)`)
	runErr(t, d, "s", `CREATE TABLE e (id INTEGER)`)
	runErr(t, d, "s", `SELECT nope FROM e`)
	runErr(t, d, "s", `SELECT * FROM e WHERE (SELECT 1)`)
	runErr(t, d, "s", `INSERT INTO e (id) VALUES (1/0)`)
	runErr(t, d, "s", `SELEC 1`)
	runErr(t, d, "s", `CREATE TABLE e2 (a INTEGER, a TEXT)`)
	runErr(t, d, "s", `SELECT a FROM e HAVING count(*) > 1`)
}

func TestRightJoinAndOrder(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE l (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, s, `CREATE TABLE r (id INTEGER PRIMARY KEY, l_id INTEGER, w TEXT)`)
	run(t, d, s, `INSERT INTO l VALUES (1, 'one'), (2, 'two'), (3, 'three')`)
	run(t, d, s, `INSERT INTO r VALUES (10, 1, 'a'), (11, 2, 'b'), (12, 99, 'c')`)

	// RIGHT JOIN：右表全部保留
	assertRows(t, run(t, d, s, `
		SELECT r.w, l.v FROM l RIGHT JOIN r ON l.id = r.l_id ORDER BY r.id`), []string{
		"a|one", "b|two", "c|<nil>",
	})

	// 多条件 + 排序 + 限制
	assertRows(t, run(t, d, s, `
		SELECT v FROM l WHERE id >= 1 AND v <> 'two' ORDER BY v DESC LIMIT 1`), []string{"three"})

	// 三表连接
	run(t, d, s, `CREATE TABLE m (id INTEGER PRIMARY KEY, r_id INTEGER)`)
	run(t, d, s, `INSERT INTO m VALUES (100, 10), (101, 11)`)
	assertRows(t, run(t, d, s, `
		SELECT l.v, r.w, m.id FROM l
		JOIN r ON r.l_id = l.id
		JOIN m ON m.r_id = r.id
		ORDER BY m.id`), []string{"one|a|100", "two|b|101"})

	// DISTINCT
	assertRows(t, run(t, d, s, `SELECT DISTINCT l_id FROM r WHERE l_id < 50`), []string{"1", "2"})
}
