package db

import (
	"os"
	"path/filepath"
	"testing"

	"godb/storage"
)

// trashEntries 列出数据目录里回收站的备份目录名。
func trashEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := storage.TrashEntries(dir)
	if err != nil {
		t.Fatalf("列回收站: %v", err)
	}
	return ents
}

// readManifest 读最新一份备份的 manifest。
func readManifest(t *testing.T, dir string) storage.TrashManifest {
	t.Helper()
	ents := trashEntries(t, dir)
	if len(ents) == 0 {
		t.Fatalf("回收站里没有备份")
	}
	mf, err := readTrashManifest(filepath.Join(dir, storage.TrashDirName(), ents[0]))
	if err != nil {
		t.Fatalf("读 manifest: %v", err)
	}
	return mf
}

// DROP TABLE 之前必须留一份备份，而且备份里是删除时的数据（内存里那份）。
func TestDropTableBacksUpToTrash(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s", `CREATE TABLE t_drop (id INTEGER, name TEXT)`)
	run(t, d, "s", `INSERT INTO t_drop VALUES (1,'a'),(2,'b'),(3,'c')`)
	run(t, d, "s", `DROP TABLE t_drop`)

	mf := readManifest(t, d.dir)
	if mf.Reason != storage.TrashReasonDropTable {
		t.Fatalf("原因 = %q，期望 %q", mf.Reason, storage.TrashReasonDropTable)
	}
	if len(mf.Defs) != 1 || mf.Defs[0].Name != "t_drop" {
		t.Fatalf("备份里的表定义不对: %+v", mf.Defs)
	}
	if len(mf.Tables) != 1 || mf.Tables[0].Rows != 3 {
		t.Fatalf("备份行数不对: %+v", mf.Tables)
	}
	if mf.Tables[0].From != "memory" {
		// 数据还在内存里，应该优先从内存抓，否则会捞回旧快照
		t.Fatalf("数据来源 = %q，期望 memory", mf.Tables[0].From)
	}
	if _, ok := d.Cat.Get("t_drop"); ok {
		t.Fatalf("表应该已经被删掉")
	}
}

// 恢复把表结构和行一起捞回来，并且 WAL 重放不会把数据翻倍。
func TestRestoreTrashRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d, "s", `CREATE TABLE t_lost (id INTEGER, name TEXT)`)
	run(t, d, "s", `INSERT INTO t_lost VALUES (1,'一'),(2,'二')`)
	run(t, d, "s", `DROP TABLE t_lost`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rep, err := RestoreTrash(dir, "latest")
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if len(rep.Restored) != 1 || rep.Restored[0].Name != "t_lost" {
		t.Fatalf("恢复结果不对: %+v", rep)
	}
	if rep.Restored[0].Rows != 2 {
		t.Fatalf("恢复行数 = %d，期望 2", rep.Restored[0].Rows)
	}

	// 重新打开：数据要还在，而且不能因为重放 WAL 变成 2 倍
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	res := run(t, d2, "s2", `SELECT id, name FROM t_lost ORDER BY id`)
	if len(res) == 0 || len(res[0].Rows) != 2 {
		t.Fatalf("恢复后行数 = %v，期望 2 行", res)
	}
	if got := d2.Store.RowCount("t_lost"); got != 2 {
		t.Fatalf("RowCount = %d，期望 2（WAL 重放不应重复应用）", got)
	}
}

// 清空库（DropAll / 载入示例数据）之前也必须备份。
func TestDropAllBacksUpToTrash(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s", `CREATE TABLE a1 (id INTEGER)`)
	run(t, d, "s", `CREATE TABLE a2 (id INTEGER)`)
	run(t, d, "s", `INSERT INTO a1 VALUES (7)`)

	if err := d.DropAll(); err != nil {
		t.Fatalf("DropAll: %v", err)
	}
	mf := readManifest(t, d.dir)
	if mf.Reason != storage.TrashReasonDropAll {
		t.Fatalf("原因 = %q，期望 %q", mf.Reason, storage.TrashReasonDropAll)
	}
	if len(mf.Defs) != 2 {
		t.Fatalf("备份表数 = %d，期望 2", len(mf.Defs))
	}
	names := map[string]int{}
	for _, tt := range mf.Tables {
		names[tt.Name] = tt.Rows
	}
	if names["a1"] != 1 || names["a2"] != 0 {
		t.Fatalf("备份行数不对: %+v", names)
	}
	if got := d.Tables(); len(got) != 0 {
		t.Fatalf("DropAll 之后应该没有表，实际 %d 张", len(got))
	}
}

// 同名表已存在时恢复只跳过，不覆盖 —— 恢复动作本身不能再造成数据损失。
func TestRestoreSkipsExistingTable(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s", `CREATE TABLE dup (id INTEGER)`)
	run(t, d, "s", `INSERT INTO dup VALUES (1)`)
	run(t, d, "s", `DROP TABLE dup`)

	// 建一张同名但内容不同的表
	run(t, d, "s", `CREATE TABLE dup (id INTEGER)`)
	run(t, d, "s", `INSERT INTO dup VALUES (1),(2),(3)`)

	rep, err := RestoreTrash(d.dir, "latest")
	if err != nil {
		t.Fatalf("恢复: %v", err)
	}
	if len(rep.Restored) != 0 || len(rep.Skipped) != 1 || rep.Skipped[0] != "dup" {
		t.Fatalf("应该跳过同名表: %+v", rep)
	}
	if got := d.Store.RowCount("dup"); got != 3 {
		t.Fatalf("恢复不该覆盖已存在的表，行数 = %d", got)
	}
}

// 空回收站 / 不存在的备份名都要给出明确错误，而不是静默成功。
func TestRestoreErrors(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if _, err := RestoreTrash(dir, "latest"); err == nil {
		t.Fatalf("空回收站应该报错")
	}
	if _, err := RestoreTrash(dir, "不存在的备份"); err == nil {
		t.Fatalf("不存在的备份名应该报错")
	}
}

// 备份文件必须真的落在 .trash 下，并且能被 catalog 之外的信息还原（manifest 自带表定义）。
func TestTrashDirLayout(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s", `CREATE TABLE lay (id INTEGER)`)
	run(t, d, "s", `INSERT INTO lay VALUES (1)`)
	run(t, d, "s", `DROP TABLE lay`)

	ents := trashEntries(t, d.dir)
	if len(ents) != 1 {
		t.Fatalf("备份数 = %d，期望 1", len(ents))
	}
	base := filepath.Join(d.dir, storage.TrashDirName(), ents[0])
	for _, name := range []string{"manifest.json", "catalog.json"} {
		if _, err := os.Stat(filepath.Join(base, name)); err != nil {
			t.Fatalf("备份里缺少 %s: %v", name, err)
		}
	}
	files, err := os.ReadDir(filepath.Join(base, "tables"))
	if err != nil {
		t.Fatalf("读备份表目录: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("备份表文件数 = %d，期望 1", len(files))
	}
	info, err := files[0].Info()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() <= 24 {
		t.Fatalf("备份文件只有 %d 字节，应该包含行数据", info.Size())
	}
}
