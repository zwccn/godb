package storage

// TableSizes 是监控（/api/space、/api/monitor）的数据源。它有两件容易写错的事：
//
//  1. 它同时想读两类状态 —— map 类的（s.Data / loaded / tableRows / tableFree）
//     由 loadMu 保护，表内的（td.Live / td.pages）由 CommitMu 保护。少拿一把锁
//     就是数据竞争：Go 对「map 并发读写」会直接致命 panic，对字段只是脏读。
//  2. 它还要 os.Stat 每个文件 —— 文件 IO 不能放进任何锁里。
//
// 所以实现是「loadMu 抄指针 → CommitMu 读字段 → 无锁 stat 文件」三段不嵌套。
// 这里既验数值，也验「边提交边轮询不炸」。

import (
	"fmt"
	"testing"

	"godb/catalog"
	"godb/types"
)

func newSizesStore(t *testing.T, names ...string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	cat := catalog.New()
	for _, n := range names {
		tbl := &catalog.Table{Name: n, Columns: []*catalog.Column{
			catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
		}}
		if err := cat.Add(tbl); err != nil {
			t.Fatalf("add %s: %v", n, err)
		}
	}
	if err := cat.Save(dir); err != nil {
		t.Fatalf("save catalog: %v", err)
	}
	s, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, dir
}

func insertRows(t *testing.T, s *Store, table string, from, to int) {
	t.Helper()
	tx := s.Begin()
	for i := from; i <= to; i++ {
		if _, err := tx.Insert(table, types.Row{types.Int(int64(i))}); err != nil {
			t.Fatalf("insert %s %d: %v", table, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func findSize(t *testing.T, list []TableSize, name string) TableSize {
	t.Helper()
	for _, ts := range list {
		if ts.Name == name {
			return ts
		}
	}
	t.Fatalf("TableSizes 里没有 %s：%+v", name, list)
	return TableSize{}
}

// 加载态：行数与页数走内存；落过盘的表必须有字节数。
func TestTableSizesLoaded(t *testing.T) {
	s, _ := newSizesStore(t, "ta", "tb")
	defer s.Close()

	insertRows(t, s, "ta", 1, 5)
	insertRows(t, s, "tb", 1, 3)
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	list := s.TableSizes()
	ta := findSize(t, list, "ta")
	tb := findSize(t, list, "tb")
	if ta.Rows != 5 || tb.Rows != 3 {
		t.Fatalf("行数 = ta %d / tb %d，期望 5 / 3", ta.Rows, tb.Rows)
	}
	for _, ts := range []TableSize{ta, tb} {
		if ts.Bytes <= 0 || ts.Pages <= 0 {
			t.Fatalf("%s 落盘后应有字节与页数，实际 %d 字节 / %d 页", ts.Name, ts.Bytes, ts.Pages)
		}
		if !ts.FillKnown || ts.Fill < 0 || ts.Fill > 1 {
			t.Fatalf("%s 密度应在 [0,1] 且已知，实际 known=%v fill=%v", ts.Name, ts.FillKnown, ts.Fill)
		}
		// checkpoint 不等于「整理」：VacuumAt 只有 VACUUM / VACUUM FULL 才会写。
		// 把这两件事混起来，面板上就会出现「刚落过盘，却显示从未整理」的假象。
		if ts.VacuumAt != 0 {
			t.Fatalf("%s 没做过 VACUUM，不该有整理时间（实际 %d）", ts.Name, ts.VacuumAt)
		}
	}
	// 列表按字节数从大到小（前端直接按顺序画，不再排）
	if len(list) != 2 || list[0].Bytes < list[1].Bytes {
		t.Fatalf("应按字节数降序：%+v", list)
	}

	// 真整理一次：只有被动过的那张表有整理时间
	if _, err := s.Vacuum("ta"); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	after := s.TableSizes()
	if ts := findSize(t, after, "ta"); ts.VacuumAt == 0 {
		t.Fatalf("VACUUM 之后 ta 应记下整理时间")
	}
	if ts := findSize(t, after, "tb"); ts.VacuumAt != 0 {
		t.Fatalf("没整理过的 tb 不该有整理时间（实际 %d）", ts.VacuumAt)
	}
}

// 未加载态：行数只能来自 meta。这条是「打开库但没查过那张表」的路径 ——
// 如果 meta 没记，监控面板会把一张有数据的表显示成 0 行。
func TestTableSizesUnloaded(t *testing.T) {
	s, dir := newSizesStore(t, "tc")
	insertRows(t, s, "tc", 1, 7)
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重开：一行都没加载
	cat, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	ts := findSize(t, s2.TableSizes(), "tc")
	if ts.Rows != 7 {
		t.Fatalf("未加载的表行数 = %d，期望 7（应来自 meta，不是 0）", ts.Rows)
	}
	if ts.Bytes <= 0 || ts.Pages <= 0 {
		t.Fatalf("未加载的表也要按文件大小报字节/页数，实际 %d / %d", ts.Bytes, ts.Pages)
	}
}

// 一边提交、一边轮询：不能 panic，也不能出现负数行数/越界密度。
//
// 这不能替代 `go test -race`（本机没有 gcc），但能抓住最致命的一类：
// map 并发读写会直接致命 panic（进程级），字段读到正在被改的空闲空间图
// 也可能越界 —— 那正是「监控页偶发 500 且没有线索」的来源。
func TestTableSizesWhileCommitting(t *testing.T) {
	s, _ := newSizesStore(t, "td")
	defer s.Close()
	insertRows(t, s, "td", 1, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 2; i <= 120; i++ {
			tx := s.Begin()
			if _, err := tx.Insert("td", types.Row{types.Int(int64(i))}); err != nil {
				return
			}
			if err := tx.Commit(); err != nil {
				return
			}
		}
	}()

	polls := 0
	for {
		select {
		case <-done:
			// 最后再读一次：此时 120 行都应可见
			ts := findSize(t, s.TableSizes(), "td")
			if ts.Rows != 120 {
				t.Fatalf("并发写入结束后行数 = %d，期望 120", ts.Rows)
			}
			return
		default:
		}
		for _, ts := range s.TableSizes() {
			if ts.Rows < 0 {
				t.Fatalf("%s 读到负数行数 %d（撕裂值）", ts.Name, ts.Rows)
			}
			if ts.FillKnown && (ts.Fill < 0 || ts.Fill > 1) {
				t.Fatalf("%s 密度越界 %v", ts.Name, ts.Fill)
			}
			if ts.Pages < 0 {
				t.Fatalf("%s 页数为负 %d", ts.Name, ts.Pages)
			}
		}
		polls++
		if polls > 100000 {
			t.Fatal(fmt.Sprintf("轮询 %d 次仍未等到写入结束", polls))
		}
	}
}
