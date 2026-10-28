package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"godb/hrtime"
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
			s.Repaired.Add(int64(rep))
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
	// 手动 CHECKPOINT / 退出时的 Flush 也算一次落盘：更新统计并清零计数，
	// 否则后台定时器还会因为「计数没清」再补写一次快照。
	// （这一步现在由 checkpointBodyLocked 内部完成）
	return s.forceCheckpointLocked(CPReasonManual)
}

// CheckpointDDL 是 DDL 之后的强制落盘。
//
// 触发原因单独记成 ddl：DDL 引起的落盘往往是一次整表重写，正好能解释
// 「为什么刚才那几十毫秒 fsync 变慢了」。
func (s *Store) CheckpointDDL() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	return s.forceCheckpointLocked(CPReasonDDL)
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
	// Flush（手动 CHECKPOINT / 进程退出）同样算一次落盘：更新统计并清零
	// 提交计数，否则后台定时器会因为「计数没清」再补写一次快照。
	return s.forceCheckpointLocked(CPReasonFlush)
}

// snapshotJob 是一次「已经拍好的快照」：只持有行切片的引用，不含序列化结果。
//
// 行是不可变的（UPDATE 复制整行、提交时重组的是切片头），所以在锁外读这些行
// 是安全的 —— 这正是能把「序列化 + fsync」挪出 CommitMu 的前提。
type snapshotJob struct {
	gen     int64
	tables  []snapshotTable
	written int64
}

type snapshotTable struct {
	id   int64
	key  string
	rows []types.Row // 槽位数组的拷贝（提交会原地替换槽位，不能直接引用）
	live int
	// dirtyCnt 是拍快照时这张表的累计脏行数，收尾时用来判断期间有没有新改动
	dirtyCnt int64
}

// checkpointStartLocked 拍快照。调用方必须持有 CommitMu。
//
// 这里必须**拷一份槽位数组**：提交是原地替换 `td.Rows[slot]`（而不是整片换掉），
// 所以直接引用 td.Rows 会让写盘过程读到「半新半旧」的行。拷贝只是指针数组，
// 20 万行约 3 ms，而且只在需要写盘的脏表上做。
func (s *Store) checkpointStartLocked() *snapshotJob {
	job := &snapshotJob{gen: s.appliedGen}
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		td := s.Data[key]
		if td == nil || !s.loaded[key] {
			continue
		}
		if !s.dirty[key] && s.hasSnap[key] {
			continue // 没改动就跳过
		}
		rows := make([]types.Row, len(td.Rows))
		copy(rows, td.Rows)
		job.tables = append(job.tables, snapshotTable{
			id: t.ID, key: key, rows: rows, live: td.Live, dirtyCnt: s.dirtyRows[key],
		})
	}
	return job
}

// checkpointWrite 在**不持 CommitMu** 的时候序列化并写盘。
func (s *Store) checkpointWrite(job *snapshotJob) error {
	var written int64
	for _, t := range job.tables {
		n, err := writeTableSnap(s.Dir, t.id, job.gen, t.rows)
		if err != nil {
			return err
		}
		written += n
	}
	job.written = written
	return nil
}

// checkpointFinishLocked 写盘成功后更新内存状态、清理孤儿文件、写 meta、轮转 WAL。
// 调用方必须持有 CommitMu。
func (s *Store) checkpointFinishLocked(job *snapshotJob) error {
	// 一张表都不需要重写说明数据早就在磁盘上了。这时只剩一件事值得做：把
	// WAL 轮转掉（启动时重放完的那批记录就靠这一步清空）。WAL 本来就空的话，
	// 写 meta 和轮转都是白做功 —— 一次 tmp+fsync+rename 加一次删文件重建。
	if len(job.tables) == 0 && (s.wal == nil || s.wal.LiveBytes() == 0) {
		s.commits = 0
		s.snapBytes = 0 // 这一轮确实一个字节都没写，别把上一轮的数字留着
		return nil
	}
	for _, t := range job.tables {
		// 写盘期间又改了这张表 → dirty 不能清，改动留在 WAL 里等下一轮。
		// 少了这个判断，那些改动会被永久标记成「已落盘」而丢掉。
		if s.dirtyRows[t.key] == t.dirtyCnt {
			s.dirty[t.key] = false
			s.dirtyRows[t.key] = 0
		}
		s.hasSnap[t.key] = true
		s.tableGen[t.key] = job.gen
		s.tableRows[t.key] = t.live
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

	if err := s.writeMeta(job.gen); err != nil {
		return err
	}
	s.snapshotGen = job.gen
	s.commits = 0
	s.snapBytes = job.written
	s.written.Add(job.written)
	if s.wal != nil {
		return s.wal.Rotate(job.gen + 1)
	}
	return nil
}

// checkpointBodyLocked 走完一整轮：拍快照（锁内）→ 写盘（锁外）→ 收尾（锁内）。
//
// 这么拆是为了把几十毫秒的「序列化 + fsync」移出 CommitMu：之前整段都在锁里，
// 一次 checkpoint 会把所有写事务堵在门外（40 万行的表约 50 ms）。拆完之后
// 锁内只剩「拷一遍切片头 + 更新几个 map」，几毫秒量级。
//
// 调用方必须持有 CommitMu，并且仍在 SchemaMu 的锁内 —— 行数据在锁外被读的
// 前提是期间没有 DDL 能改它们。checkpoint 的所有触发点都满足：提交在语句的
// 读锁内，DDL 的 checkpoint 在写锁内，后台 ticker 自己先拿读锁。
func (s *Store) checkpointBodyLocked(reason string) error {
	t0 := hrtime.Now()
	job := s.checkpointStartLocked()
	s.CommitMu.Unlock()
	err := func() error {
		s.cpMu.Lock()
		defer s.cpMu.Unlock()
		return s.checkpointWrite(job)
	}()
	s.CommitMu.Lock()
	if err != nil {
		return err
	}
	if err := s.checkpointFinishLocked(job); err != nil {
		return err
	}
	s.noteCheckpointLocked(reason, hrtime.Since(t0), job.written, len(job.tables))
	return nil
}

// tryCheckpointLocked 用于提交路径与后台定时器：已经有 checkpoint 在写盘时直接
// 放弃这一次（返回 false）。数据在 WAL 里不会丢，等它写完下一轮自然会触发。
//
// 不能排队等 —— 两个 checkpoint 抢着写、后拍的先写完，旧数据就会覆盖新文件，
// 而 WAL 又被轮转掉，那就是真的丢数据了。
//
// 返回值表示「这一轮到底落了盘没有」：提交路径要靠它决定还用不用等 fsync。
func (s *Store) tryCheckpointLocked(reason string) (bool, error) {
	if !s.cpBusy.CompareAndSwap(false, true) {
		return false, nil
	}
	defer s.cpBusy.Store(false)
	return true, s.checkpointBodyLocked(reason)
}

// forceCheckpointLocked 用于 CHECKPOINT 语句、DDL 与关闭：语义要求返回时确实已落盘，
// 所以撞上正在写的那一轮就等它。（轮询期间放开 CommitMu，别把提交堵死。）
func (s *Store) forceCheckpointLocked(reason string) error {
	for !s.cpBusy.CompareAndSwap(false, true) {
		s.CommitMu.Unlock()
		time.Sleep(2 * time.Millisecond)
		s.CommitMu.Lock()
	}
	defer s.cpBusy.Store(false)
	return s.checkpointBodyLocked(reason)
}

// SnapshotInfo 是给 CHECKPOINT 语句/状态查询用的快照信息。
type SnapshotInfo struct {
	// Generation 是磁盘快照覆盖到的 WAL 代次。
	Generation int64
	// NextGeneration 是下一批写入使用的代次。
	NextGeneration int64
	Rows           int
	// Bytes 是磁盘上表文件的总大小（不是这一次写了多少）。
	Bytes int64
	// Written 是上一次 checkpoint 实际写进磁盘的字节数。
	Written int64
	// WrittenTotal 是本进程累计写入的字节数 —— 用它判断「改一行到底让磁盘写了多少」。
	WrittenTotal int64
	WALBytes     int64
	WALFrames    int64
	WALSyncs     int64
	Repaired     int64
	// Loaded / Tables 说明按需加载的进度。
	Loaded int
	Tables int
	// Dirty 是上一次 checkpoint 之后有改动的表数。
	Dirty int
}

// Info 返回持久化层的当前状态。
func (s *Store) Info() SnapshotInfo {
	info := SnapshotInfo{
		Generation:   s.snapshotGen,
		Repaired:     s.Repaired.Load(),
		Tables:       len(s.Cat.Tables),
		Written:      s.snapBytes,
		WrittenTotal: s.written.Load(),
	}
	for key, td := range s.Data {
		if s.loaded[key] {
			info.Loaded++
			info.Rows += td.Live // 槽位数组可能带空洞，Live 才是真实行数
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
