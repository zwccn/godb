package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 单行能存 1MB 文本：行里只留指针，数据在 .lob 文件里。
func TestBigTextStoredInLob(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE art (id INTEGER PRIMARY KEY, body TEXT)`)

	big := strings.Repeat("x", 900000) // 900KB（ASCII：字符数 == 字节数）
	run(t, d, "s", fmt.Sprintf(`INSERT INTO art VALUES (1, '%s')`, big))
	run(t, d, "s", `INSERT INTO art VALUES (2, 'short')`)
	assertRows(t, run(t, d, "s", `SELECT length(body) FROM art WHERE id = 1`), []string{fmt.Sprint(len(big))})

	// 行数据页只占一页（<16KB），正文全在 .lob 里
	ct, _ := d.Store.Cat.Get("art")
	dat := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.dat", ct.ID))
	lob := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.lob", ct.ID))
	run(t, d, "s", `CHECKPOINT`)
	datSize := fileSize(t, dat)
	lobSize := fileSize(t, lob)
	t.Logf("数据页 %d 字节，大字段 %d 字节", datSize, lobSize)
	if datSize > 2*8192 {
		t.Fatalf("两行的表占了 %d 字节数据页，大字段没被外置", datSize)
	}
	if lobSize < int64(len(big)) {
		t.Fatalf("大字段文件只有 %d 字节，正文没写进去", lobSize)
	}

	// 重启后内容一模一样
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT length(body) FROM art WHERE id = 1`), []string{fmt.Sprint(len(big))})
	got := rowsOf(t, run(t, d2, "s", `SELECT body FROM art WHERE id = 1`))
	if len(got) != 1 {
		t.Fatalf("查回 %d 行", len(got))
	}
	if str, _ := got[0][0].(string); str != big {
		t.Fatalf("重启后的大字段内容不一致（读到 %d 字节）", len(str))
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return st.Size()
}

// 大字段必须能扛住「拔电」：正文已经 fsync 过、行在 WAL 里重放回来。
func TestBigTextSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE art (id INTEGER PRIMARY KEY, body TEXT)`)
	big := strings.Repeat("y", 300000)
	run(t, d, "s", fmt.Sprintf(`INSERT INTO art VALUES (1, '%s')`, big))
	// 不 flush：模拟进程被 kill（页还没写，行只在 WAL 里）
	if err := d.Store.CloseWithoutFlush(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT length(body) FROM art`), []string{"300000"})
	got := rowsOf(t, run(t, d2, "s", `SELECT body FROM art WHERE id = 1`))
	if str, _ := got[0][0].(string); str != big {
		t.Fatalf("重放之后正文变了（读到 %d 字节）", len(str))
	}
}

// UPDATE 换掉一个大字段：旧正文变成孤儿（追加写），但数据必须还是对的。
func TestBigTextUpdateAndDrop(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE upd (id INTEGER PRIMARY KEY, body TEXT)`)
	first := strings.Repeat("a", 5000)
	second := strings.Repeat("b", 7000)
	// 先落一次盘：旧版本真的写进大字段文件了，再看 UPDATE 留下的孤儿
	run(t, d, "s", fmt.Sprintf(`INSERT INTO upd VALUES (1, '%s')`, first))
	run(t, d, "s", `CHECKPOINT`)
	run(t, d, "s", fmt.Sprintf(`UPDATE upd SET body = '%s' WHERE id = 1`, second))
	run(t, d, "s", `CHECKPOINT`)

	ct, _ := d.Store.Cat.Get("upd")
	lob := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.lob", ct.ID))
	// 追加写：两个版本都在文件里（旧版本等 VACUUM 回收）
	if n := fileSize(t, lob); n < int64(len(first)+len(second)) {
		t.Fatalf("大字段文件 %d 字节，两个版本应该都在（追加写）", n)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	got := rowsOf(t, run(t, d2, "s", `SELECT body FROM upd`))
	if str, _ := got[0][0].(string); str != second {
		t.Fatalf("UPDATE 之后读到的不是新值（读到 %d 字节）", len(str))
	}

	// DROP 之后大字段文件也要消失
	run(t, d2, "s", `DROP TABLE upd`)
	run(t, d2, "s", `CHECKPOINT`)
	if _, err := os.Stat(lob); !os.IsNotExist(err) {
		t.Fatalf("DROP 之后 .lob 还在：%v", err)
	}
}

// TRUNCATE 要把大字段文件也清空（否则空表还占着几百 MB）。
func TestTruncateClearsLob(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE t1 (id INTEGER PRIMARY KEY, body TEXT)`)
	run(t, d, "s", fmt.Sprintf(`INSERT INTO t1 VALUES (1, '%s')`, strings.Repeat("z", 200000)))
	run(t, d, "s", `CHECKPOINT`)
	ct, _ := d.Store.Cat.Get("t1")
	lob := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.lob", ct.ID))
	if fileSize(t, lob) < 200000 {
		t.Fatal("大字段没写进去")
	}
	run(t, d, "s", `TRUNCATE t1`)
	run(t, d, "s", `CHECKPOINT`)
	if n := fileSize(t, lob); n != 0 {
		t.Fatalf("TRUNCATE 之后 .lob 还有 %d 字节", n)
	}
}

// 索引键长有上限：文本能被存成 1MB，但索引键必须放得进一页。
func TestLongTextIndexRejected(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE ix (id INTEGER PRIMARY KEY, body TEXT)`)
	run(t, d, "s", `CREATE INDEX ix_body ON ix (body)`)
	err = runErr(t, d, "s", fmt.Sprintf(`INSERT INTO ix VALUES (1, '%s')`, strings.Repeat("q", 5000)))
	if err == nil || !strings.Contains(err.Error(), "index row size") {
		t.Fatalf("期望「索引键过长」的错误，实际 %v", err)
	}
	// 小值照常
	run(t, d, "s", `INSERT INTO ix VALUES (2, 'ok')`)
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM ix`), []string{"1"})

	// 数据先在、索引后建：同样要拒绝（否则索引建出来就是残的）
	run(t, d, "s", `CREATE TABLE pre (id INTEGER PRIMARY KEY, body TEXT)`)
	run(t, d, "s", fmt.Sprintf(`INSERT INTO pre VALUES (1, '%s')`, strings.Repeat("w", 5000)))
	if err := runErr(t, d, "s", `CREATE INDEX pre_body ON pre (body)`); !strings.Contains(err.Error(), "index row size") {
		t.Fatalf("CREATE INDEX 期望报索引键过长，实际 %v", err)
	}
}

// VACUUM FULL 整份重写：大字段里那些「UPDATE 留下的旧正文」要被收掉，
// 同时 RID 不变，所以索引照样能用（点查走索引）。
func TestVacuumFullReclaimsLob(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	run(t, d, "s", `CREATE TABLE art (id INTEGER PRIMARY KEY, tag TEXT, body TEXT)`)

	const n = 40
	var sb strings.Builder
	sb.WriteString(`INSERT INTO art VALUES `)
	for i := 1; i <= n; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'t%d','%s')", i, i, strings.Repeat("a", 20000))
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `CHECKPOINT`)

	ct, _ := d.Store.Cat.Get("art")
	lob := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.lob", ct.ID))
	dat := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.dat", ct.ID))
	before := fileSize(t, lob)
	// 每行再 UPDATE 一次：旧正文全变成垃圾（追加写）
	for round := 0; round < 3; round++ {
		run(t, d, "s", fmt.Sprintf(`UPDATE art SET body = '%s' WHERE id <= %d`, strings.Repeat("b", 20000), n))
		run(t, d, "s", `CHECKPOINT`)
	}
	fat := fileSize(t, lob)
	if fat < 4*before {
		t.Fatalf("三次 UPDATE 之后大字段只有 %d 字节（原来 %d），不像追加写", fat, before)
	}
	datBefore := fileSize(t, dat)

	// VACUUM 只统计，不动文件
	res := run(t, d, "s", `VACUUM art`)
	rows := rowsOf(t, res)
	t.Logf("VACUUM 报告：%v", rows[0])
	if rows[0][5].(int64) != fat {
		t.Logf("VACUUM 报的 lob_bytes = %v（实际 %d）", rows[0][5], fat)
	}
	if rows[0][6].(int64) <= 0 || rows[0][6].(int64) >= fat {
		t.Fatalf("lob_live 应该是一个正的、小于文件大小的数，实际 %v", rows[0][6])
	}

	// VACUUM FULL 整份重写
	res = run(t, d, "s", `VACUUM FULL art`)
	rows = rowsOf(t, res)
	t.Logf("VACUUM FULL 报告：%v", rows[0])
	after := fileSize(t, lob)
	if after >= fat/2 {
		t.Fatalf("重写之后大字段还有 %d 字节（重写前 %d），没回收多少", after, fat)
	}
	if n*20000 > after || after >= n*20000+1024*(n+4) {
		t.Fatalf("重写之后大字段 %d 字节，应该约等于 %d", after, n*20000)
	}
	if datAfter := fileSize(t, dat); datAfter > datBefore+8192 {
		t.Fatalf("重写之后页文件变大了：%d → %d", datBefore, datAfter)
	}

	// 数据、索引、重启都要正常
	assertRows(t, run(t, d, "s", `SELECT count(*), min(id), max(id) FROM art`), []string{"40|1|40"})
	assertRows(t, run(t, d, "s", `SELECT length(body) FROM art WHERE id = 17`), []string{"20000"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM art WHERE tag = 't33'`), []string{"1"})

	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM art`), []string{"40"})
	assertRows(t, run(t, d2, "s", `SELECT length(body) FROM art WHERE id = 40`), []string{"20000"})
	if n := fileSize(t, lob); n >= fat/2 {
		t.Fatalf("重启之后大字段又变回 %d 字节", n)
	}
}

// 大字段也要能进回收站、再捞回来（DROP → 恢复 → 重启）。
func TestBigTextThroughTrash(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d, "s", `CREATE TABLE lost (id INTEGER PRIMARY KEY, body TEXT)`)
	big := strings.Repeat("k", 400000)
	run(t, d, "s", fmt.Sprintf(`INSERT INTO lost VALUES (1, '%s')`, big))
	run(t, d, "s", `DROP TABLE lost`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := RestoreTrash(dir, "latest"); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT length(body) FROM lost`), []string{"400000"})
	got := rowsOf(t, run(t, d2, "s", `SELECT body FROM lost WHERE id = 1`))
	if str, _ := got[0][0].(string); str != big {
		t.Fatalf("恢复之后正文不一致（%d 字节）", len(str))
	}
}
