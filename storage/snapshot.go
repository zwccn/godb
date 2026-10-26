package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"godb/types"
)

const (
	// metaFile 记录下一批 WAL 记录该用哪个代次。
	metaFile = "meta.json"
	// legacySnapshotFile 是改动前的单文件全量快照，只在迁移时读一次。
	legacySnapshotFile = "snapshot.json"
)

// metaInfo 是 meta.json 的内容。
type metaInfo struct {
	Generation int64 `json:"generation"`
	Format     int   `json:"format"`
}

// snapFormat 是当前磁盘格式版本（2 = 分表快照）。
const snapFormat = 2

func (s *Store) metaPath() string { return filepath.Join(s.Dir, metaFile) }

func (s *Store) legacySnapshotPath() string { return filepath.Join(s.Dir, legacySnapshotFile) }

// loadMeta 读 meta.json，返回代次；文件不存在返回 0。
func (s *Store) loadMeta() (int64, error) {
	b, err := os.ReadFile(s.metaPath())
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

func (s *Store) writeMeta(gen int64) error {
	b, err := json.Marshal(&metaInfo{Generation: gen, Format: snapFormat})
	if err != nil {
		return err
	}
	return writeFileAtomic(s.metaPath(), b)
}

// writeFileAtomic 用 tmp + fsync + rename 原子写文件。
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadSnapshot 读磁盘状态：meta.json + 每张表的文件头。
//
// 只读文件头（24 字节/表），不读行数据 —— 行在第一次真正访问时才加载
// （见 ensureLoaded）。这样表很多、数据很大时冷启动也不用把全部数据读进内存。
func (s *Store) loadSnapshot() error {
	metaGen, err := s.loadMeta()
	if err != nil {
		return err
	}
	// 旧的单文件快照：只在 meta.json 不存在时迁移一次
	if _, statErr := os.Stat(s.metaPath()); os.IsNotExist(statErr) {
		if err := s.migrateLegacySnapshot(); err != nil {
			return err
		}
		metaGen = s.appliedGen
	}
	s.metaGen = metaGen
	if metaGen > s.appliedGen {
		s.appliedGen = metaGen
	}
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		hdr, err := readTableSnapHeader(tableFileName(s.Dir, t.ID))
		if err != nil {
			return err
		}
		if !hdr.Exists {
			continue
		}
		s.hasSnap[key] = true
		s.tableGen[key] = hdr.Generation
		s.tableRows[key] = hdr.Rows
	}
	return nil
}

// migrateLegacySnapshot 把旧的单文件 snapshot.json 迁移成分表文件。
// 只读一次：迁移成功后把旧文件改名成 .bak，之后再也不碰。
func (s *Store) migrateLegacySnapshot() error {
	path := s.legacySnapshotPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var old struct {
		Generation int64 `json:"generation"`
		Tables     map[string]struct {
			Rows [][]any `json:"rows"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		return fmt.Errorf("解析旧快照失败: %w", err)
	}
	for name, ts := range old.Tables {
		td := s.Table(name)
		if td == nil {
			continue
		}
		t, ok := s.Cat.Get(name)
		if !ok {
			continue
		}
		key := cleanName(name)
		rows := make([]types.Row, 0, len(ts.Rows))
		for _, raw := range ts.Rows {
			vals := make([]types.Value, len(raw))
			for i, v := range raw {
				vals[i] = legacyValue(v)
			}
			row, rep := castRow(t, vals)
			s.Repaired += int64(rep)
			rows = append(rows, row)
		}
		td.Rows = rows
		s.loaded[key] = true
		s.dirty[key] = true // 让接下来那次 checkpoint 写出分表文件
	}
	s.appliedGen = old.Generation
	s.metaGen = old.Generation
	if err := os.Rename(path, path+".bak"); err != nil {
		return err
	}
	return nil
}

// Checkpoint 把内存数据写成快照，然后轮转 WAL。
func (s *Store) Checkpoint() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	if err := s.checkpointLocked(); err != nil {
		return err
	}
	// 手动 CHECKPOINT / 退出时的 Flush 也算一次落盘：更新统计并清零计数，
	// 否则后台定时器还会因为「计数没清」再补写一次快照。
	s.noteCheckpointLocked()
	return nil
}

// Flush 强制落盘：先把 WAL 里未 fsync 的记录刷下去，再做一次 checkpoint。
// CHECKPOINT 语句和优雅关闭都走这里。
func (s *Store) Flush() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	if s.wal != nil {
		seq := s.wal.Written()
		s.CommitMu.Unlock()
		err := s.wal.SyncTo(seq)
		s.CommitMu.Lock()
		if err != nil {
			return err
		}
	}
	if err := s.checkpointLocked(); err != nil {
		return err
	}
	// Flush（手动 CHECKPOINT / 进程退出）同样算一次落盘：更新统计并清零
	// 提交计数，否则后台定时器会因为「计数没清」再补写一次快照。
	s.noteCheckpointLocked()
	return nil
}

// checkpointLocked 在已持有 CommitMu 时执行 checkpoint。
//
// 关键点：**只重写有改动的表**。以前每次 checkpoint 都把整库序列化一遍，
// 40 万行要 500ms+，期间所有写事务被堵住；现在写一张表的代价只和那张表有关。
//
// 顺序：写脏表 → 删掉已不存在的表 → 写 meta → 轮转 WAL。
// 崩在中间也安全：每张表的文件在文件头里带自己的代次，重放时按
// "记录代次 > 该表文件代次" 过滤，既不会重复应用，也不会丢。
func (s *Store) checkpointLocked() error {
	gen := s.appliedGen
	var written int64
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		td := s.Data[key]
		if td == nil || !s.loaded[key] {
			continue
		}
		if !s.dirty[key] && s.hasSnap[key] {
			continue // 没改动就跳过
		}
		n, err := writeTableSnap(s.Dir, t.ID, gen, td.Rows)
		if err != nil {
			return err
		}
		written += n
		s.dirty[key] = false
		s.hasSnap[key] = true
		s.tableGen[key] = gen
		s.tableRows[key] = len(td.Rows)
	}

	// 清理已 DROP 的表留下的数据文件
	live := make(map[int64]bool, len(s.Cat.Tables))
	for _, t := range s.Cat.Tables {
		live[t.ID] = true
	}
	if files, err := os.ReadDir(filepath.Join(s.Dir, "tables")); err == nil {
		for _, de := range files {
			var id int64
			if _, err := fmt.Sscanf(de.Name(), "t%d.snp", &id); err != nil {
				continue
			}
			if !live[id] {
				_ = os.Remove(filepath.Join(s.Dir, "tables", de.Name()))
			}
		}
	}

	if err := s.writeMeta(gen); err != nil {
		return err
	}
	s.snapshotGen = gen
	s.commits = 0
	s.snapBytes = written
	if s.wal != nil {
		return s.wal.Rotate(gen + 1)
	}
	return nil
}

// SnapshotInfo 是给 CHECKPOINT 语句/状态查询用的快照信息。
type SnapshotInfo struct {
	// Generation 是磁盘快照覆盖到的 WAL 代次。
	Generation int64
	// NextGeneration 是下一批写入使用的代次。
	NextGeneration int64
	Rows           int
	Bytes          int64
	WALBytes       int64
	WALFrames      int64
	WALSyncs       int64
	Repaired       int64
	// Loaded / Tables 说明按需加载的进度。
	Loaded int
	Tables int
	// Dirty 是上一次 checkpoint 之后有改动的表数。
	Dirty int
}

// Info 返回持久化层的当前状态。
func (s *Store) Info() SnapshotInfo {
	info := SnapshotInfo{
		Generation: s.snapshotGen,
		Repaired:   s.Repaired,
		Tables:     len(s.Cat.Tables),
	}
	for key, td := range s.Data {
		if s.loaded[key] {
			info.Loaded++
			info.Rows += len(td.Rows)
		} else {
			info.Rows += s.tableRows[key]
		}
		if s.dirty[key] {
			info.Dirty++
		}
	}
	if files, err := os.ReadDir(filepath.Join(s.Dir, "tables")); err == nil {
		for _, de := range files {
			if fi, err := de.Info(); err == nil {
				info.Bytes += fi.Size()
			}
		}
	}
	if st, err := os.Stat(s.metaPath()); err == nil {
		info.Bytes += st.Size()
	}
	if s.wal != nil {
		info.NextGeneration = s.wal.Gen()
		frames, _, syncs := s.wal.Stats()
		info.WALFrames, info.WALSyncs = frames, syncs
		if st, err := os.Stat(s.wal.path); err == nil {
			info.WALBytes = st.Size()
		}
	}
	return info
}

// TableNames 返回按名字排序的表名（调试用）。
func (s *Store) TableNames() []string {
	out := make([]string, 0, len(s.Cat.Tables))
	for _, t := range s.Cat.Tables {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func parseRFC3339(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
