package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync/atomic"
)

// 索引文件：一个索引一个文件，页 0 是头页，页 1 起是 B+Tree 的节点。
//
// 为什么不写在堆文件里：堆页的空间回收（VACUUM/重整）和索引完全无关，
// 混在一起会让「表页满」和「索引页满」互相牵连；而且索引可以整体重建，
// 单独一个文件就能「删了重来」。
//
// 一致性策略（和方案确认时一致）：**索引不写 WAL**。索引页只在 checkpoint
// 时刷盘，头页里记着它对应的表代次；启动时比对不上、或任何一页 CRC 坏，
// 就把这棵索引整棵重建（全表扫一遍，20 万行几百毫秒）。换来的是：
// 不需要索引 redo 记录、不需要索引页的全页镜像，恢复路径只有一条。
const (
	idxPageMagic   = "GDBI"
	idxHeaderMagic = "GDBIX1\x00\x00"
	idxPageVersion = 1

	// 页头 28 字节，和堆页保持一致的形状：
	//	0  magic "GDBI"
	//	4  pageNo u32
	//	8  flags  u16   (bit0 = 叶子)
	//	10 count  u16   条目数
	//	12 freeLo u16   槽位目录末尾
	//	14 freeHi u16   元组区起点
	//	16 next   u32   叶子：右兄弟页号
	//	20 prev   u32   叶子：左兄弟页号
	//	24 crc    u32   整页校验（算之前把这 4 字节清零）
	//	28 槽位目录，每项 8 字节：off u16 | len u16 | val u32
	//	   val 在叶子里是行槽位号（RID），在内部节点里是子页号
	idxPageHeaderSize = 28
	idxSlotSize       = 8
	// idxMaxKeyLen 限制单键长度：一页至少要能放两个键，否则分裂就没意义了。
	idxMaxKeyLen = PageSize/2 - 64
)

var (
	errIdxMagic = fmt.Errorf("storage: 索引页魔数不对")
	errIdxCRC   = fmt.Errorf("storage: 索引页校验和不匹配（页损坏）")
)

const idxNoPage = ^uint32(0)

// idxSlotView 是槽位目录里的一项（只在页缓冲上解析，不复制键）。
type idxPage struct{ data []byte }

func (p idxPage) pageNo() uint32 { return binary.LittleEndian.Uint32(p.data[4:]) }
func (p idxPage) leaf() bool     { return binary.LittleEndian.Uint16(p.data[8:])&1 != 0 }
func (p idxPage) count() int     { return int(binary.LittleEndian.Uint16(p.data[10:])) }
func (p idxPage) freeLo() int    { return int(binary.LittleEndian.Uint16(p.data[12:])) }
func (p idxPage) freeHi() int    { return int(binary.LittleEndian.Uint16(p.data[14:])) }
func (p idxPage) next() uint32   { return binary.LittleEndian.Uint32(p.data[16:]) }
func (p idxPage) prev() uint32   { return binary.LittleEndian.Uint32(p.data[20:]) }

func (p idxPage) setCount(n int)   { binary.LittleEndian.PutUint16(p.data[10:], uint16(n)) }
func (p idxPage) setFreeLo(n int)  { binary.LittleEndian.PutUint16(p.data[12:], uint16(n)) }
func (p idxPage) setFreeHi(n int)  { binary.LittleEndian.PutUint16(p.data[14:], uint16(n)) }
func (p idxPage) setNext(v uint32) { binary.LittleEndian.PutUint32(p.data[16:], v) }
func (p idxPage) setPrev(v uint32) { binary.LittleEndian.PutUint32(p.data[20:], v) }
func (p idxPage) setLeaf(leaf bool) {
	if leaf {
		binary.LittleEndian.PutUint16(p.data[8:], 1)
	} else {
		binary.LittleEndian.PutUint16(p.data[8:], 0)
	}
}

func (p idxPage) slotAt(i int) (off, ln int, val uint32) {
	b := p.data[idxPageHeaderSize+i*idxSlotSize:]
	return int(binary.LittleEndian.Uint16(b[0:])), int(binary.LittleEndian.Uint16(b[2:])),
		binary.LittleEndian.Uint32(b[4:])
}

func (p idxPage) setSlot(i, off, ln int, val uint32) {
	b := p.data[idxPageHeaderSize+i*idxSlotSize:]
	binary.LittleEndian.PutUint16(b[0:], uint16(off))
	binary.LittleEndian.PutUint16(b[2:], uint16(ln))
	binary.LittleEndian.PutUint32(b[4:], val)
}

// key 返回第 i 个条目的键（页缓冲上的切片，只在持锁期间有效）。
func (p idxPage) key(i int) []byte {
	off, ln, _ := p.slotAt(i)
	if off+ln > len(p.data) || ln == 0 {
		return nil
	}
	return p.data[off : off+ln]
}

func (p idxPage) val(i int) uint32 {
	_, _, v := p.slotAt(i)
	return v
}

// initIdxPage 把一页初始化成空页。
func initIdxPage(data []byte, page uint32, leaf bool) {
	for i := range data {
		data[i] = 0
	}
	copy(data[0:4], idxPageMagic)
	binary.LittleEndian.PutUint32(data[4:], page)
	p := idxPage{data: data}
	p.setLeaf(leaf)
	p.setCount(0)
	p.setFreeLo(idxPageHeaderSize)
	p.setFreeHi(PageSize)
	p.setNext(idxNoPage)
	p.setPrev(idxNoPage)
	p.stampCRC()
}

// stampCRC 重算整页校验（crc 字段先清零）。只在「页要落盘之前」调用一次：
// 每次插入都算一遍 8KB 的 CRC 太贵（批量插入 20 万行会多出几百毫秒）。
func (p idxPage) stampCRC() {
	var zeros [4]byte
	sum := crc32.Update(0, crcTable, p.data[0:24])
	sum = crc32.Update(sum, crcTable, zeros[:])
	sum = crc32.Update(sum, crcTable, p.data[28:])
	binary.LittleEndian.PutUint32(p.data[24:], sum)
}

// verifyIdxPage 校验魔数与 CRC（页从文件读进池时调用一次）。
func verifyIdxPage(raw []byte) error {
	if len(raw) != PageSize {
		return errPageShort
	}
	zero := true
	for i := 0; i+8 <= PageSize; i += 8 {
		if binary.LittleEndian.Uint64(raw[i:]) != 0 {
			zero = false
			break
		}
	}
	if zero {
		return nil // 文件尾之后的新页
	}
	if string(raw[0:4]) != idxPageMagic {
		return errIdxMagic
	}
	want := binary.LittleEndian.Uint32(raw[24:28])
	var zeros [4]byte
	sum := crc32.Update(0, crcTable, raw[0:24])
	sum = crc32.Update(sum, crcTable, zeros[:])
	sum = crc32.Update(sum, crcTable, raw[28:])
	if sum != want {
		return errIdxCRC
	}
	return nil
}

// usedBytes 是「条目实际占的字节」（含槽位目录），用来判断紧凑一下是否够放。
func (p idxPage) usedBytes() int {
	n := 0
	for i := 0; i < p.count(); i++ {
		_, ln, _ := p.slotAt(i)
		n += ln + idxSlotSize
	}
	return n + idxPageHeaderSize
}

// compact 把元组区重新压实（删除留下的洞收回来），槽位顺序不变。
func (p idxPage) compact() {
	n := p.count()
	if n == 0 {
		p.setFreeLo(idxPageHeaderSize)
		p.setFreeHi(PageSize)
		return
	}
	buf := make([]byte, PageSize)
	copy(buf[0:idxPageHeaderSize], p.data[0:idxPageHeaderSize])
	hi := PageSize
	for i := 0; i < n; i++ {
		off, ln, val := p.slotAt(i)
		hi -= ln
		copy(buf[hi:], p.data[off:off+ln])
		b := buf[idxPageHeaderSize+i*idxSlotSize:]
		binary.LittleEndian.PutUint16(b[0:], uint16(hi))
		binary.LittleEndian.PutUint16(b[2:], uint16(ln))
		binary.LittleEndian.PutUint32(b[4:], val)
	}
	copy(p.data, buf)
	p.setFreeLo(idxPageHeaderSize + n*idxSlotSize)
	p.setFreeHi(hi)
}

// insertAt 在第 i 个位置插入 (key, val)。空间不够返回 false（调用方先 compact、
// 再不行就分裂）。
func (p idxPage) insertAt(i int, key []byte, val uint32) bool {
	if len(key) > idxMaxKeyLen {
		return false
	}
	n := p.count()
	need := idxSlotSize + len(key)
	if p.freeHi()-p.freeLo() < need {
		return false
	}
	off := p.freeHi() - len(key)
	copy(p.data[off:], key)
	base := idxPageHeaderSize
	// 槽位目录整体右移一格（8 字节 × 最多几百项，比重新编码整页便宜得多）
	copy(p.data[base+(i+1)*idxSlotSize:base+(n+1)*idxSlotSize],
		p.data[base+i*idxSlotSize:base+n*idxSlotSize])
	p.setSlot(i, off, len(key), val)
	p.setFreeHi(off)
	p.setFreeLo(p.freeLo() + idxSlotSize)
	p.setCount(n + 1)
	return true
}

// removeAt 删掉第 i 个条目（只挪槽位目录，元组字节留下当洞，等 compact）。
func (p idxPage) removeAt(i int) {
	n := p.count()
	if i < 0 || i >= n {
		return
	}
	copy(p.data[idxPageHeaderSize+i*idxSlotSize:],
		p.data[idxPageHeaderSize+(i+1)*idxSlotSize:idxPageHeaderSize+n*idxSlotSize])
	p.setCount(n - 1)
	p.setFreeLo(p.freeLo() - idxSlotSize)
}

// searchFirstGE 返回第一个键 >= key 的位置（二叉查找）。
func (p idxPage) searchFirstGE(key []byte) int {
	lo, hi := 0, p.count()
	for lo < hi {
		mid := (lo + hi) / 2
		if bytesCompare(p.key(mid), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// searchFirstGT 返回第一个键 > key 的位置。
func (p idxPage) searchFirstGT(key []byte) int {
	lo, hi := 0, p.count()
	for lo < hi {
		mid := (lo + hi) / 2
		if bytesCompare(p.key(mid), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func bytesCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// ---- 文件层 ----

// indexFile 是一个索引的页文件 + 缓冲池 + 树根信息。
type indexFile struct {
	pf   *pageFile
	pool *bufPool
	path string
	dir  string

	// ident 是这个索引的身份（"pk" / 索引名 / "uq:1,3"），写在头页里。
	// 文件名只带一个哈希，所以身份靠头页校验：对不上就重建。
	ident string

	gen    int64  // 写入时的表代次
	root   uint32 // 根页号
	height uint16
	count  int64
	hi     uint32 // 下一个可分配的页号

	headerDirty bool

	// nodes / emptyLeaves 是「重建判断」用的增量计数：遍历整棵树去数节点太重
	// （提交之后可能每轮都要问一次）。
	nodes       uint32
	emptyLeaves uint32

	// err 是粘性错误：读页/写页失败时记下来，由 checkpoint 或启动日志报出去。
	// （索引页损坏的正确处理是「整棵重建」，所以这里不逐次中断读路径。）
	err atomic.Pointer[error]

	fresh bool // 文件是刚建的（或身份/CRC 不匹配被清空），调用方决定是否重建
}

// indexFilePath 生成索引文件路径：文件名里带表 ID 和身份哈希，可读性靠日志补。
func indexFilePath(dir string, tableID int64, ident string) string {
	return filepath.Join(dir, "tables", fmt.Sprintf("i%06d.%08x.idx", tableID, crc32.ChecksumIEEE([]byte(ident))))
}

// openIndexFile 打开（或新建）一个索引文件。tableGen 是表文件已经落盘的代次：
// 头页里的 gen 与之不同，说明「索引比堆旧」，需要重建（fresh=true 的另一半情况）。
func openIndexFile(dir string, tableID int64, ident string, budget *memBudget, wantGen int64) (*indexFile, error) {
	path := indexFilePath(dir, tableID, ident)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	pf, err := openPageFile(path)
	if err != nil {
		return nil, err
	}
	f := &indexFile{pf: pf, path: path, dir: dir, ident: ident, gen: wantGen}
	f.pool = newBufPool(pf, budget, defaultPoolPages)
	f.pool.verify = verifyIdxPage
	f.pool.pack = func(fr *bufFrame) error {
		idxPage{data: fr.data}.stampCRC()
		return nil
	}
	f.hi = pf.pageCount()

	if pf.pageCount() == 0 {
		if err := f.initEmpty(); err != nil {
			return nil, err
		}
		return f, nil
	}
	if err := f.readHeader(); err != nil {
		// 头页坏了 / 身份不对：整个索引作废，清空重建
		if cerr := f.clear(); cerr != nil {
			return nil, cerr
		}
		return f, nil
	}
	// 头页里的代次必须等于 meta 里记的代次：不等就说明这一份索引
	// 可能是「另一个时刻」写的（checkpoint 崩在中间、元数据被回滚过）→ 重建。
	if f.gen != wantGen {
		f.fresh = true
		return f, nil
	}
	// 代次对得上，再把叶子链走一遍：只读索引页、不解码任何行。
	// 这一步比「重建索引」便宜一个量级（20 万行的表：读 4MB 索引 vs 解码 20 万行），
	// 但能抓到 CRC 坏页、断链、条目数和头页对不上 —— 全都退化成一件事：重建。
	if err := f.verifyIntegrity(); err != nil {
		f.fresh = true
	}
	return f, nil
}

// verifyIntegrity 沿叶子链走一遍，校验：链没断、键全局有序、条目数与头页一致。
func (f *indexFile) verifyIntegrity() error {
	page := f.root
	for depth := 0; ; depth++ {
		if depth > 32 {
			return fmt.Errorf("storage: 索引树太深（可能成环）")
		}
		var leaf bool
		var left uint32
		if err := f.readIdx(page, func(p idxPage) error {
			leaf = p.leaf()
			if !leaf {
				if p.count() > 0 {
					left = p.val(0)
				} else {
					left = p.next()
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if leaf {
			break
		}
		if left == idxNoPage {
			return fmt.Errorf("storage: 索引内部节点没有孩子")
		}
		page = left
	}

	total := int64(0)
	prevPage := idxNoPage
	var prevKey []byte
	for page != idxNoPage {
		var (
			cnt   int
			nx    uint32
			broke error
		)
		err := f.readIdx(page, func(p idxPage) error {
			cnt, nx = p.count(), p.next()
			if prevPage != idxNoPage && p.prev() != prevPage {
				broke = fmt.Errorf("storage: 索引叶子链的 prev 断了（第 %d 页）", page)
				return nil
			}
			for i := 0; i < cnt; i++ {
				k := p.key(i)
				if len(prevKey) > 0 && bytesCompare(k, prevKey) < 0 {
					broke = fmt.Errorf("storage: 索引键不有序（第 %d 页第 %d 条）", page, i)
					return nil
				}
				prevKey = append(prevKey[:0], k...)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if broke != nil {
			return broke
		}
		total += int64(cnt)
		prevPage = page
		page = nx
	}
	if total != f.count {
		return fmt.Errorf("storage: 索引条目数 %d，头页记的是 %d", total, f.count)
	}
	return nil
}

// initEmpty 建一个只有空根叶的索引文件。
func (f *indexFile) initEmpty() error {
	if err := f.pf.truncate(0); err != nil {
		return err
	}
	f.hi = 1
	// 根 = 页 1，空叶子
	root := uint32(1)
	data := make([]byte, PageSize)
	initIdxPage(data, root, true)
	if err := f.pf.writePage(root, data); err != nil {
		return err
	}
	f.hi = root + 1
	f.pf.pages = f.hi
	f.root, f.height, f.count = root, 1, 0
	f.nodes, f.emptyLeaves = 1, 0
	// 「还没被 checkpoint 写过」的标记：meta 里没有这个索引的记录时，
	// 期望值也是 -1，刚建的空索引因此天然有效（不用白重建一次）。
	f.gen = -1
	f.headerDirty = true
	return f.writeHeader()
}

// clear 把文件清空并重新初始化（索引作废时用）。
func (f *indexFile) clear() error {
	f.pool.releaseAll()
	f.fresh = true
	return f.initEmpty()
}

// readHeader 读页 0 的头信息。
func (f *indexFile) readHeader() error {
	buf := make([]byte, PageSize)
	if _, err := f.pf.f.ReadAt(buf, 0); err != nil {
		return err
	}
	if string(buf[0:8]) != idxHeaderMagic {
		return fmt.Errorf("storage: 索引头页魔数不对（%s）", f.path)
	}
	if v := binary.LittleEndian.Uint16(buf[8:]); v != idxPageVersion {
		return fmt.Errorf("storage: 索引格式版本 %d 不认识", v)
	}
	identLen := int(binary.LittleEndian.Uint16(buf[12:]))
	if identLen > 200 || 64+identLen > PageSize {
		return fmt.Errorf("storage: 索引头页身份字段越界")
	}
	if string(buf[64:64+identLen]) != f.ident {
		return fmt.Errorf("storage: 索引文件身份不匹配（想要 %q）", f.ident)
	}
	want := binary.LittleEndian.Uint32(buf[48:52])
	var zeros [4]byte
	sum := crc32.Update(0, crcTable, buf[0:48])
	sum = crc32.Update(sum, crcTable, zeros[:])
	sum = crc32.Update(sum, crcTable, buf[52:])
	if sum != want {
		return fmt.Errorf("storage: 索引头页校验和不匹配")
	}
	f.root = binary.LittleEndian.Uint32(buf[16:])
	f.height = binary.LittleEndian.Uint16(buf[20:])
	f.count = int64(binary.LittleEndian.Uint64(buf[24:]))
	f.gen = int64(binary.LittleEndian.Uint64(buf[32:]))
	f.hi = binary.LittleEndian.Uint32(buf[40:])
	if f.root == 0 || f.root >= f.hi {
		return fmt.Errorf("storage: 索引头页的根页号 %d 越界", f.root)
	}
	if f.height == 0 {
		return fmt.Errorf("storage: 索引头页的树高为 0")
	}
	if f.hi < f.pf.pageCount() {
		f.hi = f.pf.pageCount()
	}
	return nil
}

// writeHeader 写页 0（含 CRC），并 fsync。checkpoint 与建树之后各调一次。
func (f *indexFile) writeHeader() error {
	buf := make([]byte, PageSize)
	copy(buf[0:8], idxHeaderMagic)
	binary.LittleEndian.PutUint16(buf[8:], idxPageVersion)
	ident := f.ident
	if len(ident) > 200 {
		ident = ident[:200]
	}
	binary.LittleEndian.PutUint16(buf[12:], uint16(len(ident)))
	copy(buf[64:], ident)
	binary.LittleEndian.PutUint32(buf[16:], f.root)
	binary.LittleEndian.PutUint16(buf[20:], f.height)
	binary.LittleEndian.PutUint64(buf[24:], uint64(f.count))
	binary.LittleEndian.PutUint64(buf[32:], uint64(f.gen))
	binary.LittleEndian.PutUint32(buf[40:], f.hi)
	var zeros [4]byte
	sum := crc32.Update(0, crcTable, buf[0:48])
	sum = crc32.Update(sum, crcTable, zeros[:])
	sum = crc32.Update(sum, crcTable, buf[52:])
	binary.LittleEndian.PutUint32(buf[48:], sum)
	if err := f.pf.writePage(0, buf); err != nil {
		return err
	}
	f.headerDirty = false
	return nil
}

// allocPage 分配一个新页号（只追加，不回收：索引可以整体重建）。
// 不动 pf.pages —— 那一页还没写过，写的时候 writePage 会自己把文件撑长。
func (f *indexFile) allocPage() uint32 {
	p := f.hi
	f.hi++
	f.nodes++
	return p
}

// frame 取一页并钉住；调用方负责 unpin。
func (f *indexFile) frame(page uint32) (*bufFrame, error) {
	return f.pool.fetch(page)
}

// readIdx 以只读视图访问某页（内部自动 unpin）。
func (f *indexFile) readIdx(page uint32, fn func(p idxPage) error) error {
	fr, err := f.pool.fetch(page)
	if err != nil {
		return err
	}
	defer f.pool.unpin(fr, false)
	fr.latch.RLock()
	defer fr.latch.RUnlock()
	return fn(idxPage{data: fr.data})
}

// writeIdx 以可写视图访问某页；fn 返回后整页标脏（CRC 由 pool.pack 在落盘前补）。
func (f *indexFile) writeIdx(page uint32, fn func(p idxPage) error) error {
	fr, err := f.pool.fetch(page)
	if err != nil {
		return err
	}
	fr.latch.Lock()
	err = fn(idxPage{data: fr.data})
	fr.latch.Unlock()
	f.pool.unpin(fr, err == nil)
	return err
}

// setErr 记录粘性错误（第一次发生时的那个最有用）。
func (f *indexFile) setErr(err error) {
	if err == nil {
		return
	}
	if f.err.Load() == nil {
		f.err.Store(&err)
	}
}

func (f *indexFile) takeErr() error {
	if p := f.err.Load(); p != nil {
		return *p
	}
	return nil
}

// flush 把脏页刷盘、写头页、fsync。checkpoint 调用；gen 是这一轮落盘的代次。
func (f *indexFile) flush(gen int64) (int, error) {
	n, err := f.pool.flushAll()
	if err != nil {
		f.setErr(err)
		return n, err
	}
	f.gen = gen
	f.headerDirty = true
	if err := f.writeHeader(); err != nil {
		f.setErr(err)
		return n, err
	}
	if err := f.pf.sync(); err != nil {
		f.setErr(err)
		return n, err
	}
	return n, f.takeErr()
}

// close 关文件：**不刷脏页**，直接丢。
//
// 索引的正确性只依赖「头页代次 == 表代次」。关库时刷页但不推进代次，会让
// 「页内容比代次新」——重启后要么白重建（代次没变会被当成有效），要么把
// 半写的页当成有效索引。所以关闭就是丢掉，下次打开按代次判断。
func (f *indexFile) close() error {
	f.pool.releaseDiscard()
	if f.pool.budget != nil {
		f.pool.budget.unregister(f.pool)
	}
	return f.pf.close()
}

// reset 清空整棵树（TRUNCATE / 重建前）。
func (f *indexFile) reset() error {
	f.pool.releaseAll()
	return f.initEmpty()
}

// pageStats 返回 (页数, 树高, 条目数)，给监控与 EXPLAIN 用。
func (f *indexFile) pageStats() (pages int, height int, entries int64) {
	if f == nil {
		return 0, 0, 0
	}
	return int(f.hi) - 1, int(f.height), f.count
}
