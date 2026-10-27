package db

import (
	"fmt"
	"strings"
	"testing"
)

// 等值查询要真的走索引，而不是「建了个索引但还在全表扫」。
func TestIndexScanForEquality(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ixq (id int4 PRIMARY KEY, name text, score int4)`)
	for i := 0; i < 200; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO ixq VALUES (%d, 'n%d', %d)`, i, i, i*3))
	}
	run(t, d, s, `CREATE INDEX ixq_name ON ixq (name)`)
	run(t, d, s, `CREATE INDEX ixq_score ON ixq (score)`)

	// 二级索引
	plan := fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM ixq WHERE name = 'n42'`)))
	if !strings.Contains(plan, "Index Scan using ixq_name") {
		t.Fatalf("应走 ixq_name 索引，实际计划: %s", plan)
	}
	assertRows(t, run(t, d, s, `SELECT id, name FROM ixq WHERE name = 'n42'`), []string{"42|n42"})

	// 主键也走索引：以前主键哈希只用来判重，查数据还是全表扫
	plan = fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM ixq WHERE id = 7`)))
	if !strings.Contains(plan, "Index Scan using ixq_pkey") {
		t.Fatalf("主键等值应走哈希索引，实际计划: %s", plan)
	}
	assertRows(t, run(t, d, s, `SELECT name FROM ixq WHERE id = 7`), []string{"n7"})

	// 非等值条件哈希索引帮不上忙，仍然全表扫（结果必须照样对）
	plan = fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM ixq WHERE score > 500`)))
	if !strings.Contains(plan, "Seq Scan") {
		t.Fatalf("范围条件应退回全表扫描，实际计划: %s", plan)
	}

	// 索引只缩小候选集，剩余条件仍由 filter 判：命中多行时不能多也不能少
	run(t, d, s, `INSERT INTO ixq VALUES (500, 'n42', 999)`)
	assertRows(t, run(t, d, s, `SELECT id FROM ixq WHERE name = 'n42' ORDER BY id`), []string{"42", "500"})
	assertRows(t, run(t, d, s, `SELECT id FROM ixq WHERE name = 'n42' AND score = 999`), []string{"500"})
	// 常量写反（字面量在左）也要能识别
	assertRows(t, run(t, d, s, `SELECT id FROM ixq WHERE 'n42' = name AND 42 = id`), []string{"42"})

	// NULL 常量：= NULL 恒为未知，一行都不该返回
	assertRows(t, run(t, d, s, `SELECT id FROM ixq WHERE name = NULL`), nil)

	// 复合索引：所有列都被等值约束才用
	run(t, d, s, `CREATE INDEX ixq_name_score ON ixq (name, score)`)
	plan = fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM ixq WHERE name = 'n42' AND score = 999`)))
	if !strings.Contains(plan, "Index Scan using ixq_name_score") {
		t.Fatalf("两列都有等值条件时应走复合索引，实际计划: %s", plan)
	}
	// 只约束一列时不能选复合索引（前缀匹配没实现，选了就会漏行）
	plan = fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM ixq WHERE name = 'n42'`)))
	if strings.Contains(plan, "ixq_name_score") {
		t.Fatalf("只约束首列时不该用需要完整键的复合索引，实际计划: %s", plan)
	}
	// 没有 ORDER BY 就不该依赖顺序：索引返回同一键下多行的次序不保证
	assertRows(t, run(t, d, s, `SELECT id FROM ixq WHERE name = 'n42' ORDER BY id`), []string{"42", "500"})
}

// UNIQUE 索引必须真的拦住重复：之前它只写进 catalog 和系统目录，插重复值照样成功。
func TestUniqueIndexEnforced(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ux (id int4 PRIMARY KEY, code text)`)
	run(t, d, s, `CREATE UNIQUE INDEX ux_code ON ux (code)`)

	run(t, d, s, `INSERT INTO ux VALUES (1, 'a')`)
	if err := runErr(t, d, s, `INSERT INTO ux VALUES (2, 'a')`); !strings.Contains(err.Error(), "ux_code") {
		t.Fatalf("重复值应以索引名报错，实际: %v", err)
	}
	// 同一条 INSERT 内的重复也要拦
	if err := runErr(t, d, s, `INSERT INTO ux VALUES (3, 'b'), (4, 'b')`); !strings.Contains(err.Error(), "ux_code") {
		t.Fatalf("同语句内重复应报错，实际: %v", err)
	}
	// 语句失败后不能留下半截数据
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ux`), []string{"1"})

	run(t, d, s, `INSERT INTO ux VALUES (2, 'b')`)
	// UPDATE 把值改成已存在的也要拦
	if err := runErr(t, d, s, `UPDATE ux SET code = 'a' WHERE id = 2`); !strings.Contains(err.Error(), "ux_code") {
		t.Fatalf("UPDATE 撞唯一索引应报错，实际: %v", err)
	}
	// 改自己那一行（值不变）不该报错
	run(t, d, s, `UPDATE ux SET code = 'b' WHERE id = 2`)

	// NULL 不参与唯一性判断：多行 NULL 合法
	run(t, d, s, `INSERT INTO ux VALUES (5, NULL), (6, NULL)`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ux`), []string{"4"})

	// 唯一索引自带哈希表，判重是 O(1)，但结果必须和列级 UNIQUE 一致
	run(t, d, s, `CREATE TABLE uy (id int4 PRIMARY KEY, code text UNIQUE)`)
	run(t, d, s, `INSERT INTO uy VALUES (1, 'a')`)
	if err := runErr(t, d, s, `INSERT INTO uy VALUES (2, 'a')`); err == nil {
		t.Fatal("列级 UNIQUE 也应拦住")
	}
}

// 存量数据里已经有重复值时，建唯一索引要直接失败（和 PostgreSQL 一致）。
func TestUniqueIndexRejectsExistingDuplicates(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dup (id int4 PRIMARY KEY, code text)`)
	run(t, d, s, `INSERT INTO dup VALUES (1, 'a'), (2, 'a')`)
	err := runErr(t, d, s, `CREATE UNIQUE INDEX dup_code ON dup (code)`)
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("应报重复值，实际: %v", err)
	}
	// 建失败后不能留下一条半成品索引
	res := rowsOf(t, run(t, d, s, `SELECT count(*) FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'dup_code'`))
	if fmt.Sprint(res[0][0]) != "0" {
		t.Fatal("建失败不该留下索引")
	}
	// 去掉重复后能建成功
	run(t, d, s, `DELETE FROM dup WHERE id = 2`)
	run(t, d, s, `CREATE UNIQUE INDEX dup_code ON dup (code)`)
}

// 索引查的是「事务视角」：自己未提交的增删改必须看得见，否则读到的是旧世界。
func TestIndexSeesOwnChanges(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE tx (id int4 PRIMARY KEY, code text)`)
	run(t, d, s, `CREATE INDEX tx_code ON tx (code)`)
	run(t, d, s, `INSERT INTO tx VALUES (1, 'a'), (2, 'b')`)

	run(t, d, s, `BEGIN`)
	// 事务内新插入
	run(t, d, s, `INSERT INTO tx VALUES (3, 'c')`)
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'c'`), []string{"3"})
	// 事务内删除：索引里还有这行的行号，必须被过滤掉
	run(t, d, s, `DELETE FROM tx WHERE id = 1`)
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'a'`), nil)
	// 事务内改键：旧值查不到、新值查得到
	run(t, d, s, `UPDATE tx SET code = 'z' WHERE id = 2`)
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'b'`), nil)
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'z'`), []string{"2"})
	// 回滚后一切恢复（索引跟着已提交态走）
	run(t, d, s, `ROLLBACK`)
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'a'`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'b'`), []string{"2"})
	assertRows(t, run(t, d, s, `SELECT id FROM tx WHERE code = 'c'`), nil)
}

// DDL 之后索引要跟着走：删列连带删索引、改列名同步、改表名不失效。
func TestIndexFollowsDDL(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dd (id int4 PRIMARY KEY, code text, extra int4)`)
	run(t, d, s, `INSERT INTO dd VALUES (1, 'a', 10)`)
	run(t, d, s, `CREATE INDEX dd_code ON dd (code)`)

	// 改列名：索引要跟着改名，否则按旧列名解析不到列、索引静默失效
	run(t, d, s, `ALTER TABLE dd RENAME COLUMN code TO label`)
	assertRows(t, run(t, d, s, `SELECT id FROM dd WHERE label = 'a'`), []string{"1"})
	plan := fmt.Sprint(rowsOf(t, run(t, d, s, `EXPLAIN SELECT * FROM dd WHERE label = 'a'`)))
	if !strings.Contains(plan, "Index Scan using dd_code") {
		t.Fatalf("改列名后索引应仍可用，实际计划: %s", plan)
	}

	// 改表名：索引还在，查询照旧
	run(t, d, s, `ALTER TABLE dd RENAME TO dd2`)
	assertRows(t, run(t, d, s, `SELECT id FROM dd2 WHERE label = 'a'`), []string{"1"})

	// 删掉被索引引用的列：索引一起删（留着会指向错位的列）
	res := run(t, d, s, `ALTER TABLE dd2 DROP COLUMN label`)
	if !strings.Contains(res[0].Message, "dd_code") {
		t.Fatalf("应提示连带删了索引，实际: %q", res[0].Message)
	}
	r := rowsOf(t, run(t, d, s, `SELECT count(*) FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'dd_code'`))
	if fmt.Sprint(r[0][0]) != "0" {
		t.Fatal("依赖被删列的索引应一起消失")
	}
	assertRows(t, run(t, d, s, `SELECT id FROM dd2`), []string{"1"})
}

// 重启后索引要能看到老数据（catalog 持久化 + 加载时重建哈希表）。
func TestIndexWorksAfterRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := d.Session("s")
	for _, q := range []string{
		`CREATE TABLE rx (id int4 PRIMARY KEY, code text)`,
		`INSERT INTO rx VALUES (1, 'a'), (2, 'b')`,
		`CREATE INDEX rx_code ON rx (code)`,
		`CREATE UNIQUE INDEX rx_code_u ON rx (code)`,
	} {
		if _, err := d.Exec(s, q); err != nil {
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
	s2 := d2.Session("s2")
	res, err := d2.Exec(s2, `SELECT id FROM rx WHERE code = 'b'`)
	if err != nil || len(res[0].Rows) != 1 {
		t.Fatalf("重启后索引应查到老数据: %v %v", res, err)
	}
	// 唯一索引重启后同样要生效
	if _, err := d2.Exec(s2, `INSERT INTO rx VALUES (3, 'a')`); err == nil {
		t.Fatal("重启后唯一索引应仍然生效")
	}
}

// UPDATE / DELETE 的 WHERE 也走索引：按主键改一行不该全表扫。
// 这里靠 affected 行数验证「候选集没少也没多」——选错索引会直接露出来。
func TestIndexUsedByDML(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dm (id int4 PRIMARY KEY, code text, qty int4)`)
	for i := 0; i < 100; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO dm VALUES (%d, 'c%d', %d)`, i, i%10, i))
	}
	run(t, d, s, `CREATE INDEX dm_code ON dm (code)`)

	// 主键条件：只动一行
	res := run(t, d, s, `UPDATE dm SET qty = 999 WHERE id = 50`)
	if res[0].Affected != 1 {
		t.Fatalf("按主键更新应只影响 1 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT qty FROM dm WHERE id = 50`), []string{"999"})

	// 二级索引条件：命中 code='c3' 的 10 行，其余不能碰
	res = run(t, d, s, `UPDATE dm SET qty = qty + 1 WHERE code = 'c3'`)
	if res[0].Affected != 10 {
		t.Fatalf("按索引更新应影响 10 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM dm WHERE code = 'c3' AND qty = 4`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM dm`), []string{"100"})

	// 索引 + 残余条件：索引只保证 code 相等，qty 那一条还得由 filter 判
	res = run(t, d, s, `DELETE FROM dm WHERE code = 'c3' AND qty < 20`)
	if res[0].Affected != 2 {
		t.Fatalf("应删掉 qty=4/14 两行，实际 %d", res[0].Affected)
	}

	// 键不存在时不能误伤（这是「走索引返回空」和「退回全表」最容易搞错的地方）
	res = run(t, d, s, `DELETE FROM dm WHERE id = 99999`)
	if res[0].Affected != 0 {
		t.Fatalf("不存在的键不该删任何行，实际 %d", res[0].Affected)
	}
	res = run(t, d, s, `UPDATE dm SET qty = 0 WHERE code = 'nope'`)
	if res[0].Affected != 0 {
		t.Fatalf("不存在的索引键不该更新任何行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM dm`), []string{"98"})

	// 上面的 qty+1 让 c3 和 c4 撞了值，建唯一索引必须据此失败
	if err := runErr(t, d, s, `CREATE UNIQUE INDEX dm_qty ON dm (qty)`); !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("存量有重复时建唯一索引应失败，实际: %v", err)
	}
	run(t, d, s, `UPDATE dm SET qty = id * 1000`)
	run(t, d, s, `CREATE UNIQUE INDEX dm_qty ON dm (qty)`)

	// 批量改到重复值：唯一索引要拦住，且整条语句一行都不能生效
	if err := runErr(t, d, s, `UPDATE dm SET qty = 1000`); !strings.Contains(err.Error(), "dm_qty") {
		t.Fatalf("批量改重复应以索引名报错，实际: %v", err)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM dm WHERE qty = 1000`), []string{"1"})
}

// 改动索引列之后，索引查询必须跟着新值走（不能靠旧键查出来）。
func TestIndexTracksKeyUpdate(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE kt (id int4 PRIMARY KEY, code text)`)
	run(t, d, s, `INSERT INTO kt VALUES (1, 'old')`)
	run(t, d, s, `CREATE INDEX kt_code ON kt (code)`)

	run(t, d, s, `UPDATE kt SET code = 'new' WHERE id = 1`)
	assertRows(t, run(t, d, s, `SELECT id FROM kt WHERE code = 'new'`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT id FROM kt WHERE code = 'old'`), nil)

	// 同一事务里连续改两次：第二次要按中间值找到那一行
	run(t, d, s, `BEGIN`)
	run(t, d, s, `UPDATE kt SET code = 'mid' WHERE id = 1`)
	run(t, d, s, `UPDATE kt SET code = 'last' WHERE code = 'mid'`)
	run(t, d, s, `COMMIT`)
	assertRows(t, run(t, d, s, `SELECT code FROM kt WHERE id = 1`), []string{"last"})
}

// pg_get_indexdef / pg_get_constraintdef：psql 的 \d 靠它们拼索引与约束的定义，
// 返回空串会让索引行只剩一个逗号（之前就是这样）。
func TestIndexDefFunctions(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE df (id int4 PRIMARY KEY, code text, qty int4)`)
	run(t, d, s, `CREATE INDEX df_code ON df (code)`)
	run(t, d, s, `CREATE UNIQUE INDEX df_qty ON df (qty)`)

	// 普通索引：不该被写成 UNIQUE
	def := fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT pg_catalog.pg_get_indexdef(i.indexrelid)
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'df_code'`))[0][0])
	if def != "CREATE INDEX df_code ON public.df USING btree (code)" {
		t.Fatalf("pg_get_indexdef(df_code) = %q", def)
	}
	// 唯一索引 + 三参形式（psql 就是这么调的）
	def = fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT pg_catalog.pg_get_indexdef(i.indexrelid, 0, true)
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'df_qty'`))[0][0])
	if def != "CREATE UNIQUE INDEX df_qty ON public.df USING btree (qty)" {
		t.Fatalf("pg_get_indexdef(df_qty) = %q", def)
	}

	// 约束定义：主键
	def = fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT pg_catalog.pg_get_constraintdef(con.oid)
		FROM pg_catalog.pg_constraint con JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		WHERE c.relname = 'df' AND con.contype = 'p'`))[0][0])
	if def != "PRIMARY KEY (id)" {
		t.Fatalf("主键定义 = %q", def)
	}
	// 外键
	run(t, d, s, `CREATE TABLE dfc (id int4 PRIMARY KEY, pid int4 REFERENCES df(id))`)
	def = fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT pg_catalog.pg_get_constraintdef(con.oid)
		FROM pg_catalog.pg_constraint con JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		WHERE c.relname = 'dfc' AND con.contype = 'f'`))[0][0])
	if def != "FOREIGN KEY (pid) REFERENCES df(id)" {
		t.Fatalf("外键定义 = %q", def)
	}
	// 未知 OID 给空串而不是报错（客户端的查询会扫全表，不该因此整条失败）
	assertRows(t, run(t, d, s, `SELECT pg_catalog.pg_get_indexdef(999999)`), []string{""})
}
