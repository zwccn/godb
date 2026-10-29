package storage

import (
	"sort"
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

	// pages 是页式堆文件。Rows 仍然是内存里的权威副本；提交只记「哪些行变了」，
	// checkpoint 时再成批写进页 —— 提交路径因此和以前一样快（不用编解码整页），
	// 而写盘仍然是「只写脏页」。
	// nil 表示这张表还没挂上（新建但没写过、或从没落地）。
	pages *tablePages

	// pendingSlots/pendingRows 是「自上次 checkpoint 以来变化过的行」，
	// 二者对齐；pendingRows[i] == nil 表示这一行被删了。
	// checkpoint 时按槽位去重（同一行改多次只留最后一次）再写页。
	pendingSlots []int
	pendingRows  []types.Row
	// dir/id 记下来，是为了「第一次写的时候才开文件」——
	// 只读表不产生 .dat，少一堆空文件。
	dir string
	id  int64
}

func newTableData(name string) *TableData {
	return &TableData{Name: name, pk: newBTree()}
}

// attachPages 给表挂上页文件（幂等）。id <= 0 表示这张表还没拿到稳定 ID。
func (td *TableData) attachPages(dir string, id int64) error {
	if id <= 0 {
		return nil
	}
	if td.pages != nil || td.dir == dir && td.id == id {
		return nil
	}
	tp, err := openTablePages(tablePagePath(dir, id), defaultTablePoolPages)
	if err != nil {
		return err
	}
	td.pages = tp
	td.dir, td.id = dir, id
	return nil
}

// notePending 记下这次提交改了哪些行，等 checkpoint 成批写页。
//
// 为什么不在这里直接写页：写页要 decode 整页 + encode 整页（含 8KB CRC），
// 每行一次会让提交慢一个量级（实测 5 万行插入 21s → 279s），而且会把组提交
// 打散（提交变慢 → 并发提交挤不到同一个 fsync 窗口里）。
func (td *TableData) notePending(slots []int, rows []types.Row) {
	if td.pages == nil || len(slots) == 0 {
		return
	}
	td.pendingSlots = append(td.pendingSlots, slots...)
	td.pendingRows = append(td.pendingRows, rows...)
}

// dropPending 丢掉待落盘行（全表清空时用：行都没了，再写回去就是复活）。
func (td *TableData) dropPending() {
	td.pendingSlots = nil
	td.pendingRows = nil
}

// drainPending 把待落盘行按槽位合并（最后一次为准），编码成页里存的元组。
func (td *TableData) drainPending() ([]int, []pendingRow, error) {
	if len(td.pendingSlots) == 0 {
		return nil, nil, nil
	}
	final := make(map[int]types.Row, len(td.pendingSlots))
	for i, slot := range td.pendingSlots {
		final[slot] = td.pendingRows[i]
	}
	dels := make([]int, 0, len(final))
	puts := make([]pendingRow, 0, len(final))
	for slot, row := range final {
		if row == nil {
			dels = append(dels, slot)
			continue
		}
		tuple, err := encodeRowTuple(row)
		if err != nil {
			return nil, nil, err
		}
		puts = append(puts, pendingRow{slot: slot, tuple: tuple})
	}
	// 排序只为让「同一批改动」产生同样的页布局（可复现、便于对比文件）
	sort.Ints(dels)
	sort.Slice(puts, func(i, j int) bool { return puts[i].slot < puts[j].slot })
	td.pendingSlots = nil
	td.pendingRows = nil
	return dels, puts, nil
}

// pageLoad 从页文件读回全表（槽位号 == RID，不能重排）。
func (td *TableData) pageLoad(t *catalog.Table) ([]types.Row, int, error) {
	if td.pages == nil {
		return nil, 0, nil
	}
	return td.pages.loadRows(t)
}

// pageStage 把待落盘行写进页（只改池里的页，不落盘），返回全页镜像。
//
// 这一步和「刷脏页」必须分成两步：镜像要先写进 WAL，页才能落盘
// （否则断电把页写了一半，镜像还没来得及落盘，这页就废了）。
func (td *TableData) pageStage() ([]pageImage, error) {
	if td.pages == nil {
		return nil, nil
	}
	dels, puts, err := td.drainPending()
	if err != nil {
		return nil, err
	}
	if len(dels) == 0 && len(puts) == 0 {
		return nil, nil
	}
	return td.pages.write(dels, puts)
}

// pageFlush 刷脏页 + 清 FPI 标记（只在镜像已经写进 WAL 之后调用）。
func (td *TableData) pageFlush() (int, error) {
	if td.pages == nil {
		return 0, nil
	}
	return td.pages.checkpoint()
}

// pageDirty 报告还有多少改动没落盘（待写入的行 + 池里的脏页）。
func (td *TableData) pageDirty() int {
	if td.pages == nil {
		return 0
	}
	return len(td.pendingSlots) + td.pages.dirtyPages()
}

// closePages 关掉页文件（DROP TABLE / 表被换掉时）。
func (td *TableData) closePages() {
	if td.pages != nil {
		_ = td.pages.close()
		td.pages = nil
	}
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
