package storage

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// 页文件 + 缓冲池（第 1 期：只做「页怎么存、怎么进出内存」，
// 还没有接入 TableData —— 那一步在第 1 期下半段）。
//
// 设计上的三个决定（和方案确认时一致）：
//  1. 淘汰用 CLOCK（近似 LRU）：一个 ref 位 + 一个环形游标，O(1)，不用维护链表；
//  2. **禁止 steal**：未提交的页不出内存，所以脏页一定是「已提交但还没刷盘」，
//     淘汰时直接刷掉即可，不需要 undo 日志；
//  3. 页 I/O 放在池锁内做。读一页就是一次 pread（几微秒），换来的是
//     「页正在读入却被别的 goroutine 拿去用」这类竞态根本不存在。
//     真到了瓶颈再挪到锁外（届时需要 per-frame 的 loading 状态），现在不值得。

// defaultTablePoolPages 是单表缓冲池的默认页数（1024 × 8KB = 8MB）。
// 第 1 期 Rows 仍然全驻内存，池子只承担「页的写回缓存」，所以不必给太大；
// 第 2 期让行可换出时，这里要改成按 config.storage.bufferPoolMB 在表间分配。
const defaultTablePoolPages = 1024

var errPoolBusy = errors.New("storage: 缓冲池里没有可淘汰的页（全被钉住了）")

// pageFile 是一张表的页文件：第 n 页在 n*PageSize 处。
type pageFile struct {
	path  string
	f     *os.File
	pages uint32
}

func openPageFile(path string) (*pageFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if size%PageSize != 0 {
		f.Close()
		return nil, fmt.Errorf("storage: %s 长度 %d 不是 %d 的整数倍，文件已损坏", path, size, PageSize)
	}
	return &pageFile{path: path, f: f, pages: uint32(size / PageSize)}, nil
}

func (pf *pageFile) pageCount() uint32 { return pf.pages }

// readPage 读第 id 页。id >= 页数时返回全零页（新页的第一种状态）。
func (pf *pageFile) readPage(id uint32) ([]byte, error) {
	buf := make([]byte, PageSize)
	if id >= pf.pages {
		return buf, nil
	}
	if _, err := pf.f.ReadAt(buf, int64(id)*PageSize); err != nil {
		return nil, err
	}
	return buf, nil
}

func (pf *pageFile) writePage(id uint32, data []byte) error {
	if len(data) != PageSize {
		return errPageShort
	}
	if _, err := pf.f.WriteAt(data, int64(id)*PageSize); err != nil {
		return err
	}
	if id >= pf.pages {
		pf.pages = id + 1
	}
	return nil
}

func (pf *pageFile) sync() error { return pf.f.Sync() }

// truncate 把文件截到 n 页（VACUUM 回收尾巴上的空页用）。
func (pf *pageFile) truncate(n uint32) error {
	if n >= pf.pages {
		return nil
	}
	if err := pf.f.Truncate(int64(n) * PageSize); err != nil {
		return err
	}
	pf.pages = n
	return nil
}

func (pf *pageFile) close() error {
	if pf.f == nil {
		return nil
	}
	err := pf.f.Close()
	pf.f = nil
	return err
}

// bufFrame 是池里的一个页框。
type bufFrame struct {
	id    uint32
	data  []byte
	dirty bool
	ref   bool // CLOCK 的引用位：被访问过就置 1，淘汰时先清 0 再让一轮
	pins  int
	latch sync.RWMutex // 保护 data：读加 RLock、写加 Lock
}

// bufPool 是定长页框数组 + CLOCK 游标。
type bufPool struct {
	mu     sync.Mutex
	pf     *pageFile
	cap    int
	frames map[uint32]*bufFrame
	ring   []*bufFrame
	hand   int

	hits, misses, evicts, flushes int64
	pinnedBlocked                 int64
}

func newBufPool(pf *pageFile, cap int) *bufPool {
	if cap < 2 {
		cap = 2
	}
	return &bufPool{pf: pf, cap: cap, frames: make(map[uint32]*bufFrame, cap)}
}

// fetch 取一页并把它钉住（pins++）。调用方用完必须 unpin。
// 拿到手后还要自己加 f.latch（读 RLock / 写 Lock）。
func (bp *bufPool) fetch(id uint32) (*bufFrame, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if f, ok := bp.frames[id]; ok {
		f.pins++
		f.ref = true
		bp.hits++
		return f, nil
	}
	bp.misses++

	var victim *bufFrame
	slot := -1
	fresh := false
	if len(bp.ring) < bp.cap {
		victim = &bufFrame{data: make([]byte, PageSize)}
		fresh = true
	} else {
		found := false
		// 最坏转两圈：第一圈只清 ref 位，第二圈才真的淘汰
		for i := 0; i < bp.cap*2 && !found; i++ {
			f := bp.ring[bp.hand]
			idx := bp.hand
			bp.hand = (bp.hand + 1) % bp.cap
			if f.pins > 0 {
				continue
			}
			if f.ref {
				f.ref = false
				continue
			}
			victim, slot, found = f, idx, true
		}
		if !found {
			bp.pinnedBlocked++
			return nil, errPoolBusy
		}
		bp.evicts++
	}

	// 淘汰的是脏页：先刷掉再复用（禁止 steal 的副作用就是这个刷盘点）
	if !fresh {
		if victim.dirty {
			if err := bp.pf.writePage(victim.id, victim.data); err != nil {
				return nil, err
			}
			bp.flushes++
			victim.dirty = false
		}
		delete(bp.frames, victim.id)
	}

	data, err := bp.pf.readPage(id)
	if err != nil {
		return nil, err
	}
	copy(victim.data, data)
	victim.id = id
	victim.dirty = false
	victim.ref = true
	victim.pins = 1
	bp.frames[id] = victim
	if slot >= 0 {
		bp.ring[slot] = victim
	} else {
		bp.ring = append(bp.ring, victim)
	}
	return victim, nil
}

// unpin 放掉钉子；dirty 表示这次改过页内容。
func (bp *bufPool) unpin(f *bufFrame, dirty bool) {
	bp.mu.Lock()
	if f.pins > 0 {
		f.pins--
	}
	if dirty {
		f.dirty = true
	}
	bp.mu.Unlock()
}

// dirtyCount 返回池里还有多少脏页（checkpoint 用它决定要不要真去刷）。
func (bp *bufPool) dirtyCount() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	n := 0
	for _, f := range bp.frames {
		if f.dirty {
			n++
		}
	}
	return n
}

// flushAll 把所有脏页写回并 fsync。checkpoint 就是「刷脏页 + 记 LSN」，
// 不再是整表重写。
func (bp *bufPool) flushAll() (int, error) {
	bp.mu.Lock()
	list := make([]*bufFrame, 0, len(bp.frames))
	for _, f := range bp.frames {
		list = append(list, f)
	}
	bp.mu.Unlock()

	n := 0
	for _, f := range list {
		// 拿 RLock：写者（持 Lock）会等我们写完，不会读到半截页
		f.latch.RLock()
		bp.mu.Lock()
		dirty, id := f.dirty, f.id
		bp.mu.Unlock()
		if dirty {
			if err := bp.pf.writePage(id, f.data); err != nil {
				f.latch.RUnlock()
				return n, err
			}
			bp.mu.Lock()
			f.dirty = false
			bp.flushes++
			bp.mu.Unlock()
			n++
		}
		f.latch.RUnlock()
	}
	return n, bp.pf.sync()
}

// drop 把某页从池里摘掉（表被删 / TRUNCATE 时用）。
func (bp *bufPool) drop(id uint32) {
	bp.mu.Lock()
	if f, ok := bp.frames[id]; ok && f.pins == 0 {
		delete(bp.frames, id)
		f.id = 0
		f.dirty = false
	}
	bp.mu.Unlock()
}

type poolStats struct {
	Cap      int
	Resident int
	Hits     int64
	Misses   int64
	Evicts   int64
	Flushes  int64
	Blocked  int64
}

func (bp *bufPool) stats() poolStats {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return poolStats{
		Cap: bp.cap, Resident: len(bp.frames),
		Hits: bp.hits, Misses: bp.misses, Evicts: bp.evicts,
		Flushes: bp.flushes, Blocked: bp.pinnedBlocked,
	}
}

func (bp *bufPool) hitRate() float64 {
	s := bp.stats()
	if s.Hits+s.Misses == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Hits+s.Misses)
}
