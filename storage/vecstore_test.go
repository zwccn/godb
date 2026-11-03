package storage

import (
	"os"
	"path/filepath"
	"testing"

	"godb/catalog"
	"godb/types"
)

// vecStore 起一个只含一张向量表的库：vt(id int, e vector(3))。
func newVecStore(t *testing.T) (string, *catalog.Catalog, *Store) {
	t.Helper()
	dir := t.TempDir()
	cat := &catalog.Catalog{}
	tbl := &catalog.Table{Name: "vt", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
		catalog.NewColumn("e", types.Type{Kind: types.KVector, Name: "vector(3)", Len: 3}),
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
	t.Cleanup(func() { _ = s.Close() })
	return dir, cat, s
}

// vecCol 返回表里向量列的定位信息。
func vecColOf(t *testing.T, cat *catalog.Catalog) VecColumn {
	t.Helper()
	tb, ok := cat.Get("vt")
	if !ok {
		t.Fatal("表 vt 不存在")
	}
	for _, c := range tb.Columns {
		if types.Kind(c.Kind) == types.KVector {
			return VecColumn{TableID: tb.ID, ColumnID: c.ID, Dim: c.Len, AttNum: 2}
		}
	}
	t.Fatal("vt 没有向量列")
	return VecColumn{}
}

// 建表就该有文件；插入后行里存槽位号，文件里能读回原值。
func TestVecColumnInsertAndRead(t *testing.T) {
	dir, cat, s := newVecStore(t)
	col := vecColOf(t, cat)

	path := vecPath(dir, col.TableID, col.ColumnID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("建表后应有向量列文件 %s: %v", path, err)
	}

	tx := s.Begin()
	if _, err := tx.Insert("vt", types.Row{types.Int(1), types.VectorLiteral("[1,2,3]", 3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Insert("vt", types.Row{types.Int(2), types.VectorLiteral("[4,5,6]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 行里必须是槽位引用 —— 不能还是内联文本，否则 WAL 里就存了 3 KB 的向量
	td := s.Table("vt")
	if td.Live != 2 {
		t.Fatalf("行数 %d，期望 2", td.Live)
	}
	var ids []int64
	for i := 0; i < td.SlotCount(); i++ {
		row, ok := td.RowAt(i)
		if !ok {
			continue
		}
		v := row[1]
		if v.IsVecInline() {
			t.Fatalf("第 %d 行的向量还是内联文本，没有物化", i)
		}
		id, ok := v.VecID()
		if !ok {
			t.Fatalf("第 %d 行的向量没有槽位号", i)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("取到 %d 个槽位号，期望 2", len(ids))
	}

	// 读回来要和写进去的一致
	buf := make([]float32, 3)
	got, err := s.VecAt(col, ids[0], buf)
	if err != nil {
		t.Fatalf("VecAt: %v", err)
	}
	want := []float32{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("槽位 %d 读回 %v，期望 %v", ids[0], got, want)
		}
	}
}

// 重启后向量必须还在（列文件不依赖 WAL 重放）。
func TestVecColumnSurvivesRestart(t *testing.T) {
	dir, cat, s := newVecStore(t)
	col := vecColOf(t, cat)

	tx := s.Begin()
	if _, err := tx.Insert("vt", types.Row{types.Int(1), types.VectorLiteral("[0.5,-0.5,0]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer s2.Close()

	td := s2.Table("vt")
	if td.Live != 1 {
		t.Fatalf("重启后行数 %d，期望 1", td.Live)
	}
	row, ok := td.RowAt(0)
	if !ok {
		t.Fatal("重启后第 0 行不见了")
	}
	id, ok := row[1].VecID()
	if !ok {
		t.Fatal("重启后向量没有槽位号")
	}
	buf := make([]float32, 3)
	got, err := s2.VecAt(col, id, buf)
	if err != nil {
		t.Fatalf("VecAt: %v", err)
	}
	want := []float32{0.5, -0.5, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("重启后读回 %v，期望 %v", got, want)
		}
	}
}

// 更新会分配新槽位、回收旧槽位；删除会回收槽位。
func TestVecColumnUpdateRecyclesSlot(t *testing.T) {
	_, cat, s := newVecStore(t)
	col := vecColOf(t, cat)

	tx := s.Begin()
	if _, err := tx.Insert("vt", types.Row{types.Int(1), types.VectorLiteral("[1,0,0]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	td := s.Table("vt")
	row, _ := td.RowAt(0)
	oldID, _ := row[1].VecID()

	// 改向量
	tx = s.Begin()
	if err := tx.Update("vt", Ref{Slot: 0}, types.Row{types.Int(1), types.VectorLiteral("[0,1,0]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	row, _ = td.RowAt(0)
	newID, _ := row[1].VecID()
	if newID == oldID {
		t.Fatalf("更新后槽位号没变（%d），无法验证回收", newID)
	}
	buf := make([]float32, 3)
	got, err := s.VecAt(col, newID, buf)
	if err != nil {
		t.Fatalf("VecAt: %v", err)
	}
	if got[0] != 0 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("更新后读回 %v，期望 [0 1 0]", got)
	}

	// 旧槽位应该已经回到空闲位图：再插一行会优先复用它（空闲数 -1）
	vf, _ := s.vecFile(col)
	freeBefore := vf.FreeSlots()
	if freeBefore != 1 {
		t.Fatalf("更新后空闲槽位数 %d，期望 1（旧槽位应被回收）", freeBefore)
	}
	tx = s.Begin()
	if _, err := tx.Insert("vt", types.Row{types.Int(2), types.VectorLiteral("[9,9,9]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if vf.FreeSlots() != freeBefore-1 {
		t.Fatalf("插入后空闲槽位数 %d，期望 %d（应该复用回收的槽位）", vf.FreeSlots(), freeBefore-1)
	}

	// 删除一行，空闲槽位 +1
	tx = s.Begin()
	if err := tx.Delete("vt", Ref{Slot: 0}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if vf.FreeSlots() != freeBefore {
		t.Fatalf("删除后空闲槽位数 %d，期望 %d", vf.FreeSlots(), freeBefore)
	}
}

// 删列后不再被引用的向量文件要移进回收站，而不是留在原地白占空间。
func TestVecColumnDropColumnMovesFile(t *testing.T) {
	dir, cat, s := newVecStore(t)
	col := vecColOf(t, cat)
	path := vecPath(dir, col.TableID, col.ColumnID)

	tb, _ := cat.Get("vt")
	// 直接从 catalog 里去掉向量列，再让 Store 对账（exec 层删列也是这条路径）
	kept := tb.Columns[:1]
	tb.Columns = kept
	tb.Rebuild()
	if err := s.SyncVectorFiles(); err != nil {
		t.Fatalf("SyncVectorFiles: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("删列后原文件应已移走，stat err = %v", err)
	}
	trashed := filepath.Join(dir, ".trash", "vec", filepath.Base(path))
	if _, err := os.Stat(trashed); err != nil {
		t.Fatalf("回收站里应有 %s: %v", trashed, err)
	}
}

// 崩溃（不刷盘）之后重开：已提交的向量要能读回。
func TestVecColumnCrashRecovery(t *testing.T) {
	dir, cat, s := newVecStore(t)
	col := vecColOf(t, cat)

	tx := s.Begin()
	if _, err := tx.Insert("vt", types.Row{types.Int(7), types.VectorLiteral("[7,8,9]", 3)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 模拟进程被 kill：不 checkpoint、不刷页
	if err := s.CloseWithoutFlush(); err != nil {
		t.Fatalf("close without flush: %v", err)
	}

	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer s2.Close()
	td := s2.Table("vt")
	if td.Live != 1 {
		t.Fatalf("崩溃恢复后行数 %d，期望 1", td.Live)
	}
	row, ok := td.RowAt(0)
	if !ok {
		t.Fatal("崩溃恢复后行不见了")
	}
	id, ok := row[1].VecID()
	if !ok {
		t.Fatalf("崩溃恢复后向量没有槽位号，值 = %q", row[1].TextForm())
	}
	buf := make([]float32, 3)
	got, err := s2.VecAt(col, id, buf)
	if err != nil {
		t.Fatalf("VecAt: %v", err)
	}
	if got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("崩溃恢复后读回 %v，期望 [7 8 9]", got)
	}
}

// ALTER TABLE ADD COLUMN 之后必须立刻 checkpoint。
//
// 这条不是「向量专属」的坑，但它是向量测试先撞出来的：ADD COLUMN 会调
// CompactRows() 把行紧凑重排（槽位号整体前移），而 WAL 里更早的 DELETE/UPDATE
// 记录还带着旧槽位号。不落盘就直接崩，重放会把改动应用到错的行上 ——
// 实测表现是重启后多一行。
func TestAlterCompactRowsNeedsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cat := &catalog.Catalog{}
	tbl := &catalog.Table{Name: "t", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
	}}
	if err := cat.Add(tbl); err != nil {
		t.Fatal(err)
	}
	if err := cat.Save(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx := s.Begin()
	for i := 1; i <= 3; i++ {
		if _, err := tx.Insert("t", types.Row{types.Int(int64(i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 删中间一行，制造空洞
	tx = s.Begin()
	if err := tx.Delete("t", Ref{Slot: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 压实（ADD COLUMN 的第一步）
	td := s.Table("t")
	td.CompactRows()
	// 压紧之后必须 checkpoint，否则 WAL 里的旧槽位号会对不上
	if err := s.CheckpointDDL(); err != nil {
		t.Fatalf("CheckpointDDL: %v", err)
	}
	if err := s.CloseWithoutFlush(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Table("t").Live; got != 2 {
		t.Fatalf("重开后行数 %d，期望 2（压紧后必须落盘）", got)
	}
}
