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
		if err := tp.put(i, tupleN(i)); err != nil {
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

	if err := tp.put(0, tupleN(1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// 变长覆盖：短 → 长 → 短，每次都要读得回新内容
	long := append(tupleN(2), bytes.Repeat([]byte(" "), 600)...)
	for _, want := range [][]byte{tupleN(1), long, tupleN(3)} {
		if err := tp.put(0, want); err != nil {
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
		if err := tp.put(i, tupleN(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := tp.del(3); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := tp.get(3); ok {
		t.Fatal("删掉的行还能读到")
	}
	// RID 复用：同一个槽位放新行
	if err := tp.put(3, tupleN(99)); err != nil {
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
		if err := tp.put(i, big); err != nil {
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
		if err := tp.put(i, tupleN(i)); err != nil {
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
	if err := tp.put(0, tupleN(7)); err != nil {
		t.Fatalf("reset 后写新行: %v", err)
	}
	if got, ok := tp.get(0); !ok || !bytes.Equal(got, tupleN(7)) {
		t.Fatalf("reset 后 get(0) = %q", got)
	}
}
