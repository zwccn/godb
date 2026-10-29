package storage

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// 页文件 + 缓冲池。
//
// 设计上的三个决定（和方案确认时一致）：
//  1. 淘汰用 CLOCK（近似 LRU）：一个 ref 位 + 一个环形游标，O(1)，不用维护链表；
//  2. **禁止 steal**：未提交的页不出内存，所以脏页一定是「已提交但还没刷盘」，
//     淘汰时直接刷掉即可，不需要 undo 日志；
//  3. 页 I/O 放在池锁内做。读一页就是一次 pread（几微秒），换来的是
//     「页正在读入却被别的 goroutine 拿去用」这类竞态根本不存在。
//     真到了瓶颈再挪到锁外（届时需要 per-frame 的 loading 状态），现在不值得。
//
// 行能换出之后补了两件事：
//   - **全局内存预算**（memBudget）：以前是「每张表各 1024 页」，20 张表等于
//     160MB，跟配置里那个数字完全不是一回事。现在是所有表的页框加在一起
//     不超过预算；一个表要用别人的额度，就让「框最多的那张表」先吐一页。
//   - **顺序预取**（prefetch）：扫表按 RID 递增，页号基本单调，检测到连续就
//     一次 pread 读 8 页塞进池子，把「一页一次 syscall + 等 I/O」摊平。

// defaultPoolPages 是全局缓冲池的默认页数（1024 × 8KB = 8MB）。
const defaultPoolPages = 1024

// defaultPrefetchDepth 是一次顺序预取读几页（8 页 = 64KB，一次 pread）。
// 是变量而不是常量：测试/诊断要能把它关掉（0 = 不预取），对照组才好比。
var defaultPrefetchDepth = 8

var errPoolBusy = errors.New("storage: 缓冲池里没有可淘汰的页（全被钉住了）")

// memBudget 是全库共享的页框预算。
//
// 计数用 atomic（不在池锁里拿预算锁，避免「池锁 → 预算锁」和
// 「预算锁 → 池锁」构成环）；pools 的增删才用互斥锁。
type memBudget struct {
	total int64
	used  atomic.Int64

	mu    sync.Mutex
	pools []*bufPool
}

func newMemBudget(pages int) *memBudget {
	if pages <= 0 {
		pages = defaultPoolPages
	}
	return &memBudget{total: int64(pages)}
}

func (b *memBudget) totalPages() int { return int(b.total) }
func (b *memBudget) usedPages() int  { return int(b.used.Load()) }

func (b *memBudget) freePages() int {
	n := b.total - b.used.Load()
	if n < 0 {
		return 0
	}
	return int(n)
}

func (b *memBudget) register(p *bufPool) {
	b.mu.Lock()
	b.pools = append(b.pools, p)
	b.mu.Unlock()
}

func (b *memBudget) unregister(p *bufPool) {
	b.mu.Lock()
	for i, q := range b.pools {
		if q == p {
			b.pools = append(b.pools[:i], b.pools[i+1:]...)
			break
		}
	}
	b.mu.Unlock()
}

// take 申请一个页框额度。
func (b *memBudget) take() bool {
	for {
		used := b.used.Load()
		if used >= b.total {
			return false
		}
		if b.used.CompareAndSwap(used, used+1) {
			return true
		}
	}
}

func (b *memBudget) release() { b.used.Add(-1) }

// reclaimOne 让**别的表**先吐出一页，给调用方腾一个额度。
//
// 挑框最多的池子先试：它最可能有多余的页，也让预算在表间大致均摊。
// 先排序再放锁，再逐个试 —— 池锁只在 tryEvictOne 里拿，
// 「预算锁 → 池锁」这条边只出现在 resident()（只读 len(frames)），
// 而没有任何路径会「持池锁去等预算锁」（take/release 都是 atomic）。
func (b *memBudget) reclaimOne(exclude *bufPool) bool {
	b.mu.Lock()
	cands := make([]*bufPool, 0, len(b.pools))
	for _, p := range b.pools {
		if p != exclude {
			cands = append(cands, p)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].resident() > cands[j].resident() })
	b.mu.Unlock()

	for _, p := range cands {
		if p.tryEvictOne() {
			return true
		}
	}
	return false
}

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

// readPages 一次读多页（顺序预取用）。ids 连续时只发一次 pread，
// 不连续就退化成逐页读。文件尾之后的部分给全零页。
func (pf *pageFile) readPages(ids []uint32) ([][]byte, error) {
	out := make([][]byte, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	contiguous := int(ids[len(ids)-1]-ids[0])+1 == len(ids)
	if !contiguous {
		for i, id := range ids {
			b, err := pf.readPage(id)
			if err != nil {
				return nil, err
			}
			out[i] = b
		}
		return out, nil
	}
	first := ids[0]
	n := len(ids)
	avail := 0
	if first < pf.pages {
		avail = int(pf.pages - first)
		if avail > n {
			avail = n
		}
	}
	// 一次分配、一次 pread，返回的每个页都是这一块内存的切片 —— 零拷贝，
	// 后面装进页框时再拷一次就够（以前是「读进 raw → 拷到每页 buf → 再拷进页框」）。
	raw := make([]byte, n*PageSize)
	if avail > 0 {
		if _, err := pf.f.ReadAt(raw[:avail*PageSize], int64(first)*PageSize); err != nil {
			return nil, err
		}
	}
	for i := 0; i < n; i++ {
		out[i] = raw[i*PageSize : (i+1)*PageSize]
	}
	return out, nil
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

// bufPool 是页框集合 + CLOCK 游标。
//
// ring 是「紧凑的」框数组：淘汰一页就从里面摘掉（和末尾交换后截断），
// 所以长度就是当前驻留的框数，没有空洞要扫。
type bufPool struct {
	mu     sync.Mutex
	pf     *pageFile
	budget *memBudget
	// cap 只在没有预算（budget == nil）时生效：固定大小的池子，
	// 给离线工具（备份导入）和一些单表测试用。
	cap    int
	frames map[uint32]*bufFrame
	ring   []*bufFrame
	hand   int

	// evictBlocked 由上层（TableData）注入：返回 true 表示「这张表正在
	// checkpoint / VACUUM」，此时只允许淘汰**干净页**（脏页要等全页镜像
	// 先写进 WAL 才能落盘，淘汰会把它提前刷出去）。
	evictBlocked func() bool

	hits, misses, evicts, flushes int64
	prefetched                    int64
	overflow                      int64
	pinnedBlocked                 int64

	// onEvict 在一页被淘汰（或从池里摘掉）时回调，给上层「把行从内存里放掉」用。
	onEvict func(page uint32)
}

func newBufPool(pf *pageFile, budget *memBudget, cap int) *bufPool {
	if cap < 2 {
		cap = 2
	}
	bp := &bufPool{pf: pf, budget: budget, cap: cap, frames: make(map[uint32]*bufFrame, 16)}
	if budget != nil {
		budget.register(bp)
	}
	return bp
}

// fetch 取一页并把它钉住（pins++）。调用方用完必须 unpin。
// 拿到手后还要自己加 f.latch（读 RLock / 写 Lock）。
//
// 预算满了、本表又没有可淘汰的框（全被钉住）时，先让别的表吐一页再重试；
// 还是不行就临时超一格（软上限）—— 读失败比多占 8KB 严重得多。
const noPage = ^uint32(0)

// fetch 取一页；如果顺手淘汰了别的页，在**放开池锁之后**通知上层
// （回调要改行数据，不能攥着池锁做）。
func (bp *bufPool) fetch(id uint32) (*bufFrame, error) {
	f, err := bp.fetchRetry(id, false)
	if err == nil {
		return f, nil
	}
	if err != errPoolBusy {
		return nil, err
	}
	// 自己的框全被钉住、预算又满了：换个表要额度再试一次
	if bp.budget != nil && bp.budget.reclaimOne(bp) {
		f, err = bp.fetchRetry(id, false)
		if err == nil {
			return f, nil
		}
		if err != errPoolBusy {
			return nil, err
		}
	}
	// 别的表也在 checkpoint（脏页受保护，谁也不让）：临时超预算一格。
	// 读失败比多占 8KB 严重得多，超出的那一格随后被淘汰就还回去了。
	return bp.fetchRetry(id, true)
}

func (bp *bufPool) fetchRetry(id uint32, force bool) (*bufFrame, error) {
	f, evicted, err := bp.fetchLocked(id, force)
	if err != nil {
		return nil, err
	}
	// 淘汰回调要改行数据，必须放开池锁之后再叫
	if evicted != noPage && bp.onEvict != nil {
		bp.onEvict(evicted)
	}
	return f, nil
}

func (bp *bufPool) fetchLocked(id uint32, force bool) (*bufFrame, uint32, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if f, ok := bp.frames[id]; ok {
		f.pins++
		f.ref = true
		bp.hits++
		return f, noPage, nil
	}
	bp.misses++

	data, err := bp.pf.readPage(id)
	if err != nil {
		return nil, noPage, err
	}
	// 校验一次（魔数 + CRC）：换入路径按槽位直取元组，不会再去 decode 整页
	if err := verifyPage(data); err != nil {
		return nil, noPage, err
	}
	f, evicted, err := bp.installLocked(id, data, force)
	if err != nil {
		return nil, noPage, err
	}
	f.pins = 1
	return f, evicted, nil
}

// installLocked 把一个**已经读好的**页装进池里。调用方必须持 bp.mu。
//
// 返回的 frame 是「未钉住」的，谁用谁加 pins（fetch 加，预取不加）。
// force 表示允许超预算（预算满且别的表腾不出来时的兜底）。
func (bp *bufPool) installLocked(id uint32, data []byte, force bool) (*bufFrame, uint32, error) {
	evicted := noPage

	grow := false
	if bp.budget == nil {
		grow = len(bp.ring) < bp.cap
	} else if bp.budget.take() {
		grow = true
	} else if force {
		// 借一格：这里必须真的记上（used 会超过 total），否则后面的 release
		// 会把账算歪 —— 页框越攒越多、预算却永远显示「没满」。
		bp.budget.used.Add(1)
		bp.overflow++
		grow = true
	}

	if grow {
		f := &bufFrame{data: make([]byte, PageSize)}
		copy(f.data, data)
		f.id = id
		f.ref = true
		bp.frames[id] = f
		bp.ring = append(bp.ring, f)
		return f, evicted, nil
	}

	idx := bp.findVictimLocked()
	if idx < 0 {
		bp.pinnedBlocked++
		return nil, noPage, errPoolBusy
	}
	victim := bp.ring[idx]
	if victim.dirty {
		if err := bp.pf.writePage(victim.id, victim.data); err != nil {
			return nil, noPage, err
		}
		bp.flushes++
		victim.dirty = false
	}
	evicted = victim.id
	delete(bp.frames, victim.id)
	bp.evicts++

	// 复用同一个框（不还也不重新申请额度 —— 额度原地转手）
	copy(victim.data, data)
	victim.id = id
	victim.dirty = false
	victim.ref = true
	bp.frames[id] = victim
	return victim, evicted, nil
}

// findVictimLocked 找一个可以淘汰的框，返回它在 ring 里的下标（没有则 -1）。
// 最坏转两圈：第一圈只清 ref 位，第二圈才真的淘汰。
func (bp *bufPool) findVictimLocked() int {
	n := len(bp.ring)
	if n == 0 {
		return -1
	}
	if bp.hand >= n {
		bp.hand = 0
	}
	blocked := bp.evictBlocked != nil && bp.evictBlocked()
	for i := 0; i < n*2; i++ {
		idx := bp.hand
		bp.hand = (bp.hand + 1) % n
		f := bp.ring[idx]
		if f.pins > 0 {
			continue
		}
		if blocked && f.dirty {
			// 这张表正在 checkpoint：脏页的全页镜像可能还没进 WAL，
			// 先刷出去就会留下「写了一半又没镜像可修」的页。
			continue
		}
		if f.ref {
			f.ref = false
			continue
		}
		return idx
	}
	return -1
}

// removeRingLocked 把第 idx 个框从 ring 里摘掉（和末尾交换后截断，保持紧凑）。
func (bp *bufPool) removeRingLocked(idx int) {
	last := len(bp.ring) - 1
	if idx < 0 || idx > last {
		return
	}
	if idx != last {
		bp.ring[idx] = bp.ring[last]
	}
	bp.ring[last] = nil
	bp.ring = bp.ring[:last]
	if len(bp.ring) == 0 || bp.hand >= len(bp.ring) {
		bp.hand = 0
	}
}

func (bp *bufPool) indexOfRingLocked(f *bufFrame) int {
	for i, q := range bp.ring {
		if q == f {
			return i
		}
	}
	return -1
}

// resident 报告驻留的框数（预算回收时按它挑对象）。
func (bp *bufPool) resident() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.ring)
}

// tryEvictOne 主动吐一页出来（跨表回收用）。成功就还一个预算额度。
func (bp *bufPool) tryEvictOne() bool {
	bp.mu.Lock()
	idx := bp.findVictimLocked()
	if idx < 0 {
		bp.mu.Unlock()
		return false
	}
	f := bp.ring[idx]
	if f.dirty {
		if err := bp.pf.writePage(f.id, f.data); err != nil {
			bp.mu.Unlock()
			return false
		}
		bp.flushes++
		f.dirty = false
	}
	id := f.id
	delete(bp.frames, id)
	bp.removeRingLocked(idx)
	bp.evicts++
	if bp.budget != nil {
		bp.budget.release()
	}
	bp.mu.Unlock()

	if bp.onEvict != nil {
		bp.onEvict(id)
	}
	return true
}

// prefetch 顺序预取：把 ids 里还不在池里的页一次读进来。
//
// 只做「装了不用」的动作：装进来不钉住（pins=0），马上被淘汰也不可惜。
// 返回值是真正装进池的页数（诊断/测试用）。
func (bp *bufPool) prefetch(ids []uint32) int {
	if len(ids) == 0 {
		return 0
	}
	bp.mu.Lock()
	need := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if _, ok := bp.frames[id]; !ok {
			need = append(need, id)
		}
	}
	bp.mu.Unlock()
	if len(need) == 0 {
		return 0
	}

	// I/O 放在锁外：批量 pread 可能几十微秒，不值得攥着池锁做
	datas, err := bp.pf.readPages(need)
	if err != nil {
		return 0
	}

	var evicted []uint32
	added := 0
	for i, id := range need {
		// 坏页不预取：真读它的时候再报错（这里静默更安全，别把预取变成故障源）
		if verifyPage(datas[i]) != nil {
			continue
		}
		bp.mu.Lock()
		if _, ok := bp.frames[id]; ok {
			bp.mu.Unlock()
			continue
		}
		f, ev, err := bp.installLocked(id, datas[i], false)
		if err != nil {
			bp.mu.Unlock()
			break // 预算满且本表无可淘汰的页：预取是锦上添花，不必强求
		}
		f.pins = 0 // 预取不钉住
		bp.prefetched++
		bp.mu.Unlock()
		if ev != noPage {
			evicted = append(evicted, ev)
		}
		added++
	}
	for _, id := range evicted {
		if bp.onEvict != nil {
			bp.onEvict(id)
		}
	}
	return added
}

// overBudget 返回「超出预算的页框数」（0 表示没超）。
func (bp *bufPool) overBudget() int {
	if bp.budget != nil {
		if n := bp.budget.usedPages() - bp.budget.totalPages(); n > 0 {
			return n
		}
		return 0
	}
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if n := len(bp.ring) - bp.cap; n > 0 {
		return n
	}
	return 0
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

// has 报告某页是不是还在池里（换出时用：池里的是「热页」，留着）。
func (bp *bufPool) has(id uint32) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	_, ok := bp.frames[id]
	return ok
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

// drop 把某页从池里摘掉（表被删 / TRUNCATE / 还原页镜像时用）。
func (bp *bufPool) drop(id uint32) {
	bp.mu.Lock()
	f, ok := bp.frames[id]
	if ok && f.pins == 0 {
		delete(bp.frames, id)
		if idx := bp.indexOfRingLocked(f); idx >= 0 {
			bp.removeRingLocked(idx)
		}
		f.id = 0
		f.dirty = false
		if bp.budget != nil {
			bp.budget.release()
		}
	}
	bp.mu.Unlock()
}

// releaseAll 把整池页框退回预算（关表 / TRUNCATE 时用）。
// 脏页先刷一遍再放手，否则「内存里有、盘上没有」的内容会丢。
func (bp *bufPool) releaseAll() {
	if _, err := bp.flushAll(); err != nil {
		// 刷不动就宁可留着内存，也不要是「丢了也没人知道」
		return
	}
	bp.mu.Lock()
	n := len(bp.ring)
	bp.frames = make(map[uint32]*bufFrame, 16)
	bp.ring = nil
	bp.hand = 0
	bp.mu.Unlock()
	if bp.budget != nil {
		for i := 0; i < n; i++ {
			bp.budget.release()
		}
	}
}

// closePool 关表时调用：退额度 + 从预算里注销。
func (bp *bufPool) closePool() {
	bp.releaseAll()
	if bp.budget != nil {
		bp.budget.unregister(bp)
	}
}

type poolStats struct {
	Cap        int
	Resident   int
	Hits       int64
	Misses     int64
	Evicts     int64
	Flushes    int64
	Prefetched int64
	Overflow   int64
	Blocked    int64
	Used       int64 // 全库已用页框（有预算时）
	Total      int64 // 全库预算（有预算时）
}

func (bp *bufPool) stats() poolStats {
	bp.mu.Lock()
	s := poolStats{
		Cap: bp.cap, Resident: len(bp.frames),
		Hits: bp.hits, Misses: bp.misses, Evicts: bp.evicts,
		Flushes: bp.flushes, Prefetched: bp.prefetched,
		Overflow: bp.overflow, Blocked: bp.pinnedBlocked,
	}
	bp.mu.Unlock()
	if bp.budget != nil {
		s.Used = int64(bp.budget.usedPages())
		s.Total = int64(bp.budget.totalPages())
	}
	return s
}

func (bp *bufPool) hitRate() float64 {
	s := bp.stats()
	if s.Hits+s.Misses == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Hits+s.Misses)
}

// SetPrefetchDepth 调整顺序预取深度（0 = 关闭）。诊断与基准对照组用。
func SetPrefetchDepth(n int) { defaultPrefetchDepth = n }

// setDepth 是 SetPrefetchDepth 的内部别名（storage 包内测试用）。
func setDepth(n int) { defaultPrefetchDepth = n }
