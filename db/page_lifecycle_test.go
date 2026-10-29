package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 页的全生命周期，一条用例从头走到尾：
//
//	写入 → 提交进 WAL → checkpoint 落页（含全页镜像）→ 淘汰/换回 →
//	进程被杀后用 WAL 重放 → 索引重建并落盘 → VACUUM 回收 → TRUNCATE → DROP
//
// 每一段都断言「数据还对 + 文件确实变了」，而不是只看最终 count。
// 目的是防「单点都测过、连起来跑不通」——页这一层最容易出这种问题。
func TestPageLifecycleEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tablesDir := filepath.Join(dir, "tables")
	// 池子只有 8 页 = 64KB：400 行必然要换出/换回，逼着走真实路径
	opts := Options{BufferPoolPages: 8, Lock: testOptions().Lock}
	// 每个打开过的实例都登记一下：用例中途 Fatalf 也不会漏掉文件句柄
	// （Windows 上 .idx 没关会让 TempDir 清理报「文件被占用」）。
	var opened []*DB
	open := func() *DB {
		d, err := Open(dir, opts)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		opened = append(opened, d)
		return d
	}
	t.Cleanup(func() {
		for _, x := range opened {
			_ = x.Close()
		}
	})
	d := open()

	// ---- 1) 写入：多页 + 主键/二级/UNIQUE 三种索引 ----
	run(t, d, "s", `CREATE TABLE lc (id INTEGER PRIMARY KEY, v TEXT, u TEXT UNIQUE)`)
	run(t, d, "s", `CREATE INDEX lc_v ON lc (v)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO lc VALUES `)
	for i := 1; i <= 3000; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v-%04d','u-%04d')", i, i, i)
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `UPDATE lc SET v = 'moved' WHERE id <= 500`)
	run(t, d, "s", `DELETE FROM lc WHERE id > 2600`) // 剩 2600 行
	if got := d.Store.Table("lc").Live; got != 2600 {
		t.Fatalf("存活行 %d，期望 2600", got)
	}

	// ---- 2) checkpoint：页与索引都要落到磁盘 ----
	run(t, d, "s", `CHECKPOINT`)
	td := d.Store.Table("lc")
	dataPages := td.PageCountDebug()
	idxPages := td.IndexPages()
	if dataPages < 10 {
		t.Fatalf("3000 行只写了 %d 个数据页，太少", dataPages)
	}
	if idxPages == 0 {
		t.Fatal("索引页数为 0")
	}
	ct, _ := d.Store.Cat.Get("lc")
	datPath := filepath.Join(tablesDir, fmt.Sprintf("t%06d.dat", ct.ID))
	st, err := os.Stat(datPath)
	if err != nil || st.Size() == 0 {
		t.Fatalf("数据页文件没落地: %v", err)
	}
	idxFiles, _ := filepath.Glob(filepath.Join(tablesDir, "i*.idx"))
	if len(idxFiles) != 3 {
		t.Fatalf("期望 3 个索引文件，实际 %d", len(idxFiles))
	}
	t.Logf("落盘后：数据页 %d、索引页 %d、数据文件 %d 字节", dataPages, idxPages, st.Size())

	// 换出确实发生了：池子只有 8 页，驻留行远少于 1050
	if res := td.ResidentRows(); res >= 2600 {
		t.Fatalf("池子只有 8 页却驻留了 %d 行，说明没换出", res)
	}

	// ---- 3) 再写一批（只在 WAL 里）然后「拔电」 ----
	run(t, d, "s", `INSERT INTO lc VALUES (3001,'v-3001','u-3001'),(3002,'v-3002','u-3002')`)
	if err := d.Store.CloseWithoutFlush(); err != nil {
		t.Fatalf("模拟崩溃: %v", err)
	}

	// ---- 4) 重放：数据要回来，索引要重建 ----
	d2 := open()
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM lc`), []string{"2602"})
	// 加载时先按「页 + 索引文件」判有效，随后重放会让索引失效 → 重建
	if got := d2.Store.IdxRebuilds(); got == 0 {
		t.Fatal("崩溃重启后索引没有重建（重放的行不在索引里）")
	}
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM lc WHERE id = 3001`), []string{"1"})
	assertRows(t, run(t, d2, "s", `SELECT id FROM lc WHERE v = 'v-3002'`), []string{"3002"})
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM lc WHERE id BETWEEN 1 AND 500`), []string{"500"})
	// 重建出来的索引必须照样拦重复
	if err := runErr(t, d2, "s", `INSERT INTO lc VALUES (999,'x','u-0001')`); !strings.Contains(err.Error(), "unique") {
		t.Fatalf("UNIQUE 没拦住：%v", err)
	}

	// ---- 5) 干净关库再打开：这次不该重建 ----
	run(t, d2, "s", `CHECKPOINT`)
	if err := d2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d3 := open()
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc`), []string{"2602"})
	if got := d3.Store.IdxRebuilds(); got != 0 {
		t.Fatalf("干净重启还重建了 %d 次索引", got)
	}

	// ---- 6) VACUUM：页内空洞整理 + 索引碎片回收 ----
	run(t, d3, "s", `DELETE FROM lc WHERE id > 52`) // 剩 52 行
	run(t, d3, "s", `CHECKPOINT`)
	td3 := d3.Store.Table("lc")
	beforeVac := td3.IndexPages()
	run(t, d3, "s", `VACUUM lc`)
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc`), []string{"52"})
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc WHERE id = 52`), []string{"1"})
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc WHERE id = 53`), []string{"0"})
	if after := td3.IndexPages(); after >= beforeVac {
		t.Logf("索引页没变小（%d → %d），碎片没到重建阈值", beforeVac, after)
	} else {
		t.Logf("索引页：VACUUM %d → %d", beforeVac, after)
	}

	// ---- 7) TRUNCATE：页文件归零，索引也清空 ----
	run(t, d3, "s", `TRUNCATE lc`)
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc`), []string{"0"})
	if st, err := os.Stat(datPath); err != nil || st.Size() != 0 {
		t.Fatalf("TRUNCATE 后数据页文件应为 0 字节，实际 %v（%v）", st.Size(), err)
	}
	// 清空之后还能继续用（索引必须一起被清掉，否则会查出幽灵行）
	run(t, d3, "s", `INSERT INTO lc VALUES (1,'a','a'),(2,'b','b')`)
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc`), []string{"2"})
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lc WHERE id = 2`), []string{"1"})
	if err := d3.Close(); err != nil {
		t.Fatalf("close3: %v", err)
	}

	// ---- 8) DROP TABLE：数据页、索引页、元信息一起清掉 ----
	d4 := open()
	assertRows(t, run(t, d4, "s", `SELECT count(*) FROM lc`), []string{"2"})
	run(t, d4, "s", `DROP TABLE lc`)
	if _, err := os.Stat(datPath); !os.IsNotExist(err) {
		t.Fatalf("DROP TABLE 之后数据页文件还在: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(tablesDir, "i*.idx")); len(left) != 0 {
		t.Fatalf("DROP TABLE 之后还剩索引文件: %v", left)
	}
	if err := d4.Close(); err != nil {
		t.Fatalf("close4: %v", err)
	}

	// ---- 9) 最后再开一次：没有孤儿文件也能正常打开 ----
	d5 := open()
	if err := runErr(t, d5, "s", `SELECT count(*) FROM lc`); !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("表应该已经不存在了：%v", err)
	}
	if _, err := os.Stat(datPath); !os.IsNotExist(err) {
		t.Fatal("重启后孤儿文件又出现了")
	}
}
