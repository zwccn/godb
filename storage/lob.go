package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"strings"
	"sync"
)

// LOB 文件：一张表一个 `tables/tXXXXXX.lob`，纯追加写的字节流。
//
// 为什么不用「溢出页链」（PG TOAST / InnoDB off-page 那一套）：
//
//   - 指针追逐：读一个 1MB 的字段要顺着几十上百个 8KB 页寻道，顺序 I/O 被
//     硬生生拆成随机 I/O；
//   - 缓存污染：大字段流经全局缓冲池时会把页框（默认 1024 个）挤干净，
//     B+Tree 的根节点和热点小行一起被淘汰；
//   - VACUUM 噩梦：大字段一 UPDATE，旧溢出页立刻变成散落各处的孤儿。
//
// 所以这里把大字段从页体系里**彻底摘出去**：不进缓冲池、不切页、只追加。
// 行里留一个定长的「指针」（偏移 + 长度），读的时候按需 pread 一次。
//
// 文件里每个 blob 自带一个小头：
//
//	0  "GDBL"
//	4  uint32  数据长度
//	8  uint32  CRC32C(数据)
//	12 数据
//
// 指针里存的就是这个头的偏移（不是数据偏移）。多出来的 12 字节换来两件事：
// 单条大字段自己能校验（页 CRC 只管页里那几十字节的指针，管不到 1MB 正文），
// 以及「偏移指错了」能当场报出来而不是把别人的数据当自己的返回。
const (
	lobMagic   = "GDBL"
	lobHdrSize = 12
	// tagLOB 是行编码里的「这会话是个指针」标签。和 binwal.go 的 tag* 并列，
	// 但只出现在**堆页里的行**里：WAL 存的始终是完整值（日志要能独立重放）。
	tagLOB = 7
	// lobRefSize 是行里一个 LOB 指针占的字节：tag + 偏移(8) + 长度(4)。
	lobRefSize = 13

	// lobThreshold 是「多大的文本才值得搬去 LOB 文件」。
	//
	// 2KB：比一页的四分之一略大。太小的值搬出去反而更贵（一次 pread 的
	// 系统调用 + 12 字节头 + 指针，比直接内联多花三倍空间/时间）。
	// 超过它的值一定要外置 —— 否则一行塞两个 3KB 文本就装不下一页了。
	lobThreshold = 2048
)

// lobFile 是一条追加写的字节流文件。
//
// 打开是**惰性**的：绝大多数表根本没有大字段，不该为它们建空文件、占句柄。
// 第一次写入（或第一次读到指针）时才真正 open。
type lobFile struct {
	path string

	mu  sync.RWMutex
	f   *os.File
	end int64 // 文件当前长度（追加位置）

	// unsynced 是「还没 fsync 的字节数」。checkpoint 会把大字段先落盘再写页，
	// 所以这个数只在「一轮 checkpoint 之内」是正的。
	unsynced int64

	// 统计（监控/测试用）
	Appends     int64
	AppendBytes int64
	Reads       int64
	ReadBytes   int64
	Syncs       int64
}

func newLobFile(path string) *lobFile { return &lobFile{path: path} }

// lobPathOf 由堆页文件路径推出大字段文件路径（t000003.dat → t000003.lob）。
func lobPathOf(pagesPath string) string {
	return strings.TrimSuffix(pagesPath, ".dat") + ".lob"
}

// ensure 打开文件（调用方必须持有写锁）。
func (l *lobFile) ensure() error {
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("storage: 打开大字段文件失败: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.end = f, st.Size()
	return nil
}

// existing 只在文件已经存在时打开（读路径用：没有文件就是「没有大字段」）。
func (l *lobFile) existing() error {
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("storage: 大字段文件不存在或打不开: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.end = f, st.Size()
	return nil
}

// handle 返回一个可读的句柄。读路径不创建文件：文件不存在就说明这张表
// 根本没有大字段（指针是坏的）。
func (l *lobFile) handle() (*os.File, error) {
	l.mu.RLock()
	if l.f != nil {
		f := l.f
		l.mu.RUnlock()
		return f, nil
	}
	l.mu.RUnlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.existing(); err != nil {
		return nil, err
	}
	return l.f, nil
}

// append 追加一个 blob，返回它在文件里的偏移（指向 blob 头）。
func (l *lobFile) append(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensure(); err != nil {
		return 0, err
	}
	off := l.end
	var hdr [lobHdrSize]byte
	copy(hdr[0:4], lobMagic)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(data)))
	binary.LittleEndian.PutUint32(hdr[8:], crc32.Checksum(data, crcTable))
	if _, err := l.f.WriteAt(hdr[:], off); err != nil {
		return 0, fmt.Errorf("storage: 写大字段失败: %w", err)
	}
	if _, err := l.f.WriteAt(data, off+lobHdrSize); err != nil {
		return 0, fmt.Errorf("storage: 写大字段失败: %w", err)
	}
	l.end = off + lobHdrSize + int64(len(data))
	l.unsynced += lobHdrSize + int64(len(data))
	l.Appends++
	l.AppendBytes += int64(len(data))
	return off, nil
}

// read 读回一个 blob（off 是 append 返回的偏移，n 是指针里记的长度）。
func (l *lobFile) read(off int64, n int) ([]byte, error) {
	if n < 0 || off < 0 {
		return nil, fmt.Errorf("storage: 大字段指针非法（off=%d len=%d）", off, n)
	}
	f, err := l.handle()
	if err != nil {
		return nil, err
	}

	buf := make([]byte, lobHdrSize+n)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("storage: 读大字段失败（off=%d len=%d）: %w", off, n, err)
	}
	if string(buf[0:4]) != lobMagic {
		return nil, fmt.Errorf("storage: 大字段头损坏（off=%d）", off)
	}
	if got := int(binary.LittleEndian.Uint32(buf[4:])); got != n {
		return nil, fmt.Errorf("storage: 大字段长度不符（指针 %d，文件里 %d）", n, got)
	}
	if want := binary.LittleEndian.Uint32(buf[8:]); crc32.Checksum(buf[lobHdrSize:], crcTable) != want {
		return nil, fmt.Errorf("storage: 大字段校验和不匹配（off=%d，数据损坏）", off)
	}
	data := buf[lobHdrSize:]

	l.mu.Lock()
	l.Reads++
	l.ReadBytes += int64(n)
	l.mu.Unlock()
	return data, nil
}

// sync 把追加的数据刷到盘上（没写过就什么都不做）。
func (l *lobFile) sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil || l.unsynced == 0 {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("storage: 落盘大字段失败: %w", err)
	}
	l.unsynced = 0
	l.Syncs++
	return nil
}

// size 返回文件长度（没有文件就是 0）。
func (l *lobFile) size() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.f != nil {
		return l.end
	}
	if st, err := os.Stat(l.path); err == nil {
		return st.Size()
	}
	return 0
}

// truncate 清空整个文件（TRUNCATE TABLE 用）。
func (l *lobFile) truncate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensure(); err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	l.end = 0
	l.unsynced = 0
	return nil
}

// close 关文件（脏数据先刷）。
func (l *lobFile) close() error {
	if err := l.sync(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
