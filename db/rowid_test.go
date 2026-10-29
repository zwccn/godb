package db

import (
	"fmt"
	"strings"
	"testing"

	"godb/storage"
	"godb/types"
)

// 改一行不能动其他行的槽位：索引里存的就是槽位号，一动全废，
// 又会退化成「每次提交整表重建」。
func TestSlotsStayStableAcrossUpdates(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE st (id int4 PRIMARY KEY, code text)`)
	for i := 1; i <= 10; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO st VALUES (%d,'c%d')`, i, i))
	}
	before := slotsOf(t, d, "st")
	run(t, d, s, `UPDATE st SET code = 'zz' WHERE id = 5`)
	after := slotsOf(t, d, "st")

	if len(after) != len(before) {
		t.Fatalf("行数变了：%d -> %d", len(before), len(after))
	}
	// 被改的那行槽位也不能变（原地替换，索引只需换键）
	for id, slot := range before {
		if after[id] != slot {
			t.Fatalf("id=%d 的槽位从 %d 变成 %d", id, slot, after[id])
		}
	}
	assertRows(t, run(t, d, s, `SELECT id FROM st WHERE code = 'zz'`), []string{"5"})
}

// slotsOf 用事务扫描拿到 id → 槽位的映射。
func slotsOf(t *testing.T, d *DB, table string) map[int64]int {
	t.Helper()
	tx := d.Store.Begin()
	defer tx.Rollback()
	out := map[int64]int{}
	if err := tx.Scan(table, func(ref storage.Ref, row types.Row) error {
		out[row[0].I] = ref.Slot
		return nil
	}); err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	return out
}

// 删除只留空槽，下一次插入应该复用它（而不是让数组无限长）。
func TestDeletedSlotGetsReused(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ru (id int4 PRIMARY KEY, v text)`)
	for i := 1; i <= 5; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO ru VALUES (%d,'v%d')`, i, i))
	}
	run(t, d, s, `DELETE FROM ru WHERE id = 3`)
	td := d.Store.Table("ru")
	if td.Live != 4 {
		t.Fatalf("删除后 Live 应为 4，实际 %d", td.Live)
	}
	if td.SlotCount() != 5 {
		t.Fatalf("删除不该搬移其它行，槽位数组应仍是 5，实际 %d", td.SlotCount())
	}

	run(t, d, s, `INSERT INTO ru VALUES (6,'v6')`)
	if td.SlotCount() != 5 {
		t.Fatalf("插入应复用空槽，槽位数组仍应是 5，实际 %d", td.SlotCount())
	}
	if td.Live != 5 {
		t.Fatalf("插入后 Live 应为 5，实际 %d", td.Live)
	}
	assertRows(t, run(t, d, s, `SELECT id, v FROM ru ORDER BY id`),
		[]string{"1|v1", "2|v2", "4|v4", "5|v5", "6|v6"})
	// 复用的槽位不能把旧行捞回来
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ru WHERE v = 'v3'`), []string{"0"})
}

// 删掉的行不能从快照里「复活」：磁盘上永远写紧凑数据。
func TestDeletedRowsStayDeletedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	s := "s"
	run(t, d, s, `CREATE TABLE hr (id int4 PRIMARY KEY, v text)`)
	for i := 1; i <= 100; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO hr VALUES (%d,'v%d')`, i, i))
	}
	run(t, d, s, `CREATE INDEX hr_v ON hr (v)`)
	run(t, d, s, `DELETE FROM hr WHERE id % 2 = 0`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM hr`), []string{"50"})
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT count(*) FROM hr`), []string{"50"})
	assertRows(t, run(t, d2, s, `SELECT count(*) FROM hr WHERE v = 'v2'`), []string{"0"})
	assertRows(t, run(t, d2, s, `SELECT id FROM hr WHERE v = 'v3'`), []string{"3"})
	// 索引也跟着重启重建了
	res := rowsOf(t, run(t, d2, s, `EXPLAIN SELECT * FROM hr WHERE v = 'v3'`))
	if !strings.Contains(fmt.Sprint(res), "Index Scan using hr_v") {
		t.Fatalf("重启后二级索引应仍可用，计划: %v", res)
	}
}

// 增量维护的索引要跟得上：改二级索引列、改主键、删除、复用槽位插入。
func TestIndexFollowsIncrementalWrites(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE iw (id int4 PRIMARY KEY, code text)`)
	run(t, d, s, `CREATE INDEX iw_code ON iw (code)`)
	run(t, d, s, `INSERT INTO iw VALUES (1,'c1'),(2,'c2'),(3,'c3')`)

	// 改二级索引列
	run(t, d, s, `UPDATE iw SET code = 'new' WHERE id = 2`)
	assertRows(t, run(t, d, s, `SELECT id FROM iw WHERE code = 'new'`), []string{"2"})
	assertRows(t, run(t, d, s, `SELECT id FROM iw WHERE code = 'c2'`), nil)

	// 改主键
	run(t, d, s, `UPDATE iw SET id = 20 WHERE id = 3`)
	assertRows(t, run(t, d, s, `SELECT code FROM iw WHERE id = 20`), []string{"c3"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM iw WHERE id = 3`), []string{"0"})

	// 删除 + 复用空槽插入
	run(t, d, s, `DELETE FROM iw WHERE id = 1`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM iw WHERE code = 'c1'`), []string{"0"})
	run(t, d, s, `INSERT INTO iw VALUES (1,'c1')`)
	assertRows(t, run(t, d, s, `SELECT id FROM iw WHERE code = 'c1'`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM iw`), []string{"3"})

	// 唯一索引在复用槽位后仍然判重
	run(t, d, s, `CREATE UNIQUE INDEX iw_code_u ON iw (code)`)
	if err := runErr(t, d, s, `INSERT INTO iw VALUES (99,'c1')`); err == nil {
		t.Fatal("复用槽位后唯一索引应仍然拦重复")
	}
}

// TRUNCATE 之后槽位应该被清空，再插入从 0 开始，行数正确。
func TestTruncateThenInsert(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE tr (id int4 PRIMARY KEY, v text)`)
	for i := 1; i <= 50; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO tr VALUES (%d,'v%d')`, i, i))
	}
	run(t, d, s, `TRUNCATE TABLE tr`)
	td := d.Store.Table("tr")
	if td.Live != 0 {
		t.Fatalf("TRUNCATE 后 Live 应为 0，实际 %d", td.Live)
	}
	if td.SlotCount() != 0 {
		t.Fatalf("全表清空后应该把槽位数组一起放掉，实际长度 %d", td.SlotCount())
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM tr`), []string{"0"})

	run(t, d, s, `INSERT INTO tr VALUES (1,'a'),(2,'b'),(3,'c')`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM tr`), []string{"3"})
	assertRows(t, run(t, d, s, `SELECT v FROM tr WHERE id = 2`), []string{"b"})
}

// 行数统计必须走 Live（槽位数组带空洞），否则客户端看到的 reltuples 会虚高。
func TestRowCountUsesLiveSlots(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ct (id int4 PRIMARY KEY, v text)`)
	for i := 1; i <= 30; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO ct VALUES (%d,'v%d')`, i, i))
	}
	run(t, d, s, `DELETE FROM ct WHERE id <= 10`)
	td := d.Store.Table("ct")
	if td.SlotCount() != 30 || td.Live != 20 {
		t.Fatalf("应该有 30 个槽位、20 行，实际 %d / %d", td.SlotCount(), td.Live)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ct`), []string{"20"})
	// 客户端拿的 reltuples 也是这个数
	res := rowsOf(t, run(t, d, s, `SELECT reltuples FROM pg_catalog.pg_class WHERE relname = 'ct'`))
	if fmt.Sprint(res[0][0]) != "20" {
		t.Fatalf("reltuples 应为 20，实际 %v", res[0][0])
	}
}
