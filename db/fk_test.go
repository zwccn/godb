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

// ALTER TABLE ... ADD FOREIGN KEY：给已有表补约束。
//
// 这条路径是「表设计器建外键」的后端，必需的行为有三件：
// 加完立刻生效、已有孤儿行必须被拒、重启后还在。
func TestAddForeignKeyViaAlter(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ap (id int4 PRIMARY KEY, name text)`)
	run(t, d, s, `CREATE TABLE ac (id int4 PRIMARY KEY, p_id int4)`)
	run(t, d, s, `INSERT INTO ap VALUES (1,'a'), (2,'b')`)
	run(t, d, s, `INSERT INTO ac VALUES (10,1), (11,NULL)`)

	run(t, d, s, `ALTER TABLE ac ADD CONSTRAINT ac_p_id_fk FOREIGN KEY (p_id) REFERENCES ap(id)`)
	// 立刻生效
	if err := runErr(t, d, s, `INSERT INTO ac VALUES (12,99)`); err == nil {
		t.Fatal("加完外键后，插入不存在的父行应该被拒")
	}
	run(t, d, s, `INSERT INTO ac VALUES (13,2)`)

	// 父行被引用时不能删
	if err := runErr(t, d, s, `DELETE FROM ap WHERE id = 2`); err == nil {
		t.Fatal("被引用的父行应该删不掉")
	}

	// pg_constraint 里要能看到这条约束（客户端靠它显示外键）
	res := run(t, d, s, `SELECT conname FROM pg_catalog.pg_constraint WHERE contype = 'f'`)
	found := false
	for _, r := range res[0].Rows {
		if fmt.Sprint(r[0]) == "ac_p_id_fk" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pg_constraint 里找不到 ac_p_id_fk: %v", res[0].Rows)
	}
}

// 已有孤儿行时不能加约束：加完就会「插入不了、删父行也不行」，是被卡死而不是报错。
func TestAddForeignKeyRejectsOrphans(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE op (id int4 PRIMARY KEY)`)
	run(t, d, s, `CREATE TABLE oc (id int4 PRIMARY KEY, p_id int4)`)
	run(t, d, s, `INSERT INTO op VALUES (1)`)
	run(t, d, s, `INSERT INTO oc VALUES (10,1), (11,77)`)

	err := runErr(t, d, s, `ALTER TABLE oc ADD FOREIGN KEY (p_id) REFERENCES op(id)`)
	if err == nil || !strings.Contains(err.Error(), "violates foreign key constraint") {
		t.Fatalf("期望「已有行违反外键」，实际 %v", err)
	}
	// 失败不能留下半条约束
	run(t, d, s, `INSERT INTO oc (id) VALUES (12)`)
	// 把孤儿行清掉之后可以加
	run(t, d, s, `DELETE FROM oc WHERE id = 11`)
	run(t, d, s, `ALTER TABLE oc ADD FOREIGN KEY (p_id) REFERENCES op(id)`)
	if err := runErr(t, d, s, `INSERT INTO oc VALUES (13,88)`); err == nil {
		t.Fatal("清掉孤儿行后加约束应成功，并拦住不存在的父行")
	}
}

// 类型不匹配要在建约束时就说清楚，而不是等到写数据时每行都报「没有父行」。
func TestAddForeignKeyTypeMismatch(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE tp (id int4 PRIMARY KEY)`)
	run(t, d, s, `CREATE TABLE tc (id int4 PRIMARY KEY, p_id text)`)
	err := runErr(t, d, s, `ALTER TABLE tc ADD FOREIGN KEY (p_id) REFERENCES tp(id)`)
	if err == nil || !strings.Contains(err.Error(), "cannot be implemented") {
		t.Fatalf("期望类型不兼容报错，实际 %v", err)
	}
}

// 引用列必须是主键或唯一列：否则「至多一行」无法保证。
func TestAddForeignKeyNeedsUniqueParent(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE up (id int4 PRIMARY KEY, tag text)`)
	run(t, d, s, `CREATE TABLE uc (id int4 PRIMARY KEY, tag text)`)
	err := runErr(t, d, s, `ALTER TABLE uc ADD FOREIGN KEY (tag) REFERENCES up(tag)`)
	if err == nil || !strings.Contains(err.Error(), "no unique constraint matching") {
		t.Fatalf("期望「引用列不唯一」报错，实际 %v", err)
	}
}

// DROP CONSTRAINT 只删外键，删规则和其它约束名要给出能看懂的报错。
func TestDropForeignKeyConstraint(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dp (id int4 PRIMARY KEY)`)
	run(t, d, s, `CREATE TABLE dc (id int4 PRIMARY KEY, p_id int4 REFERENCES dp(id))`)
	run(t, d, s, `INSERT INTO dp VALUES (1)`)

	if err := runErr(t, d, s, `ALTER TABLE dc DROP CONSTRAINT no_such_fk`); err == nil {
		t.Fatal("删不存在的约束应该报错")
	}
	run(t, d, s, `ALTER TABLE dc DROP CONSTRAINT dc_p_id_fkey`)
	// 约束没了，孤儿行可以插
	run(t, d, s, `INSERT INTO dc VALUES (10, 99)`)
}

// 改列名 / 改表名要把外键元数据一起带着走。
//
// 不同步的后果不是「少个功能」，是这张表之后每一次写入都报
// 「column X does not exist」—— 而那个名字在表结构里已经搜不到了。
func TestRenameKeepsForeignKeys(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE rp (id int4 PRIMARY KEY)`)
	run(t, d, s, `CREATE TABLE rc (id int4 PRIMARY KEY, p_id int4 REFERENCES rp(id))`)
	run(t, d, s, `INSERT INTO rp VALUES (1)`)

	run(t, d, s, `ALTER TABLE rc RENAME COLUMN p_id TO parent_id`)
	if err := runErr(t, d, s, `INSERT INTO rc VALUES (10, 99)`); err == nil {
		t.Fatal("子表列改名后外键应该还在（拦不存在的父行）")
	}
	run(t, d, s, `INSERT INTO rc VALUES (11, 1)`)

	run(t, d, s, `ALTER TABLE rp RENAME COLUMN id TO pid`)
	if err := runErr(t, d, s, `INSERT INTO rc VALUES (12, 77)`); err == nil {
		t.Fatal("父表列改名后外键应该还在")
	}
	run(t, d, s, `INSERT INTO rc VALUES (13, 1)`)

	run(t, d, s, `ALTER TABLE rp RENAME TO rp2`)
	if err := runErr(t, d, s, `INSERT INTO rc VALUES (14, 88)`); err == nil {
		t.Fatal("父表改名后外键应该还在")
	}
	run(t, d, s, `INSERT INTO rc VALUES (15, 1)`)
}

// 删列：本表的外键跟着删（和索引一样），别的表指着这一列时要拦住 ——
// 悄悄删掉会让子表失去约束、还可能留下孤儿行。
func TestDropColumnWithForeignKeys(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE xp (id int4 PRIMARY KEY, extra int4)`)
	run(t, d, s, `CREATE TABLE xc (id int4 PRIMARY KEY, p_id int4 REFERENCES xp(id))`)
	run(t, d, s, `INSERT INTO xp VALUES (1, 5)`)
	run(t, d, s, `INSERT INTO xc VALUES (10, 1)`)

	// 被别的表引用 → 拦住
	err := runErr(t, d, s, `ALTER TABLE xp DROP COLUMN id`)
	if err == nil || !strings.Contains(err.Error(), "depends on it") {
		t.Fatalf("删被引用的列应报错，实际 %v", err)
	}

	// 本表的外键用到的列被删 → 连带删掉外键，之后可以自由插入
	run(t, d, s, `ALTER TABLE xc DROP COLUMN p_id`)
	run(t, d, s, `INSERT INTO xc (id) VALUES (11)`)
}

// 补上的外键要能熬过重启：catalog.json 是唯一依据。
func TestAddedForeignKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sess := d.Session("s")
	for _, q := range []string{
		`CREATE TABLE sp (id int4 PRIMARY KEY)`,
		`CREATE TABLE sc (id int4 PRIMARY KEY, p_id int4)`,
		`INSERT INTO sp VALUES (1)`,
		`ALTER TABLE sc ADD CONSTRAINT sc_fk FOREIGN KEY (p_id) REFERENCES sp(id)`,
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
	if _, err := d2.Exec(s2, `INSERT INTO sc VALUES (2, 99)`); err == nil {
		t.Fatal("重启后补加的外键应该还在")
	}
	if _, err := d2.Exec(s2, `INSERT INTO sc VALUES (3, 1)`); err != nil {
		t.Fatalf("合法插入应通过: %v", err)
	}
}
