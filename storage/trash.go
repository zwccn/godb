package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"godb/catalog"
	"godb/types"
)

// 回收站：删表（DROP TABLE / 清空库 / 重建样例）之前，把表和当时的 catalog
// 一起复制到 <data>/.trash/<时间戳>-<原因>/，让「不可逆」变成「可捞回来」。
//
// 为什么不做成「删到回收站而不是真删」：表数据文件的命名基于稳定 ID，删掉的
// 表 ID 不复用，所以复制一份是最小侵入的做法 —— 删除路径完全不变，只是多了一
// 份冷备份。代价是磁盘多占一份被删表的大小，所以只保留最近 trashKeep 份。
const (
	trashDirName = ".trash"
	// trashKeep 是保留的备份份数（按时间从新到旧）。一次 DROP 一张表算一份，
	// 一次清空库也只算一份。
	trashKeep = 20
	// TrashReasonDropTable / DropAll / Sample 是写进 manifest 的原因标签。
	TrashReasonDropTable = "drop-table"
	TrashReasonDropAll   = "drop-all"
)

// TrashTable 是回收站里一张表的数据情况。
type TrashTable struct {
	ID int64 `json:"id"`
	// Name 是删除时的表名（RENAME 之后的名字）。
	Name string `json:"name"`
	Rows int    `json:"rows"`
	// Bytes 是备份文件大小。
	Bytes int64 `json:"bytes"`
	// File 是备份文件相对回收站目录的路径；空表示这张表没有数据文件（空表从未落盘）。
	File string `json:"file,omitempty"`
	// From 说明数据取自内存（memory，最新）还是磁盘快照（disk，可能旧一些）。
	From string `json:"from"`
}

// TrashManifest 是回收站里 manifest.json 的内容。
//
// Defs 存的是完整的表定义，恢复时不用再去猜列类型：即使 catalog.json 也被
// 覆盖了，光靠这份 manifest 就能把表结构和数据一起还原。
type TrashManifest struct {
	At      string           `json:"at"`
	Reason  string           `json:"reason"`
	Catalog string           `json:"catalog,omitempty"`
	Tables  []TrashTable     `json:"tables"`
	Defs    []*catalog.Table `json:"defs"`
}

// TrashTables 把若干张表备份到回收站，返回备份目录。
//
// names 为空表示「当前所有表」（清空库用）。返回的 error 非 nil 时调用方
// **不应该**继续删除 —— 备份都没写成功，删了就真没了。
func (s *Store) TrashTables(reason string, names []string) (string, error) {
	if s.Cat == nil {
		return "", nil
	}
	targets := make([]*catalog.Table, 0, len(names))
	if len(names) == 0 {
		targets = append(targets, s.Cat.Tables...)
	} else {
		for _, n := range names {
			if t, ok := s.Cat.Get(n); ok {
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	dir, err := s.newTrashDir(reason)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "tables"), 0o755); err != nil {
		return "", err
	}

	mf := TrashManifest{At: time.Now().Format(time.RFC3339), Reason: reason}
	// catalog.json 原样存一份：README 式的兜底，出问题时至少能看到当时的全貌。
	if b, err := os.ReadFile(filepath.Join(s.Dir, "catalog.json")); err == nil {
		if err := writeFileAtomic(filepath.Join(dir, "catalog.json"), b); err != nil {
			return "", err
		}
		mf.Catalog = "catalog.json"
	}

	// 只把「表 + 行的槽位数组拷贝」抓在锁内，编码和写盘放到锁外：
	// 回收站要 fsync，不能在 loadMu 里做，否则会把整库的按需加载堵住。
	type pendingTable struct {
		def  *catalog.Table
		rows []types.Row
		live int
	}
	pending := make([]pendingTable, 0, len(targets))
	s.loadMu.RLock()
	for _, t := range targets {
		key := cleanName(t.Name)
		if !s.loaded[key] || s.Data[key] == nil {
			// 没加载过：留空，下面退回读磁盘快照
			pending = append(pending, pendingTable{def: t})
			continue
		}
		td := s.Data[key]
		rows := make([]types.Row, len(td.Rows))
		copy(rows, td.Rows)
		pending = append(pending, pendingTable{def: t, rows: rows, live: td.Live})
	}
	s.loadMu.RUnlock()

	for _, p := range pending {
		t := p.def
		name := fmt.Sprintf("t%06d.snp", t.ID)
		dst := filepath.Join(dir, "tables", name)
		tt := TrashTable{ID: t.ID, Name: t.Name, From: "none"}
		if p.rows != nil {
			// 内存里是最新的（可能还没 checkpoint），优先用它，否则会捞回一份旧数据。
			buf := encodeTableSnap(0, p.rows)
			if err := writeFileAtomic(dst, buf); err != nil {
				return "", err
			}
			tt.Rows, tt.Bytes, tt.From = p.live, int64(len(buf)), "memory"
			tt.File = relTrashPath("tables", name)
		} else if b, err := os.ReadFile(tableFileName(s.Dir, t.ID)); err == nil {
			if err := writeFileAtomic(dst, b); err != nil {
				return "", err
			}
			hdr, _ := readTableSnapHeader(dst)
			tt.Rows, tt.Bytes, tt.From = hdr.Rows, int64(len(b)), "disk"
			tt.File = relTrashPath("tables", name)
		}
		mf.Tables = append(mf.Tables, tt)
		mf.Defs = append(mf.Defs, t)
	}

	mb, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(dir, "manifest.json"), mb); err != nil {
		return "", err
	}
	s.pruneTrash()
	return dir, nil
}

// TrashEntries 列出回收站里的备份（目录名，从新到旧）。
func TrashEntries(dir string) ([]string, error) {
	root := filepath.Join(dir, trashDirName)
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// TrashDirName 是对外暴露的回收站目录名（web / 命令行提示要用）。
func TrashDirName() string { return trashDirName }

func (s *Store) newTrashDir(reason string) (string, error) {
	root := filepath.Join(s.Dir, trashDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	base := time.Now().Format("20060102-150405") + "-" + sanitizeReason(reason)
	dir := filepath.Join(root, base)
	for i := 2; i < 100; i++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
	}
	return "", fmt.Errorf("回收站目录重名过多: %s", base)
}

// pruneTrash 只保留最近 trashKeep 份。目录名以时间戳开头，按名字倒序即可。
func (s *Store) pruneTrash() {
	root := filepath.Join(s.Dir, trashDirName)
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	dirs := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= trashKeep {
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, name := range dirs[trashKeep:] {
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

// sanitizeReason 把原因标签压成目录名安全的形状。
func sanitizeReason(reason string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(reason) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "trash"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func relTrashPath(elem ...string) string {
	return filepath.ToSlash(filepath.Join(elem...))
}
