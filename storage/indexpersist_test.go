package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 索引文件往返：建树 → 刷盘 → 重开，内容与结构必须一模一样，
// 而且**不能**被判定成「需要重建」（否则等于没落盘）。
func TestIndexFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := int64(42)
	f, err := openIndexFile(dir, id, "pk", newMemBudget(64), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tr := newBTree(f)
	const n = 3000
	for i := 0; i < n; i++ {
		// 混合重复键与变长键：分裂路径和重复键跨叶都要覆盖
		tr.Insert(fmt.Sprintf("k%05d", i%997), i)
	}
	want := checkBTree(t, tr)
	height, count := tr.Height(), tr.Len()
	if _, err := f.flush(7); err != nil {
		t.Fatalf("flush: %v", err)
	}
	path := f.path
	if err := f.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("索引文件没写出来: %v", err)
	}

	// 代次对得上：直接可用
	f2, err := openIndexFile(dir, id, "pk", newMemBudget(64), 7)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer f2.close()
	if f2.fresh {
		t.Fatal("代次一致却被判定要重建 —— 索引等于没落盘")
	}
	tr2 := newBTree(f2)
	if tr2.Len() != count || tr2.Height() != height {
		t.Fatalf("重开后 Len=%d Height=%d，期望 %d/%d", tr2.Len(), tr2.Height(), count, height)
	}
	got := checkBTree(t, tr2)
	if len(got) != len(want) {
		t.Fatalf("重开后 %d 条，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}
	for _, k := range []string{"k00000", "k00496", "k00996"} {
		if len(tr2.Get(k)) == 0 {
			t.Fatalf("重开后 Get(%s) 为空", k)
		}
	}

	// 代次对不上（堆比索引新）：必须重建
	f3, err := openIndexFile(dir, id, "pk", newMemBudget(64), 8)
	if err != nil {
		t.Fatalf("reopen stale: %v", err)
	}
	defer f3.close()
	if !f3.fresh {
		t.Fatal("代次落后却当成有效索引 —— 会读到和堆对不上的键")
	}
}

// 身份不符：同一个文件被别的索引复用（重建表 / 改名），必须作废重建。
func TestIndexIdentityMismatchRebuilds(t *testing.T) {
	dir := t.TempDir()
	f, err := openIndexFile(dir, 7, "ix:a", newMemBudget(16), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	newBTree(f).Insert("x", 1)
	if _, err := f.flush(3); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := f.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 名字不同 → 哈希不同 → 其实文件名都不同。这里手工把「别的身份」写进同一个文件，
	// 模拟「哈希撞了 / 文件被冒用」。
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b[64] = 'X' // 改身份的第一个字节（头页身份区从 64 开始）
	if err := os.WriteFile(f.path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f2, err := openIndexFile(dir, 7, "ix:a", newMemBudget(16), 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer f2.close()
	if !f2.fresh {
		t.Fatal("身份被改过却没被判成失效")
	}
}

// 页坏掉（CRC 不匹配）：打开时走一遍叶子链就该发现，退化成重建。
func TestCorruptIndexPageMarksFresh(t *testing.T) {
	dir := t.TempDir()
	f, err := openIndexFile(dir, 9, "pk", newMemBudget(64), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tr := newBTree(f)
	for i := 0; i < 2000; i++ {
		tr.Insert(fmt.Sprintf("k%05d", i), i)
	}
	if _, err := f.flush(11); err != nil {
		t.Fatalf("flush: %v", err)
	}
	path := f.path
	if err := f.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 把页 1 之后的叶子页内容弄坏（不改头页：头页代次仍然对得上）
	fh, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	junk := make([]byte, PageSize)
	for i := range junk {
		junk[i] = 0xAB
	}
	if _, err := fh.WriteAt(junk, int64(PageSize)); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	fh.Close()

	f2, err := openIndexFile(dir, 9, "pk", newMemBudget(64), 11)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer f2.close()
	if !f2.fresh {
		t.Fatal("坏页没被发现 —— 会拿着坏索引查出错误结果")
	}
}

// 批量构建：Build 的结果必须和逐条 Insert 完全一致（顺序、条数、树高）。
func TestIndexBuildMatchesInsert(t *testing.T) {
	pairs := make([]idxPair, 0, 4000)
	for i := 0; i < 4000; i++ {
		pairs = append(pairs, idxPair{Key: fmt.Sprintf("k%05d", (i*37)%4000), Slot: i})
	}
	built, err := openIndexFile(t.TempDir(), 1, "pk", newMemBudget(128), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer built.close()
	tb := newBTree(built)
	if err := tb.Build(pairs); err != nil {
		t.Fatalf("build: %v", err)
	}
	oneByOne, err := openIndexFile(t.TempDir(), 2, "pk", newMemBudget(128), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer oneByOne.close()
	ti := newBTree(oneByOne)
	for _, p := range pairs {
		ti.Insert(p.Key, p.Slot)
	}
	if tb.Len() != ti.Len() {
		t.Fatalf("Build 后 %d 条，逐条插入 %d 条", tb.Len(), ti.Len())
	}
	var a, b []string
	tb.Walk(func(k string, s int) { a = append(a, fmt.Sprintf("%s/%d", k, s)) })
	ti.Walk(func(k string, s int) { b = append(b, fmt.Sprintf("%s/%d", k, s)) })
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 条 Build=%q 逐条=%q", i, a[i], b[i])
		}
	}
	if tb.Height() > ti.Height() {
		t.Fatalf("Build 的树比逐条插入还高：%d > %d", tb.Height(), ti.Height())
	}
	// Build 的叶子填充率应该接近满（逐条插入分裂只有 ~50%）
	if tb.Height() == 0 {
		t.Fatal("树高为 0")
	}
	_ = filepath.Join
}
