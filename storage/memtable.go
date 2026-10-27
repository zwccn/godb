package storage

import (
	"strconv"
	"strings"

	"godb/catalog"
	"godb/types"
)

// TableData 是一张表在内存中的行数据（堆表）。
//
// 行占据固定槽位：删除只把槽位置空并记进 free，不会挪动后面的行。
// 这样索引里存的「槽位号」在一次提交之后依然有效，索引就能增量维护
// （改一行只需删一个键、插一个键），不必每次提交都整表重建。
type TableData struct {
	Name string
	// Rows 是槽位数组，空槽为 nil。Live 是非空行数。
	Rows []types.Row
	Live int
	// free 是可复用的空槽（LIFO）。只在提交时压入/弹出，事务内删除的行
	// 仍然占着槽位（它只是被标记成删了），所以事务内插入不会复用自己刚删的槽。
	free []int

	// pk 是主键索引：保序键 → 槽位。用 B+Tree 而不是哈希表，这样主键也能走
	// 范围扫描（`id BETWEEN`、`ORDER BY id`、`min/max`），而且键天然有序。
	pk  *btree
	Seq int64 // 自增列当前值

	// second 是用户建的二级索引：索引名（小写）→ B+Tree。
	second map[string]*btree

	// unique 是非主键 UNIQUE 列组的索引：列组签名（"1,3"）→ B+Tree。
	unique map[string]*btree
}

func newTableData(name string) *TableData {
	return &TableData{Name: name, pk: newBTree()}
}

// alloc 分配一个槽位（优先复用空槽）。
func (td *TableData) alloc() int {
	if n := len(td.free); n > 0 {
		slot := td.free[n-1]
		td.free = td.free[:n-1]
		return slot
	}
	td.Rows = append(td.Rows, nil)
	return len(td.Rows) - 1
}

// release 释放槽位（末尾且无其它空槽时直接截断数组）。
func (td *TableData) release(slot int) {
	if slot == len(td.Rows)-1 && len(td.free) == 0 {
		td.Rows = td.Rows[:slot]
		return
	}
	td.free = append(td.free, slot)
}

// Recompute 重算 Live 与 free（加载、重放、整表重建之后调用）。
func (td *TableData) Recompute() {
	td.Live = 0
	td.free = td.free[:0]
	for i, r := range td.Rows {
		if r == nil {
			td.free = append(td.free, i)
			continue
		}
		td.Live++
	}
}

// Compact 压缩空槽并重建索引。行号整体变了，所以索引必须重建。
func (td *TableData) Compact(t *catalog.Table) {
	if len(td.free) == 0 {
		return
	}
	td.CompactRows()
	td.RebuildIndex(t)
}

// CompactRows 只压缩空槽，不动索引 —— 调用方随后必须 RebuildIndex。
//
// ALTER TABLE 会按下标重写每一行，空槽必须先压实，否则会被当成
// 「全默认值的一行」写回去，行数凭空多出来。
func (td *TableData) CompactRows() {
	if len(td.free) == 0 {
		return
	}
	rows := make([]types.Row, 0, td.Live)
	for _, r := range td.Rows {
		if r != nil {
			rows = append(rows, r)
		}
	}
	td.Rows = rows
	td.free = nil
	td.Live = len(rows)
}

// RebuildIndex 重建所有索引（主键 + 用户二级索引 + UNIQUE 列组）。
//
// 只在「行号整体变了」的时候调用：加载表、重放 WAL、ALTER TABLE、建索引、
// Compact。正常提交走增量维护（IndexInsert / IndexDelete / IndexUpdate）。
func (td *TableData) RebuildIndex(t *catalog.Table) {
	td.Recompute()
	pk := t.PKIdxes()
	tree := newBTree()
	buf := make([]byte, 0, 32)
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		var ok bool
		buf, ok = AppendOrderKeyInto(buf[:0], r, pk)
		if !ok {
			continue
		}
		tree.Insert(string(buf), i)
	}
	td.pk = tree
	td.RebuildSecond(t)
	td.RebuildUniqueKeys(t)
}

// secondIndexCols 解析出每个用户索引的名字与列位置；解析不出列名的索引整条跳过
// （列被删了），这样规划器看不到它，也就不会拿它查出错误结果。
func secondIndexCols(t *catalog.Table) ([]string, [][]int) {
	if len(t.Indexes) == 0 {
		return nil, nil
	}
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
	return names, colsOf
}

// RebuildSecond 重建全部用户二级索引。
//
// 含 NULL 的行不进索引：和 PostgreSQL 一致（`col = NULL` 永远不成立，
// 键里塞个 NULL 只会让等值查找命中一堆不该命中的行）。
func (td *TableData) RebuildSecond(t *catalog.Table) {
	names, colsOf := secondIndexCols(t)
	if len(names) == 0 {
		td.second = nil
		return
	}
	next := make(map[string]*btree, len(names))
	for _, n := range names {
		next[n] = newBTree()
	}
	buf := make([]byte, 0, 32)
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		for j, name := range names {
			var ok bool
			buf, ok = AppendOrderKeyInto(buf[:0], r, colsOf[j])
			if !ok {
				continue
			}
			next[name].Insert(string(buf), i)
		}
	}
	td.second = next
}

// RebuildUniqueKeys 重建非主键 UNIQUE 列的索引。
func (td *TableData) RebuildUniqueKeys(t *catalog.Table) {
	pk := t.PKIdxes()
	var groups [][]int
	for _, g := range t.UniqueGroups() {
		if sameCols(g, pk) {
			continue // 主键已经走 td.pk 了
		}
		groups = append(groups, g)
	}
	if len(groups) == 0 {
		td.unique = nil
		return
	}
	next := make(map[string]*btree, len(groups))
	for _, g := range groups {
		next[colsSig(g)] = newBTree()
	}
	buf := make([]byte, 0, 32)
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		for _, g := range groups {
			var ok bool
			buf, ok = AppendOrderKeyInto(buf[:0], r, g)
			if !ok {
				continue
			}
			next[colsSig(g)].Insert(string(buf), i)
		}
	}
	td.unique = next
}

// ---------- 索引的增量维护 ----------

// IndexInsert 把一行的所有键加进索引。
func (td *TableData) IndexInsert(t *catalog.Table, slot int, row types.Row) {
	if row == nil || t == nil {
		return
	}
	if k, ok := OrderKeyOfRow(row, t.PKIdxes()); ok {
		td.pk.Insert(k, slot)
	}
	if len(td.second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j, name := range names {
			tree := td.second[name]
			if tree == nil {
				continue
			}
			if k, ok := OrderKeyOfRow(row, colsOf[j]); ok {
				tree.Insert(k, slot)
			}
		}
	}
	for sig, tree := range td.unique {
		if k, ok := OrderKeyOfRow(row, colsFromSig(sig)); ok {
			tree.Insert(k, slot)
		}
	}
}

// IndexDelete 把一行的所有键从索引里摘掉。
func (td *TableData) IndexDelete(t *catalog.Table, slot int, row types.Row) {
	if row == nil || t == nil {
		return
	}
	if k, ok := OrderKeyOfRow(row, t.PKIdxes()); ok {
		td.pk.Delete(k, slot)
	}
	if len(td.second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j, name := range names {
			tree := td.second[name]
			if tree == nil {
				continue
			}
			if k, ok := OrderKeyOfRow(row, colsOf[j]); ok {
				tree.Delete(k, slot)
			}
		}
	}
	for sig, tree := range td.unique {
		if k, ok := OrderKeyOfRow(row, colsFromSig(sig)); ok {
			tree.Delete(k, slot)
		}
	}
}

// IndexUpdate 处理「同一槽位换了内容」：键没变就什么都不做，键变了就换一个键。
func (td *TableData) IndexUpdate(t *catalog.Table, slot int, oldRow, newRow types.Row) {
	if t == nil || oldRow == nil || newRow == nil {
		return
	}
	swapKey(td.pk, slot, oldRow, newRow, t.PKIdxes())
	if len(td.second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j, name := range names {
			tree := td.second[name]
			if tree == nil {
				continue
			}
			swapKey(tree, slot, oldRow, newRow, colsOf[j])
		}
	}
	for sig, tree := range td.unique {
		swapKey(tree, slot, oldRow, newRow, colsFromSig(sig))
	}
}

// swapKey 在树上把老键换成新键（键相同就什么都不做）。
func swapKey(tree *btree, slot int, oldRow, newRow types.Row, cols []int) {
	k1, ok1 := OrderKeyOfRow(oldRow, cols)
	k2, ok2 := OrderKeyOfRow(newRow, cols)
	if ok1 && ok2 && k1 == k2 {
		return
	}
	if ok1 {
		tree.Delete(k1, slot)
	}
	if ok2 {
		tree.Insert(k2, slot)
	}
}

// PKTree 返回主键索引树。
func (td *TableData) PKTree() *btree { return td.pk }

// IndexTree 返回某条用户索引的树（没有则 nil）。
func (td *TableData) IndexTree(name string) *btree {
	if td.second == nil {
		return nil
	}
	return td.second[cleanName(name)]
}

// UniqueTree 返回某个 UNIQUE 列组的树（没有则 nil）。
func (td *TableData) UniqueTree(sig string) *btree {
	if td.unique == nil {
		return nil
	}
	return td.unique[sig]
}

// IndexFragmented 判断索引是否因为大量删除而碎片化（延迟合并的兜底）。
func (td *TableData) IndexFragmented() bool {
	if td.pk != nil && td.pk.NeedRebuild() {
		return true
	}
	for _, tree := range td.second {
		if tree.NeedRebuild() {
			return true
		}
	}
	for _, tree := range td.unique {
		if tree.NeedRebuild() {
			return true
		}
	}
	return false
}

// ColsSig 把列位置列表渲染成签名（UNIQUE 列组的 map 键）。
func ColsSig(cols []int) string { return colsSig(cols) }

// colsFromSig 把 UNIQUE 列组的签名（"1,3"）还原成列位置。
func colsFromSig(sig string) []int {
	parts := strings.Split(sig, ",")
	cols := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		cols = append(cols, n)
	}
	return cols
}

// cleanName 统一用小写做内部键。
func cleanName(n string) string { return strings.ToLower(n) }

// colsSig 把列位置列表渲染成稳定签名，用作 UNIQUE 索引的 map 键。
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
