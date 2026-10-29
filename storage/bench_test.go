package storage

import (
	"fmt"
	"testing"

	"godb/catalog"
	"godb/types"
)

// benchTable 造一张 n 行的表（4 列，主键 + withSecondary 个二级索引）。
func benchTable(n, withSecondary int) (*TableData, *catalog.Table) {
	t := &catalog.Table{Name: "b", Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
		catalog.NewColumn("code", types.Type{Kind: types.KText, Name: "text"}),
		catalog.NewColumn("qty", types.Type{Kind: types.KInt, Name: "integer"}),
		catalog.NewColumn("note", types.Type{Kind: types.KText, Name: "text"}),
	}}
	t.Columns[0].PrimaryKey = true
	for i := 0; i < withSecondary; i++ {
		t.Indexes = append(t.Indexes, &catalog.Index{
			Name:    fmt.Sprintf("b_ix%d", i),
			Columns: []string{"code"},
		})
	}
	t.Rebuild()

	td := newTableData("b")
	rows := make([]types.Row, n)
	for i := range rows {
		rows[i] = types.Row{
			types.Int(int64(i)),
			types.Text(fmt.Sprintf("code-%d", i)),
			types.Int(int64(i % 97)),
			types.Text("note"),
		}
	}
	td.rows = rows
	return td, t
}

// 每次提交都会整表重建索引 —— 这条基准量它到底值多少。
func BenchmarkRebuildIndexPKOnly(b *testing.B) {
	td, t := benchTable(200000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		td.RebuildIndex(t)
	}
}

func BenchmarkRebuildIndexWithTwoSecondary(b *testing.B) {
	td, t := benchTable(200000, 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		td.RebuildIndex(t)
	}
}

// 整表重组行切片（复制一遍行指针）的代价。
//
// 提交路径已经不用它了（改成原地替换槽位），留在这里当对照：
// 这就是以前每次提交都要付的那 3 ms，也是 checkpoint 拍快照要付的拷贝成本。
func BenchmarkResliceRows(b *testing.B) {
	td, _ := benchTable(200000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newRows := make([]types.Row, 0, len(td.rows))
		for _, row := range td.rows {
			newRows = append(newRows, row)
		}
		if len(newRows) != len(td.rows) {
			b.Fatal("bad length")
		}
	}
}

// 只算主键的 key 编码（RebuildIndex 里最贵的那一步：每行拼一个字符串）。
func BenchmarkKeyEncode200k(b *testing.B) {
	td, t := benchTable(200000, 0)
	pk := t.PKIdxes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vals := make([]types.Value, len(pk))
		n := 0
		for _, row := range td.rows {
			for j, ci := range pk {
				vals[j] = row[ci]
			}
			if types.Key(vals) != "" {
				n++
			}
		}
		if n != len(td.rows) {
			b.Fatal("bad count")
		}
	}
}
