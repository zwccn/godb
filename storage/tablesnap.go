package storage

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"godb/catalog"
	"godb/types"
)

// 分表快照：每张表一个文件，checkpoint 时只重写「有改动」的那几张。
//
// 文件布局（小端）：
//
//	0   8  magic "GDBTSNAP"
//	8   8  generation   —— 本文件已包含哪些代次的 WAL 记录（<= 它的都算在内）
//	16  4  rowCount
//	20  4  payload 的 CRC32C
//	24  .. 行数据（每行：u32 列数 + 各列值，见 binwal.go 的 appendRow）
//
// 文件头只有 24 字节，所以启动时只读头就能拿到「代次 + 行数」，
// 不用把整个文件读进内存 —— 这是按需加载（lazy load）的基础。
const tableSnapMagic = "GDBTSNAP"

const tableSnapHeader = 24

// tableFileName 返回某张表的数据文件名。用 catalog 分配的稳定 ID，
// 这样 RENAME 不需要搬文件，DROP 之后 ID 也不复用，不会互相覆盖。
func tableFileName(dir string, id int64) string {
	return filepath.Join(dir, "tables", fmt.Sprintf("t%06d.snp", id))
}

// tablePagePath 是「页式堆文件」的路径：和 .snp 用同一个表 ID，
// 只是后缀不同，DROP 之后 ID 也不复用，不会互相覆盖。
func tablePagePath(dir string, id int64) string {
	return filepath.Join(dir, "tables", fmt.Sprintf("t%06d.dat", id))
}

// tableSnapHeaderInfo 是文件头里读出来的元信息。
type tableSnapHeaderInfo struct {
	Generation int64
	Rows       int
	Exists     bool
}

// readTableSnapHeader 只读文件头，不读行数据。
func readTableSnapHeader(path string) (tableSnapHeaderInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tableSnapHeaderInfo{}, nil
		}
		return tableSnapHeaderInfo{}, err
	}
	defer f.Close()
	hdr := make([]byte, tableSnapHeader)
	if _, err := io.ReadFull(f, hdr); err != nil {
		// 空文件或写了一半：当作不存在，后面会把这张表重写一遍
		return tableSnapHeaderInfo{}, nil
	}
	if string(hdr[:8]) != tableSnapMagic {
		return tableSnapHeaderInfo{}, nil
	}
	return tableSnapHeaderInfo{
		Generation: int64(binary.LittleEndian.Uint64(hdr[8:])),
		Rows:       int(binary.LittleEndian.Uint32(hdr[16:])),
		Exists:     true,
	}, nil
}

// encodeTableSnap 把整张表编码成文件字节。
//
// 空槽（被删除、还没被复用的槽位）不写进文件：磁盘上永远是紧凑的，
// 加载回来直接就是连续行号，不需要再压一次。
func encodeTableSnap(gen int64, rows []types.Row) []byte {
	payload := make([]byte, 0, len(rows)*64)
	n := 0
	for _, r := range rows {
		if r == nil {
			continue
		}
		payload = appendRow(payload, r)
		n++
	}

	buf := make([]byte, tableSnapHeader+len(payload))
	copy(buf[:8], tableSnapMagic)
	binary.LittleEndian.PutUint64(buf[8:], uint64(gen))
	binary.LittleEndian.PutUint32(buf[16:], uint32(n))
	binary.LittleEndian.PutUint32(buf[20:], crc32.Checksum(payload, crcTable))
	copy(buf[tableSnapHeader:], payload)
	return buf
}

// writeTableSnap 原子写入一张表的全部行（tmp + fsync + rename）。
func writeTableSnap(dir string, id int64, gen int64, rows []types.Row) (int64, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o755); err != nil {
		return 0, err
	}
	buf := encodeTableSnap(gen, rows)
	if err := writeFileAtomic(tableFileName(dir, id), buf); err != nil {
		return 0, err
	}
	return int64(len(buf)), nil
}

// TableSnapPath 返回某张表数据文件的完整路径。
// 回收站恢复要按 ID 直接操作文件，所以这里把命名规则暴露出去。
func TableSnapPath(dir string, id int64) string { return tableFileName(dir, id) }

// InstallTableSnap 把一份 .snp 备份（回收站里的那份）装成页文件。
//
// 备份文件仍是 .snp 格式（一份紧凑的行流，适合当交换格式），装回来时
// 展开成页：RID 就是行号。恢复出来的表 ID 是新的，页文件也按新 ID 命名。
//
// 代次不需要在这里盖：恢复的表不在 meta.json 里，loadSnapshot 会把
// 「有页文件但不在 meta 里」的表当作「已包含当前代次」，从而不被 WAL 重放
// 倒灌（备份是 DROP 之前的快照，重放它之后的记录会把删掉的行又插回来）。
func InstallTableSnap(dir string, id int64, data []byte) (int64, error) {
	if len(data) < tableSnapHeader || string(data[:8]) != tableSnapMagic {
		return 0, fmt.Errorf("不是表数据文件（缺少 %s 文件头）", tableSnapMagic)
	}
	payload := data[tableSnapHeader:]
	if crc32.Checksum(payload, crcTable) != binary.LittleEndian.Uint32(data[20:]) {
		return 0, fmt.Errorf("备份文件校验和不匹配，可能已损坏")
	}
	var rows []types.Row
	for p := 0; p < len(payload); {
		row, next, err := readRowValues(payload, p)
		if err != nil {
			return 0, fmt.Errorf("解析备份数据失败: %w", err)
		}
		rows = append(rows, row)
		p = next
	}

	tp, err := openTablePages(tablePagePath(dir, id), defaultTablePoolPages)
	if err != nil {
		return 0, err
	}
	defer tp.close()

	puts := make([]pendingRow, 0, len(rows))
	for i, r := range rows {
		tuple, err := encodeRowTuple(r)
		if err != nil {
			return 0, err
		}
		puts = append(puts, pendingRow{slot: i, tuple: tuple})
	}
	if _, err := tp.putBatch(puts); err != nil {
		return 0, err
	}
	n, err := tp.checkpoint()
	if err != nil {
		return 0, err
	}
	return int64(n) * PageSize, nil
}

// MetaGeneration 读 meta.json 里的当前代次（文件不存在返回 0）。
func MetaGeneration(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var m metaInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, fmt.Errorf("解析 %s 失败: %w", metaFile, err)
	}
	return m.Generation, nil
}

// readTableSnap 读出整张表的行，并按当前表结构对齐（列数不一致时补齐/截断）。
func readTableSnap(dir string, id int64, t *catalog.Table) ([]types.Row, int64, int, error) {
	path := tableFileName(dir, id)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, err
	}
	if len(b) < tableSnapHeader || string(b[:8]) != tableSnapMagic {
		return nil, 0, 0, nil
	}
	gen := int64(binary.LittleEndian.Uint64(b[8:]))
	want := int(binary.LittleEndian.Uint32(b[16:]))
	sum := binary.LittleEndian.Uint32(b[20:])
	payload := b[tableSnapHeader:]
	if crc32.Checksum(payload, crcTable) != sum {
		return nil, gen, 0, fmt.Errorf("表数据文件校验和不匹配: %s", path)
	}
	rows := make([]types.Row, 0, want)
	off := 0
	repaired := 0
	for i := 0; i < want; i++ {
		vals, next, err := readRowValues(payload, off)
		if err != nil {
			// 尾部损坏：保留已读出的部分
			break
		}
		off = next
		row, rep := castRow(t, vals)
		repaired += rep
		rows = append(rows, row)
	}
	return rows, gen, repaired, nil
}

// removeTableSnap 删除某张表的数据文件（DROP TABLE 之后由 checkpoint 调用）。
func removeTableSnap(dir string, id int64) error {
	err := os.Remove(tableFileName(dir, id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// removeTablePages 删掉页式堆文件（DROP TABLE）。
func removeTablePages(dir string, id int64) error {
	err := os.Remove(tablePagePath(dir, id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
