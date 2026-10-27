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

	Index map[string]int // 主键编码 → 槽位
	Seq   int64          // 自增列当前值

	// Second 是用户建的二级索引：索引名（小写）→ 键 → 槽位列表。
	Second map[string]map[string][]int

	// UniqueKeys 是非主键 UNIQUE 列组的哈希索引：列组签名（"1,3"）→ 键 → 槽位。
	UniqueKeys map[string]map[string][]int
}

func newTableData(name string) *TableData {
	return &TableData{Name: name, Index: map[string]int{}}
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

// release 释放槽位（必须是当前最后一个或进入 free 列表）。
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

// RebuildIndex 重建所有哈希索引（主键 + 用户二级索引 + UNIQUE 列组）。
//
// 只在「行号整体变了」的时候调用：加载表、重放 WAL、ALTER TABLE、建索引、
// Compact。正常提交走增量维护（IndexInsert / IndexDelete / IndexUpdate）。
func (td *TableData) RebuildIndex(t *catalog.Table) {
	td.Recompute()
	pk := t.PKIdxes()
	idx := make(map[string]int, td.Live)
	buf := make([]types.Value, len(pk))
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		if k, ok := appendKeyInto(buf, r, pk); ok {
			idx[k] = i
		}
	}
	td.Index = idx
	td.RebuildSecond(t)
	td.RebuildUniqueKeys(t)
}

// RebuildSecond 重建全部用户二级索引。
//
// 含 NULL 的行不进索引：和 PostgreSQL 一致（`col = NULL` 永远不成立，
// 键里塞个 NULL 只会让等值查找命中一堆不该命中的行）。
func (td *TableData) RebuildSecond(t *catalog.Table) {
	names, colsOf := secondIndexCols(t)
	if len(names) == 0 {
		td.Second = nil
		return
	}
	next := make(map[string]map[string][]int, len(names))
	for _, n := range names {
		next[n] = make(map[string][]int, td.Live)
	}
	bufs := make([][]types.Value, len(names))
	for j := range names {
		bufs[j] = make([]types.Value, len(colsOf[j]))
	}
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		for j := range names {
			k, ok := appendKeyInto(bufs[j], r, colsOf[j])
			if !ok {
				continue
			}
			m := next[names[j]]
			m[k] = append(m[k], i)
		}
	}
	td.Second = next
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
		next[colsSig(g)] = make(map[string][]int, td.Live)
	}
	bufs := make([][]types.Value, len(groups))
	for j, g := range groups {
		bufs[j] = make([]types.Value, len(g))
	}
	for i, r := range td.Rows {
		if r == nil {
			continue
		}
		for j, g := range groups {
			k, ok := appendKeyInto(bufs[j], r, g)
			if !ok {
				continue
			}
			m := next[colsSig(g)]
			m[k] = append(m[k], i)
		}
	}
	td.UniqueKeys = next
}

// ---------- 索引的增量维护 ----------

// IndexInsert 把一行的所有键加进索引。
func (td *TableData) IndexInsert(t *catalog.Table, slot int, row types.Row) {
	if row == nil {
		return
	}
	if k, ok := rowKey(row, t.PKIdxes()); ok {
		td.Index[k] = slot
	}
	if len(td.Second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j := range names {
			m := td.Second[names[j]]
			if m == nil {
				continue
			}
			if k, ok := rowKey(row, colsOf[j]); ok {
				m[k] = append(m[k], slot)
			}
		}
	}
	for sig, m := range td.UniqueKeys {
		if k, ok := rowKey(row, colsFromSig(t, sig)); ok {
			m[k] = append(m[k], slot)
		}
	}
}

// IndexDelete 把一行的所有键从索引里摘掉。
func (td *TableData) IndexDelete(t *catalog.Table, slot int, row types.Row) {
	if row == nil {
		return
	}
	if k, ok := rowKey(row, t.PKIdxes()); ok {
		// 只删指向自己的那一项：防止「同键已改指向新槽位」时误删别人的
		if cur, exists := td.Index[k]; exists && cur == slot {
			delete(td.Index, k)
		}
	}
	if len(td.Second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j := range names {
			if k, ok := rowKey(row, colsOf[j]); ok {
				dropSlot(td.Second[names[j]], k, slot)
			}
		}
	}
	for sig, m := range td.UniqueKeys {
		if k, ok := rowKey(row, colsFromSig(t, sig)); ok {
			dropSlot(m, k, slot)
		}
	}
}

// IndexUpdate 处理「同一槽位换了内容」：键没变就什么都不做，键变了就换一个键。
func (td *TableData) IndexUpdate(t *catalog.Table, slot int, oldRow, newRow types.Row) {
	if t == nil || oldRow == nil || newRow == nil {
		return
	}
	pk := t.PKIdxes()
	k1, ok1 := rowKey(oldRow, pk)
	k2, ok2 := rowKey(newRow, pk)
	if ok1 && (!ok2 || k1 != k2) {
		if cur, exists := td.Index[k1]; exists && cur == slot {
			delete(td.Index, k1)
		}
	}
	if ok2 {
		td.Index[k2] = slot
	}
	if len(td.Second) > 0 {
		names, colsOf := secondIndexCols(t)
		for j := range names {
			m := td.Second[names[j]]
			if m == nil {
				continue
			}
			swapKey(m, slot, oldRow, newRow, colsOf[j])
		}
	}
	for sig, m := range td.UniqueKeys {
		swapKey(m, slot, oldRow, newRow, colsFromSig(t, sig))
	}
}

// swapKey 在「键 → 槽位列表」里把老键换成新键（键相同就什么都不做）。
func swapKey(m map[string][]int, slot int, oldRow, newRow types.Row, cols []int) {
	k1, ok1 := rowKey(oldRow, cols)
	k2, ok2 := rowKey(newRow, cols)
	same := ok1 && ok2 && k1 == k2
	if same {
		return
	}
	if ok1 {
		dropSlot(m, k1, slot)
	}
	if ok2 {
		m[k2] = append(m[k2], slot)
	}
}

// dropSlot 从「键 → 槽位列表」里摘掉一个槽位。
//
// 用 swap-remove：同一键下的行数通常很少（唯一索引就 1 个），最坏情况
// 是低基数列（所有行同值），那时这里是 O(k) —— 仍远好于整表重建的 O(n)。
func dropSlot(m map[string][]int, key string, slot int) {
	if m == nil {
		return
	}
	list, ok := m[key]
	if !ok {
		return
	}
	for i, s := range list {
		if s == slot {
			list[i] = list[len(list)-1]
			list = list[:len(list)-1]
			if len(list) == 0 {
				delete(m, key)
			} else {
				m[key] = list
			}
			return
		}
	}
}

// appendKeyInto 复用外部 buf 计算键，避免整表重建时每行都分配一个切片。
func appendKeyInto(buf []types.Value, row types.Row, cols []int) (string, bool) {
	if len(cols) == 0 {
		return "", false
	}
	for i, ci := range cols {
		if ci >= len(row) || row[ci].IsNull() {
			return "", false
		}
		if i < len(buf) {
			buf[i] = row[ci]
		} else {
			buf = append(buf, row[ci])
		}
	}
	return types.Key(buf[:len(cols)]), true
}

// rowKey 返回行在给定列组上的编码键；含 NULL 或缺列时 ok=false（不入索引）。
func rowKey(row types.Row, cols []int) (string, bool) {
	if len(cols) == 0 {
		return "", false
	}
	vals := make([]types.Value, len(cols))
	for i, ci := range cols {
		if ci >= len(row) || row[ci].IsNull() {
			return "", false
		}
		vals[i] = row[ci]
	}
	return types.Key(vals), true
}

// colsFromSig 把 UniqueKeys 的签名（"1,3"）还原成列位置。
func colsFromSig(t *catalog.Table, sig string) []int {
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
