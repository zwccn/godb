package storage

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	// rows 是槽位数组。nil 表示「这个槽没有驻留的行」——可能是被删了，
	// 也可能是被换出到页文件了（见 rowAt 的判定）。Live 是真实行数。
	rows []types.Row
	Live int
	// free 是可复用的空槽（LIFO）。只在提交时压入/弹出，事务内删除的行
	// 仍然占着槽位（它只是被标记成删了），所以事务内插入不会复用自己刚删的槽。
	free []int

	// pk 是主键索引：保序键 → 槽位。用 B+Tree 而不是哈希表，这样主键也能走
	// 范围扫描（`id BETWEEN`、`ORDER BY id`、`min/max`），而且键天然有序。
	// 树本身在页文件里（一个索引一个文件），内存里只有页缓存。
	pk  *btree
	Seq int64 // 自增列当前值

	// second 是用户建的二级索引：索引名（小写）→ B+Tree。
	second map[string]*btree

	// unique 是非主键 UNIQUE 列组的索引：列组签名（"1,3"）→ B+Tree。
	unique map[string]*btree

	// indexTrees 是「索引身份 → 树」的登记表（"pk" / "ix:名字" / "uq:1,3"），
	// 上面三个 map 只是按不同维度索引它。同一个索引只开一个文件、一个缓冲池。
	indexTrees map[string]*btree
	// idxNeedRebuild 记「打开时发现文件失效（代次落后 / 身份不符 / 校验坏）」的索引，
	// 行加载完之后统一重建。
	idxNeedRebuild map[string]bool
	// budget / indexGen 是挂索引文件时要用的上下文（预算 + 表已落盘的代次）。
	budget   *memBudget
	indexGen map[string]int64

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
	// gone 记录「自上次 checkpoint 以来被删掉的槽位」。
	//
	// 行会被换出（rows[slot]=nil），光看 nil 分不清「删了」和「不在内存里」，
	// 这张位图就是补上的那一个 bit。checkpoint 之后清空（删除已经写进页里）。
	gone []bool

	// fullSync 表示整表内容都变了（DDL 重写列），checkpoint 要把每一行都写回页，
	// 而不是只写 pending 里那几个槽。
	fullSync bool

	// tbl 是 catalog 里的表定义 —— 从页里解码行要用它对齐列与类型。
	tbl *catalog.Table

	// pageMu 让「换入换出」和「checkpoint 写页」不撞车：
	// 读路径取 RLock（可能触发缓冲池淘汰），checkpoint/vacuum 取 Lock
	// （这期间不许淘汰，否则脏页可能在镜像落盘前就被刷出去）。
	pageMu sync.RWMutex

	// evictBlocked 和 pageMu 写锁同进同出：全库共享预算之后，**别的表**的
	// 读也可能淘汰本表的页（跨表回收），光靠本表的 pageMu 拦不住，
	// 所以给缓冲池一个「这张表正在写页」的标志，让它只淘汰干净页。
	evictBlocked atomic.Bool

	// dir/id 记下来，是为了「第一次写的时候才开文件」——
	// 只读表不产生 .dat，少一堆空文件。
	dir string
	id  int64
}

func newTableData(name string) *TableData {
	// 索引树要挂在索引文件上，而文件要等表第一次落地（拿到稳定 ID）才存在，
	// 所以这里不建树：所有树方法对 nil 接收者都是「空索引」。
	return &TableData{Name: name}
}

// attachPages 给表挂上页文件（幂等）。id <= 0 表示这张表还没拿到稳定 ID。
//
// t 是 catalog 里的表定义：从页里换入行时要靠它对齐列与类型。
// budget 是**全库共享**的页框预算（nil = 不参与预算，固定小池）。
func (td *TableData) attachPages(dir string, id int64, t *catalog.Table, budget *memBudget, idxGen map[string]int64) error {
	if t != nil {
		td.tbl = t
	}
	if id <= 0 {
		return nil
	}
	if td.pages != nil {
		return nil
	}
	if td.dir == dir && td.id == id {
		return nil
	}
	tp, err := openTablePages(tablePagePath(dir, id), budget, defaultPoolPages)
	if err != nil {
		return err
	}
	// 缓冲池淘汰一页时，把这一页上的行从内存里放掉 —— 这就是「换出」。
	tp.onEvict = td.evictPage
	// checkpoint / VACUUM 期间别让别的表把我的脏页刷出去
	tp.guardEvict(td.evictBlocked.Load)
	td.pages = tp
	td.dir, td.id = dir, id
	td.budget, td.indexGen = budget, idxGen
	// 索引文件跟着表一起打开（身份不对或代次落后就标记「待重建」）
	if err := td.openIndexFiles(t, idxGen); err != nil {
		return err
	}
	return nil
}

// openIndexFiles 打开表上所有索引的页文件（幂等）。
func (td *TableData) openIndexFiles(t *catalog.Table, idxGen map[string]int64) error {
	if t == nil || td.id <= 0 {
		return nil
	}
	if td.indexTrees == nil {
		td.indexTrees = map[string]*btree{}
	}
	if td.idxNeedRebuild == nil {
		td.idxNeedRebuild = map[string]bool{}
	}
	td.indexGen = idxGen
	pk := t.PKIdxes()
	if len(pk) > 0 {
		td.pk = td.openIndexTree("pk")
	}
	names, _ := secondIndexCols(t)
	if len(names) > 0 {
		td.second = make(map[string]*btree, len(names))
		for _, n := range names {
			td.second[n] = td.openIndexTree("ix:" + n)
		}
	}
	groups := td.uniqueGroups(t)
	if len(groups) > 0 {
		td.unique = make(map[string]*btree, len(groups))
		for _, g := range groups {
			sig := colsSig(g)
			td.unique[sig] = td.openIndexTree("uq:" + sig)
		}
	}
	return nil
}

// openIndexTree 取（或打开）某个索引的树。打不开就当没有这个索引 —— 反正下次
// checkpoint 会因为它没被写过而保持旧代次，重启时统一重建。
func (td *TableData) openIndexTree(ident string) *btree {
	if td.dir == "" || td.id <= 0 {
		return nil
	}
	if tr, ok := td.indexTrees[ident]; ok {
		return tr
	}
	f, err := openIndexFile(td.dir, td.id, ident, td.budget, td.indexGen[ident])
	if err != nil {
		return nil
	}
	if f.fresh {
		td.idxNeedRebuild[ident] = true
	}
	tr := newBTree(f)
	td.indexTrees[ident] = tr
	return tr
}

// uniqueGroups 返回需要单独建树（不跟主键重合）的 UNIQUE 列组。
func (td *TableData) uniqueGroups(t *catalog.Table) [][]int {
	pk := t.PKIdxes()
	var groups [][]int
	for _, g := range t.UniqueGroups() {
		if sameCols(g, pk) {
			continue // 主键已经走 td.pk 了
		}
		groups = append(groups, g)
	}
	return groups
}

// lockPages 拿页的独占权（checkpoint / VACUUM 用）：这期间本表不许换页落盘。
func (td *TableData) lockPages() {
	td.pageMu.Lock()
	td.evictBlocked.Store(true)
}

func (td *TableData) unlockPages() {
	td.evictBlocked.Store(false)
	td.pageMu.Unlock()
}

// evictPage 是缓冲池淘汰一页时的回调：把这一页上的行从内存里放掉。
//
// 两种情况必须保留：表里还有没落盘的改动（pending / fullSync）——
// 那些行只存在于内存里（或只在 pending 里），页文件上的还是旧版本。
func (td *TableData) evictPage(page uint32) {
	if len(td.pendingSlots) > 0 || td.fullSync || td.pages == nil {
		return
	}
	for _, slot := range td.pages.byPage[page] {
		if slot >= 0 && slot < len(td.rows) {
			td.rows[slot] = nil
		}
	}
}

// rowAt 取一个槽位的行：内存里有就直接给，没有就从页文件换进来。
//
// 第二个返回值为 false 表示「这个槽没有行」（删了 / 从来没存在过）。
func (td *TableData) rowAt(slot int) (types.Row, bool) {
	if slot < 0 || slot >= len(td.rows) {
		return nil, false
	}
	if r := td.rows[slot]; r != nil {
		return r, true
	}
	if slot < len(td.gone) && td.gone[slot] {
		return nil, false
	}
	if td.pages == nil || td.tbl == nil {
		return nil, false
	}
	td.pageMu.RLock()
	// 顺序扫描时一次性把整页的行解出来：否则每一行都要「取页 → 加锁 → 查表 →
	// 拷元组」走一遍（20 万行扫描里这一项比读页本身还贵）。
	if page, seq := td.pages.seqRead(slot); seq {
		td.materializePage(page)
		if r := td.rows[slot]; r != nil {
			td.pageMu.RUnlock()
			return r, true
		}
	}
	tuple, ok := td.pages.get(slot)
	td.pageMu.RUnlock()
	if !ok {
		return nil, false
	}
	row, _, err := decodeRowTuple(tuple, td.tbl)
	if err != nil {
		return nil, false
	}
	td.rows[slot] = row
	return row, true
}

// materializePage 把一整页的行解码进内存（顺序扫描专用）。
//
// 只在「顺着读」的时候做：点查命中某一行时把整页 100 多行都解出来纯属浪费。
// 跳过已删除的槽位（valid/gone 是权威），否则会把删掉的行「解回来」。
func (td *TableData) materializePage(page uint32) {
	if td.pages == nil || td.tbl == nil {
		return
	}
	_ = td.pages.visitPage(page, func(slot int, tuple []byte) error {
		if slot < 0 || slot >= len(td.rows) || td.rows[slot] != nil {
			return nil
		}
		if slot < len(td.pages.valid) && !td.pages.valid[slot] {
			return nil
		}
		if slot < len(td.gone) && td.gone[slot] {
			return nil
		}
		row, _, err := decodeRowTuple(tuple, td.tbl)
		if err != nil {
			return nil
		}
		td.rows[slot] = row
		return nil
	})
	// 物化之后这些行不会再走 tp.get，所以「顺序指针 + 预取窗口」要在这里推进一步，
	// 否则扫描会卡在「物化一页就断掉」的循环里（下一页判不出是顺序读）。
	td.pages.maybePrefetch(page)
}

// hasRow 判断一个槽位上到底有没有行（不管在不在内存里）。
func (td *TableData) hasRow(slot int) bool {
	if slot < 0 || slot >= len(td.rows) {
		return false
	}
	if td.rows[slot] != nil {
		return true
	}
	if slot < len(td.gone) && td.gone[slot] {
		return false
	}
	return td.pages != nil && slot < len(td.pages.valid) && td.pages.valid[slot]
}

// setRow 写入一个槽位的行（只在包内用：提交、重放、DDL 改写）。
func (td *TableData) setRow(slot int, row types.Row) {
	for len(td.rows) <= slot {
		td.rows = append(td.rows, nil)
	}
	td.rows[slot] = row
	if slot < len(td.gone) {
		td.gone[slot] = false
	}
}

// dropRow 在内存里标掉一个槽位（删除）。真正的删除要到 checkpoint 才写进页。
func (td *TableData) dropRow(slot int) {
	if slot >= 0 && slot < len(td.rows) {
		td.rows[slot] = nil
	}
	for len(td.gone) <= slot {
		td.gone = append(td.gone, false)
	}
	if slot >= 0 {
		td.gone[slot] = true
	}
}

// SlotCount 是槽位数组的长度（行号上界，不是行数 —— 行数看 Live）。
func (td *TableData) SlotCount() int { return len(td.rows) }

// RowAt 是对外的 rowAt（给 db / exec 层用）。
func (td *TableData) RowAt(slot int) (types.Row, bool) { return td.rowAt(slot) }

// SetRow 是对外的 setRow（DDL 按下标重写行时用）。
func (td *TableData) SetRow(slot int, row types.Row) { td.setRow(slot, row) }

// MarkFullSync 标「整表都要重写」：DDL 改列之后每行内容都可能变，
// 只写 pending 那几个槽会漏（例如 ADD COLUMN ... DEFAULT 的值）。
func (td *TableData) MarkFullSync() { td.markFullSync() }

// markFullSync 标「整表都要重写」：DDL 改列之后每行内容都可能变，
// 只写 pending 那几个槽会漏（例如 ADD COLUMN ... DEFAULT 的值）。
func (td *TableData) markFullSync() {
	td.fullSync = true
	td.pendingSlots = nil
	td.pendingRows = nil
}

// HasResidentRows 报告还有多少行驻留在内存里（监控/测试用）。
func (td *TableData) ResidentRows() int {
	n := 0
	for _, r := range td.rows {
		if r != nil {
			n++
		}
	}
	return n
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
	td.fullSync = false
	td.gone = nil
}

// drainPending 把待落盘行按槽位合并（最后一次为准），编码成页里存的元组。
//
// 三种来源：pending（逐行改动）、gone（删除）、fullSync（DDL 重写整表）。
// 这里**不清空状态** —— 写页成功之后调用 clearPending 才算落地，
// 中途失败下次 checkpoint 还能重试。
func (td *TableData) drainPending() ([]int, []pendingRow, error) {
	if len(td.pendingSlots) == 0 && !td.fullSync && len(td.gone) == 0 {
		return nil, nil, nil
	}
	final := make(map[int]types.Row, len(td.pendingSlots))
	if td.fullSync {
		// 整表重写：每一行都要写回去（行可能被换出，走 rowAt 取回来）
		for i := 0; i < len(td.rows); i++ {
			if r, ok := td.rowAt(i); ok {
				final[i] = r
			} else {
				final[i] = nil
			}
		}
	}
	for slot := range td.gone {
		if td.gone[slot] {
			final[slot] = nil
		}
	}
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
	return dels, puts, nil
}

// clearPending 在改动确实写进页之后清掉待落盘状态。
func (td *TableData) clearPending() {
	td.pendingSlots = nil
	td.pendingRows = nil
	td.fullSync = false
	td.gone = nil
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
		td.clearPending()
		return nil, nil
	}
	fpis, err := td.pages.write(dels, puts)
	if err != nil {
		return nil, err
	}
	td.clearPending()
	return fpis, nil
}

// pageFlush 刷脏页 + 清 FPI 标记（只在镜像已经写进 WAL 之后调用），
// 然后把不在缓冲池里的页的行从内存里放掉。
func (td *TableData) pageFlush() (int, error) {
	if td.pages == nil {
		return 0, nil
	}
	n, err := td.pages.checkpoint()
	if err != nil {
		return n, err
	}
	// checkpoint 期间为了「镜像先于页落盘」暂时超了预算（脏页不许提前刷出去），
	// 现在镜像已经在 WAL 里、页也刷完了，把超出预算的框还回去。
	td.pages.shrink()
	td.trimResident()
	return n, nil
}

// trimResident 把「不在缓冲池里」的页上的行从内存里放掉。
//
// 时机很关键：只有「内存和磁盘一致」时才能放（否则放掉的是唯一副本）。
// 所以它在 checkpoint 刷完页之后调用，并且有 pending/fullSync 时直接跳过。
//
// 留在池里的是「热页」——它们马上可能还要用，放着能省一次读盘。
func (td *TableData) trimResident() {
	if td.pages == nil || len(td.pendingSlots) > 0 || td.fullSync {
		return
	}
	for page, slots := range td.pages.byPage {
		if td.pages.pool.has(page) {
			continue
		}
		for _, slot := range slots {
			if slot >= 0 && slot < len(td.rows) {
				td.rows[slot] = nil
			}
		}
	}
}

// PageCountDebug 是页数（测试/诊断用）。
func (td *TableData) PageCountDebug() int {
	if td.pages == nil {
		return 0
	}
	return int(td.pages.pf.pageCount())
}

// TrimResident 是对外的 trimResident（测试/诊断用）。
func (td *TableData) TrimResident() { td.trimResident() }

// pageDirty 报告还有多少改动没落盘（待写入的行 + 池里的脏页）。
func (td *TableData) pageDirty() int {
	if td.pages == nil {
		return 0
	}
	n := len(td.pendingSlots) + td.pages.dirtyPages() + td.dirtyIndexPages()
	if td.fullSync {
		n++
	}
	if len(td.gone) > 0 {
		n++
	}
	return n
}

// closePages 关掉页文件与索引文件（DROP TABLE / 表被换掉时）。
func (td *TableData) closePages() {
	if td.pages != nil {
		_ = td.pages.close()
		td.pages = nil
	}
	for _, tr := range td.indexTrees {
		if tr != nil && tr.f != nil {
			_ = tr.f.close()
		}
	}
	td.indexTrees = nil
	td.pk, td.second, td.unique = nil, nil, nil
}

// flushIndexes 把索引的脏页刷盘并把头页的代次推进到 gen（checkpoint 调用）。
// 返回写回的页数，以及「每个索引现在落到了哪一代」（写进 meta）。
func (td *TableData) flushIndexes(gen int64) (int, map[string]int64, error) {
	pages := 0
	gens := make(map[string]int64, len(td.indexTrees))
	for ident, tr := range td.indexTrees {
		if tr == nil || tr.f == nil {
			continue
		}
		n, err := tr.f.flush(gen)
		pages += n
		if err != nil {
			return pages, gens, err
		}
		gens[ident] = gen
	}
	return pages, gens, nil
}

// dirtyIndexPages 是索引池里还没落盘的脏页数（checkpoint 用它判断这张表要不要写）。
func (td *TableData) dirtyIndexPages() int {
	n := 0
	for _, tr := range td.indexTrees {
		if tr != nil && tr.f != nil {
			n += tr.f.pool.dirtyCount()
		}
	}
	return n
}

// resetIndexes 清空所有索引（TRUNCATE 用）。
func (td *TableData) resetIndexes() {
	for _, tr := range td.indexTrees {
		if tr != nil {
			tr.Reset()
		}
	}
	td.idxNeedRebuild = nil
}

// DropIndexTree 关掉某个索引的树并删掉它的页文件（DROP INDEX 用）。
func (td *TableData) DropIndexTree(name string) {
	if td.indexTrees == nil {
		return
	}
	ident := "ix:" + cleanName(name)
	tr := td.indexTrees[ident]
	if tr == nil {
		return
	}
	if tr.f != nil {
		path := tr.f.path
		_ = tr.f.close()
		_ = os.Remove(path)
	}
	delete(td.indexTrees, ident)
	if td.second != nil {
		delete(td.second, cleanName(name))
	}
}

// IndexPages 汇总索引页数（监控/EXPLAIN 用）。
func (td *TableData) IndexPages() int {
	n := 0
	for _, tr := range td.indexTrees {
		if p, _, _ := tr.PageStats(); p > 0 {
			n += p
		}
	}
	return n
}

// alloc 分配一个槽位（优先复用空槽）。
func (td *TableData) alloc() int {
	if n := len(td.free); n > 0 {
		slot := td.free[n-1]
		td.free = td.free[:n-1]
		if slot < len(td.gone) {
			td.gone[slot] = false
		}
		return slot
	}
	td.rows = append(td.rows, nil)
	return len(td.rows) - 1
}

// release 释放槽位（末尾且无其它空槽时直接截断数组）。
func (td *TableData) release(slot int) {
	if slot == len(td.rows)-1 && len(td.free) == 0 {
		td.rows = td.rows[:slot]
		return
	}
	td.free = append(td.free, slot)
}

// Recompute 重算 Live 与 free（加载、重放、整表重建之后调用）。
func (td *TableData) Recompute() {
	td.Live = 0
	td.free = td.free[:0]
	for i := range td.rows {
		if td.hasRow(i) {
			td.Live++
			continue
		}
		td.free = append(td.free, i)
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
	for i := range td.rows {
		// 被换出的行也要取回来，否则压缩会把它们当成空槽丢掉
		if r, ok := td.rowAt(i); ok {
			rows = append(rows, r)
		}
	}
	td.gone = nil
	td.rows = rows
	td.free = nil
	td.Live = len(rows)
}

// RebuildIndex 重建所有索引（主键 + 用户二级索引 + UNIQUE 列组）。
//
// 只在「行号整体变了」的时候调用：加载表、重放 WAL、ALTER TABLE、建索引、
// Compact。正常提交走增量维护（IndexInsert / IndexDelete / IndexUpdate）。
func (td *TableData) RebuildIndex(t *catalog.Table) {
	td.Recompute()
	if err := td.openIndexFiles(t, td.indexGen); err != nil {
		return
	}
	if td.pk != nil {
		td.pk.Build(td.collectPairs(t.PKIdxes()))
	}
	td.RebuildSecond(t)
	td.RebuildUniqueKeys(t)
	td.idxNeedRebuild = nil
}

// collectPairs 把「行 → 索引键」扫一遍，收集成待建树的 (键, 槽位) 列表。
// 走 rowAt：被换出的行会从页里换回来（换进来的又可能被淘汰，不能直接读 td.rows）。
func (td *TableData) collectPairs(cols []int) []idxPair {
	if len(cols) == 0 {
		return nil
	}
	pairs := make([]idxPair, 0, td.Live)
	buf := make([]byte, 0, 32)
	for i := range td.rows {
		r, ok := td.rowAt(i)
		if !ok {
			continue
		}
		buf, ok = AppendOrderKeyInto(buf[:0], r, cols)
		if !ok {
			continue
		}
		pairs = append(pairs, idxPair{Key: string(buf), Slot: i})
	}
	return pairs
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
	// 先确保每个索引都有树（新建的索引这时候才开文件）
	if err := td.openIndexFiles(t, td.indexGen); err != nil {
		return
	}
	if len(names) == 0 {
		td.second = nil
		return
	}
	for j, name := range names {
		tree := td.second[name]
		if tree == nil {
			continue
		}
		tree.Build(td.collectPairs(colsOf[j]))
	}
}

// RebuildUniqueKeys 重建非主键 UNIQUE 列的索引。
func (td *TableData) RebuildUniqueKeys(t *catalog.Table) {
	groups := td.uniqueGroups(t)
	if err := td.openIndexFiles(t, td.indexGen); err != nil {
		return
	}
	if len(groups) == 0 {
		td.unique = nil
		return
	}
	for _, g := range groups {
		tree := td.unique[colsSig(g)]
		if tree == nil {
			continue
		}
		tree.Build(td.collectPairs(g))
	}
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

// PoolStats 返回缓冲池的统计（测试/监控用）。
func (td *TableData) PoolStats() (cap, resident, used, total int, prefetched, evicts int64) {
	if td.pages == nil {
		return 0, 0, 0, 0, 0, 0
	}
	s := td.pages.pool.stats()
	return s.Cap, s.Resident, int(s.Used), int(s.Total), s.Prefetched, s.Evicts
}
