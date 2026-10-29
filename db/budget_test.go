package db

import (
	"fmt"
	"strings"
	"testing"
)

// 全局内存预算：两张表**共享**同一份页数上限。
//
// 以前是「每张表各 1024 页」，表一多就偷偷吃掉几倍内存；改成全库共享之后，
// 两张表都装不下自己，但谁也不会因为「另一位占着额度」而读不出来。
func TestGlobalBudgetAcrossTables(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 4, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	s := "s"
	pad := strings.Repeat("z", 120)
	for _, name := range []string{"ga", "gb"} {
		run(t, d, s, fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, v TEXT)`, name))
		var sb strings.Builder
		fmt.Fprintf(&sb, `INSERT INTO %s VALUES `, name)
		for i := 1; i <= 400; i++ {
			if i > 1 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "(%d,'%s-%03d')", i, pad, i)
		}
		run(t, d, s, sb.String())
	}
	run(t, d, s, `CHECKPOINT`)

	ta, tb := d.Store.Table("ga"), d.Store.Table("gb")
	ra, rb := ta.ResidentRows(), tb.ResidentRows()
	t.Logf("驻留行数：ga %d / gb %d（各 400 行，池子共 4 页）", ra, rb)
	// 4 页 ≈ 一百多行，两张表不可能都整张留在内存里
	if ra+rb > 400 {
		t.Fatalf("两张表一共驻留 %d 行，池子只有 4 页，说明预算没生效", ra+rb)
	}
	// 预算账要平：用掉的页数不超过配置值
	_, _, used, total, _, _ := ta.PoolStats()
	if total != 4 || used > 4 {
		t.Fatalf("预算 used=%d total=%d，期望不超过 4", used, total)
	}

	// 两边都要读得对（换出去的从页文件换回来）
	assertRows(t, run(t, d, s, `SELECT count(*), sum(id) FROM ga`), []string{"400|80200"})
	assertRows(t, run(t, d, s, `SELECT count(*), sum(id) FROM gb`), []string{"400|80200"})
	// 交错扫两张表：谁也不该把另一位挤出错误结果
	assertRows(t, run(t, d, s, `SELECT (SELECT count(*) FROM ga) + (SELECT count(*) FROM gb)`), []string{"800"})
	assertRows(t, run(t, d, s, `SELECT v FROM ga WHERE id = 300`), []string{pad + "-300"})
	assertRows(t, run(t, d, s, `SELECT v FROM gb WHERE id = 300`), []string{pad + "-300"})
}
