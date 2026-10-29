package storage

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	"godb/catalog"
	"godb/types"
)

// tablePages 是一张表的「堆文件」：页文件 + 缓冲池 + RID 目录。
//
// RID（全局槽位号）就是索引里存的值，**永远不变**；页内位置（page, slot）
// 会随着插入/删除/整理变化，所以这里维护一张 RID → 页内位置的目录，
// 并且这份映射在页头里也有一份（slot.gid），重开表时靠它重建。
//
// 这一层只认识「元组字节」，不认识 catalog —— 行的编码/解码（JSON 行）
// 在调用方做，这样页层不会因为表结构变化而改。
type rowLoc struct {
	page uint32
	slot uint16
}

type tablePages struct {
	pf   *pageFile
	pool *bufPool
	path string

	// loc[RID] = 页内位置；loc 的长度等于「已分配过的最大 RID + 1」。
	// 用 valid 位图而不是 map：千万行下 map 的指针开销比 8 字节大多了。
	loc   []rowLoc
	valid []bool

	// byPage[page] = 该页里有哪些 RID（淘汰整页时要把这些 RID 标回「不在内存」）。
	byPage map[uint32][]int

	// nextPage 是「找空页」的起点：插入优先往最后几页里塞，避免每条 INSERT
	// 都从头扫一遍页。
	nextPage uint32
	// hi 是下一个可用页号。**不能用文件大小推**：脏页还在池里没落盘，
	// 文件大小会落后于实际用了多少页，推出来就会「重新分配一个还在用的页」。
	hi uint32

	// fpi 记录「自上次 checkpoint 以来，这一页有没有写过全页镜像」。
	// 防页撕裂：8KB 落盘不是原子的（可能只写了前 4KB 就断电），CRC 只能**发现**
	// 坏页、修不回来。所以每页在一次 checkpoint 之后**第一次**被改时，把整页
	// 镜像塞进 WAL；恢复时先用镜像还原整页，再重放逻辑记录。
	fpi map[uint32]bool

	// onEvict 在缓冲池淘汰某页时回调（上层用它把这一页的行从内存里放掉）。
	onEvict func(page uint32)

	// raLast 是「上一次读到哪一页」，raNext 是「预取窗口到哪一页为止」。
	// 顺序预取靠它们判断访问是不是连续的、窗口用完没有。
	// noPage 表示还没有上一次。用 atomic：读路径多 goroutine 并发。
	raLast atomic.Uint32
	raNext atomic.Uint32

	// decodes 统计「解码了多少次页」。这是写路径的核心成本指标：
	// 按行写 → 行数 次；按页批量写 → 页数 次。测试用它守住回归。
	decodes int64
}

// openTablePages 打开一张表的页文件。budget 为 nil 表示不参与全局预算
// （离线工具：备份导入时用一个固定大小的临时池）。
func openTablePages(path string, budget *memBudget, poolPages int) (*tablePages, error) {
	// 全新库第一次写表时 tables/ 目录还不存在，先建出来
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	pf, err := openPageFile(path)
	if err != nil {
		return nil, err
	}
	tp := &tablePages{
		pf:     pf,
		pool:   newBufPool(pf, budget, poolPages),
		path:   path,
		byPage: make(map[uint32][]int),
		hi:     pf.pageCount(),
		fpi:    make(map[uint32]bool),
	}
	tp.raLast.Store(noPage)
	// 池淘汰一页 → 通知上层（TableData 把这一页上的行从内存里放掉）
	tp.pool.onEvict = func(page uint32) {
		if tp.onEvict != nil {
			tp.onEvict(page)
		}
	}
	return tp, nil
}

// guardEvict 由 TableData 注入：这张表在 checkpoint / VACUUM 期间
// 不许把脏页刷出去（全页镜像还没进 WAL）。
func (tp *tablePages) guardEvict(fn func() bool) { tp.pool.evictBlocked = fn }

// maybePrefetch 判断这次读是不是「顺着往后走」，是就预取后面几页。
//
// 判据只看页号：RID 递增扫描（全表扫、范围扫、建索引）页号基本单调；
// 点查（主键命中某一行）页号乱跳，这里自然不会触发。
func (tp *tablePages) maybePrefetch(page uint32) {
	prev := tp.raLast.Swap(page)
	if prev == noPage || page != prev+1 {
		// 不是顺着走（点查、范围跳页）：窗口作废，下次连续读可以从当前位置重开
		tp.raNext.Store(page + 1)
		return
	}
	// 窗口里还有没读完的页就不重复预取 —— 否则每跨一页都发一次读，
	// 等于把「一次读 8 页」拆回「一页读一次」，白折腾。
	if page < tp.raNext.Load() {
		return
	}
	depth := defaultPrefetchDepth
	if depth <= 0 {
		return
	}
	if bp := tp.pool.budget; bp != nil {
		// 有富余就只读到富余为止（别为了预取去挤掉 8 页）；
		// 一点富余都没有也照常预取 8 页 —— 扫描是一去不回头的，
		// 刚读过的页马上就可以让位，这正是预取要利用的地方。
		if free := bp.freePages(); free > 0 && free < depth {
			depth = free
		}
	}
	hi := tp.hi
	ids := make([]uint32, 0, depth)
	for i := 1; i <= depth; i++ {
		p := page + uint32(i)
		if p >= hi {
			break
		}
		ids = append(ids, p)
	}
	if len(ids) == 0 {
		return
	}
	tp.raNext.Store(page + 1 + uint32(len(ids)))
	tp.pool.prefetch(ids)
}

func (tp *tablePages) pageCount() uint32 { return tp.pf.pageCount() }

// fetch 取一页并钉住；调用方负责 unpin（页的 latch 自己加）。
func (tp *tablePages) fetch(page uint32) (*bufFrame, error) { return tp.pool.fetch(page) }

// pageImage 是一页的整页副本，用来写进 WAL 的「首改全页镜像」记录。
type pageImage struct {
	page uint32
	data []byte
}

// pageBatch 是「一次提交里的页级批处理」：本轮碰到的页只解码一次，
// 最后每页只编码一次。putBatch / delBatch 共用它，避免两套逻辑各走各的。
type pageBatch struct {
	tp    *tablePages
	cache map[uint32]*page
	order []uint32
	fpis  []pageImage
	done  map[uint32]bool
}

func (tp *tablePages) newBatch() *pageBatch {
	return &pageBatch{
		tp:    tp,
		cache: make(map[uint32]*page, 8),
		done:  make(map[uint32]bool, 8),
	}
}

// get 取一页的解码结果；第一次碰到这页时顺手取全页镜像（防撕裂用）。
func (b *pageBatch) get(id uint32) (*page, error) {
	if p, ok := b.cache[id]; ok {
		return p, nil
	}
	img, err := b.tp.noteFPI(id)
	if err != nil {
		return nil, err
	}
	if img != nil && !b.done[id] {
		b.fpis = append(b.fpis, pageImage{page: id, data: img})
		b.done[id] = true
	}
	f, err := b.tp.fetch(id)
	if err != nil {
		return nil, err
	}
	p, err := decodeOrInitPage(f.data, id)
	b.tp.pool.unpin(f, false)
	if err != nil {
		return nil, err
	}
	b.tp.decodes++
	b.cache[id] = p
	b.order = append(b.order, id)
	return p, nil
}

// last 返回本轮最后碰到的页号（批量插入时优先往这页续着塞）。
func (b *pageBatch) last() (uint32, bool) {
	if len(b.order) == 0 {
		return 0, false
	}
	return b.order[len(b.order)-1], true
}

// flush 每页编码一次并写回页框，返回要写进 WAL 的全页镜像。
func (b *pageBatch) flush() ([]pageImage, error) {
	for _, id := range b.order {
		buf := b.cache[id].encode()
		f, err := b.tp.fetch(id)
		if err != nil {
			return b.fpis, err
		}
		f.latch.Lock()
		copy(f.data, buf)
		f.latch.Unlock()
		b.tp.pool.unpin(f, true)
	}
	if len(b.order) > 0 {
		if last := b.order[len(b.order)-1]; last >= b.tp.nextPage {
			b.tp.nextPage = last
		}
	}
	return b.fpis, nil
}

// noteFPI 在「即将修改某页」之前调用：如果这是自上次 checkpoint 以来第一次改它，
// 返回整页镜像（调用方必须把它写进 WAL），否则返回 nil。
//
// 只读页、不改页的路径不要调它，否则 WAL 会被无用镜像撑大。
func (tp *tablePages) noteFPI(page uint32) ([]byte, error) {
	if tp.fpi[page] {
		return nil, nil
	}
	f, err := tp.fetch(page)
	if err != nil {
		return nil, err
	}
	img := make([]byte, PageSize)
	f.latch.RLock()
	copy(img, f.data)
	f.latch.RUnlock()
	tp.pool.unpin(f, false)
	tp.fpi[page] = true
	return img, nil
}

// afterCheckpoint：checkpoint 之后所有页都已经是「磁盘上的样子」，
// 下次修改要重新带一次全页镜像。
func (tp *tablePages) afterCheckpoint() { tp.fpi = make(map[uint32]bool) }

// checkpoint = 刷脏页 + 清掉全页镜像标记。返回实际写回的页数。
func (tp *tablePages) checkpoint() (int, error) {
	n, err := tp.flush()
	tp.afterCheckpoint()
	return n, err
}

// ensure 保证 RID 的目录槽位存在。
func (tp *tablePages) ensure(slot int) {
	for len(tp.loc) <= slot {
		tp.loc = append(tp.loc, rowLoc{})
		tp.valid = append(tp.valid, false)
	}
}

// pendingRow 是一条待写入的行：RID + 已编码的元组。
type pendingRow struct {
	slot  int
	tuple []byte
}

// put 写入一行（单行便捷入口，内部走批量路径）。
func (tp *tablePages) put(slot int, tuple []byte) ([]pageImage, error) {
	return tp.putBatch([]pendingRow{{slot: slot, tuple: tuple}})
}

// putBatch 批量写入：**同一页只解码一次、编码一次**。
//
// 这一点是性能命门：一次提交里改了多少行并不重要，重要的是**碰到了多少页**。
// 按行写的话（每行 decode 整页 + encode 整页 + 8KB CRC），5 万行插入要 279s；
// 按页批量后同样的数据只做「页数」次编解码。
//
// 返回值是要写进 WAL 的全页镜像（每页在本次 checkpoint 周期内只出现一次）。
func (tp *tablePages) putBatch(rows []pendingRow) ([]pageImage, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	b := tp.newBatch()
	if err := b.putRows(rows); err != nil {
		return nil, err
	}
	return b.flush()
}

// delBatch 批量删行：同样按页批量，一次提交里删 5 万行也只做「页数」次编解码。
func (tp *tablePages) delBatch(slots []int) ([]pageImage, error) {
	if len(slots) == 0 {
		return nil, nil
	}
	b := tp.newBatch()
	b.delSlots(slots)
	return b.flush()
}

// write 把「删除 + 写入」放进同一个批次：一次提交里同一页可能既删又写，
// 合并成一批才不会 decode/encode 两遍。
func (tp *tablePages) write(dels []int, rows []pendingRow) ([]pageImage, error) {
	if len(dels) == 0 && len(rows) == 0 {
		return nil, nil
	}
	b := tp.newBatch()
	b.delSlots(dels)
	if err := b.putRows(rows); err != nil {
		return nil, err
	}
	return b.flush()
}

// delSlots 在批次里删行（不落盘，等 flush）。
func (b *pageBatch) delSlots(slots []int) {
	tp := b.tp
	for _, slot := range slots {
		if slot < 0 || slot >= len(tp.loc) || !tp.valid[slot] {
			continue
		}
		at := tp.loc[slot]
		p, err := b.get(at.page)
		if err != nil {
			continue
		}
		_ = p.del(int(at.slot))
		tp.valid[slot] = false
		tp.removeFromPageIndex(slot, at)
	}
}

// putRows 在批次里写入一批行（不落盘，等 flush）。
func (b *pageBatch) putRows(rows []pendingRow) error {
	tp := b.tp
	for _, row := range rows {
		if len(row.tuple) > MaxTupleSize {
			return errTupleTooLarge
		}
		tp.ensure(row.slot)

		// 已经有位置：先删旧的（长度可能变了），再插新的
		if tp.valid[row.slot] {
			at := tp.loc[row.slot]
			p, err := b.get(at.page)
			if err != nil {
				return err
			}
			_ = p.del(int(at.slot))
			local, err := p.insertOrCompact(row.tuple, uint32(row.slot))
			if err == nil {
				tp.loc[row.slot] = rowLoc{page: at.page, slot: uint16(local)}
				continue
			}
			if err != errPageFull {
				return err
			}
			tp.valid[row.slot] = false
			tp.removeFromPageIndex(row.slot, at)
		}

		// 找地方插：先续着上一页塞，再让 findPage 找，最后开新页兜底
		if cur, ok := b.last(); ok {
			if p, err := b.get(cur); err == nil {
				if local, err := p.insertOrCompact(row.tuple, uint32(row.slot)); err == nil {
					tp.loc[row.slot] = rowLoc{page: cur, slot: uint16(local)}
					tp.valid[row.slot] = true
					tp.byPage[cur] = append(tp.byPage[cur], row.slot)
					continue
				}
			}
		}
		page, isNew, err := tp.findPage(len(row.tuple))
		if err != nil {
			return err
		}
		if isNew {
			tp.hi = page + 1
		}
		p, err := b.get(page)
		if err != nil {
			return err
		}
		local, err := p.insertOrCompact(row.tuple, uint32(row.slot))
		if err != nil {
			// 兜底：开一个全新的页，一定放得下
			page = tp.hi
			tp.hi++
			p, err = b.get(page)
			if err != nil {
				return err
			}
			local, err = p.insertOrCompact(row.tuple, uint32(row.slot))
			if err != nil {
				return err
			}
		}
		tp.loc[row.slot] = rowLoc{page: page, slot: uint16(local)}
		tp.valid[row.slot] = true
		tp.byPage[page] = append(tp.byPage[page], row.slot)
	}
	return nil
}

// get 读一行（返回副本）。
//
// 只按槽位目录取那一段字节，**不解码整页**：一行一行扫的时候，按页解码会让
// 每行都付一次「解码整页 ~120 个元组」的钱（实测 20 万行扫描 1.2s → 改成
// 现在这样是几十毫秒）。页头的 CRC 在页从文件读进池时校验一次，所以这里
// 不必再验。
func (tp *tablePages) get(slot int) ([]byte, bool) {
	if slot < 0 || slot >= len(tp.loc) || !tp.valid[slot] {
		return nil, false
	}
	at := tp.loc[slot]
	f, err := tp.fetch(at.page)
	if err != nil {
		return nil, false
	}
	defer tp.pool.unpin(f, false)
	// 顺序预取：当前页还钉着（不会被预取挤掉），顺手把后面几页读进池
	tp.maybePrefetch(at.page)

	f.latch.RLock()
	defer f.latch.RUnlock()
	off := pageHeaderSize + int(at.slot)*slotEntrySize
	if off+slotEntrySize > len(f.data) {
		return nil, false
	}
	tOff := int(binary.LittleEndian.Uint16(f.data[off:]))
	tLen := int(binary.LittleEndian.Uint16(f.data[off+2:]))
	if tLen == 0 || tOff+tLen > len(f.data) {
		return nil, false
	}
	out := make([]byte, tLen)
	copy(out, f.data[tOff:tOff+tLen])
	return out, true
}

// visitPage 解出某一页上的所有行：回调 (RID, 元组)。
//
// 元组是页缓冲里的切片，只在回调期间有效（调用方要么当场解码，要么自己拷走）。
// 顺序扫描时上层用它一次物化整页的行，省掉「每行一次取页 + 拷元组」。
func (tp *tablePages) visitPage(page uint32, fn func(slot int, tuple []byte) error) error {
	f, err := tp.fetch(page)
	if err != nil {
		return err
	}
	defer tp.pool.unpin(f, false)
	f.latch.RLock()
	defer f.latch.RUnlock()
	p, err := decodeOrInitPage(f.data, page)
	if err != nil {
		return err
	}
	for i := range p.slots {
		tuple, ok := p.get(i)
		if !ok {
			continue
		}
		if err := fn(int(p.slots[i].gid), tuple); err != nil {
			return err
		}
	}
	return nil
}

// pageOf 返回一个 RID 所在的页；第二个返回值表示这个槽位有没有行。
func (tp *tablePages) pageOf(slot int) (uint32, bool) {
	if slot < 0 || slot >= len(tp.loc) || !tp.valid[slot] {
		return 0, false
	}
	return tp.loc[slot].page, true
}

// seqRead 判断「这一行所在的页是不是顺着刚才那页读的」（顺序扫描的信号）。
func (tp *tablePages) seqRead(slot int) (uint32, bool) {
	page, ok := tp.pageOf(slot)
	if !ok {
		return 0, false
	}
	prev := tp.raLast.Load()
	if prev != noPage && (page == prev || page == prev+1) {
		return page, true
	}
	return page, false
}

// del 删一行（单行便捷入口，内部走批量路径）。
func (tp *tablePages) del(slot int) ([]pageImage, error) {
	return tp.delBatch([]int{slot})
}

// removeFromPageIndex 把 RID 从「哪一页有哪些行」的索引里摘掉。
func (tp *tablePages) removeFromPageIndex(rid int, at rowLoc) {
	list := tp.byPage[at.page]
	for i, s := range list {
		if s == rid {
			tp.byPage[at.page] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// findPage 找一页能放下 size 字节的元组；都放不下就返回新页号（isNew）。
func (tp *tablePages) findPage(size int) (uint32, bool, error) {
	total := tp.hi
	for i := 0; i < 8 && i < int(total); i++ {
		page := total - 1 - uint32(i)
		f, err := tp.fetch(page)
		if err != nil {
			return 0, false, err
		}
		f.latch.RLock()
		p, err := decodeOrInitPage(f.data, page)
		f.latch.RUnlock()
		tp.pool.unpin(f, false)
		if err != nil {
			return 0, false, err
		}
		// liveCount()==0：这页只剩洞，整理一下就能用
		if p.freeBytes() >= size+slotEntrySize || p.liveCount() == 0 {
			return page, false, nil
		}
	}
	return total, true, nil // 新页
}

// loadRows 读全表，按 RID 放进槽位数组（空洞是 nil）。
//
// 这是「重启后内存槽位号 == 页里的 RID」的唯一保证：索引里存的是 RID，
// 加载时把行搬了位置，索引就全错了。
func (tp *tablePages) loadRows(t *catalog.Table) ([]types.Row, int, error) {
	var rows []types.Row
	repaired := 0
	err := tp.loadAll(func(slot int, tuple []byte) error {
		row, rep, err := decodeRowTuple(tuple, t)
		if err != nil {
			return err
		}
		repaired += rep
		for len(rows) <= slot {
			rows = append(rows, nil)
		}
		rows[slot] = row
		return nil
	})
	if err != nil {
		return nil, repaired, err
	}
	return rows, repaired, nil
}

// shrink 把超出全局预算的页框还回去（checkpoint 之后调用：这时池里的页
// 都已经落盘，淘汰不需要写盘，纯粹是「把多借的内存还掉」）。
func (tp *tablePages) shrink() int {
	n := 0
	for tp.pool.overBudget() > 0 {
		if !tp.pool.tryEvictOne() {
			break
		}
		n++
	}
	return n
}

// dirtyPages 返回池里还有多少脏页。
func (tp *tablePages) dirtyPages() int { return tp.pool.dirtyCount() }

// applyImage 把一页的整页镜像写回页文件（崩溃恢复用：页撕裂时靠它还原）。
func (tp *tablePages) applyImage(page uint32, img []byte) error {
	if len(img) != PageSize {
		return fmt.Errorf("页镜像长度 %d，应为 %d", len(img), PageSize)
	}
	// 池里可能还缓存着坏页，先摘掉，免得之后又读到它。
	// writePage 写超出文件末尾会自动把文件撑长，不需要先 truncate。
	tp.pool.drop(page)
	if err := tp.pf.writePage(page, img); err != nil {
		return err
	}
	if page >= tp.hi {
		tp.hi = page + 1
	}
	return nil
}

// loadAll 读全表：按页顺序回调 (RID, 元组)。同时把 RID 目录重建出来。
func (tp *tablePages) loadAll(fn func(slot int, tuple []byte) error) error {
	n := tp.pf.pageCount()
	tp.byPage = make(map[uint32][]int, n)
	for page := uint32(0); page < n; page++ {
		// 启动加载也是顺序读，顺手预取（大表启动能省掉一截 syscall 往返）
		tp.maybePrefetch(page)
		f, err := tp.fetch(page)
		if err != nil {
			return fmt.Errorf("读第 %d 页：%w", page, err)
		}
		f.latch.RLock()
		p, err := decodeOrInitPage(f.data, page)
		f.latch.RUnlock()
		if err != nil {
			tp.pool.unpin(f, false)
			return fmt.Errorf("第 %d 页损坏：%w", page, err)
		}
		tp.pool.unpin(f, false)
		for i := range p.slots {
			tuple, ok := p.get(i)
			if !ok {
				continue
			}
			slot := int(p.slots[i].gid)
			tp.ensure(slot)
			tp.loc[slot] = rowLoc{page: page, slot: uint16(i)}
			tp.valid[slot] = true
			tp.byPage[page] = append(tp.byPage[page], slot)
			if err := fn(slot, tuple); err != nil {
				return err
			}
		}
	}
	if n > 0 {
		tp.nextPage = n - 1
	}
	tp.hi = n
	return nil
}

// flush 把脏页写回并 fsync。
func (tp *tablePages) flush() (int, error) { return tp.pool.flushAll() }

// reset 清空整张表（TRUNCATE）：页文件重来，目录清空。
//
// 池里的页框也必须一起放掉 —— 否则文件截到 0 页之后，池里还留着旧页，
// 下一次插入会「命中」那些旧内容，把删掉的行又写回去。
func (tp *tablePages) reset() error {
	tp.pool.releaseAll()
	if err := tp.pf.truncate(0); err != nil {
		return err
	}
	tp.loc = nil
	tp.valid = nil
	tp.byPage = make(map[uint32][]int)
	tp.nextPage = 0
	tp.hi = 0
	tp.raLast.Store(noPage)
	return nil
}

// vacuumPages 逐页整理空洞（把删掉的行占的字节收回来），返回整理过的页数
// 和要写进 WAL 的全页镜像。
//
// 只搬元组字节，不动槽位目录 —— 所以 RID（槽位号）在这一步完全不受影响，
// 索引、loc 目录、byPage 都不用改。
func (tp *tablePages) vacuumPages() (int, []pageImage, error) {
	n := tp.pf.pageCount()
	var fpis []pageImage
	compacted := 0
	for page := uint32(0); page < n; page++ {
		f, err := tp.fetch(page)
		if err != nil {
			return compacted, fpis, err
		}
		f.latch.RLock()
		p, err := decodeOrInitPage(f.data, page)
		f.latch.RUnlock()
		if err != nil {
			tp.pool.unpin(f, false)
			return compacted, fpis, err
		}
		if p.holeBytes() == 0 {
			tp.pool.unpin(f, false)
			continue
		}
		// 防撕裂：改这一页之前先取整页镜像（和写入路径同一套规则）
		img, err := tp.noteFPI(page)
		if err != nil {
			tp.pool.unpin(f, false)
			return compacted, fpis, err
		}
		if img != nil {
			fpis = append(fpis, pageImage{page: page, data: img})
		}
		p.compact()
		buf := p.encode()
		f.latch.Lock()
		copy(f.data, buf)
		f.latch.Unlock()
		tp.pool.unpin(f, true)
		compacted++
	}
	return compacted, fpis, nil
}

// reclaim 回收「整页都是空的」页：把它们从文件里摘掉（第 3 期的 VACUUM 用）。
// 现在只整理文件尾部：中间的空页留在原位（页号不能动，RID 目录指着它）。
func (tp *tablePages) reclaim() (uint32, error) {
	n := tp.pf.pageCount()
	var empty []uint32
	for page := uint32(0); page < n; page++ {
		f, err := tp.fetch(page)
		if err != nil {
			return 0, err
		}
		f.latch.RLock()
		p, err := decodeOrInitPage(f.data, page)
		f.latch.RUnlock()
		tp.pool.unpin(f, false)
		if err != nil {
			return 0, err
		}
		if p.liveCount() == 0 {
			empty = append(empty, page)
		}
	}
	if len(empty) == 0 {
		return 0, nil
	}
	sort.Slice(empty, func(i, j int) bool { return empty[i] > empty[j] })
	trimmed := uint32(0)
	for _, page := range empty {
		if page != tp.pf.pageCount()-1 {
			break // 只摘尾部连续的，中间的留着（页号不能重排）
		}
		tp.pool.drop(page)
		if err := tp.pf.truncate(page); err != nil {
			return trimmed, err
		}
		tp.hi = page
		trimmed++
	}
	if tp.nextPage >= tp.hi && tp.hi > 0 {
		tp.nextPage = tp.hi - 1
	}
	return trimmed, nil
}

// close 关表：先刷脏页、把页框退回预算，再关文件。
func (tp *tablePages) close() error {
	tp.pool.closePool()
	return tp.pf.close()
}

// ---- 行编解码 ----
//
// 页里的元组用二进制（binwal.go 的 appendRow / readRowValues）：u32 列数 +
// 每个值 1 字节类型标签 + 定长/带长度前缀的载荷。和 WAL 记录、回收站备份
// 用的是同一套编码，加新类型只改一个地方。
//
// 比 JSON 省 40% 左右（早先为图省事用 JSON，一行一个数组；numeric 要写成
// {"$n":"..."}、timestamp 要写成 {"$ts":...}，光是包装就占掉不少）。
//
// 兼容性照旧交给 castRow：页里存的是「写这一行时的列数和类型」，读的时候
// 按当前表结构补齐/截断（ALTER TABLE 崩在中间留下的旧行也能读）。

func encodeRowTuple(row types.Row) ([]byte, error) {
	buf := make([]byte, 0, 8*len(row)+8)
	buf = appendRow(buf, row)
	if len(buf) > MaxTupleSize {
		return nil, errTupleTooLarge
	}
	return buf, nil
}

// decodeRowTuple 解码一行。
//
// 直接解进目标 types.Row，不像以前那样先攒一个 []types.Value 再拷一遍：
// 换出之后每一行都要走这条路，20 万行扫描光这个中间切片就是 20 万次分配
// （profile 里 mallocgc + GC 占掉四分之一时间）。
func decodeRowTuple(tuple []byte, t *catalog.Table) (types.Row, int, error) {
	if len(tuple) < 4 {
		return nil, 0, fmt.Errorf("行解码失败：行头越界")
	}
	n := int(binary.LittleEndian.Uint32(tuple))
	cols := len(t.Columns)
	row := make(types.Row, cols)
	repaired := 0
	if n != cols {
		if n > cols {
			repaired = n - cols
		} else {
			repaired = cols - n
		}
	}
	off := 4
	for i := 0; i < n; i++ {
		v, next, err := readValue(tuple, off)
		if err != nil {
			return nil, 0, fmt.Errorf("行解码失败：%w", err)
		}
		off = next
		if i >= cols {
			continue // 列被删了：多出来的值丢掉（repaired 已经记过）
		}
		// 对齐当前表结构：列数变了、类型换了（ALTER TABLE）都在这里兜住
		cv, err := types.Cast(v, t.Columns[i].Type())
		if err != nil {
			row[i] = types.Null
			repaired++
			continue
		}
		row[i] = cv
	}
	for i := n; i < cols; i++ {
		row[i] = types.Null // 列是新加的：老行没有这一列
	}
	return row, repaired, nil
}
