package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VACUUM 要把「删掉的行留下的洞」和「尾部空页」收回来，同时不动 RID
// （索引里存的就是槽位号）。
func TestVacuumReclaimsSpace(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	s := "s"
	run(t, d, s, `CREATE TABLE vc (id INTEGER PRIMARY KEY, pad TEXT)`)
	tbl, ok := d.Cat.Get("vc")
	if !ok {
		t.Fatal("catalog 里找不到 vc")
	}
	dataPath := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.dat", tbl.ID))
	size := func() int64 {
		st, err := os.Stat(dataPath)
		if err != nil {
			t.Fatalf("stat 数据文件: %v", err)
		}
		return st.Size()
	}

	pad := strings.Repeat("x", 200)
	tx := strings.Builder{}
	tx.WriteString("INSERT INTO vc VALUES ")
	for i := 1; i <= 200; i++ {
		if i > 1 {
			tx.WriteString(",")
		}
		fmt.Fprintf(&tx, "(%d,'%s')", i, pad)
	}
	run(t, d, s, tx.String())
	run(t, d, s, `CHECKPOINT`)

	before := size()
	if before < 5*8192 {
		t.Fatalf("数据文件只有 %d 字节，测试前提不成立（应该占好几页）", before)
	}

	// 删掉尾部 80% 的行：前面几页还有行，后面几页整页空了
	run(t, d, s, `DELETE FROM vc WHERE id > 40`)
	run(t, d, s, `CHECKPOINT`)
	if got := size(); got != before {
		t.Fatalf("checkpoint 不该改变文件大小（删行只置空槽位），%d → %d", before, got)
	}
	if got := d.Store.RowCount("vc"); got != 40 {
		t.Fatalf("删完行数 = %d，期望 40", got)
	}

	res := run(t, d, s, `VACUUM vc`)
	if res[0].RowCount != 1 {
		t.Fatalf("VACUUM 应该返回一张表的统计，实际 %d 行", res[0].RowCount)
	}
	row := res[0].Rows[0]
	reclaimed := row[2].(int64)
	after := size()
	if reclaimed <= 0 {
		t.Fatalf("应该回收了尾部空页，实际 %d（%d 字节 → %d 字节）", reclaimed, before, after)
	}
	if after >= before {
		t.Fatalf("VACUUM 之后文件应该变小：%d → %d", before, after)
	}
	t.Logf("200 行 → 删剩 40 行：%d 字节 → VACUUM 后 %d 字节，整理 %d 页、回收 %d 页（共 %d 页）",
		before, after, row[1].(int64), reclaimed, row[3].(int64))

	// 数据还得完整、主键索引还得能用
	assertRows(t, run(t, d, s, `SELECT count(*) FROM vc`), []string{"40"})
	assertRows(t, run(t, d, s, `SELECT id FROM vc ORDER BY id DESC LIMIT 2`), []string{"40", "39"})
	assertRows(t, run(t, d, s, `SELECT pad FROM vc WHERE id = 7`), []string{pad})

	// 重启后依然正确（页文件被整理过，行必须原样读得回来）
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT count(*), max(id) FROM vc`), []string{"40|40"})
	assertRows(t, run(t, d2, s, `SELECT pad FROM vc WHERE id = 39`), []string{pad})
}

// VACUUM 不能在事务块里跑（要改磁盘上的页，回滚不了）。
func TestVacuumRejectedInTransaction(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE vt (id INTEGER)`)
	run(t, d, s, `INSERT INTO vt VALUES (1)`)
	run(t, d, s, `BEGIN`)
	if _, err := d.Exec(d.Session(s), `VACUUM vt`); err == nil {
		t.Fatal("事务里执行 VACUUM 应该报错")
	}
	run(t, d, s, `ROLLBACK`)
	// 事务外可以用，不带表名也行
	if _, err := d.Exec(d.Session(s), `VACUUM`); err != nil {
		t.Fatalf("VACUUM（全库）应该成功: %v", err)
	}
}
