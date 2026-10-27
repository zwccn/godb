package storage

import (
	"strings"

	"godb/catalog"
	"godb/types"
)

// TableData 是一张表在内存中的行数据（堆表）。
type TableData struct {
	Name  string
	Rows  []types.Row
	Index map[string]int // 主键编码 → 行号（-1 表示无主键时不维护）
	Seq   int64          // 自增列当前值
}

func newTableData(name string) *TableData {
	return &TableData{Name: name, Index: map[string]int{}}
}

// RebuildIndex 依据主键重建哈希索引。
func (td *TableData) RebuildIndex(t *catalog.Table) {
	pk := t.PKIdxes()
	if len(pk) == 0 {
		td.Index = map[string]int{}
		return
	}
	idx := make(map[string]int, len(td.Rows))
	vals := make([]types.Value, len(pk))
	for i, r := range td.Rows {
		ok := true
		for j, ci := range pk {
			if ci >= len(r) {
				ok = false
				break
			}
			vals[j] = r[ci]
		}
		if !ok {
			continue
		}
		idx[types.Key(vals)] = i
	}
	td.Index = idx
}

// cleanName 统一用小写做内部键。
func cleanName(n string) string { return strings.ToLower(n) }
