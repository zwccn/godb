package storage

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

func tupleN(i int) []byte { return []byte(fmt.Sprintf(`{"id":%d}`, i)) }

func TestTablePagesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t1.dat")
	tp, err := openTablePages(path, 8)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	for i := 0; i < 200; i++ {
		if _, err := tp.put(i, tupleN(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := tp.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if tp.pageCount() == 0 {
		t.Fatal("一页都没写出去")
	}
	if err := tp.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	tp2, err := openTablePages(path, 8)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer tp2.close()
	got := map[int]string{}
	if err := tp2.loadAll(func(slot int, tuple []byte) error {
		got[slot] = string(tuple)
		return nil
	}); err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("读回 %d 行，期望 200", len(got))
	}
	for i := 0; i < 200; i++ {
		if got[i] != string(tupleN(i)) {
			t.Fatalf("第 %d 行 = %s", i, got[i])
		}
	}
	// 重开之后 RID 目录也要能直接用（不必先 loadAll）
	if b, ok := tp2.get(42); !ok || !bytes.Equal(b, tupleN(42)) {
		t.Fatalf("重开后 get(42) = %q, ok=%v", b, ok)
	}
}

func TestTablePagesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t2.dat")
	tp, err := openTablePages(path, 4)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	defer tp.close()

	if _, err := tp.put(0, tupleN(1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// 变长覆盖：短 → 长 → 短，每次都要读得回新内容
	long := append(tupleN(2), bytes.Repeat([]byte(" "), 600)...)
	for _, want := range [][]byte{tupleN(1), long, tupleN(3)} {
		if _, err := tp.put(0, want); err != nil {
			t.Fatalf("put: %v", err)
		}
		got, ok := tp.get(0)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("覆盖后 get(0) = %q（ok=%v），期望 %q", got, ok, want)
		}
	}
	// 覆盖不该无限开新页
	if tp.pageCount() > 3 {
		t.Fatalf("反复覆盖开了 %d 页", tp.pageCount())
	}
}

func TestTablePagesDeleteAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t3.dat")
	tp, err := openTablePages(path, 4)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := tp.put(i, tupleN(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := tp.del(3); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := tp.get(3); ok {
		t.Fatal("删掉的行还能读到")
	}
	// RID 复用：同一个槽位放新行
	if _, err := tp.put(3, tupleN(99)); err != nil {
		t.Fatalf("put 复用: %v", err)
	}
	if _, err := tp.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	tp.close()

	tp2, err := openTablePages(path, 4)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer tp2.close()
	got := map[int]string{}
	if err := tp2.loadAll(func(slot int, tuple []byte) error {
		got[slot] = string(tuple)
		return nil
	}); err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("读回 %d 行，期望 10（删除后复用同一槽位）", len(got))
	}
	if got[3] != string(tupleN(99)) {
		t.Fatalf("复用槽位内容 = %s", got[3])
	}
}

func TestTablePagesGrowAcrossPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t4.dat")
	tp, err := openTablePages(path, 3)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	defer tp.close()

	big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), 700)...)
	big = append(big, []byte(`"}`)...)
	for i := 0; i < 60; i++ {
		if _, err := tp.put(i, big); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if tp.pageCount() < 4 {
		t.Fatalf("60 行 × %d 字节只用了 %d 页，分页逻辑没生效", len(big), tp.pageCount())
	}
	// 池子只有 3 个框，60 行 8 页全靠淘汰撑着；抽查首尾
	for _, i := range []int{0, 59} {
		if got, ok := tp.get(i); !ok || !bytes.Equal(got, big) {
			t.Fatalf("第 %d 行读不回来 (ok=%v)", i, ok)
		}
	}
}

func TestTablePagesReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t5.dat")
	tp, err := openTablePages(path, 4)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	defer tp.close()
	for i := 0; i < 20; i++ {
		if _, err := tp.put(i, tupleN(i)); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := tp.reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if tp.pageCount() != 0 {
		t.Fatalf("reset 后还有 %d 页", tp.pageCount())
	}
	if _, ok := tp.get(0); ok {
		t.Fatal("reset 后还能读到旧行")
	}
	if _, err := tp.put(0, tupleN(7)); err != nil {
		t.Fatalf("reset 后写新行: %v", err)
	}
	if got, ok := tp.get(0); !ok || !bytes.Equal(got, tupleN(7)) {
		t.Fatalf("reset 后 get(0) = %q", got)
	}
}

// 防页撕裂：一页在一次 checkpoint 之后第一次被改时必须带整页镜像，
// 之后再改就不用了；镜像必须是「改之前」的样子（恢复时先用它还原，再重放）。
func TestTablePagesFullPageImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fpi.dat")
	tp, err := openTablePages(path, 4)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	defer tp.close()

	imgs, err := tp.put(0, tupleN(1))
	if err != nil {
		t.Fatalf("put 0: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("首次改页应该带 1 份全页镜像，实际 %d", len(imgs))
	}
	if imgs[0].page != 0 || len(imgs[0].data) != PageSize {
		t.Fatalf("镜像不对：page=%d len=%d", imgs[0].page, len(imgs[0].data))
	}
	// 新页的镜像是零页（还没写过），恢复时按新页处理
	zero := true
	for _, b := range imgs[0].data {
		if b != 0 {
			zero = false
			break
		}
	}
	if !zero {
		t.Fatal("新页的镜像应该是全零")
	}

	// 同一页第二次改：不再带镜像
	imgs, err = tp.put(1, tupleN(2))
	if err != nil {
		t.Fatalf("put 1: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("同一 checkpoint 内第二次改页不该再带镜像，实际 %d", len(imgs))
	}

	// checkpoint 之后标记清零，下次改页重新带镜像 —— 而且镜像里应该有 2 行
	if _, err := tp.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	imgs, err = tp.put(2, tupleN(3))
	if err != nil {
		t.Fatalf("put 2: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("checkpoint 后第一次改页应该带镜像，实际 %d", len(imgs))
	}
	p, err := decodePage(imgs[0].data)
	if err != nil {
		t.Fatalf("镜像不是合法页：%v", err)
	}
	if p.liveCount() != 2 {
		t.Fatalf("镜像里应该有 2 行（改之前的样子），实际 %d", p.liveCount())
	}

	// 删行同样要走镜像保护（新的一轮 checkpoint 之后）
	if _, err := tp.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	imgs, err = tp.del(1)
	if err != nil {
		t.Fatalf("del: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("checkpoint 后第一次删行也要带镜像，实际 %d", len(imgs))
	}
}

// 批量写的关键断言：**解码次数跟「碰到多少页」走，不跟「改了多少行」走**。
// 这条比计时稳（计时在 CI 上抖），但能抓住「又退回按行写」的回归。
func TestTablePagesBatchDecodesPerPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.dat")
	tp, err := openTablePages(path, 64)
	if err != nil {
		t.Fatalf("openTablePages: %v", err)
	}
	defer tp.close()

	const n = 2000
	rows := make([]pendingRow, n)
	for i := 0; i < n; i++ {
		rows[i] = pendingRow{slot: i, tuple: tupleN(i)}
	}
	before := tp.decodes
	if _, err := tp.putBatch(rows); err != nil {
		t.Fatalf("putBatch: %v", err)
	}
	batchDecodes := tp.decodes - before
	pages := int(tp.hi)
	if batchDecodes > int64(pages)+2 {
		t.Fatalf("%d 行批量写解码了 %d 次，应该约等于页数 %d", n, batchDecodes, pages)
	}
	if pages == 0 {
		t.Fatal("一页都没分配")
	}

	// 对照：逐行写同样多的行，解码次数≈行数
	const m = 200
	before = tp.decodes
	for i := n; i < n+m; i++ {
		if _, err := tp.put(i, tupleN(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	perRow := tp.decodes - before
	if perRow < m/2 {
		t.Fatalf("逐行写 %d 行只解码 %d 次，说明 put 没走单行路径？", m, perRow)
	}
	t.Logf("批量 %d 行解码 %d 次 / %d 页；逐行 %d 行解码 %d 次", n, batchDecodes, pages, m, perRow)

	// 内容正确性抽查
	for _, i := range []int{0, n / 2, n - 1, n + m - 1} {
		if b, ok := tp.get(i); !ok || !bytes.Equal(b, tupleN(i)) {
			t.Fatalf("第 %d 行读不回来 (ok=%v)", i, ok)
		}
	}
}
