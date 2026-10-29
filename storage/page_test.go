package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPageInsertGetDelete(t *testing.T) {
	p := newPage(7)
	rows := [][]byte{[]byte(`{"a":1}`), []byte(`{"b":"两"}`), []byte(`{"c":null}`)}
	slots := make([]int, len(rows))
	for i, r := range rows {
		s, err := p.insert(r, uint32(i))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		slots[i] = s
	}
	for i, r := range rows {
		got, ok := p.get(slots[i])
		if !ok || !bytes.Equal(got, r) {
			t.Fatalf("get %d = %q (ok=%v)，期望 %q", i, got, ok, r)
		}
	}
	if p.liveCount() != 3 {
		t.Fatalf("liveCount = %d，期望 3", p.liveCount())
	}
	if err := p.del(slots[1]); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := p.get(slots[1]); ok {
		t.Fatal("删掉的槽位还能读到")
	}
	if p.liveCount() != 2 {
		t.Fatalf("删除后 liveCount = %d，期望 2", p.liveCount())
	}
	// 槽位号要复用：删了哪个，下一个就填哪个，目录不能无限长
	s, err := p.insert([]byte(`{"d":4}`), 99)
	if err != nil {
		t.Fatalf("reuse insert: %v", err)
	}
	if s != slots[1] {
		t.Fatalf("复用槽位号 = %d，期望 %d", s, slots[1])
	}
	if p.liveCount() != 3 {
		t.Fatalf("复用后 liveCount = %d，期望 3", p.liveCount())
	}
}

func TestPageEncodeDecode(t *testing.T) {
	p := newPage(42)
	p.lsn = 123456789
	payload := [][]byte{[]byte(`{"id":1,"name":"张一"}`), []byte(`{"id":2}`)}
	for _, r := range payload {
		if _, err := p.insert(r, uint32(len(p.slots))); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	raw := p.encode()
	if len(raw) != PageSize {
		t.Fatalf("编码后长度 = %d，期望 %d", len(raw), PageSize)
	}
	q, err := decodePage(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if q.id != 42 || q.lsn != 123456789 {
		t.Fatalf("id/lsn 没带过去：%d / %d", q.id, q.lsn)
	}
	for i, want := range payload {
		got, ok := q.get(i)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("第 %d 行 = %q (ok=%v)", i, got, ok)
		}
	}
	if q.freeBytes() != p.freeBytes() {
		t.Fatalf("空闲空间不一致：%d vs %d", q.freeBytes(), p.freeBytes())
	}
}

func TestPageDetectCorruption(t *testing.T) {
	p := newPage(1)
	if _, err := p.insert([]byte(`{"x":1}`), 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	raw := p.encode()
	raw[PageSize-1] ^= 0xFF
	if _, err := decodePage(raw); err != errPageCRC {
		t.Fatalf("改一个字节后 decode 返回 %v，期望 %v", err, errPageCRC)
	}
	// 长度不对也要报错，而不是越界 panic
	if _, err := decodePage(raw[:100]); err != errPageShort {
		t.Fatalf("短页返回 %v，期望 %v", err, errPageShort)
	}
}

func TestPageRejectsOversizeTuple(t *testing.T) {
	p := newPage(1)
	big := make([]byte, MaxTupleSize+1)
	if _, err := p.insert(big, 0); err != errTupleTooLarge {
		t.Fatalf("超长行返回 %v，期望 %v", err, errTupleTooLarge)
	}
}

func TestPageCompactReclaimsHoles(t *testing.T) {
	p := newPage(3)
	payload := bytes.Repeat([]byte("x"), 512)
	slots := make([]int, 0, 8)
	for i := 0; i < 8; i++ {
		s, err := p.insertOrCompact(payload, uint32(i))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		slots = append(slots, s)
	}
	// 删掉一半，空间应该能通过整理拿回来
	for i := 0; i < 4; i++ {
		p.del(slots[i])
	}
	p.compact()
	if p.freeBytes() < 4*len(payload) {
		t.Fatalf("整理后空闲 %d，期望至少 %d", p.freeBytes(), 4*len(payload))
	}
	// 整理不能动槽位号：剩下那四行还在原来的槽位上
	for i := 4; i < 8; i++ {
		got, ok := p.get(slots[i])
		if !ok || !bytes.Equal(got, payload) {
			t.Fatalf("整理后槽位 %d 内容不对 (ok=%v)", slots[i], ok)
		}
	}
}

func TestPageFileAndPool(t *testing.T) {
	dir := t.TempDir()
	pf, err := openPageFile(filepath.Join(dir, "t1.dat"))
	if err != nil {
		t.Fatalf("openPageFile: %v", err)
	}
	defer pf.close()

	pool := newBufPool(pf, 2)
	// 写两页
	for id := uint32(0); id < 2; id++ {
		f, err := pool.fetch(id)
		if err != nil {
			t.Fatalf("fetch %d: %v", id, err)
		}
		p := newPage(id)
		if _, err := p.insert([]byte(`{"id":` + string(rune('0'+id)) + `}`), id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		copy(f.data, p.encode())
		pool.unpin(f, true)
	}
	if _, err := pool.flushAll(); err != nil {
		t.Fatalf("flushAll: %v", err)
	}
	if pf.pageCount() != 2 {
		t.Fatalf("页数 = %d，期望 2", pf.pageCount())
	}

	// 池子只有 2 个框：读第 3 页会把前两页挤掉，脏页要先落盘
	f, err := pool.fetch(5)
	if err != nil {
		t.Fatalf("fetch 5: %v", err)
	}
	pool.unpin(f, false)
	if st := pool.stats(); st.Blocked != 0 {
		t.Fatalf("不该出现「全被钉住」")
	}

	// 重新打开文件，读回第 1 页
	pf2, err := openPageFile(pf.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer pf2.close()
	raw, err := pf2.readPage(1)
	if err != nil {
		t.Fatalf("readPage: %v", err)
	}
	p2, err := decodePage(raw)
	if err != nil {
		t.Fatalf("decode 落盘的页: %v", err)
	}
	if got, ok := p2.get(0); !ok || !bytes.Equal(got, []byte(`{"id":1}`)) {
		t.Fatalf("读回第 1 页第 0 行 = %q (ok=%v)", got, ok)
	}
}

func TestPoolEvictsDirtyPage(t *testing.T) {
	dir := t.TempDir()
	pf, err := openPageFile(filepath.Join(dir, "t.dat"))
	if err != nil {
		t.Fatalf("openPageFile: %v", err)
	}
	defer pf.close()

	pool := newBufPool(pf, 2)
	write := func(id uint32, body string) {
		f, err := pool.fetch(id)
		if err != nil {
			t.Fatalf("fetch %d: %v", id, err)
		}
		p := newPage(id)
		if _, err := p.insert([]byte(body), id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		f.latch.Lock()
		copy(f.data, p.encode())
		f.latch.Unlock()
		pool.unpin(f, true)
	}
	write(0, `{"v":"zero"}`)
	write(1, `{"v":"one"}`)
	write(2, `{"v":"two"}`) // 挤掉第 0 页，脏页必须先写盘

	raw, err := pf.readPage(0)
	if err != nil {
		t.Fatalf("readPage: %v", err)
	}
	p, err := decodePage(raw)
	if err != nil {
		t.Fatalf("第 0 页没被正确刷盘：%v", err)
	}
	if got, _ := p.get(0); !bytes.Equal(got, []byte(`{"v":"zero"}`)) {
		t.Fatalf("第 0 页内容 = %q", got)
	}
	if st := pool.stats(); st.Evicts == 0 || st.Flushes == 0 {
		t.Fatalf("统计不对：%+v", st)
	}
}

func TestPageFileRejectsBadSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.dat")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openPageFile(path); err == nil {
		t.Fatal("长度不是页整数倍的文件应该报错")
	}
}
