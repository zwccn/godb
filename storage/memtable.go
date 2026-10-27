package storage

import (
	"strconv"
	"strings"

	"godb/catalog"
	"godb/types"
)

// TableData 是一张表在内存中的行数据（堆表）。
type TableData struct {
	Name  string
	Rows  []types.Row
	Index map[string]int // 主键编码 → 行号（空表示无主键）
	Seq   int64          // 自增列当前值

	// Second 是用户建的二级索引：索引名（小写）→ 索引键 → 行号列表。
	//
	// 只在提交后重建（和数据一样是「已提交态」），事务内的增删改由
	// Tx.IndexLookup 用 overlay 补齐，所以不需要在写路径上做增量维护。
	// 唯一索引也走这张表：值列表长度恒为 0 或 1。
	Second map[string]map[string][]int

	// UniqueKeys 是非主键 UNIQUE 列组的哈希索引：列组签名（"1,3"）→ 键 → 行号。
	//
	// 不带它的话，每条 INSERT 都要为每个 UNIQUE 列全表扫一遍，批量导入
	// 会退化成 O(n²)。列级 UNIQUE 的语义没变，只是判重从扫描换成查表。
	UniqueKeys map[string]map[string][]int
}

func newTableData(name string) *TableData {
	return &TableData{Name: name, Index: map[string]int{}}
}

// RebuildIndex 重建哈希索引（主键 + 用户二级索引）。
//
// 每次提交后都会调用一次：行切片是重新组装的，行号全变了，
// 增量维护省不下多少，反而容易和写集的应用顺序打架。
func (td *TableData) RebuildIndex(t *catalog.Table) {
	pk := t.PKIdxes()
	if len(pk) == 0 {
		td.Index = map[string]int{}
	} else {
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
	td.RebuildSecond(t)
	td.RebuildUniqueKeys(t)
}

// RebuildUniqueKeys 重建非主键 UNIQUE 列的哈希索引。
func (td *TableData) RebuildUniqueKeys(t *catalog.Table) {
	pk := t.PKIdxes()
	var groups [][]int
	for _, g := range t.UniqueGroups() {
		if sameCols(g, pk) {
			continue // 主键已经走 td.Index 了
		}
		groups = append(groups, g)
	}
	if len(groups) == 0 {
		td.UniqueKeys = nil
		return
	}
	next := make(map[string]map[string][]int, len(groups))
	for _, g := range groups {
		next[colsSig(g)] = make(map[string][]int, len(td.Rows))
	}
	vals := make([]types.Value, 0, 2)
	for ri, row := range td.Rows {
		for _, g := range groups {
			vals = vals[:0]
			ok := true
			for _, ci := range g {
				if ci >= len(row) || row[ci].IsNull() {
					ok = false
					break
				}
				vals = append(vals, row[ci])
			}
			if !ok {
				continue
			}
			m := next[colsSig(g)]
			k := types.Key(vals)
			m[k] = append(m[k], ri)
		}
	}
	td.UniqueKeys = next
}

// RebuildSecond 重建全部用户二级索引。
//
// 含 NULL 的行不进索引：和 PostgreSQL 一致（`col = NULL` 永远不成立，
// 键里塞个 NULL 只会让等值查找命中一堆不该命中的行）。
func (td *TableData) RebuildSecond(t *catalog.Table) {
	if len(t.Indexes) == 0 {
		td.Second = nil
		return
	}
	// 先把每个索引的列号解析好，解析不出列名（列被删了）的索引整条跳过，
	// 这样规划器看不到它、也就不会拿它查出错误结果。
	names := make([]string, 0, len(t.Indexes))
	colsOf := make([][]int, 0, len(t.Indexes))
	for _, ux := range t.Indexes {
		cols := make([]int, 0, len(ux.Columns))
		ok := len(ux.Columns) > 0
		for _, cn := range ux.Columns {
			ci, exists := t.ColumnIdx(cn)
			if !exists {
				ok = false
				break
			}
			cols = append(cols, ci)
		}
		if !ok {
			continue
		}
		names = append(names, strings.ToLower(ux.Name))
		colsOf = append(colsOf, cols)
	}
	if len(names) == 0 {
		td.Second = nil
		return
	}
	next := make(map[string]map[string][]int, len(names))
	for _, n := range names {
		next[n] = make(map[string][]int, len(td.Rows))
	}
	vals := make([]types.Value, 0, 4)
	for ri, row := range td.Rows {
		for i := range names {
			cols := colsOf[i]
			if len(cols) > len(row) {
				continue
			}
			vals = vals[:0]
			ok := true
			for _, ci := range cols {
				if row[ci].IsNull() {
					ok = false
					break
				}
				vals = append(vals, row[ci])
			}
			if !ok {
				continue
			}
			m := next[names[i]]
			k := types.Key(vals)
			m[k] = append(m[k], ri)
		}
	}
	td.Second = next
}

// cleanName 统一用小写做内部键。
func cleanName(n string) string { return strings.ToLower(n) }

// colsSig 把列位置列表渲染成稳定签名，用作 UniqueKeys 的 map 键。
func colsSig(cols []int) string {
	var b strings.Builder
	for i, ci := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(ci))
	}
	return b.String()
}
