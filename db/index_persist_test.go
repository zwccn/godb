package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 建一张有主键 + 二级索引 + UNIQUE 的表，写满几页。
func seedIndexTable(t *testing.T, d *DB, n int) {
	t.Helper()
	run(t, d, "s", `CREATE TABLE ip (id INTEGER PRIMARY KEY, v TEXT, u TEXT UNIQUE)`)
	run(t, d, "s", `CREATE INDEX ip_v ON ip (v)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO ip VALUES `)
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v-%04d','u-%04d')", i, i, i)
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `CHECKPOINT`)
}

// 索引落盘的核心断言：重启之后**不重建**，而且照样查得对。
func TestIndexPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedIndexTable(t, d, 600)
	td := d.Store.Table("ip")
	if pages := td.IndexPages(); pages == 0 {
		t.Fatal("索引一个页都没落地")
	} else {
		t.Logf("索引页数：%d（主键 + 二级 + UNIQUE）", pages)
	}
	if got := d.Store.IdxRebuilds(); got != 0 {
		t.Fatalf("首次建表不该算重建，实际 %d 次", got)
	}
	// 索引文件真的在磁盘上
	matches, _ := filepath.Glob(filepath.Join(dir, "tables", "i*.idx"))
	if len(matches) != 3 {
		t.Fatalf("期望 3 个索引文件（主键/二级/UNIQUE），实际 %d：%v", len(matches), matches)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT count(*), sum(id) FROM ip`), []string{"600|180300"})
	assertRows(t, run(t, d2, "s", `SELECT v FROM ip WHERE id = 123`), []string{"v-0123"})
	// 二级索引（走索引取回行，不扫表）
	assertRows(t, run(t, d2, "s", `SELECT id FROM ip WHERE v = 'v-0456'`), []string{"456"})
	// UNIQUE 索引还要拦重复（重启后走的是落盘的树，不是重建出来的）
	if err := runErr(t, d2, "s", `INSERT INTO ip VALUES (9999,'x','u-0001')`); !strings.Contains(err.Error(), "unique") {
		t.Fatalf("期望 UNIQUE 冲突，实际 %v", err)
	}
	if got := d2.Store.IdxRebuilds(); got != 0 {
		t.Fatalf("重启后重建了 %d 次索引 —— 落盘等于没用", got)
	}

	// 重启后继续增删改：页式树的增量维护必须对
	run(t, d2, "s", `DELETE FROM ip WHERE id = 7`)
	run(t, d2, "s", `UPDATE ip SET v = 'changed' WHERE id = 300`)
	run(t, d2, "s", `INSERT INTO ip VALUES (601,'v-0601','u-0601')`)
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM ip`), []string{"600"})
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM ip WHERE v = 'changed'`), []string{"1"})
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM ip WHERE v = 'v-0300'`), []string{"0"})
	run(t, d2, "s", `CHECKPOINT`)
	if err := d2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}

	d3, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen2: %v", err)
	}
	defer d3.Close()
	if got := d3.Store.IdxRebuilds(); got != 0 {
		t.Fatalf("第二次重启又重建了 %d 次索引", got)
	}
	assertRows(t, run(t, d3, "s", `SELECT count(*), sum(id) FROM ip`), []string{"600|180894"})
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM ip WHERE id = 7`), []string{"0"})
	assertRows(t, run(t, d3, "s", `SELECT v FROM ip WHERE id = 601`), []string{"v-0601"})
}

// 索引页坏掉（模拟断电写了半页 / 磁盘坏道）：打开时应该认出坏页、整棵重建，
// 而不是拿着坏索引返回错误结果。
func TestCorruptIndexHealsByRebuild(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedIndexTable(t, d, 400)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 把每个索引文件除头页之外的内容全弄坏
	matches, _ := filepath.Glob(filepath.Join(dir, "tables", "i*.idx"))
	if len(matches) == 0 {
		t.Fatal("没找到索引文件")
	}
	junk := make([]byte, 8192)
	for i := range junk {
		junk[i] = 0x5A
	}
	for _, m := range matches {
		fh, err := os.OpenFile(m, os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("open %s: %v", m, err)
		}
		if _, err := fh.WriteAt(junk, 8192); err != nil {
			t.Fatalf("corrupt %s: %v", m, err)
		}
		fh.Close()
	}

	d2, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	// 表是懒加载的：先碰一次它，索引文件才会被打开、校验、按需重建
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM ip`), []string{"400"})
	if got := d2.Store.IdxRebuilds(); got == 0 {
		t.Fatal("坏索引没被识别出来 —— 会拿坏索引查出错误结果")
	} else {
		t.Logf("坏索引触发重建 %d 次", got)
	}
	assertRows(t, run(t, d2, "s", `SELECT count(*), sum(id) FROM ip`), []string{"400|80200"})
	assertRows(t, run(t, d2, "s", `SELECT id FROM ip WHERE v = 'v-0399'`), []string{"399"})
	assertRows(t, run(t, d2, "s", `SELECT u FROM ip WHERE id = 1`), []string{"u-0001"})
}

// TRUNCATE / DROP INDEX / DROP TABLE 要顺手把索引文件收拾掉，不能留孤儿。
func TestIndexFilesCleanedUp(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	seedIndexTable(t, d, 50)
	tablesDir := filepath.Join(dir, "tables")
	count := func() int {
		m, _ := filepath.Glob(filepath.Join(tablesDir, "i*.idx"))
		return len(m)
	}
	if got := count(); got != 3 {
		t.Fatalf("期望 3 个索引文件，实际 %d", got)
	}
	run(t, d, "s", `DROP INDEX ip_v`)
	if got := count(); got != 2 {
		t.Fatalf("DROP INDEX 之后应剩 2 个索引文件，实际 %d", got)
	}
	run(t, d, "s", `TRUNCATE ip`)
	run(t, d, "s", `CHECKPOINT`)
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM ip`), []string{"0"})
	// 清空之后还能继续用（索引必须也是空的，否则会查出幽灵行）
	run(t, d, "s", `INSERT INTO ip VALUES (1,'a','a')`)
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM ip`), []string{"1"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM ip WHERE id = 1`), []string{"1"})
	run(t, d, "s", `DROP TABLE ip`)
	if got := count(); got != 0 {
		t.Fatalf("DROP TABLE 之后索引文件应清空，实际 %d", got)
	}
}

// 大批删除之后 VACUUM 要顺手重建碎片索引（延迟合并只摘键、不合并页）。
func TestVacuumRebuildsFragmentedIndex(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	run(t, d, "s", `CREATE TABLE vf (id INTEGER PRIMARY KEY, v TEXT)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO vf VALUES `)
	for i := 1; i <= 3000; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v%d')", i, i)
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `CHECKPOINT`)
	td := d.Store.Table("vf")
	before := td.IndexPages()
	run(t, d, "s", `DELETE FROM vf WHERE id > 100`)
	run(t, d, "s", `CHECKPOINT`)
	run(t, d, "s", `VACUUM vf`)
	after := td.IndexPages()
	t.Logf("索引页：删前 %d，删后 VACUUM %d", before, after)
	if after == 0 || after >= before {
		t.Fatalf("VACUUM 之后索引没缩小：%d → %d", before, after)
	}
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM vf`), []string{"100"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM vf WHERE id = 100`), []string{"1"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM vf WHERE id = 101`), []string{"0"})
}
