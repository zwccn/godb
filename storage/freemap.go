package storage

// freeMap（FSM，空闲空间图）回答一个问题：**哪一页还放得下这条元组？**
//
// 没有它的时候，插入只会往回看最后 8 页（`findPage` 的老实现），于是「中间页被删出来的
// 空洞永远用不上」——表文件只涨不缩，VACUUM 把字节收回来了、插入却还是去开新页。
//
// 结构：每页一个「空闲档位」（64 字节一档），同档的页用双向链表串起来，桶头是页号。
// 所有操作都是 O(1)：换档 = 摘链 + 挂到新桶头。
//
//	class[page] = 0..126  有空闲（值是「还能放多少字节 / 64」）
//	            = 127     整页空着（洞也算），能放任何东西 —— 最好的兜底
//	            = 255     不在图里（刚分配还没写、或被回收了）
//
// 为什么 64 字节一档：太细 → 桶多、命中的页常常「差一点点放不下」，白跑一次
// insert 失败 + compact；太粗 → 挑到的页浪费空间大。64 字节约等于最小元组的量级。
//
// 并发：**不自己加锁**。它只在页的写路径上被改（提交落盘、VACUUM、TRUNCATE、加载），
// 那些路径都持有 TableData.pageMu 的写锁；读路径（取行）不碰它。
const (
	fsmBucketBytes = 64
	fsmBuckets     = 128
	// fsmEmpty 是「整页空」的档位：比任何「有存活行」的档都大，
	// 所以 best-fit 会先挑更合适的页，最后才用空页。
	fsmEmpty = fsmBuckets - 1
	// fsmNoClass 表示这一页不在图里。
	fsmNoClass = 255
)

type freeMap struct {
	head [fsmBuckets]uint32 // 每档的桶头（页号）
	next []uint32           // 按页号索引的链表指针
	prev []uint32
	cls  []uint8
}

func newFreeMap() *freeMap {
	m := &freeMap{}
	m.reset()
	return m
}

func (m *freeMap) reset() {
	for i := range m.head {
		m.head[i] = noPage
	}
	m.next = nil
	m.prev = nil
	m.cls = nil
}

// grow 让按页号索引的数组长到能容纳 page。
func (m *freeMap) grow(page uint32) {
	for uint32(len(m.cls)) <= page {
		m.cls = append(m.cls, fsmNoClass)
		m.next = append(m.next, noPage)
		m.prev = append(m.prev, noPage)
	}
}

// update 记下某一页的空闲情况：free 是「整理之后还能用的字节」，
// live 是存活行数（0 表示整页空，可以放任何东西）。
func (m *freeMap) update(page uint32, free, live int) {
	if page == noPage {
		return
	}
	m.grow(page)
	var want uint8
	if live == 0 {
		want = fsmEmpty
	} else {
		b := free / fsmBucketBytes
		if b < 0 {
			b = 0
		}
		if b >= fsmEmpty {
			b = fsmEmpty - 1 // 有存活行但很空：归到次高档，别冒充「整页空」
		}
		want = uint8(b)
	}
	if m.cls[page] == want {
		return
	}
	m.unlink(page)
	m.cls[page] = want
	m.next[page] = m.head[want]
	m.prev[page] = noPage
	if m.head[want] != noPage {
		m.prev[m.head[want]] = page
	}
	m.head[want] = page
}

// unlink 把一页从它所在的档里摘掉（不改 cls），桶为空时把桶头复位。
func (m *freeMap) unlink(page uint32) {
	c := m.cls[page]
	if c == fsmNoClass {
		return
	}
	pv, nx := m.prev[page], m.next[page]
	if pv == noPage {
		m.head[c] = nx
	} else {
		m.next[pv] = nx
	}
	if nx != noPage {
		m.prev[nx] = pv
	}
	m.prev[page], m.next[page] = noPage, noPage
	m.cls[page] = fsmNoClass
}

// remove 把一页彻底移出图（页被回收 / 文件截断）。
func (m *freeMap) remove(page uint32) {
	if uint32(len(m.cls)) <= page {
		return
	}
	m.unlink(page)
}

// find 找一页能放下 need 字节的：从「刚好够」的档往上找（best-fit）。
//
// 往上找而不是往下找：先把「塞得下但快满了」的页填满，空闲空间才会集中在少数页上，
// 表也就不容易长；整页空的页排在最后，是实在没有合适页时的兜底。
func (m *freeMap) find(need int) (uint32, bool) {
	if need < 0 {
		need = 0
	}
	// 档位是 floor(free/64)：档 c 装的是 free ∈ [c*64, c*64+63] 的页。
	// 所以「放得下 need 字节」的最小档是 ceil(need/64)，不是 need/64 ——
	// 用 floor 会让 need=51 从档 0 开始找，而档 0 里是「只剩 0~63 字节」的满页：
	// 它们排在 best-fit 最前面，每次都命中、每次都放不下，插入只好开新页。
	// （实测：删 90% 再插回等量数据，表文件 1450 页 → 2754 页。）
	b := (need + fsmBucketBytes - 1) / fsmBucketBytes
	if b >= fsmBuckets {
		return noPage, false
	}
	for c := b; c < fsmBuckets; c++ {
		if m.head[c] != noPage {
			return m.head[c], true
		}
	}
	return noPage, false
}

// stats 返回图里的页数与每档页数（测试与诊断用）。
func (m *freeMap) stats() (pages int, perBucket [fsmBuckets]int) {
	for p, c := range m.cls {
		if c == fsmNoClass {
			continue
		}
		pages++
		perBucket[c]++
		_ = p
	}
	return pages, perBucket
}
