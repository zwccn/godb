package storage

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"godb/types"
)

// 只读浏览：把「页」这一层打开给人看（页地图 + 页检查器 + 页内二进制/文本）。
//
// 性能上的关键决定：**直接读文件、不进缓冲池**。
// 走 tp.fetch 会把整张表的页都摸进池子，把别人的热页挤掉 —— 一个诊断功能不该让
// 正常查询变慢。代价是看不到还没落盘的脏页（看到的是「磁盘上的样子」），
// 而这正好是排查布局问题时想看的那一份；界面上要写明。
//
// 另外这些接口都是「点开才算」的：一次 O(页数) 的顺序读（1450 页 ≈ 11 MB），
// 不跟着监控轮询跑。

// pageMapMaxPages 是单次扫描的页数上限：再大就分页取，避免一次把几百 MB 读进来。
const pageMapMaxPages = 4096

// PageInfo 是页地图里的一格。
type PageInfo struct {
	No    uint32 `json:"no"`
	Live  int    `json:"live"`  // 存活元组 / 条目数
	Slots int    `json:"slots"` // 槽位总数（含空槽）
	Used  int    `json:"used"`  // 元组占的字节
	Free  int    `json:"free"`  // 真实空闲
	Hole  int    `json:"hole"`  // 洞（只有数据页有：整理一次才能变成 Free）
	// Role 区分索引文件的第 0 页：它是「头页」（记 root / 高度 / 条目数 / 代次），
	// 不是 B+Tree 的节点。不分开的话它会被当成一个空叶子，看着像「树里有个空页」。
	Role string `json:"role,omitempty"` // header | leaf | inner
	Bad  bool   `json:"bad,omitempty"`
}

// PageMap 是一次页扫描的结果（可能只是前一段，Next 非 0 表示还有）。
type PageMap struct {
	Table string     `json:"table"`
	Kind  string     `json:"kind"`            // data | index
	Index string     `json:"index,omitempty"` // kind=index 时的索引名
	Total int        `json:"total"`
	From  int        `json:"from"`
	Next  int        `json:"next"`
	Bytes int64      `json:"bytes"` // 这次实际读了多少字节（给人看代价）
	Pages []PageInfo `json:"pages"`
	Note  string     `json:"note,omitempty"`
}

// PageDetail 是一页的完整内容：页头 + 槽位 + 元组/条目 + 原始字节 + 文本串。
type PageDetail struct {
	Table string           `json:"table"`
	Kind  string           `json:"kind"`
	Index string           `json:"index,omitempty"`
	No    uint32           `json:"no"`
	Total int              `json:"total"`
	CRC   string           `json:"crc"` // ok | bad | zero（页还没写过）
	Raw   string           `json:"raw"` // base64，十六进制视图用
	Data  *DataPageView    `json:"data,omitempty"`
	Idx   *IndexPageView   `json:"indexPage,omitempty"`
	Hdr   *IndexHeaderView `json:"indexHeader,omitempty"`
	Texts []TextRun        `json:"texts"`
}

// DataPageView 是数据页的可读结构。
type DataPageView struct {
	Magic   string      `json:"magic"`
	PageID  uint32      `json:"pageId"`
	Slots   int         `json:"slots"`
	FreeLo  int         `json:"freeLo"`
	FreeHi  int         `json:"freeHi"`
	LSN     uint64      `json:"lsn"`
	Entries []DataEntry `json:"entries"`
}

// DataEntry 是页里的一条元组（列值已解码，长文本截断）。
type DataEntry struct {
	No   int      `json:"no"`
	Off  int      `json:"off"`
	Len  int      `json:"len"`
	GID  uint32   `json:"gid"` // 全局槽位号 = 索引里存的 RID
	Live bool     `json:"live"`
	Cols []string `json:"cols"`
	Note string   `json:"note,omitempty"`
}

// IndexPageView 是索引页的可读结构。
type IndexPageView struct {
	Leaf    bool         `json:"leaf"`
	Role    string       `json:"role"`
	Count   int          `json:"count"`
	Next    int64        `json:"next"` // -1 表示没有
	Prev    int64        `json:"prev"`
	Entries []IndexEntry `json:"entries"`
}

// IndexHeaderView 是索引文件的头页（第 0 页）：树有多高、根在哪、一共多少条目。
type IndexHeaderView struct {
	Root    int64  `json:"root"`
	Height  int    `json:"height"`
	Count   int64  `json:"count"`
	Gen     int64  `json:"gen"`
	Ident   string `json:"ident"`
	Version int    `json:"version"`
}

// IndexEntry 是索引页里的一项：键 → RID（叶子）或子页号（内部节点）。
type IndexEntry struct {
	I    int      `json:"i"`
	Key  string   `json:"key"`  // 键的文本形式（不可打印时给十六进制）
	Vals []string `json:"vals"` // 解码后的键列值
	Val  uint32   `json:"val"`  // 叶子：RID；内部节点：子页号
	Len  int      `json:"len"`  // 键字节数
}

// TextRun 是页里一段「看起来是文本」的字节（页检查器的文本视图用）。
type TextRun struct {
	Off  int    `json:"off"`
	Len  int    `json:"len"`
	Text string `json:"text"`
}

// rawPageFile 是「直接读文件」的句柄：不进缓冲池，只读。
type rawPageFile struct {
	f     *os.File
	pages int
}

func openRawPages(path string) (*rawPageFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rawPageFile{f: f, pages: int(st.Size() / PageSize)}, nil
}

func (r *rawPageFile) close() { r.f.Close() }

// readChunk 读 [from, from+n) 这些页；越界的那部分给全零页（和页文件的语义一致）。
func (r *rawPageFile) readChunk(from uint32, n int) ([][]byte, int, error) {
	out := make([][]byte, n)
	avail := 0
	if int(from) < r.pages {
		avail = r.pages - int(from)
		if avail > n {
			avail = n
		}
	}
	var buf []byte
	if avail > 0 {
		buf = make([]byte, avail*PageSize)
		if _, err := r.f.ReadAt(buf, int64(from)*PageSize); err != nil {
			return nil, 0, err
		}
	}
	for i := 0; i < n; i++ {
		if i < avail {
			out[i] = buf[i*PageSize : (i+1)*PageSize]
			continue
		}
		out[i] = make([]byte, PageSize) // 文件尾之后：全零
	}
	return out, avail * PageSize, nil
}

// browseTarget 解析「看哪张表的哪个文件」。
type browseTarget struct {
	table   string // 展示用的表名
	path    string
	kind    string // data | index
	index   string // 索引名（kind=index）
	idxCols int    // 索引列数（解键用）
	loaded  bool
}

func (s *Store) browseTarget(table, kind, index string) (browseTarget, error) {
	key := cleanName(table)
	if s.Cat == nil {
		return browseTarget{}, fmt.Errorf("catalog 还没就绪")
	}
	t, ok := s.Cat.Get(table)
	if !ok {
		return browseTarget{}, fmt.Errorf("relation %q does not exist", table)
	}
	bt := browseTarget{table: t.Name, kind: "data"}
	if kind == "index" {
		name := index
		bt.kind, bt.index = "index", name
		found := false
		for _, ix := range t.Indexes {
			if cleanName(ix.Name) == cleanName(name) || ix.Name == name {
				bt.idxCols = len(ix.Columns)
				bt.index = ix.Name
				found = true
				break
			}
		}
		if !found {
			return browseTarget{}, fmt.Errorf("索引 %q 不存在", index)
		}
		bt.path = indexFilePath(s.Dir, t.ID, "ix:"+cleanName(bt.index))
	} else {
		bt.path = tablePagePath(s.Dir, t.ID)
	}
	s.loadMu.RLock()
	bt.loaded = s.loaded[key]
	s.loadMu.RUnlock()
	return bt, nil
}

// PageMap 扫一段页，给出每页的占用情况。
//
// from/limit 是分页参数：limit 默认 1000、上限 pageMapMaxPages，返回的 Next 为 0
// 表示已经到文件末尾。扫描是顺序读 + 只解页头与槽位目录，不解元组。
func (s *Store) PageMap(table, kind, index string, from, limit int) (PageMap, error) {
	bt, err := s.browseTarget(table, kind, index)
	if err != nil {
		return PageMap{}, err
	}
	if from < 0 {
		from = 0
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > pageMapMaxPages {
		limit = pageMapMaxPages
	}
	rf, err := openRawPages(bt.path)
	if err != nil {
		// 还没落过盘的表：没有文件就是空地图，不算错误
		if os.IsNotExist(err) {
			return PageMap{Table: bt.table, Kind: bt.kind, Index: bt.index, Note: "这张表还没落过盘（数据只在 WAL 里）"}, nil
		}
		return PageMap{}, err
	}
	defer rf.close()

	pm := PageMap{Table: bt.table, Kind: bt.kind, Index: bt.index, Total: rf.pages, From: from}
	if rf.pages == 0 {
		pm.Note = "文件还是空的（还没有页落盘）"
		return pm, nil
	}
	end := from + limit
	if end > rf.pages {
		end = rf.pages
	}
	out := make([]PageInfo, 0, end-from)
	for start := from; start < end; start += 256 {
		n := end - start
		if n > 256 {
			n = 256
		}
		chunk, got, err := rf.readChunk(uint32(start), n)
		if err != nil {
			return pm, err
		}
		pm.Bytes += int64(got)
		for i := 0; i < n; i++ {
			no := uint32(start + i)
			if bt.kind == "index" {
				out = append(out, parseIdxPageInfo(chunk[i], no))
			} else {
				out = append(out, parseDataPageInfo(chunk[i], no))
			}
		}
	}
	pm.Pages = out
	if end < rf.pages {
		pm.Next = end
	}
	return pm, nil
}

// parseDataPageInfo 只读页头与槽位目录，不碰元组内容。
func parseDataPageInfo(b []byte, no uint32) PageInfo {
	pi := PageInfo{No: no}
	if len(b) < pageHeaderSize || string(b[0:4]) != pageMagic {
		pi.Bad = true
		return pi
	}
	slots := int(binary.LittleEndian.Uint16(b[8:]))
	freeLo := int(binary.LittleEndian.Uint16(b[10:]))
	freeHi := int(binary.LittleEndian.Uint16(b[12:]))
	pi.Slots = slots
	pi.Free = freeHi - freeLo
	liveBytes := 0
	for i := 0; i < slots; i++ {
		off := pageHeaderSize + i*slotEntrySize
		if off+slotEntrySize > len(b) {
			break
		}
		ln := int(binary.LittleEndian.Uint16(b[off+2:]))
		if ln == 0 {
			continue
		}
		pi.Live++
		liveBytes += ln
	}
	pi.Used = liveBytes
	// 和 page.holeBytes() 同一口径：整理之后能用的空闲 - 现在的真实空闲
	freeAfter := PageSize - pageHeaderSize - slots*slotEntrySize - liveBytes
	if h := freeAfter - pi.Free; h > 0 {
		pi.Hole = h
	}
	return pi
}

// parseIdxPageInfo 同上，索引页没有「洞」这个概念（删键只摘槽位）。
func parseIdxPageInfo(b []byte, no uint32) PageInfo {
	pi := PageInfo{No: no}
	if len(b) < idxPageHeaderSize || string(b[0:4]) != idxPageMagic {
		pi.Bad = true
		return pi
	}
	if no == 0 {
		pi.Role = "header"
		if string(b[0:8]) != idxHeaderMagic {
			pi.Bad = true
		}
		return pi
	}
	p := idxPage{data: b}
	pi.Slots = p.count()
	pi.Live = p.count()
	pi.Free = p.freeHi() - p.freeLo()
	pi.Used = p.freeLo() - idxPageHeaderSize + (PageSize - p.freeHi())
	if p.leaf() {
		pi.Role = "leaf"
	} else {
		pi.Role = "inner"
	}
	return pi
}

// PageDetail 读一页并解析出完整结构（含原始字节与文本串）。
func (s *Store) PageDetail(table, kind, index string, no uint32) (*PageDetail, error) {
	bt, err := s.browseTarget(table, kind, index)
	if err != nil {
		return nil, err
	}
	rf, err := openRawPages(bt.path)
	if err != nil {
		return nil, err
	}
	defer rf.close()
	if int(no) >= rf.pages {
		return nil, fmt.Errorf("页号 %d 超出范围（一共 %d 页）", no, rf.pages)
	}
	chunk, _, err := rf.readChunk(no, 1)
	if err != nil {
		return nil, err
	}
	b := chunk[0]
	d := &PageDetail{
		Table: bt.table, Kind: bt.kind, Index: bt.index,
		No: no, Total: rf.pages,
		Raw: base64.StdEncoding.EncodeToString(b),
	}
	d.Texts = extractTextRuns(b, 200)

	if bt.kind == "index" {
		if no == 0 && string(b[0:8]) == idxHeaderMagic {
			d.CRC = "ok"
			d.Hdr = &IndexHeaderView{
				Root:    int64(binary.LittleEndian.Uint32(b[16:])),
				Height:  int(binary.LittleEndian.Uint16(b[20:])),
				Count:   int64(binary.LittleEndian.Uint64(b[24:])),
				Gen:     int64(binary.LittleEndian.Uint64(b[32:])),
				Version: int(binary.LittleEndian.Uint16(b[8:])),
			}
			if n := int(binary.LittleEndian.Uint16(b[12:])); n > 0 && 64+n <= len(b) {
				d.Hdr.Ident = truncate(string(b[64:64+n]), 60)
			}
			return d, nil
		}
		if string(b[0:4]) != idxPageMagic {
			d.CRC = "zero"
			return d, nil
		}
		p := idxPage{data: b}
		v := &IndexPageView{
			Leaf:  p.leaf(),
			Role:  "leaf",
			Count: p.count(),
			Next:  idxPageOrNone(p.next()),
			Prev:  idxPageOrNone(p.prev()),
		}
		for i := 0; i < v.Count && i < 400; i++ {
			kb := p.key(i)
			e := IndexEntry{I: i, Val: p.val(i), Len: len(kb), Key: printableKey(kb)}
			if bt.idxCols > 0 {
				if vals, ok := DecodeOrderKey(string(kb), bt.idxCols); ok {
					for _, vv := range vals {
						e.Vals = append(e.Vals, formatValue(vv))
					}
				}
			}
			v.Entries = append(v.Entries, e)
		}
		if !v.Leaf {
			v.Role = "inner"
		}
		d.Idx = v
		d.CRC = "ok"
		return d, nil
	}

	if string(b[0:4]) != pageMagic {
		d.CRC = "zero"
		return d, nil
	}
	v := &DataPageView{
		Magic:  string(b[0:4]),
		PageID: binary.LittleEndian.Uint32(b[4:]),
		Slots:  int(binary.LittleEndian.Uint16(b[8:])),
		FreeLo: int(binary.LittleEndian.Uint16(b[10:])),
		FreeHi: int(binary.LittleEndian.Uint16(b[12:])),
		LSN:    binary.LittleEndian.Uint64(b[16:]),
	}
	for i := 0; i < v.Slots && i < 400; i++ {
		off := pageHeaderSize + i*slotEntrySize
		if off+slotEntrySize > len(b) {
			break
		}
		e := DataEntry{
			No:  i,
			Off: int(binary.LittleEndian.Uint16(b[off:])),
			Len: int(binary.LittleEndian.Uint16(b[off+2:])),
			GID: binary.LittleEndian.Uint32(b[off+4:]),
		}
		e.Live = e.Len > 0
		if e.Live {
			e.Cols, e.Note = decodeTupleForDisplay(b, e.Off, e.Len)
		}
		v.Entries = append(v.Entries, e)
	}
	d.Data = v
	d.CRC = "ok"
	return d, nil
}

func idxPageOrNone(v uint32) int64 {
	if v == idxNoPage {
		return -1
	}
	return int64(v)
}

// decodeTupleForDisplay 解一条元组给界面看：列值转文本、长文本截断。
//
// 不复用 decodeRowTupleInto：那条路径要 tp（读大字段）且会按当前表结构做类型对齐，
// 这里要的是「页里到底写了什么」，所以原样解、不 Cast，解不动就记一句说明。
func decodeTupleForDisplay(page []byte, off, ln int) ([]string, string) {
	if off < 0 || ln < 4 || off+ln > len(page) {
		return nil, "元组越界"
	}
	tuple := page[off : off+ln]
	n := int(binary.LittleEndian.Uint32(tuple))
	out := make([]string, 0, n)
	pos := 4
	note := ""
	for i := 0; i < n; i++ {
		if pos >= len(tuple) {
			note = fmt.Sprintf("第 %d 列之后数据不完整", i)
			break
		}
		if tuple[pos] == tagLOB {
			// 大字段指针：不进 .lob 读正文（那是另一次随机读），只报偏移与长度
			if pos+lobRefSize > len(tuple) {
				out = append(out, "[大字段指针不完整]")
				break
			}
			o := int64(binary.LittleEndian.Uint64(tuple[pos+1:]))
			l := int(binary.LittleEndian.Uint32(tuple[pos+9:]))
			out = append(out, fmt.Sprintf("[大字段 %s @%d]", fmtBytes(l), o))
			pos += lobRefSize
			continue
		}
		v, next, err := readValue(tuple, pos)
		if err != nil {
			note = "列解码失败：" + err.Error()
			break
		}
		pos = next
		out = append(out, formatValue(v))
	}
	return out, note
}

// formatValue 把值变成界面上看得懂的文本（浏览用，不是 SQL 的输出格式）。
func formatValue(v types.Value) string {
	switch v.K {
	case types.KNull:
		return "NULL"
	case types.KText:
		return truncate(strings.ToValidUTF8(v.S, "?"), 120)
	case types.KInt:
		return strconv.FormatInt(v.I, 10)
	case types.KFloat:
		return strconv.FormatFloat(math.Float64frombits(uint64(v.I)), 'g', -1, 64)
	case types.KBool:
		if v.I != 0 {
			return "true"
		}
		return "false"
	case types.KDecimal:
		return v.S
	case types.KTime:
		return time.Unix(0, v.I).Format("2006-01-02 15:04:05")
	case types.KInterval:
		return fmt.Sprintf("%dns", v.I)
	}
	return fmt.Sprintf("%v", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 按字符截，别把一个 UTF-8 序列切开
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// printableKey 把索引键变成可读文本：大多数字节可打印就用原文，否则给十六进制。
func printableKey(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	ok := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			ok++
		}
	}
	if ok*4 >= len(b)*3 {
		return truncate(string(b), 80)
	}
	const hexdigits = "0123456789abcdef"
	var sb strings.Builder
	for i, c := range b {
		if i >= 32 {
			sb.WriteString("…")
			break
		}
		sb.WriteByte(hexdigits[c>>4])
		sb.WriteByte(hexdigits[c&0xf])
	}
	return sb.String()
}

// extractTextRuns 从一页原始字节里挑出「像文本」的连续片段。
//
// 判据故意宽松：可打印的 Unicode（不含控制字符）连续 ≥6 字节。页头、槽位目录、
// 整数都会被过滤掉，业务文本（URL、名字、JSON 片段）会留下 —— 这是「打开黑箱」
// 最省事的一步。上限 cap 条，避免一页塞满短串把 JSON 撑大。
func extractTextRuns(b []byte, cap int) []TextRun {
	var out []TextRun
	start := -1
	length := 0
	flush := func(end int) {
		if start >= 0 && length >= 6 {
			txt := string(b[start:end])
			out = append(out, TextRun{Off: start, Len: length, Text: truncate(txt, 160)})
		}
		start, length = -1, 0
	}
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		good := r != utf8.RuneError && !unicode.IsControl(r) && unicode.IsPrint(r) && r != 0
		if good {
			if start < 0 {
				start = i
			}
			length += size
			i += size
			continue
		}
		flush(i)
		i += size
	}
	flush(len(b))
	if len(out) > cap {
		out = out[:cap]
	}
	return out
}

// fmtBytes 是本文件里只用来拼一句「大字段多大」的小工具。
func fmtBytes(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.Itoa(n) + " B"
}
