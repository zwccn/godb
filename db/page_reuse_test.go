package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataFileSize 返回目录里唯一那张表的数据页文件大小（测试里一个库只建一张表）。
func dataFileSize(t *testing.T, dir string) int64 {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "tables", "t*.dat"))
	if err != nil || len(m) != 1 {
		t.Fatalf("期望 1 个数据文件，实际 %v（%v）", m, err)
	}
	st, err := os.Stat(m[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return st.Size()
}

// 掏空中间一片行之后再插入：**表文件不该继续涨**（空洞要被复用）。
//
// 没有空闲空间图（FSM）时，插入只往回看最后 8 页，中间页被删出来的空间永远用不上，
// 于是「删一半再插一半」会让文件翻倍。这条用例就是盯这个的。
func TestMiddlePageReuseKeepsFileSmall(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 16, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	run(t, d, "s", `CREATE TABLE re (id INTEGER PRIMARY KEY, pad TEXT)`)
	pad := strings.Repeat("p", 200)

	insert := func(from, to int) {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO re VALUES `)
		for i := from; i <= to; i++ {
			if i > from {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "(%d,'%s-%06d')", i, pad, i)
		}
		run(t, d, "s", sb.String())
	}

	insert(1, 2000)
	run(t, d, "s", `CHECKPOINT`)
	full := dataFileSize(t, dir)
	if full < 32*1024 {
		t.Fatalf("2000 行只占 %d 字节，测试数据不够大", full)
	}

	// 从中间挖掉 1200 行（不是尾部：尾部会被 VACUUM 正常摘掉，不考验 FSM），
	// 再 VACUUM 把这些洞整理成「真实空闲」——这正是 FSM 要认的那个数。
	run(t, d, "s", `DELETE FROM re WHERE id > 400 AND id <= 1600`)
	run(t, d, "s", `CHECKPOINT`)
	run(t, d, "s", `VACUUM re`)
	afterDelete := dataFileSize(t, dir)

	// 再插 1200 行：应该填进中间那些洞，而不是往文件尾追加新页
	insert(5001, 6200)
	run(t, d, "s", `CHECKPOINT`)
	afterInsert := dataFileSize(t, dir)
	t.Logf("文件大小：满 %d → 删后 %d → 再插 %d 字节", full, afterDelete, afterInsert)
	if afterInsert > afterDelete {
		t.Fatalf("中间页的空洞没被复用：删后 %d 字节，插入后涨到 %d 字节", afterDelete, afterInsert)
	}
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM re`), []string{"2000"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM re WHERE id = 5001`), []string{"1"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM re WHERE id > 400 AND id <= 1600`), []string{"0"})

	// 重启（空闲图由加载时重建）之后同样成立
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, Options{BufferPoolPages: 16, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM re`), []string{"2000"})
	run(t, d2, "s", `DELETE FROM re WHERE id > 5000 AND id <= 6200`)
	run(t, d2, "s", `CHECKPOINT`)
	run(t, d2, "s", `VACUUM re`) // 重启后的图是加载时重建的，VACUUM 再把它刷新成真实空闲
	before2 := dataFileSize(t, dir)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO re VALUES `)
	for i := 8001; i <= 9200; i++ {
		if i > 8001 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'%s-%06d')", i, pad, i)
	}
	run(t, d2, "s", sb.String())
	run(t, d2, "s", `CHECKPOINT`)
	after2 := dataFileSize(t, dir)
	if after2 > before2 {
		t.Fatalf("重启后空洞没复用：%d → %d 字节", before2, after2)
	}
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM re`), []string{"2000"})
	assertRows(t, run(t, d2, "s", `SELECT pad FROM re WHERE id = 9200`), []string{pad + "-009200"})
}
