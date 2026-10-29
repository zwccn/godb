package db

import (
	"fmt"
	"strings"
	"testing"
)

// 行换出：缓冲池只有 2 页时，一张十几页的表不可能整张留在内存里。
// 这里验证「换出之后读/改/删全都还对」——换出容易，换回来不错才难。
func TestRowsEvictedFromMemory(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{
		BufferPoolPages: 2, // 两页 = 16KB，逼着不停换进换出
		Lock:            testOptions().Lock,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := "s"
	run(t, d, s, `CREATE TABLE ev (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, s, `CREATE INDEX ev_v ON ev (v)`)

	// 500 行、每行带 120 字节填充 → 约 13 页，远超 2 页的池子
	pad := strings.Repeat("p", 120)
	tx := strings.Builder{}
	tx.WriteString("INSERT INTO ev VALUES ")
	for i := 1; i <= 500; i++ {
		if i > 1 {
			tx.WriteString(",")
		}
		fmt.Fprintf(&tx, "(%d,'%s-%03d')", i, pad, i)
	}
	run(t, d, s, tx.String())
	run(t, d, s, `CHECKPOINT`)

	td := d.Store.Table("ev")
	if got := td.Live; got != 500 {
		t.Fatalf("Live = %d，期望 500", got)
	}
	// checkpoint 之后内存和磁盘一致，这一页之外的行应该已经被放掉
	afterCP := td.ResidentRows()
	if afterCP >= 500 {
		t.Fatalf("checkpoint 之后还驻留 %d 行（共 500 行），说明没有换出", afterCP)
	}

	// 全表扫一遍（会不停换进换出），驻留量必须保持在池子附近
	assertRows(t, run(t, d, s, `SELECT count(*), sum(id) FROM ev`), []string{"500|125250"})
	after := td.ResidentRows()
	// 池子只有 2 页（≈每页一两百行），留点余量
	if after > 400 {
		t.Fatalf("换出效果不够：扫描后还驻留 %d 行（池子只有 2 页）", after)
	}
	t.Logf("驻留行数：checkpoint 后 %d，扫描后 %d（共 500 行）", afterCP, after)

	// 换出去的行必须能原样换回来
	assertRows(t, run(t, d, s, `SELECT id, substr(v, 1, 1), length(v) FROM ev WHERE id = 7`),
		[]string{"7|p|124"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ev WHERE id BETWEEN 100 AND 199`), []string{"100"})
	// 二级索引（走索引取回行，同样要求换回）
	assertRows(t, run(t, d, s, `SELECT id FROM ev WHERE v = '`+pad+`-042'`), []string{"42"})

	// 改一行：老行可能不在内存里，得先换回来才能维护索引
	run(t, d, s, `UPDATE ev SET v = 'changed' WHERE id = 300`)
	assertRows(t, run(t, d, s, `SELECT v FROM ev WHERE id = 300`), []string{"changed"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ev WHERE v = '`+pad+`-300'`), []string{"0"})

	// 删一行，再插一行复用槽位
	run(t, d, s, `DELETE FROM ev WHERE id = 300`)
	run(t, d, s, `INSERT INTO ev VALUES (900, 'late')`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ev`), []string{"500"})
	assertRows(t, run(t, d, s, `SELECT v FROM ev WHERE id = 900`), []string{"late"})

	// 落盘 + 重启：换出的行必须真的在页文件里
	run(t, d, s, `CHECKPOINT`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, Options{BufferPoolPages: 2, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT count(*), sum(id) FROM ev`), []string{"500|125850"})
	assertRows(t, run(t, d2, s, `SELECT v FROM ev WHERE id = 900`), []string{"late"})
	assertRows(t, run(t, d2, s, `SELECT id FROM ev WHERE v = '`+pad+`-250'`), []string{"250"})
}

// 换出不等于丢数据：insert 之后**还没 checkpoint** 的行必须留在内存里
// （它们只在 pending 和 WAL 里，页文件上还没有）。
func TestPendingRowsStayResident(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 2, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	s := "s"
	run(t, d, s, `CREATE TABLE pr (id INTEGER PRIMARY KEY, v TEXT)`)
	pad := strings.Repeat("q", 120)
	// 一条语句插完（一次提交）：分 200 次提交会撞上自动 checkpoint，
	// pending 被清空，就不是在测「未落盘的行」了。
	vals := strings.Builder{}
	for i := 1; i <= 200; i++ {
		if i > 1 {
			vals.WriteString(",")
		}
		fmt.Fprintf(&vals, "(%d,'%s')", i, pad)
	}
	run(t, d, s, "INSERT INTO pr VALUES "+vals.String())
	// 这些行还没落盘（pending 非空），即使扫一遍也不该被换出去
	run(t, d, s, `SELECT count(*) FROM pr`)
	td := d.Store.Table("pr")
	if got := td.ResidentRows(); got != 200 {
		t.Fatalf("未落盘的行应该全部驻留，实际 %d", got)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM pr`), []string{"200"})
}
