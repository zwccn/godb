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
	// vecTrashDir 是向量文件的回收子目录名（DROP 时 trashVecFile 往这里移）。
	vecTrashDir = "vec"
	// trashVecKeep / trashVecMaxBytes 是 .trash/vec 的保留策略：最多留这么多份，
	// 且合计不超过这个字节数，超了从最旧的删。数量管「小文件攒太多」，字节管
	// 「一个文件就几个 GB」。20 份 × 大向量表本来能撑爆盘，所以两道都要。
	trashVecKeep     = 20
	trashVecMaxBytes = int64(512 << 20) // 512 MB
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
	// VecFiles 是随表一起备份的向量列文件数（0 表示这张表没有 VECTOR 列）。
	// 向量数据不在行快照里（行里只有一个槽位号），必须单独拷文件。
	VecFiles int `json:"vecFiles,omitempty"`
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
		rows := make([]types.Row, 0, td.Live)
		for i := 0; i < td.SlotCount(); i++ {
			if r, ok := td.rowAt(i); ok { // 被换出的行也要取回来
				rows = append(rows, r)
			}
		}
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

		// 向量列的数据在 tables/t<ID>.c<列>.vec 里，**不在行快照里** ——
		// 行里存的只是 Value{K:KVector, I:槽位号}，一个引用。
		// 不把文件一起拷进备份，恢复出来的就是一串指向空槽的引用
		// （实测显示成 [vector:0]，看着像有值，其实全丢了）。
		if n, err := s.copyVecFilesInto(dir, t.ID); err != nil {
			return "", err
		} else if n > 0 {
			tt.VecFiles = n
		}
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

// copyVecFilesInto 把一张表的向量列文件拷进备份目录的 vec/ 子目录，返回拷了几个。
//
// 向量数据在 tables/t<ID>.c<列>.vec，按「表 ID + 列 ID」命名；备份里也保持这个名字，
// 因为备份的 manifest 记的是旧表 ID，恢复时据此找文件、再改名成新表 ID。
//
// 一个文件都没有（表没有 VECTOR 列）不算错，返回 0 —— 绝大多数表都是这样。
func (s *Store) copyVecFilesInto(trashDir string, tableID int64) (int, error) {
	pat := filepath.Join(s.Dir, "tables", fmt.Sprintf("t%06d.c*.vec", tableID))
	files, err := filepath.Glob(pat)
	if err != nil || len(files) == 0 {
		return 0, nil
	}
	dstDir := filepath.Join(trashDir, vecTrashDir)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, src := range files {
		b, err := os.ReadFile(src)
		if err != nil {
			// 单个文件读不出来就跳过：丢一个向量列，总比整个 DROP 失败、表删不掉强。
			continue
		}
		if err := writeFileAtomic(filepath.Join(dstDir, filepath.Base(src)), b); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// InstallVecFiles 把备份里的向量列文件装回数据目录，返回装了几个。
//
// 恢复相当于一次「新的建表」（表 ID 会重新分配），而文件名里编着表 ID，
// 所以这里要把 t<旧ID>.c<列>.vec 改名成 t<新ID>.c<列>.vec。列 ID 不变 ——
// 备份里的列定义和恢复后的列定义是同一套。
func InstallVecFiles(root, trashDir string, oldID, newID int64) (int, error) {
	pat := filepath.Join(trashDir, vecTrashDir, fmt.Sprintf("t%06d.c*.vec", oldID))
	files, err := filepath.Glob(pat)
	if err != nil || len(files) == 0 {
		return 0, nil
	}
	dstDir := filepath.Join(root, "tables")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, src := range files {
		rest := strings.TrimPrefix(filepath.Base(src), fmt.Sprintf("t%06d.", oldID))
		dst := filepath.Join(dstDir, fmt.Sprintf("t%06d.%s", newID, rest))
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := writeFileAtomic(dst, b); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
		if !e.IsDir() {
			continue
		}
		// 只认「真备份」：目录里必须有 manifest.json。
		//
		// .trash/vec 是向量文件的回收目录（见 vecstore.go 的 trashVecFile），
		// 它不是一次备份。不排除的话会踩两个坑：'v' > '2'，倒序排在所有
		// 2026xxxx 目录之前，于是 -restore-trash latest 拿到的是它，去读
		// .trash/vec/manifest.json 直接报「找不到文件」—— 恢复功能整个不可用。
		if _, err := os.Stat(filepath.Join(root, e.Name(), "manifest.json")); err != nil {
			continue
		}
		out = append(out, e.Name())
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
		if !e.IsDir() {
			continue
		}
		// vec 是向量文件的回收目录，不是一次备份：既不该占 trashKeep 的名额
		// （否则备份实际只留 19 份），也不该被这里按「最旧」删掉 —— 它有自己的
		// 保留策略，见 pruneTrashVec。
		if e.Name() == vecTrashDir {
			continue
		}
		dirs = append(dirs, e.Name())
	}
	if len(dirs) > trashKeep {
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
		for _, name := range dirs[trashKeep:] {
			_ = os.RemoveAll(filepath.Join(root, name))
		}
	}
	s.pruneTrashVec()
}

// pruneTrashVec 清理 .trash/vec 里的孤儿向量文件。
//
// 这个目录收的是 DROP TABLE / DROP COLUMN 时从 tables/ 移出来的 .vec 文件
// （trashVecFile 做的）。它**不在** trashKeep 的计数里，而且因为 'v' > '2'，
// 降序后永远排在最前面，所以 pruneTrash 永远轮不到它 —— 实测跑几轮 DROP
// 就攒了 9.8 MB 且只增不减；表越大越严重（100 万 × 768 维一次 DROP 是 3 GB）。
//
// 保留策略：最近 trashVecKeep 个，且合计不超过 trashVecMaxBytes，超了从最旧删。
// 删的是纯冗余 —— 表的备份里另有一份（TrashTables 拷的），恢复走备份那份；
// 这里只是「文件最后已知位置」的残留，丢了不影响恢复。
func (s *Store) pruneTrashVec() {
	dir := filepath.Join(s.Dir, trashDirName, vecTrashDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type vecFile struct {
		name string
		mod  time.Time
		size int64
	}
	files := make([]vecFile, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, vecFile{name: e.Name(), mod: info.ModTime(), size: info.Size()})
	}
	// 新的在前：Rename 不改 mtime，所以这里排的是「文件本身的年龄」，
	// 对清理来说够用（越老的越先删）。
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	var total int64
	for i, f := range files {
		total += f.size
		if i < trashVecKeep && total <= trashVecMaxBytes {
			continue
		}
		_ = os.Remove(filepath.Join(dir, f.name))
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
