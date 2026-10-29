package storage

import (
	"fmt"
	"testing"

	"godb/catalog"
	"godb/types"
)

func TestDebugEvict(t *testing.T) {
	dir := t.TempDir()
	cat := catalog.New()
	tbl := &catalog.Table{Name: "ev", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
		catalog.NewColumn("v", types.Type{Kind: types.KText, Name: "text"}),
	}}
	if err := cat.Add(tbl); err != nil { t.Fatal(err) }
	if err := cat.Save(dir); err != nil { t.Fatal(err) }
	s, err := OpenWith(dir, cat, nil, 2)
	if err != nil { t.Fatal(err) }
	defer s.Close()

	td := s.Table("ev")
	if td == nil { t.Fatal("no td") }
	tx := s.Begin()
	for i := 0; i < 500; i++ {
		if _, err := tx.Insert("ev", types.Row{types.Int(int64(i)), types.Text(fmt.Sprintf("pad-%03d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil { t.Fatal(err) }
	if err := s.Checkpoint(); err != nil { t.Fatal(err) }

	t.Logf("poolPages=%d cap=%d pages=%d resident=%d byPage=%d",
		s.PoolPages, td.pages.pool.cap, td.pages.hi, td.ResidentRows(), len(td.pages.byPage))

	n := 0
	for i := 0; i < td.SlotCount(); i++ {
		if _, ok := td.rowAt(i); ok { n++ }
	}
	t.Logf("扫完 %d 行；resident=%d evicts=%d flushes=%d calls=%d skipped=%d",
		n, td.ResidentRows(), td.pages.pool.evicts, td.pages.pool.flushes, td.evictCalls, td.evictSkipped)
}
