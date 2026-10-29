package storage

import (
	"fmt"
	"path/filepath"
	"testing"
)

// 空闲空间图：分档、best-fit、整页空兜底、摘除。
func TestFreeMapBasics(t *testing.T) {
	m := newFreeMap()
	// 三页：几乎满、中等、整页空
	m.update(0, 100, 40)
	m.update(1, 4000, 20)
	m.update(2, 0, 0) // 整页空（live=0）
	if pages, _ := m.stats(); pages != 3 {
		t.Fatalf("图里应有 3 页，实际 %d", pages)
	}

	// 要 64 字节：100 字节的 0 号页已经够（best-fit 不往上找）
	if p, ok := m.find(64); !ok || p != 0 {
		t.Fatalf("find(64) = %d,%v，期望 0", p, ok)
	}
	// 要 200 字节：0 号页放不下，应该挑 1 号（4000）而不是 2 号（空页）
	if p, ok := m.find(200); !ok || p != 1 {
		t.Fatalf("find(200) = %d,%v，期望 1（best-fit 不该直接跳到空页）", p, ok)
	}
	// 要 8000 字节：只有整页空的 2 号页放得下
	if p, ok := m.find(8000); !ok || p != 2 {
		t.Fatalf("find(8000) = %d,%v，期望 2", p, ok)
	}
	// 比一页还大：谁也放不下（调用方会开新页）
	if _, ok := m.find(PageSize * 2); ok {
		t.Fatal("超过一页的请求不该命中")
	}

	// 0 号页被打扫干净（整页空）之后升到最高档；best-fit 仍然先挑 1 号，
	// 因为 4000 字节也够放 100 字节 —— 空页留给「谁也放不下」的时候用
	m.update(0, 8000, 0)
	if p, ok := m.find(100); !ok || p != 1 {
		t.Fatalf("find(100) = %d,%v，期望 1（best-fit 不该动整页空的那页）", p, ok)
	}
	if p, ok := m.find(6000); !ok || p != 0 {
		t.Fatalf("find(6000) = %d,%v，期望 0（只有整页空的页放得下）", p, ok)
	}
	// 摘掉 2 号页
	m.remove(2)
	if pages, _ := m.stats(); pages != 2 {
		t.Fatalf("remove 之后图里应剩 2 页，实际 %d", pages)
	}
	// 同一页反复 update 不会把它在图里复制成多份
	base, _ := m.stats()
	for i := 0; i < 10; i++ {
		m.update(1, 100*i, 5)
	}
	if pages, _ := m.stats(); pages != base {
		t.Fatalf("反复 update 之后页数变了：%d → %d", base, pages)
	}
	m.reset()
	if pages, _ := m.stats(); pages != 0 {
		t.Fatalf("reset 之后应为空图，实际 %d 页", pages)
	}
}

// 中间页被删空之后，新插入的行要**填回那一页**，而不是继续往后开新页。
//
// 这是 FSM 存在的理由：老实现只往回看最后 8 页，中间的空洞永远用不上。
func TestMiddlePageHolesAreReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.dat")
	tp, err := openTablePages(path, nil, 32)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tp.close()

	// 每行 400 字节 ≈ 每页 19 行，240 行 → 十几页
	if _, err := tp.putBatch(padRows(240, 400)); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := tp.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	pages := tp.pageCount()
	if pages < 10 {
		t.Fatalf("只有 %d 页，测试数据不够", pages)
	}

	// 掏空正中间那一页
	mid := uint32(pages / 2)
	victims := append([]int(nil), tp.byPage[mid]...)
	if len(victims) < 5 {
		t.Fatalf("中间页只有 %d 行", len(victims))
	}
	if _, err := tp.delBatch(victims); err != nil {
		t.Fatalf("删除: %v", err)
	}

	// 插入同样多的行：应该填回中间页
	before := tp.pageCount()
	slots := make([]pendingRow, 0, len(victims))
	for i := range victims {
		slot := 1000 + i // 全新槽位，不复用被删的那批
		slots = append(slots, pendingRow{slot: slot, tuple: tupleOf(slot, 400)})
	}
	if _, err := tp.putBatch(slots); err != nil {
		t.Fatalf("再插入: %v", err)
	}
	if after := tp.pageCount(); after > before {
		t.Fatalf("插入开了新页：%d → %d 页（空闲图没起作用）", before, after)
	}
	landed := 0
	for _, r := range slots {
		if tp.loc[r.slot].page == mid {
			landed++
		}
	}
	if landed < len(slots)/2 {
		t.Fatalf("只有 %d/%d 行落回了中间页 %d", landed, len(slots), mid)
	}

	// 重启（重新加载页 → 重建图）之后同样的操作也要成立
	if _, err := tp.checkpoint(); err != nil {
		t.Fatalf("checkpoint2: %v", err)
	}
	if err := tp.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	tp2, err := openTablePages(path, nil, 32)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer tp2.close()
	if err := tp2.loadAll(func(int, []byte) error { return nil }); err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	pages2 := tp2.pageCount()
	mid2 := uint32(pages2 / 3)
	victims2 := append([]int(nil), tp2.byPage[mid2]...)
	if len(victims2) == 0 {
		t.Fatalf("第 %d 页没有行", mid2)
	}
	if _, err := tp2.delBatch(victims2); err != nil {
		t.Fatalf("删除2: %v", err)
	}
	rows := make([]pendingRow, 0, len(victims2))
	for i := range victims2 {
		slot := 2000 + i
		rows = append(rows, pendingRow{slot: slot, tuple: tupleOf(slot, 400)})
	}
	before2 := tp2.pageCount()
	if _, err := tp2.putBatch(rows); err != nil {
		t.Fatalf("再插入2: %v", err)
	}
	if after := tp2.pageCount(); after > before2 {
		t.Fatalf("重启后插入又开新页了：%d → %d", before2, after)
	}
	landed2 := 0
	for _, r := range rows {
		if tp2.loc[r.slot].page == mid2 {
			landed2++
		}
	}
	if landed2 == 0 {
		t.Fatalf("重启后新行没落回第 %d 页", mid2)
	}
	// 数据都要能读回来
	for _, r := range rows {
		if got, ok := tp2.get(r.slot); !ok || string(got) != string(r.tuple) {
			t.Fatalf("第 %d 行读不回来", r.slot)
		}
	}
}

// 文件尾部整页空的页被 VACUUM 摘掉之后，不能还留在空闲图里（否则插入会往
// 一个已经越界的页号上写）。
func TestFreeMapDropsReclaimedPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reclaim.dat")
	tp, err := openTablePages(path, nil, 16)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tp.close()
	if _, err := tp.putBatch(padRows(120, 400)); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := tp.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	before := tp.pageCount()
	// 删掉最后一页上的行，再回收
	last := before - 1
	victims := append([]int(nil), tp.byPage[last]...)
	if _, err := tp.delBatch(victims); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, err := tp.reclaim(); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if after := tp.pageCount(); after >= before {
		t.Fatalf("尾部空页没摘掉：%d → %d", before, after)
	}
	if pages, _ := tp.fm.stats(); pages != int(tp.pageCount()) {
		t.Fatalf("空闲图里有 %d 页，文件只有 %d 页（可能有已越界的页号）",
			pages, tp.pageCount())
	}
	// 再插一批：不能写到越界页
	rows := make([]pendingRow, 0, 40)
	for i := 0; i < 40; i++ {
		slot := 500 + i
		rows = append(rows, pendingRow{slot: slot, tuple: tupleOf(slot, 400)})
	}
	if _, err := tp.putBatch(rows); err != nil {
		t.Fatalf("再插入: %v", err)
	}
	for _, r := range rows {
		if got, ok := tp.get(r.slot); !ok || string(got) != string(r.tuple) {
			t.Fatalf("第 %d 行读不回来（可能写到了越界页）", r.slot)
		}
	}
	fmt.Sprint(tp.pageCount())
}
