package storage

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// tupleOf 造一行约 n 字节的元组（页层只认识字节，不看行格式）。
func tupleOf(i, n int) []byte {
	buf := []byte(fmt.Sprintf(`{"id":%d,"v":"`, i))
	for len(buf)+2 < n {
		buf = append(buf, 'x')
	}
	return append(buf, '"', '}')
}

// padRows 造 count 行「每行约 size 字节」的待写入行。
func padRows(count, size int) []pendingRow {
	rows := make([]pendingRow, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, pendingRow{slot: i, tuple: tupleOf(i, size)})
	}
	return rows
}

// 全局预算：三张表共用一个页数上限，谁也不能「各留一份」。
//
// 这是把「每张表 1024 页」改成「全库共享」之后的核心不变量：
// 三张表各写 8 页左右，预算只有 8 页，写完必须还在 8 页以内，
// 而且每张表的数据都要能原样读回来（换出 ≠ 丢数据）。
func TestGlobalBudgetSharedAcrossTables(t *testing.T) {
	dir := t.TempDir()
	b := newMemBudget(8)
	var tps []*tablePages
	for i := 0; i < 3; i++ {
		tp, err := openTablePages(filepath.Join(dir, fmt.Sprintf("b%d.dat", i)), b, defaultPoolPages)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		tps = append(tps, tp)
	}

	// 每张表 300 行 × 200 字节 ≈ 8 页 → 三张表 ≈ 24 页，远超 8 页预算
	for i, tp := range tps {
		if _, err := tp.putBatch(padRows(300, 200)); err != nil {
			t.Fatalf("表 %d 写入: %v", i, err)
		}
		if _, err := tp.checkpoint(); err != nil {
			t.Fatalf("表 %d checkpoint: %v", i, err)
		}
		if used := b.usedPages(); used > 8 {
			t.Fatalf("表 %d 落盘后已用 %d 页，超过全局预算 8 页", i, used)
		}
	}

	sum := 0
	for _, tp := range tps {
		sum += tp.pool.resident()
	}
	if sum > 8 {
		t.Fatalf("三张表驻留 %d 页，超过全局预算 8 页", sum)
	}

	// 换出去的行必须能换回来
	for i, tp := range tps {
		for _, slot := range []int{0, 7, 150, 299} {
			got, ok := tp.get(slot)
			if !ok {
				t.Fatalf("表 %d 第 %d 行读不回来", i, slot)
			}
			if !bytes.Equal(got, tupleOf(slot, 200)) {
				t.Fatalf("表 %d 第 %d 行内容不对：%q", i, slot, got)
			}
		}
	}

	// 关表要把额度还回去，否则反复 DROP/建表会把预算吃干
	for _, tp := range tps {
		if err := tp.close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	if used := b.usedPages(); used != 0 {
		t.Fatalf("全部关掉之后还占着 %d 页额度", used)
	}
}

// 跨表回收：预算被 A 占满时，B 要读一页就得让 A 先吐一页出来。
// 关键不只是「能读到」，而是**额度跟着页框一起转手**，不能两边都算一份。
func TestGlobalBudgetReclaimsFromAnotherTable(t *testing.T) {
	dir := t.TempDir()
	b := newMemBudget(4)

	a, err := openTablePages(filepath.Join(dir, "a.dat"), b, defaultPoolPages)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.close()
	if _, err := a.putBatch(padRows(300, 200)); err != nil {
		t.Fatalf("a 写入: %v", err)
	}
	if _, err := a.checkpoint(); err != nil {
		t.Fatalf("a checkpoint: %v", err)
	}
	if used := b.usedPages(); used != 4 {
		t.Fatalf("a 应该占满 4 页，实际 %d 页", used)
	}

	bb, err := openTablePages(filepath.Join(dir, "b.dat"), b, defaultPoolPages)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer bb.close()
	if _, err := bb.putBatch(padRows(200, 200)); err != nil {
		t.Fatalf("b 写入: %v", err)
	}
	if _, err := bb.checkpoint(); err != nil {
		t.Fatalf("b checkpoint: %v", err)
	}

	if used := b.usedPages(); used > 4 {
		t.Fatalf("两张表一共用了 %d 页，超过预算 4 页", used)
	}
	if got := a.pool.resident() + bb.pool.resident(); got > 4 {
		t.Fatalf("驻留 %d 页，超过预算 4 页", got)
	}
	// a 的页被挤出去了，但数据还在
	for _, slot := range []int{0, 120, 299} {
		got, ok := a.get(slot)
		if !ok || !bytes.Equal(got, tupleOf(slot, 200)) {
			t.Fatalf("a 第 %d 行被挤掉之后读不回来", slot)
		}
	}
	for _, slot := range []int{0, 199} {
		if got, ok := bb.get(slot); !ok || !bytes.Equal(got, tupleOf(slot, 200)) {
			t.Fatalf("b 第 %d 行读不回来", slot)
		}
	}
}

// 顺序预取：顺着 RID 往下扫时，页号基本单调，
// 这时应该一次读进后面几页（而不是一页一个 syscall）。
func TestSequentialScanPrefetches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq.dat")

	// 先建一张 60 多页的表
	writer, err := openTablePages(path, newMemBudget(64), defaultPoolPages)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := writer.putBatch(padRows(2400, 200)); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := writer.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	pages := writer.pageCount()
	if pages < 40 {
		t.Fatalf("只写了 %d 页，测试数据不够", pages)
	}
	if err := writer.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 用一个装不下整表的池子重开（8 页 << 40 页），逼它换入换出
	tp, err := openTablePages(path, newMemBudget(8), defaultPoolPages)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer tp.close()
	slots := 0
	if err := tp.loadAll(func(slot int, tuple []byte) error {
		slots++
		return nil
	}); err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if slots != 2400 {
		t.Fatalf("loadAll 读到 %d 行，期望 2400", slots)
	}

	before := tp.pool.stats()
	for i := 0; i < slots; i++ {
		got, ok := tp.get(i)
		if !ok {
			t.Fatalf("第 %d 行读不到", i)
		}
		if !bytes.Equal(got, tupleOf(i, 200)) {
			t.Fatalf("第 %d 行内容不对", i)
		}
	}
	after := tp.pool.stats()
	if after.Prefetched <= before.Prefetched {
		t.Fatalf("顺序扫了 %d 行，一页都没预取", slots)
	}
	t.Logf("扫描 %d 行 %d 页：预取 %d 次，命中率 %.1f%%",
		slots, pages, after.Prefetched-before.Prefetched, tp.pool.hitRate()*100)
}

// 乱序访存不该触发预取：点查（按主键命中某一行）本来是跳着读的，
// 提前把「下一页」读进来纯属浪费预算。
func TestRandomAccessDoesNotPrefetch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rnd.dat")
	tp, err := openTablePages(path, newMemBudget(16), defaultPoolPages)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tp.close()
	if _, err := tp.putBatch(padRows(2400, 200)); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if err := tp.loadAll(func(int, []byte) error { return nil }); err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	base := tp.pool.stats().Prefetched

	// 跳着读：0 / 末行 / 中间若干，页号差得远，构造不出「上一页 + 1」
	for _, slot := range []int{0, 2399, 1100, 400, 1800} {
		if _, ok := tp.get(slot); !ok {
			t.Fatalf("第 %d 行读不到", slot)
		}
	}
	if got := tp.pool.stats().Prefetched - base; got != 0 {
		t.Fatalf("乱序访问也预取了 %d 次", got)
	}
}
