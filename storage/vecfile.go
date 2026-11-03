package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"godb/types"
)

// 向量列文件：定长槽位，一条向量一个槽。
//
//	0   "GDBV"
//	4   uint16  version
//	6   uint16  dim
//	8   uint32  reserved
//	12  uint64  count        已分配槽位数
//	20  uint64  reserved
//	28  uint32  crc32(头前 28 字节)
//	32  槽位 0：dim 个 float32
//	…   槽位 i 的偏移恒为 32 + i*dim*4
//
// 为什么是定长槽位而不是追加写（像 .lob 那样）：
//
//   - 追加写在 UPDATE 时会留一份垃圾，1000 万行更新一轮就是 30 GB；
//   - 没有固定偏移就没法顺序扫，而顺序扫是向量检索唯一的热路径。
//
// 定长槽位同时买到「原地更新」和「列式顺序扫描」两件事，代价是删除的槽位
// 要等复用时才回收（位图 + 空闲计数）。
//
// 数据不进 Go 堆：28.6 GB 的 []float32 即使 GC 不扫内容，也会把 GOGC=100 的
// 下一个目标抬到 57 GB。这里让 OS 页缓存当缓存，ReadAt 按块读。
const (
	vecMagic    = "GDBV"
	vecVersion  = 1
	vecHdrSize  = 32
	vecMaxDim   = types.MaxVectorDim // 维度上限只有一处定义，避免两边改漏
	vecHdrFlush = 256                // 每扩这么多槽位落一次头，避免每次插入都写头
	// vecReadBlock 是单槽读取时的预读块大小（映射视图不可用时的退路）。
	//
	// 64KB 的依据：128 维一条 512 字节 → 一块装 128 条；768 维一条 3KB → 装 21 条。
	// 再大就浪费（随机点查只用到一条），再小则块内的条数不够摊薄系统调用。
	// 顺序扫描不走这里 —— 走整文件映射视图（见 vecRegion），
	// 把块调大到 1MB 实测只从 415 ns/行降到 285 ns/行，仍比映射慢一倍。
	vecReadBlock = 64 << 10
)

var (
	// ErrVecMissing 表示槽位超出已分配范围：行还在，向量没了。
	// 调用方应当把这一行投影成 NULL，而不是让整个查询失败。
	ErrVecMissing = errors.New("storage: 向量槽位不存在")
	// ErrVecNoFile 表示这一列的向量文件没有打开（建列失败 / 启动时打不开）。
	// 和 ErrVecMissing 必须分开：前者是「这一行没数据」，后者是「这一列用不了」，
	// 混在一起会把「整个列文件都读不到」显示成一堆 NULL，没人能看出出了什么事。
	ErrVecNoFile = errors.New("storage: 向量列文件未打开")
	// ErrVecDimMismatch 表示文件头里的维度与列定义不一致（换过列类型 / 拷错文件）。
	ErrVecDimMismatch = errors.New("storage: 向量文件维度与列定义不一致")
	// ErrVecCorrupt 表示文件头校验失败。
	ErrVecCorrupt = errors.New("storage: 向量文件头损坏")
)

// nativeLittleEndian 决定能不能把 []float32 直接当字节写。
// amd64 / arm64 都是小端；其它平台走转换慢路径（不值得为它们优化）。
var nativeLittleEndian = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// VecFile 是一列向量的定长槽位文件。零值不可用，必须用 OpenVecFile。
type VecFile struct {
	path      string
	dim       int
	slotBytes int64

	mu    sync.RWMutex
	f     *os.File
	count int64 // 已分配槽位数（逻辑值，可能大于文件实际槽数）

	// 单块预读缓存：距离查询逐行取值时，相邻槽位大多落在同一块里。
	// blkIdx 是当前缓存块的序号（-1 表示没有），blkFrom 是它在文件里的起点。
	blkIdx  int64
	blkFrom int64
	blkBuf  []byte

	// 空闲槽位位图：free[i] 的第 b 位 = 槽位 i*64+b 可复用。
	// hint 是「下一个可能有空位的字」，让分配在常见情况下是 O(1)。
	free  []uint64
	nfree int
	hint  int

	// hdrEvery 计数到 vecHdrFlush 就把头刷一次；dirty 表示头里有未落盘的 count。
	hdrEvery int
	dirty    bool

	closed atomic.Bool

	// mp 是整文件的只读映射（懒建立，只增不减）。有它时逐行取值就是一次
	// 内存访问，没有系统调用、没有锁。为什么不用 per-query 的 VecView：
	// 映射的建立要 stat + CreateFileMapping + MapViewOfFile（十几微秒），
	// 一条 `LIMIT 10` 的点查也要付这份钱；而映射在进程内长期有效是安全的
	// —— 文件只增长，已映射的那段地址永远不会失效（缩文件前必须解映射，
	// 见 TruncateTo）。
	mp  atomic.Pointer[vecRegion]
	// retired 是被替换下来的旧映射，延后关闭：可能还有 goroutine 正在读它。
	// 表级 2PL 下「有人在扫（S 锁）」与「文件在长（X 锁）」不会同时发生，
	// 所以实际上没有人还在读；留 4 个的余量只为不把正确性寄托在这个论证上。
	retired []*vecRegion
	// mpFail 表示这条文件的映射建不起来（大端平台 / 32 位地址空间不够 /
	// 网络盘）。置位后整份文件的生命周期内都走块读，不再每行重试一次。
	mpFail bool

	// 统计（监控页 / 测试用），atomic 读
	Writes     int64
	WriteBytes int64
	Reads      int64
	ReadBytes  int64
	Syncs      int64
}

// vecRegion 是一份「整文件只读映射」。
//
// 用 atomic.Pointer 发布而不是加锁保护：读路径每行都要取一次，锁一次 20 ns，
// 十万行就是 2 ms —— 这类固定开销在半秒级的查询里占比不小。
type vecRegion struct {
	m    *vecMapping
	data []byte
}

// OpenVecFile 打开（必要时创建）一个向量文件。
//
// dim 来自 catalog 里的列定义，是唯一权威：文件头里的维度只用来对账，
// 对不上就报 ErrVecDimMismatch，由调用方决定「整列置 NULL」。
func OpenVecFile(path string, dim int) (*VecFile, error) {
	if dim <= 0 || dim > vecMaxDim {
		return nil, fmt.Errorf("storage: 向量维度 %d 非法（应在 1..%d）", dim, vecMaxDim)
	}
	vf := &VecFile{path: path, dim: dim, slotBytes: int64(dim) * 4, blkIdx: -1}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("storage: 打开向量文件失败: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if size < vecHdrSize {
		// 新文件（或上次连头都没写完）：按空文件重建头。
		vf.f = f
		vf.setCountLocked(0)
		if err := vf.writeHeaderLocked(); err != nil {
			f.Close()
			return nil, err
		}
		return vf, nil
	}

	var hdr [vecHdrSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("storage: 读向量文件头失败: %w", err)
	}
	if string(hdr[0:4]) != vecMagic {
		f.Close()
		return nil, fmt.Errorf("%w: magic %q", ErrVecCorrupt, string(hdr[0:4]))
	}
	if v := binary.LittleEndian.Uint16(hdr[4:]); v != vecVersion {
		f.Close()
		return nil, fmt.Errorf("%w: 版本 %d 不认识", ErrVecCorrupt, v)
	}
	if hdim := int(binary.LittleEndian.Uint16(hdr[6:])); hdim != dim {
		f.Close()
		return nil, fmt.Errorf("%w: 文件 %d 维，列定义 %d 维", ErrVecDimMismatch, hdim, dim)
	}
	if want := crc32.Checksum(hdr[:28], crcTable); binary.LittleEndian.Uint32(hdr[28:]) != want {
		f.Close()
		return nil, fmt.Errorf("%w: crc 不匹配", ErrVecCorrupt)
	}

	// 文件长度是唯一权威：
	//   - 写入必然先扩文件（WriteAt 越过末尾就扩展），所以任何「写过」的槽位
	//     一定落在文件长度之内；
	//   - 头里的 count 每 256 次扩展才落一次盘，可能偏小；
	//   - 分配了槽位但事务回滚、没写数据，头可能偏大 —— 这种槽位没有行引用，
	//     丢掉只是把序号重发一次，无害。
	// 反过来信头会出两种事故：头偏小时丢掉已提交的槽位；头偏大时
	// （文件被截断/拷了一半）把读不到的槽位当成可用，写进去就是空洞。
	derived := (size - vecHdrSize) / vf.slotBytes
	hdrCount := int64(binary.LittleEndian.Uint64(hdr[12:]))
	vf.f = f
	vf.setCountLocked(derived)
	if hdrCount != vf.count {
		// 对账后重写一次头，下次启动不用再猜
		_ = vf.writeHeaderLocked()
	}
	return vf, nil
}

// Path 返回文件路径。
func (vf *VecFile) Path() string { return vf.path }

// Dim 返回维度。
func (vf *VecFile) Dim() int { return vf.dim }

// Count 返回已分配槽位数。
func (vf *VecFile) Count() int64 {
	return vf.loadCount()
}

// setCountLocked 写 count，调用方必须持有 vf.mu 的写锁。
//
// 用 atomic 存，是为了让 View / Read 的映射快速通道能不加锁地判边界：
// count 是唯一会被并发改写、又要在锁外读的字段（位图、块缓存都只在锁里用）。
// 写仍然全在锁内，所以「谁什么时候能改」的规则没有变。
func (vf *VecFile) setCountLocked(n int64) { atomic.StoreInt64(&vf.count, n) }

// loadCount 无锁读 count。
func (vf *VecFile) loadCount() int64 { return atomic.LoadInt64(&vf.count) }

// isClosed / setClosed：closed 也用 atomic，理由同 count ——
// Read 的快速通道要在锁外判断「文件还在不在」，否则等于每行多抢一次读锁。
func (vf *VecFile) isClosed() bool { return vf.closed.Load() }
func (vf *VecFile) setClosed()     { vf.closed.Store(true) }

// FreeSlots 返回可复用的槽位数。
func (vf *VecFile) FreeSlots() int {
	vf.mu.RLock()
	defer vf.mu.RUnlock()
	return vf.nfree
}

// Bytes 返回文件当前占用的字节数（按槽位算，不含碎片外的空洞）。
func (vf *VecFile) Bytes() int64 { return vecHdrSize + vf.Count()*vf.slotBytes }

// ---- 槽位分配 ----

// Alloc 分配一个槽位号。
//
// 只改内存里的 count 与位图，不碰文件：真正的扩文件由紧接着的 Write 完成
// （WriteAt 越过文件末尾会自动扩展）。这样热路径上没有额外的 Truncate 系统调用。
func (vf *VecFile) Alloc() (int64, error) {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return 0, fmt.Errorf("storage: 向量文件已关闭")
	}
	if id, ok := vf.takeFreeLocked(); ok {
		return id, nil
	}
	id := vf.count
	vf.setCountLocked(vf.count + 1)
	vf.noteGrowLocked(1)
	return id, nil
}

// Free 归还一个槽位，供后续 Alloc 复用。不擦除数据（下次覆盖写），也不缩文件。
func (vf *VecFile) Free(id int64) {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if id < 0 || id >= vf.count {
		return
	}
	wi, bi := int(id>>6), uint(id&63)
	for len(vf.free) <= wi {
		vf.free = append(vf.free, 0)
	}
	if vf.free[wi]&(1<<bi) != 0 {
		return // 已经空闲，重复归还
	}
	vf.free[wi] |= 1 << bi
	vf.nfree++
	if wi < vf.hint {
		vf.hint = wi
	}
}

// takeFreeLocked 从位图里取一个空闲槽位。
func (vf *VecFile) takeFreeLocked() (int64, bool) {
	if vf.nfree == 0 {
		return 0, false
	}
	for wi := vf.hint; wi < len(vf.free); wi++ {
		if w := vf.free[wi]; w != 0 {
			bi := bits.TrailingZeros64(w)
			vf.free[wi] = w & (w - 1)
			vf.nfree--
			vf.hint = wi
			return int64(wi*64 + bi), true
		}
		vf.hint = wi + 1
	}
	// hint 之后没有了，从头再扫一遍（hint 只用于加速，不能当正确性依赖）
	vf.hint = 0
	for wi := 0; wi < len(vf.free); wi++ {
		if w := vf.free[wi]; w != 0 {
			bi := bits.TrailingZeros64(w)
			vf.free[wi] = w & (w - 1)
			vf.nfree--
			vf.hint = wi
			return int64(wi*64 + bi), true
		}
	}
	vf.nfree = 0 // 位图与计数不一致时自愈（不该发生，但不该让分配永久卡住）
	return 0, false
}

// noteGrowLocked count 变化后按需刷头。
func (vf *VecFile) noteGrowLocked(n int64) {
	vf.dirty = true
	vf.hdrEvery += int(n)
	if vf.hdrEvery >= vecHdrFlush {
		vf.hdrEvery = 0
		_ = vf.writeHeaderLocked()
	}
}

// ---- 读写 ----

// Write 把一条向量写到槽位 id。槽位超出 count 时自动扩展。
func (vf *VecFile) Write(id int64, vec []float32) error {
	if len(vec) != vf.dim {
		return fmt.Errorf("vector dimension mismatch: column is %d, value has %d", vf.dim, len(vec))
	}
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return fmt.Errorf("storage: 向量文件已关闭")
	}
	if id < 0 {
		return fmt.Errorf("storage: 槽位号 %d 非法", id)
	}
	if id >= vf.count {
		vf.setCountLocked(id + 1)
		vf.noteGrowLocked(1)
	}
	return vf.writeAtLocked(id, vec)
}

// Append 分配一个槽位并写入，返回槽位号。
func (vf *VecFile) Append(vec []float32) (int64, error) {
	id, err := vf.Alloc()
	if err != nil {
		return 0, err
	}
	if err := vf.Write(id, vec); err != nil {
		vf.Free(id)
		return 0, err
	}
	return id, nil
}

// WriteRange 从槽位 from 开始连续写一批向量（批量导入用）。
//
// data 的长度必须是 dim 的整数倍。省掉「每行一次 Alloc + 一次 WriteAt」，
// 导入 1000 万行时这是数量级的差别。
func (vf *VecFile) WriteRange(from int64, data []float32) error {
	if from < 0 {
		return fmt.Errorf("storage: 槽位号 %d 非法", from)
	}
	if len(data) == 0 {
		return nil
	}
	if len(data)%vf.dim != 0 {
		return fmt.Errorf("storage: 批量写入长度 %d 不是维度 %d 的整数倍", len(data), vf.dim)
	}
	n := int64(len(data) / vf.dim)
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return fmt.Errorf("storage: 向量文件已关闭")
	}
	if from+n > vf.count {
		vf.setCountLocked(from + n)
		vf.noteGrowLocked(n)
	}
	if err := vf.writeBytesLocked(from, data); err != nil {
		return err
	}
	atomic.AddInt64(&vf.Writes, n)
	atomic.AddInt64(&vf.WriteBytes, int64(len(data))*4)
	return nil
}

func (vf *VecFile) writeAtLocked(id int64, vec []float32) error {
	if err := vf.writeBytesLocked(id, vec); err != nil {
		return err
	}
	atomic.AddInt64(&vf.Writes, 1)
	atomic.AddInt64(&vf.WriteBytes, int64(len(vec))*4)
	return nil
}

func (vf *VecFile) writeBytesLocked(id int64, vec []float32) error {
	// 写完让预读块失效：同一个事务里「写一条再读回来」必须读到新值，
	// 靠更新缓存内容做一致性容易漏（写入可能跨两块），直接作废最省心。
	vf.blkIdx = -1
	off := vecHdrSize + id*vf.slotBytes
	if nativeLittleEndian {
		_, err := vf.f.WriteAt(float32Bytes(vec), off)
		if err != nil {
			return fmt.Errorf("storage: 写向量失败: %w", err)
		}
		return nil
	}
	buf := make([]byte, len(vec)*4)
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	if _, err := vf.f.WriteAt(buf, off); err != nil {
		return fmt.Errorf("storage: 写向量失败: %w", err)
	}
	return nil
}

// region 返回当前映射，没有映射时返回 nil（无锁）。
func (vf *VecFile) region() *vecRegion { return vf.mp.Load() }

// ensureMap 保证映射覆盖 [0, wantOff)。返回 nil 表示这条文件用不了映射。
//
// 什么时候会长：批量插入时文件一路变大，而映射是建立那一刻的快照。
// 增长后旧映射依然有效（只是短），所以这里只在「要读的槽位落在映射之外」
// 时才重建 —— 顺序扫描每次前进一点点，重建的间隔是「文件翻倍」级别，
// 摊到每行等于零。
func (vf *VecFile) ensureMap(wantOff int64) *vecRegion {
	if r := vf.mp.Load(); r != nil && wantOff <= int64(len(r.data)) {
		return r
	}
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() || vf.mpFail || !nativeLittleEndian || vf.f == nil {
		return nil
	}
	if r := vf.mp.Load(); r != nil && wantOff <= int64(len(r.data)) {
		return r
	}
	st, err := vf.f.Stat()
	if err != nil || st.Size() <= vecHdrSize {
		return nil
	}
	if r := vf.mp.Load(); r != nil && st.Size() <= int64(len(r.data)) {
		return r
	}
	m, err := mapFileTo(vf.f, st.Size())
	if err != nil {
		// 建不起来就彻底别再试：每行 stat 一次比慢更难查。
		vf.mpFail = true
		return nil
	}
	nr := &vecRegion{m: m, data: m.view}
	if old := vf.mp.Swap(nr); old != nil {
		vf.retired = append(vf.retired, old)
		// 留 4 份余量再关最老的：正常路径下没人还在读旧映射（见字段注释），
		// 但解除映射的代价只是「多留一会儿」，而踩到悬垂内存是进程直接崩。
		for len(vf.retired) > 4 {
			_ = vf.retired[0].m.Close()
			vf.retired = vf.retired[1:]
		}
	}
	return nr
}

// dropMap 解除所有映射。缩文件之前必须调用：映射到文件末尾之外的地址
// 一旦被访问就是进程级异常，recover 都接不住。
func (vf *VecFile) dropMap() {
	if r := vf.mp.Swap(nil); r != nil {
		_ = r.m.Close()
	}
	for _, r := range vf.retired {
		_ = r.m.Close()
	}
	vf.retired = nil
}

// View 返回槽位的零拷贝视图：直接指向文件映射，不拷贝、不加锁。
//
// ok=false 有两种情况，调用方都应退回 Read：
//   - 这条文件映射不可用（大端 / 32 位 / 网络盘）；
//   - 槽位号超出 count —— 那种情况 Read 会返回 ErrVecMissing，由它去翻译成 NULL。
//
// 返回的切片指向映射内存：只读，不要写；也不要在映射被解除后继续持有
// （只有 TruncateTo 与 Close 会解除，两者都要求拿到表级 X 锁）。
func (vf *VecFile) View(id int64) ([]float32, bool) {
	if id < 0 {
		return nil, false
	}
	off := vecHdrSize + id*vf.slotBytes
	need := off + vf.slotBytes
	r := vf.mp.Load()
	if r == nil || need > int64(len(r.data)) {
		r = vf.ensureMap(need)
	}
	// 映射覆盖不到有两种可能，都得让上层退回 Read：
	//   - 文件还没长到这个槽位（Alloc 只改内存里的 count，扩文件是下一次 Write 的事）；
	//   - 映射在这条文件上用不了（mpFail）。
	if r == nil || need > int64(len(r.data)) {
		return nil, false
	}
	// 映射可能比 count 长（分配了还没写的槽位在文件里是零），
	// 所以边界还得按 count 判一次，否则「没有数据」会被读成「全零向量」。
	if id >= vf.loadCount() {
		return nil, false
	}
	b := r.data[off : off+vf.slotBytes]
	atomic.AddInt64(&vf.Reads, 1)
	atomic.AddInt64(&vf.ReadBytes, vf.slotBytes)
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), vf.dim), true
}

// Read 读一条向量到 dst（长度至少 dim），返回 dst[:dim]。
//
// 槽位号越界返回 ErrVecMissing，调用方应把这一行投影成 NULL。
//
// 优先走整文件映射（一次内存拷贝）；映射不可用时退回「64KB 整块预读」：
// 距离查询是逐行求值的（表达式层拿不到下一行的槽位号），一行一次 ReadAt
// 在 Windows 上要 7 µs —— 10 万行就是 0.7 秒，全花在系统调用上。
func (vf *VecFile) Read(id int64, dst []float32) ([]float32, error) {
	if vf.isClosed() {
		return nil, fmt.Errorf("storage: 向量文件已关闭")
	}
	if id < 0 || id >= vf.loadCount() {
		return nil, ErrVecMissing
	}
	if cap(dst) < vf.dim {
		dst = make([]float32, vf.dim)
	}
	dst = dst[:vf.dim]
	off := vecHdrSize + id*vf.slotBytes

	// 映射覆盖到就直接拷：没有系统调用，也没有那把每行都要抢一次的文件锁。
	if r := vf.region(); r != nil && off+vf.slotBytes <= int64(len(r.data)) {
		copy(float32Bytes(dst), r.data[off:off+vf.slotBytes])
		atomic.AddInt64(&vf.Reads, 1)
		atomic.AddInt64(&vf.ReadBytes, vf.slotBytes)
		return dst, nil
	}

	vf.mu.Lock()
	blk := off / vecReadBlock
	if vf.blkIdx != blk {
		if err := vf.fillBlockLocked(blk); err != nil {
			vf.mu.Unlock()
			return nil, err
		}
	}
	rel := off - vf.blkFrom
	// 块尾可能装不下完整一条（文件正好在块边界结束）：那一条退回单槽读。
	if rel >= 0 && rel+vf.slotBytes <= int64(len(vf.blkBuf)) {
		copy(float32Bytes(dst), vf.blkBuf[rel:rel+vf.slotBytes])
		vf.mu.Unlock()
	} else {
		_, err := vf.f.ReadAt(float32Bytes(dst), off)
		vf.mu.Unlock()
		if err != nil {
			if err == io.EOF {
				return nil, ErrVecMissing
			}
			return nil, fmt.Errorf("storage: 读向量失败: %w", err)
		}
	}
	atomic.AddInt64(&vf.Reads, 1)
	atomic.AddInt64(&vf.ReadBytes, vf.slotBytes)
	return dst, nil
}

// fillBlockLocked 把第 idx 个读缓冲块读进来。调用方必须持有 vf.mu 的写锁。
func (vf *VecFile) fillBlockLocked(idx int64) error {
	off := idx * vecReadBlock
	size := int64(vecReadBlock)
	if total := vecHdrSize + vf.count*vf.slotBytes; off+size > total {
		size = total - off
	}
	if size <= 0 {
		vf.blkIdx = -1
		return ErrVecMissing
	}
	if int64(cap(vf.blkBuf)) < size {
		vf.blkBuf = make([]byte, size)
	}
	buf := vf.blkBuf[:size]
	n, err := vf.f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return fmt.Errorf("storage: 读向量块失败: %w", err)
	}
	vf.blkBuf = buf[:n]
	vf.blkFrom = off
	vf.blkIdx = idx
	return nil
}

// ReadRange 从槽位 from 开始连续读 n 条，返回 dst[:m*dim]（m 是实际读到的条数）。
//
// 顺序扫描的唯一入口：一次 ReadAt 拿一整块，摊到每行只剩一次切片。
// dst 会被复用（调用方传自己的 buffer），不够长时内部扩容。
func (vf *VecFile) ReadRange(from int64, n int, dst []float32) ([]float32, error) {
	vf.mu.RLock()
	f := vf.f
	count := vf.count
	closed := vf.isClosed()
	vf.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("storage: 向量文件已关闭")
	}
	if from < 0 || n <= 0 || from >= count {
		return dst[:0], nil
	}
	if m := int64(n); from+m > count {
		n = int(count - from)
	}
	need := n * vf.dim
	if cap(dst) < need {
		dst = make([]float32, need)
	}
	dst = dst[:need]
	off := vecHdrSize + from*vf.slotBytes
	nr, err := f.ReadAt(float32Bytes(dst), off)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("storage: 读向量块失败: %w", err)
	}
	if nr < len(dst)*4 {
		// 文件比 count 短：只可能来自外部截断，能读多少算多少
		got := nr / int(vf.slotBytes)
		dst = dst[:got*vf.dim]
	}
	atomic.AddInt64(&vf.Reads, 1)
	atomic.AddInt64(&vf.ReadBytes, int64(len(dst))*4)
	return dst, nil
}

// ---- 落盘与维护 ----

// Sync 把文件与头一起刷到盘上。提交路径在 SyncTo(seq) 之前调用它。
func (vf *VecFile) Sync() error {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return nil
	}
	if vf.dirty {
		if err := vf.writeHeaderLocked(); err != nil {
			return err
		}
	}
	if err := vf.f.Sync(); err != nil {
		return fmt.Errorf("storage: 刷向量文件失败: %w", err)
	}
	atomic.AddInt64(&vf.Syncs, 1)
	return nil
}

// TruncateTo 把文件截到 count 个槽位（恢复时按文件长度对账用）。
func (vf *VecFile) TruncateTo(count int64) error {
	if count < 0 {
		return fmt.Errorf("storage: 截断槽位数 %d 非法", count)
	}
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return fmt.Errorf("storage: 向量文件已关闭")
	}
	// 先解映射再缩文件：映射到文件末尾之后的地址一旦被访问，Windows 上是
	// 「进程级异常」，Go 的 recover 接不住。解完再截，区间就永远合法。
	vf.dropMap()
	if err := vf.f.Truncate(vecHdrSize + count*vf.slotBytes); err != nil {
		return fmt.Errorf("storage: 截断向量文件失败: %w", err)
	}
	vf.setCountLocked(count)
	// 位图里超出范围的位要清掉，否则会把已不存在的槽位分配出去
	for wi := int(count >> 6); wi < len(vf.free); wi++ {
		if wi == int(count>>6) && count&63 != 0 {
			// 只保留低位（槽位 < count），高位是垃圾
			keep := ^uint64(0) >> (64 - uint(count&63))
			vf.nfree -= bits.OnesCount64(vf.free[wi] &^ keep)
			vf.free[wi] &= keep
			continue
		}
		vf.nfree -= bits.OnesCount64(vf.free[wi])
		vf.free[wi] = 0
	}
	if vf.nfree < 0 {
		vf.nfree = 0
	}
	if vf.hint > len(vf.free) {
		vf.hint = 0
	}
	// 头必须立刻对上，否则下次启动会按旧 count 把刚截掉的槽位又算回来
	return vf.writeHeaderLocked()
}

// Close 刷头并关闭文件。
func (vf *VecFile) Close() error {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if vf.isClosed() {
		return nil
	}
	vf.setClosed()
	if vf.f == nil {
		return nil
	}
	var err error
	if vf.dirty {
		err = vf.writeHeaderLocked()
	}
	vf.dropMap()
	if cerr := vf.f.Close(); err == nil {
		err = cerr
	}
	vf.f = nil
	return err
}

func (vf *VecFile) writeHeaderLocked() error {
	if vf.f == nil {
		return nil
	}
	var hdr [vecHdrSize]byte
	copy(hdr[0:4], vecMagic)
	binary.LittleEndian.PutUint16(hdr[4:], vecVersion)
	binary.LittleEndian.PutUint16(hdr[6:], uint16(vf.dim))
	binary.LittleEndian.PutUint64(hdr[12:], uint64(vf.count))
	binary.LittleEndian.PutUint32(hdr[28:], crc32.Checksum(hdr[:28], crcTable))
	if _, err := vf.f.WriteAt(hdr[:], 0); err != nil {
		return fmt.Errorf("storage: 写向量文件头失败: %w", err)
	}
	vf.dirty = false
	return nil
}

// Stats 返回一组只读统计（监控页展示用）。
type VecFileStats struct {
	Dim        int
	Count      int64
	FreeSlots  int
	Bytes      int64
	Writes     int64
	WriteBytes int64
	Reads      int64
	ReadBytes  int64
	Syncs      int64
}

// Stats 取当前统计。
func (vf *VecFile) Stats() VecFileStats {
	vf.mu.RLock()
	count, nfree := vf.count, vf.nfree
	vf.mu.RUnlock()
	return VecFileStats{
		Dim:        vf.dim,
		Count:      count,
		FreeSlots:  nfree,
		Bytes:      vecHdrSize + count*vf.slotBytes,
		Writes:     atomic.LoadInt64(&vf.Writes),
		WriteBytes: atomic.LoadInt64(&vf.WriteBytes),
		Reads:      atomic.LoadInt64(&vf.Reads),
		ReadBytes:  atomic.LoadInt64(&vf.ReadBytes),
		Syncs:      atomic.LoadInt64(&vf.Syncs),
	}
}

// float32Bytes 返回 []float32 的字节视图（零拷贝）。
//
// 小端平台上 float32 的内存布局就是我们要的格式，直接交给 ReadAt/WriteAt，
// 省掉「每个元素一次 Uint32 转换」——768 维那样转换一轮比距离计算本身还贵。
// GC 不搬移堆对象，所以这个视图在调用期间始终有效。
func float32Bytes(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), len(v)*4)
}

// ---- 映射视图：顺序扫描的快速路径 ----

// VecView 是一次查询期间的只读映射。
//
// 为什么必须有它：实测（本机 512MB 定长向量文件，整块扫描）
//
//	ReadAt 单句柄   2.3 GB/s    4 句柄 5.6    8 句柄 7.1
//	mmap 视图       9.0 GB/s    4 线程 17.9   8 线程 37.0
//
// ReadAt 在 Windows 上同一句柄的 I/O 是被串行化的，多开句柄也只能到 7 GB/s，
// 天花板根本不在内存带宽上；mmap 把「文件页」直接变成「内存页」，吞吐与
// 堆内切片一致。差 3–5 倍，顺序扫描没有理由不走这条路。
//
// 生命周期由调用方管：一次扫描开一个，扫完 Close。之所以不需要引用计数，
// 是因为表级 2PL 保证「拿着 S 锁扫描时，没有别的事务能写这张表」——
// 扫描期间文件不会增长，映射天然有效。写完再扫时重新 NewView 即可。
type VecView struct {
	m     *vecMapping
	data  []byte
	count int64
	dim   int
	slot  int64
}

// NewView 映射当前文件内容，用于一次顺序扫描。
//
// 平台的映射能力不足时返回错误，调用方退回 ReadRange（结果一样，只是慢）。
func (vf *VecFile) NewView() (*VecView, error) {
	if !nativeLittleEndian {
		// 大端平台上 float32 的内存布局与文件格式不同，不做零拷贝（少见，退回 ReadRange）
		return nil, fmt.Errorf("storage: 大端平台不支持向量映射视图")
	}
	vf.mu.RLock()
	f := vf.f
	count := vf.count
	closed := vf.isClosed()
	vf.mu.RUnlock()
	if closed || f == nil {
		return nil, fmt.Errorf("storage: 向量文件已关闭")
	}
	// 以文件实际长度为准：count 里可能有「分配了但还没写」的槽位
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	avail := (st.Size() - vecHdrSize) / vf.slotBytes
	if avail < 0 {
		avail = 0
	}
	if count > avail {
		count = avail
	}
	v := &VecView{count: count, dim: vf.dim, slot: vf.slotBytes}
	if count == 0 {
		return v, nil
	}
	m, err := mapFileTo(f, vecHdrSize+count*vf.slotBytes)
	if err != nil {
		return nil, err
	}
	v.m = m
	v.data = m.view
	return v, nil
}

// Dim 返回维度。
func (v *VecView) Dim() int { return v.dim }

// Count 返回可读的槽位数。
func (v *VecView) Count() int64 { return v.count }

// Slice 返回 [from, from+n) 的零拷贝视图；超出范围会自动收窄。
//
// 返回的切片指向映射内存，Close 之后不可再用（调用方不应长期持有）。
func (v *VecView) Slice(from, n int64) []float32 {
	if v == nil || from < 0 || n <= 0 || from >= v.count {
		return nil
	}
	if from+n > v.count {
		n = v.count - from
	}
	off := vecHdrSize + from*v.slot
	b := v.data[off : off+n*v.slot]
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), int(n)*v.dim)
}

// At 返回单个槽位的零拷贝视图。
func (v *VecView) At(id int64) []float32 { return v.Slice(id, 1) }

// Close 解除映射。
func (v *VecView) Close() error {
	if v == nil || v.m == nil {
		return nil
	}
	err := v.m.Close()
	v.m = nil
	v.data = nil
	return err
}
