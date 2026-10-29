package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// slotted page：8 KB 一页，页头定长、槽位目录从前往后长、元组数据从后往前长。
//
//	0   magic "GDBP"
//	4   pageID   uint32
//	8   slotCount uint16
//	10  freeLo   uint16   // 目录区末尾（数据区可用的下界）
//	12  freeHi   uint16   // 元组区起点（可用的上界）
//	14  flags    uint16   // 预留
//	16  lsn      uint64   // 最近一次修改它的 WAL 位置
//	24  crc      uint32   // 整页校验（算之前先把这 4 字节清零）
//	28  slot[0]  {off uint16, len uint16, gid uint32}  // len == 0 表示这个槽是空的
//	    ...
//	    freeHi: 元组区从这里往前长
//
// 为什么用「槽位目录 + 变长元组」而不是定长行：行里有 TEXT/JSON，长度差一个数量级，
// 定长会浪费一半空间；槽位目录让「删一行」只是把 len 置 0，页内不用搬数据
// （真正的整理留给第 3 期的 VACUUM）。
const (
	// PageSize 是一页的字节数，和 PostgreSQL 默认保持一致。
	PageSize = 8192

	pageMagic      = "GDBP"
	pageHeaderSize = 28
	slotEntrySize  = 8 // off(2) + len(2) + gid(4)：带全局槽位号，重开表时能把行目录重建出来

	// MaxTupleSize 是单条元组的上限：一页放不下两行就没法「一页至少存两行」，
	// 页内整理的自由度也会消失。超了直接报错，而不是悄悄写坏页。
	// （第 1 期不做溢出页链，真要存大 TEXT 再单独加。）
	MaxTupleSize = PageSize/2 - 64

	// pageNoSlot 表示「页里没有这个槽位」。
	pageNoSlot = -1
)

var (
	errPageShort      = errors.New("storage: 页长度不足")
	errPageMagic      = errors.New("storage: 页魔数不对")
	errPageCRC        = errors.New("storage: 页校验和不匹配（页损坏）")
	errTupleTooLarge  = errors.New("storage: 单行超过一页的一半，请拆列或缩短")
	errPageFull       = errors.New("storage: 页已满")
	errSlotOutOfRange = errors.New("storage: 槽位号越界")
)

// pageSlot 是槽位目录里的一项。
type pageSlot struct {
	off uint16
	len uint16 // 0 表示空槽（行被删了），槽位号仍可复用
	// gid 是「全局槽位号」——也就是索引里存的 RID。页内位置（page, slot）会随
	// 整理/搬迁变化，但 RID 永远不变，所以必须落在页里，重开表时才能把
	// 「RID → 页内位置」这张目录重建出来。
	gid uint32
}

// page 是一张表的某一页在内存里的镜像。
type page struct {
	id    uint32
	lsn   uint64
	buf   []byte // 定长 PageSize，元组数据就在里面原地读写
	slots []pageSlot
	freeLo int
	freeHi int
}

func newPage(id uint32) *page {
	p := &page{
		id:     id,
		buf:    make([]byte, PageSize),
		freeLo: pageHeaderSize,
		freeHi: PageSize,
	}
	return p
}

// decodePage 把磁盘上的字节解析成 page（校验魔数与 CRC）。
func decodePage(raw []byte) (*page, error) {
	if len(raw) != PageSize {
		return nil, errPageShort
	}
	if string(raw[0:4]) != pageMagic {
		return nil, errPageMagic
	}
	want := binary.LittleEndian.Uint32(raw[24:28])
	tmp := make([]byte, PageSize)
	copy(tmp, raw)
	for i := 24; i < 28; i++ {
		tmp[i] = 0
	}
	if crc32.Checksum(tmp, crcTable) != want {
		return nil, errPageCRC
	}

	p := &page{
		id:     binary.LittleEndian.Uint32(raw[4:8]),
		lsn:    binary.LittleEndian.Uint64(raw[16:24]),
		buf:    make([]byte, PageSize),
		freeLo: int(binary.LittleEndian.Uint16(raw[10:12])),
		freeHi: int(binary.LittleEndian.Uint16(raw[12:14])),
	}
	copy(p.buf, raw)
	n := int(binary.LittleEndian.Uint16(raw[8:10]))
	p.slots = make([]pageSlot, n)
	for i := 0; i < n; i++ {
		off := pageHeaderSize + i*slotEntrySize
		p.slots[i] = pageSlot{
			off: binary.LittleEndian.Uint16(p.buf[off : off+2]),
			len: binary.LittleEndian.Uint16(p.buf[off+2 : off+4]),
			gid: binary.LittleEndian.Uint32(p.buf[off+4 : off+8]),
		}
	}
	if err := p.sanity(); err != nil {
		return nil, err
	}
	return p, nil
}

// decodeOrInitPage：文件尾之后读上来的是全零缓冲，那不是「坏页」而是「还没写过」，
// 按新页初始化。真正校验失败（非零但不是合法页）才报错。
func decodeOrInitPage(raw []byte, id uint32) (*page, error) {
	if len(raw) != PageSize {
		return nil, errPageShort
	}
	zero := true
	for _, b := range raw {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return newPage(id), nil
	}
	return decodePage(raw)
}

// sanity 是「防坏页」的最后一道闸：解析出来的边界必须自洽，否则后面的
// 插入/读取会越界。宁可开库时报错，也不要跑到一半 panic。
func (p *page) sanity() error {
	if p.freeLo < pageHeaderSize || p.freeHi > PageSize || p.freeLo > p.freeHi {
		return fmt.Errorf("storage: 页 %d 的空闲指针不自洽（lo=%d hi=%d）", p.id, p.freeLo, p.freeHi)
	}
	if pageHeaderSize+len(p.slots)*slotEntrySize > p.freeLo {
		return fmt.Errorf("storage: 页 %d 的槽位目录越界", p.id)
	}
	for i, s := range p.slots {
		if s.len == 0 {
			continue
		}
		end := int(s.off) + int(s.len)
		if int(s.off) < p.freeHi || end > PageSize {
			return fmt.Errorf("storage: 页 %d 的槽位 %d 越界（off=%d len=%d）", p.id, i, s.off, s.len)
		}
	}
	return nil
}

// encode 把页序列化成可直接落盘的字节（此时才算 CRC）。
func (p *page) encode() []byte {
	binary.LittleEndian.PutUint32(p.buf[0:4], 0)
	copy(p.buf[0:4], pageMagic)
	binary.LittleEndian.PutUint32(p.buf[4:8], p.id)
	binary.LittleEndian.PutUint16(p.buf[8:10], uint16(len(p.slots)))
	binary.LittleEndian.PutUint16(p.buf[10:12], uint16(p.freeLo))
	binary.LittleEndian.PutUint16(p.buf[12:14], uint16(p.freeHi))
	binary.LittleEndian.PutUint16(p.buf[14:16], 0)
	binary.LittleEndian.PutUint64(p.buf[16:24], p.lsn)
	for i, s := range p.slots {
		off := pageHeaderSize + i*slotEntrySize
		binary.LittleEndian.PutUint16(p.buf[off:off+2], s.off)
		binary.LittleEndian.PutUint16(p.buf[off+2:off+4], s.len)
		binary.LittleEndian.PutUint32(p.buf[off+4:off+8], s.gid)
	}
	binary.LittleEndian.PutUint32(p.buf[24:28], 0)
	sum := crc32.Checksum(p.buf, crcTable)
	binary.LittleEndian.PutUint32(p.buf[24:28], sum)

	out := make([]byte, PageSize)
	copy(out, p.buf)
	return out
}

// freeBytes 是这一页还能放下多少字节的元组（不含目录需要的 4 字节）。
func (p *page) freeBytes() int {
	return p.freeHi - p.freeLo
}

// liveCount 是非空槽位的数量。
func (p *page) liveCount() int {
	n := 0
	for _, s := range p.slots {
		if s.len > 0 {
			n++
		}
	}
	return n
}

// insert 往页里放一条元组，返回页内槽位号。优先复用空槽（页内不整理，
// 但槽位号可以复用，避免目录无限增长）。
func (p *page) insert(tuple []byte, gid uint32) (int, error) {
	if len(tuple) == 0 {
		return pageNoSlot, errTupleTooLarge
	}
	if len(tuple) > MaxTupleSize {
		return pageNoSlot, errTupleTooLarge
	}
	reuse := pageNoSlot
	for i, s := range p.slots {
		if s.len == 0 {
			reuse = i
			break
		}
	}
	need := len(tuple)
	if reuse == pageNoSlot {
		need += slotEntrySize // 新槽位要多占一份目录
	}
	if need > p.freeBytes() {
		return pageNoSlot, errPageFull
	}

	p.freeHi -= len(tuple)
	copy(p.buf[p.freeHi:], tuple)
	if reuse != pageNoSlot {
		p.slots[reuse] = pageSlot{off: uint16(p.freeHi), len: uint16(len(tuple)), gid: gid}
		return reuse, nil
	}
	p.slots = append(p.slots, pageSlot{off: uint16(p.freeHi), len: uint16(len(tuple)), gid: gid})
	p.freeLo += slotEntrySize
	return len(p.slots) - 1, nil
}

// get 读一条元组（返回副本，调用方随便改）。
func (p *page) get(slot int) ([]byte, bool) {
	if slot < 0 || slot >= len(p.slots) {
		return nil, false
	}
	s := p.slots[slot]
	if s.len == 0 {
		return nil, false
	}
	out := make([]byte, s.len)
	copy(out, p.buf[int(s.off):int(s.off)+int(s.len)])
	return out, true
}

// del 删除一行的元组：只把长度置 0，页内不搬数据（整理留给 VACUUM）。
func (p *page) del(slot int) error {
	if slot < 0 || slot >= len(p.slots) {
		return errSlotOutOfRange
	}
	if p.slots[slot].len == 0 {
		return nil
	}
	p.slots[slot] = pageSlot{}
	return nil
}

// compact 把活着的元组搬到页尾紧排，回收被删行留下的空洞。
//
// 槽位目录的顺序和长度都不变（槽位号是 RID 的一部分，动了就等于换了行号），
// 只有 off 会变 —— 这也正是「槽位目录 + 变长元组」的意义。
//
// 先搬到临时缓冲区再整体写回：直接原地搬，A 的目标位置可能压在还没搬的 B 上。
func (p *page) compact() {
	tmp := make([]byte, PageSize)
	dst := PageSize
	for i := range p.slots {
		s := &p.slots[i]
		if s.len == 0 {
			continue
		}
		dst -= int(s.len)
		copy(tmp[dst:], p.buf[int(s.off):int(s.off)+int(s.len)])
		s.off = uint16(dst)
	}
	copy(p.buf[dst:], tmp[dst:PageSize])
	p.freeHi = dst
}

// insertOrCompact 先直接插，页满就整理一次再插。表格数据「删一批、插一批」
// 的时候全靠它把空洞用起来，否则页会越开越多。
func (p *page) insertOrCompact(tuple []byte, gid uint32) (int, error) {
	slot, err := p.insert(tuple, gid)
	if err == nil || err != errPageFull {
		return slot, err
	}
	before := p.freeBytes()
	p.compact()
	if p.freeBytes() == before {
		return pageNoSlot, errPageFull
	}
	return p.insert(tuple, gid)
}
