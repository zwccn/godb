package storage

import (
	"encoding/json"
	"fmt"
	"sort"

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
}

func openTablePages(path string, poolPages int) (*tablePages, error) {
	pf, err := openPageFile(path)
	if err != nil {
		return nil, err
	}
	tp := &tablePages{
		pf:     pf,
		pool:   newBufPool(pf, poolPages),
		path:   path,
		byPage: make(map[uint32][]int),
		hi:     pf.pageCount(),
	}
	return tp, nil
}

func (tp *tablePages) pageCount() uint32 { return tp.pf.pageCount() }

// fetch 取一页并钉住；调用方负责 unpin（页的 latch 自己加）。
func (tp *tablePages) fetch(page uint32) (*bufFrame, error) { return tp.pool.fetch(page) }

// ensure 保证 RID 的目录槽位存在。
func (tp *tablePages) ensure(slot int) {
	for len(tp.loc) <= slot {
		tp.loc = append(tp.loc, rowLoc{})
		tp.valid = append(tp.valid, false)
	}
}

// put 写入一行（RID = slot）。已有位置就覆盖，没有就找地方插。
func (tp *tablePages) put(slot int, tuple []byte) error {
	if len(tuple) > MaxTupleSize {
		return errTupleTooLarge
	}
	tp.ensure(slot)

	// 已经有位置：先删旧元组（长度可能变了），再把新元组插同一页
	if tp.valid[slot] {
		at := tp.loc[slot]
		f, err := tp.fetch(at.page)
		if err != nil {
			return err
		}
		f.latch.Lock()
		p, err := decodeOrInitPage(f.data, at.page)
		if err != nil {
			f.latch.Unlock()
			tp.pool.unpin(f, false)
			return err
		}
		_ = p.del(int(at.slot))
		local, err := p.insertOrCompact(tuple, uint32(slot))
		if err == nil {
			if local != int(at.slot) {
				// 页内整理/复用让位置变了，目录跟着改
				tp.loc[slot] = rowLoc{page: at.page, slot: uint16(local)}
			}
			copy(f.data, p.encode())
			f.latch.Unlock()
			tp.pool.unpin(f, true)
			return nil
		}
		// 这页放不下：删掉旧元组后换一页
		copy(f.data, p.encode())
		f.latch.Unlock()
		tp.pool.unpin(f, true)
		if err == errPageFull {
			tp.valid[slot] = false
			tp.removeFromPageIndex(slot, at)
		} else {
			return err
		}
	}

	// 找一页能塞下的：从后往前找几页，都满了就开新页
	page, isNew, err := tp.findPage(len(tuple))
	if err != nil {
		return err
	}
	if isNew {
		tp.hi = page + 1
	}
	f, err := tp.fetch(page)
	if err != nil {
		return err
	}
	f.latch.Lock()
	p, err := decodeOrInitPage(f.data, page)
	if err != nil {
		f.latch.Unlock()
		tp.pool.unpin(f, false)
		return err
	}
	local, err := p.insertOrCompact(tuple, uint32(slot))
	if err != nil {
		f.latch.Unlock()
		tp.pool.unpin(f, false)
		return err
	}
	copy(f.data, p.encode())
	f.latch.Unlock()
	tp.pool.unpin(f, true)

	tp.loc[slot] = rowLoc{page: page, slot: uint16(local)}
	tp.valid[slot] = true
	tp.byPage[page] = append(tp.byPage[page], slot)
	if page >= tp.nextPage {
		tp.nextPage = page
	}
	return nil
}

// get 读一行（返回副本）。
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
	f.latch.RLock()
	defer f.latch.RUnlock()
	p, err := decodeOrInitPage(f.data, at.page)
	if err != nil {
		return nil, false
	}
	return p.get(int(at.slot))
}

// del 删一行：把元组从页里摘掉，并让 RID 失效（槽位号交给 TableData 复用了）。
func (tp *tablePages) del(slot int) error {
	if slot < 0 || slot >= len(tp.loc) || !tp.valid[slot] {
		return nil
	}
	at := tp.loc[slot]
	f, err := tp.fetch(at.page)
	if err != nil {
		return err
	}
	f.latch.Lock()
	p, err := decodeOrInitPage(f.data, at.page)
	if err != nil {
		f.latch.Unlock()
		tp.pool.unpin(f, false)
		return err
	}
	if err := p.del(int(at.slot)); err != nil && err != errSlotOutOfRange {
		f.latch.Unlock()
		tp.pool.unpin(f, false)
		return err
	}
	copy(f.data, p.encode())
	f.latch.Unlock()
	tp.pool.unpin(f, true)

	tp.valid[slot] = false
	tp.removeFromPageIndex(slot, at)
	return nil
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

// loadAll 读全表：按页顺序回调 (RID, 元组)。同时把 RID 目录重建出来。
func (tp *tablePages) loadAll(fn func(slot int, tuple []byte) error) error {
	n := tp.pf.pageCount()
	tp.byPage = make(map[uint32][]int, n)
	for page := uint32(0); page < n; page++ {
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
func (tp *tablePages) reset() error {
	if err := tp.pf.truncate(0); err != nil {
		return err
	}
	tp.loc = nil
	tp.valid = nil
	tp.byPage = make(map[uint32][]int)
	tp.nextPage = 0
	tp.hi = 0
	return nil
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

func (tp *tablePages) close() error { return tp.pf.close() }

// ---- 行编解码 ----
//
// 页里存的是 JSON 行，直接复用 codec.go 的 encodeRow/decodeRow：
//   - 编码：types.Value 自带 MarshalJSON，一行就是一个 JSON 数组
//   - 解码：decodeRow 会把「列数变了 / 单格解不出来」的旧行修成 NULL 并计数，
//     这套容错（ALTER TABLE 崩在中间）在页里同样需要，别另写一份
//
// 代价是比二进制编码大 40% 左右。第 1 期先按这个来，等存储层稳定了再换二进制。

func encodeRowTuple(row types.Row) ([]byte, error) {
	return json.Marshal(encodeRow(row))
}

func decodeRowTuple(tuple []byte, t *catalog.Table) (types.Row, int, error) {
	var raw []interface{}
	if err := json.Unmarshal(tuple, &raw); err != nil {
		return nil, 0, fmt.Errorf("行解码失败：%w", err)
	}
	row, repaired := decodeRow(t, raw)
	return row, repaired, nil
}
