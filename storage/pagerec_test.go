package storage

import (
	"fmt"
	"os"
	"testing"

	"godb/catalog"
	"godb/types"
)

// 页撕裂恢复：8KB 页写盘不是原子的。这里模拟「checkpoint 写完镜像、刷页刷到
// 一半断电」——页文件里有一页是半新半旧的垃圾，重启后必须能靠 WAL 里的
// 全页镜像还原，再重放逻辑记录，一行不少。
func TestTornPageRecoversFromFPI(t *testing.T) {
	dir := t.TempDir()
	cat := catalog.New()
	tbl := &catalog.Table{Name: "tp", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
		catalog.NewColumn("v", types.Type{Kind: types.KText, Name: "text"}),
	}}
	if err := cat.Add(tbl); err != nil {
		t.Fatalf("add table: %v", err)
	}
	if err := cat.Save(dir); err != nil {
		t.Fatalf("save catalog: %v", err)
	}

	s, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	insert := func(from, to int) {
		tx := s.Begin()
		for i := from; i <= to; i++ {
			if _, err := tx.Insert("tp", types.Row{
				types.Int(int64(i)),
				types.Text(fmt.Sprintf("第 %d 行", i)),
			}); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// 第一轮：写 120 行并落盘。此时页文件就位、FPI 标记清空。
	insert(1, 120)
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// 第二轮：再写 80 行。这些只在 WAL 里 + 内存的待落盘表里。
	insert(121, 200)

	// ---- 手工走一遍 checkpoint，但在「页已刷盘、meta 未写」处断电 ----
	td := s.Data["tp"]
	fpis, err := td.pageStage() // 待落盘行写进页（池里，不落盘）
	if err != nil {
		t.Fatalf("pageStage: %v", err)
	}
	if len(fpis) == 0 {
		t.Fatal("应该有全页镜像：这批改动碰到的页都是 checkpoint 之后第一次改")
	}
	gen := s.wal.Gen()
	recs := make([]walRec, 0, len(fpis))
	for _, img := range fpis {
		recs = append(recs, walRec{Gen: gen, Op: opFPI, Table: "tp", Slot: -1, Page: img.page, Img: img.data})
	}
	if _, err := s.wal.Append(recs); err != nil {
		t.Fatalf("append fpi: %v", err)
	}
	if err := s.wal.SyncTo(s.wal.Written()); err != nil {
		t.Fatalf("sync wal: %v", err)
	}
	if _, err := td.pageFlush(); err != nil { // 刷脏页（这里可能只刷了一半）
		t.Fatalf("pageFlush: %v", err)
	}
	// 断电：不写 meta、不轮转 WAL，页文件第 0 页被写坏（后半截还是旧数据）
	s.StopAutoCheckpoint()
	for _, d := range s.Data {
		if d != nil {
			_ = d.pages.pf.close() // 只关文件，不刷盘
		}
	}
	_ = s.wal.Close()

	dataPath := tablePagePath(dir, tbl.ID)
	f, err := os.OpenFile(dataPath, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open page file: %v", err)
	}
	// 只砸坏前半页：页头（含 CRC）和元组区的前一半变成垃圾
	junk := make([]byte, PageSize/2)
	for i := range junk {
		junk[i] = 0xAA
	}
	if _, err := f.WriteAt(junk, 0); err != nil {
		t.Fatalf("corrupt page: %v", err)
	}
	f.Close()

	// 重启：第 0 页靠镜像还原，其余页原样，WAL 重放补齐第二批
	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if err := s2.LoadError(); err != nil {
		t.Fatalf("重启后不该报加载错误（页应该已经被镜像修好）: %v", err)
	}
	if got := s2.RowCount("tp"); got != 200 {
		t.Fatalf("行数 = %d，期望 200", got)
	}
	// 逐行校验内容：镜像修好的是「改之前」的状态，重放再补上新行
	s2.LoadTables([]string{"tp"})
	tx := s2.Begin()
	defer tx.Rollback()
	seek := 0
	err = tx.Scan("tp", func(_ Ref, row types.Row) error {
		seek++
		id := row[0].I
		if want := fmt.Sprintf("第 %d 行", id); row[1].S != want {
			t.Fatalf("id=%d 的文本是 %q，期望 %q", id, row[1].S, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if seek != 200 {
		t.Fatalf("扫到 %d 行，期望 200", seek)
	}
}

// 页文件坏了又没有镜像时，必须明确报错（不能静默当空表）。
func TestCorruptPageWithoutImageReportsError(t *testing.T) {
	dir := t.TempDir()
	cat := catalog.New()
	tbl := &catalog.Table{Name: "bad", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
	}}
	if err := cat.Add(tbl); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := cat.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	s, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tx := s.Begin()
	if _, err := tx.Insert("bad", types.Row{types.Int(1)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 把整页写成垃圾（模拟磁盘坏块，而不是写了一半）
	path := tablePagePath(dir, tbl.ID)
	junk := make([]byte, PageSize)
	for i := range junk {
		junk[i] = 0x5A
	}
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	// 打开时不会读行，所以错误要等到真正访问这张表才暴露
	_ = s2.Table("bad")
	if s2.LoadError() == nil {
		t.Fatal("坏页应该报加载错误，而不是静默返回空表")
	}
}
