package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"godb/hrtime"
)

const (
	// metaFile 记录下一批 WAL 记录该用哪个代次。
	metaFile = "meta.json"
	// legacySnapshotFile 是改动前的单文件全量快照，只在迁移时读一次。
	legacySnapshotFile = "snapshot.json"
)

// metaInfo 是 meta.json 的内容。
//
// Generation 是「已经落盘的 WAL 代次」：重放只应用比它新的记录。代次是全局的
// —— 因为 checkpoint 在 CommitMu 里一次做完，不存在「一半表新、一半表旧」的
// 状态；就算中途出错，重放是幂等的，多放一遍也无害。
type metaInfo struct {
	Generation int64                `json:"generation"`
	Format     int                  `json:"format"`
	Tables     map[string]metaTable `json:"tables,omitempty"`
}

// metaTable 是每张表的信息。Rows 只为「未加载的表」回答行数查询，
// 数据本身在 t<ID>.dat 里。
type metaTable struct {
	ID   int64 `json:"id"`
	Rows int   `json:"rows"`
}

// snapFormat 是当前磁盘格式版本（3 = 页式堆文件）。
const snapFormat = 3

func (s *Store) metaPath() string { return filepath.Join(s.Dir, metaFile) }

func (s *Store) legacySnapshotPath() string { return filepath.Join(s.Dir, legacySnapshotFile) }

// loadMeta 读 meta.json，返回代次与每表信息；文件不存在返回 0。
func (s *Store) loadMeta() (int64, map[string]metaTable, error) {
	b, err := os.ReadFile(s.metaPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	var m metaInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, nil, fmt.Errorf("解析 %s 失败: %w", metaFile, err)
	}
	return m.Generation, m.Tables, nil
}

func (s *Store) writeMeta(gen int64, tables map[string]metaTable) error {
	b, err := json.Marshal(&metaInfo{Generation: gen, Format: snapFormat, Tables: tables})
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

// loadSnapshot 读磁盘状态：meta.json + 每张表的页文件。
//
// 只读 meta 里记的行数，**不读页、不读行** —— 行在第一次真正访问时才从
// 页文件装进内存（见 ensureLoaded）。表很多时冷启动也不用把数据全读一遍。
func (s *Store) loadSnapshot() error {
	metaGen, tables, err := s.loadMeta()
	if err != nil {
		return err
	}
	s.metaGen = metaGen
	if metaGen > s.appliedGen {
		s.appliedGen = metaGen
	}
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		mt, ok := tables[key]
		path := tablePagePath(s.Dir, t.ID)
		if !ok || mt.ID != t.ID {
			// 不在 meta 里（新建表 / 从没落过盘 / 回收站恢复回来的新 ID）：
			// 只要页文件在，就当它已经包含当前代次 —— 恢复出来的备份是
			// DROP 之前的快照，再被 WAL 重放一遍会把删掉的行插回来。
			if _, err := os.Stat(path); err == nil {
				s.hasSnap[key] = true
				s.tableGen[key] = metaGen
			}
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			// meta 说有、文件却不在：数据没法救，记下来让启动日志报出去，
			// 但不要因此让整个库打不开（其它表还能用）。
			s.loadErr = fmt.Errorf("表 %s 的数据文件丢失（%s）", t.Name, path)
			continue
		}
		s.hasSnap[key] = true
		s.tableRows[key] = mt.Rows
		// 页文件已经覆盖到这一代：重放从这里往后算
		s.tableGen[key] = metaGen
	}
	return nil
}

// Checkpoint 把脏页刷盘、记代次，然后轮转 WAL。
func (s *Store) Checkpoint() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	return s.forceCheckpointLocked(CPReasonManual)
}

// CheckpointDDL 是 DDL 之后的强制落盘。
//
// 触发原因单独记成 ddl：DDL 会引起一次 checkpoint，正好能解释
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
	return s.forceCheckpointLocked(CPReasonFlush)
}

// stagedFPI 是一张表在这一轮 checkpoint 里产生的全页镜像。
type stagedFPI struct {
	table string
	imgs  []pageImage
}

// checkpointBodyLocked 走完一整轮：刷脏页 → 写 meta → 轮转 WAL。
//
// 为什么整轮都在 CommitMu 里（而不是像以前那样把写盘挪出去）：
// 代次是全局的，只有在「没有提交能插进来」的前提下，才能保证
// 「meta 记的代次 = 页文件里实际包含的内容」。否则一个落在写盘期间的提交
// 会拿到旧代次，WAL 轮转后它的记录既不在页里、也不在日志里 —— 直接丢数据。
//
// 现在一次 checkpoint 只写**脏页**（实测几十页到几百页，几毫秒），
// 不像以前那样整表序列化 + fsync（20 万行 30–70ms），所以咬着锁是划算的。
func (s *Store) checkpointBodyLocked(reason string) error {
	t0 := hrtime.Now()

	// 1) 写前日志：页可以晚于日志（重放能补上），日志绝不能晚于页。
	if s.wal != nil {
		if err := s.wal.SyncTo(s.wal.Written()); err != nil {
			return err
		}
	}

	gen := s.appliedGen
	if s.wal != nil {
		gen = s.wal.Gen()
	}

	// 2) 把待落盘行写进页（只改池里的页，还没落盘），收集全页镜像。
	//
	// 这一步必须和「刷脏页」分开：镜像要先写进 WAL，页才能落盘。反过来
	// （先刷页、后写镜像）一旦断电把页写了一半，就没有任何东西能修它。
	var (
		staged   []stagedFPI
		flushed  []string
		pages    int
		bytes    int64
		firstErr error
	)
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		td := s.Data[key]
		if td == nil || !s.loaded[key] {
			continue
		}
		if td.pageDirty() == 0 {
			continue
		}
		fpis, err := td.pageStage()
		if err != nil {
			firstErr = err
			break
		}
		if len(fpis) > 0 {
			staged = append(staged, stagedFPI{table: t.Name, imgs: fpis})
		}
		flushed = append(flushed, key)
	}

	// 3) 全页镜像先写进 WAL：断电把头 4KB 写下去了、后 4KB 没写，
	//    恢复时靠这份镜像把整页还原，再重放逻辑记录。
	if firstErr == nil && len(staged) > 0 && s.wal != nil {
		recs := make([]walRec, 0, len(staged)*4)
		for _, st := range staged {
			for _, img := range st.imgs {
				recs = append(recs, walRec{Gen: gen, Op: opFPI, Table: st.table, Slot: -1, Page: img.page, Img: img.data})
			}
		}
		if _, err := s.wal.Append(recs); err != nil {
			return err
		}
		if err := s.wal.SyncTo(s.wal.Written()); err != nil {
			return err
		}
	}

	// 4) 刷脏页（只有到这一步，页才真的写进文件）。
	if firstErr == nil {
		for _, key := range flushed {
			td := s.Data[key]
			n, err := td.pageFlush()
			if err != nil {
				firstErr = err
				break
			}
			pages += n
			bytes += int64(n) * PageSize
		}
	}

	// 5) 写 meta：记下「磁盘已经覆盖到哪个代次」+ 每表行数。
	if err := s.writeMeta(gen, s.metaTablesLocked()); err != nil {
		return err
	}

	// 6) 收尾：清脏标记、记代次、删掉已 DROP 的表留下的数据文件、轮转 WAL。
	for _, key := range flushed {
		s.dirty[key] = false
		s.dirtyRows[key] = 0
		s.hasSnap[key] = true
		s.tableGen[key] = gen
		if td := s.Data[key]; td != nil {
			s.tableRows[key] = td.Live
		}
	}
	s.cleanupOrphanFiles()

	s.snapshotGen = gen
	s.commits = 0
	s.snapBytes = bytes
	s.written.Add(bytes)
	if firstErr != nil {
		return firstErr
	}
	if s.wal != nil {
		if err := s.wal.Rotate(gen + 1); err != nil {
			return err
		}
	}
	s.noteCheckpointLocked(reason, hrtime.Since(t0), bytes, pages)
	return nil
}

// metaTablesLocked 组装 meta.json 里的每表信息（行数用内存里的 Live，
// 没加载的表用上次落盘时记的行数）。
func (s *Store) metaTablesLocked() map[string]metaTable {
	tables := make(map[string]metaTable, len(s.Cat.Tables))
	s.loadMu.RLock()
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		rows := s.tableRows[key]
		if td := s.Data[key]; td != nil && s.loaded[key] {
			rows = td.Live
		}
		tables[key] = metaTable{ID: t.ID, Rows: rows}
	}
	s.loadMu.RUnlock()
	return tables
}

// cleanupOrphanFiles 删掉已 DROP 的表留下的数据文件。
func (s *Store) cleanupOrphanFiles() {
	live := make(map[int64]bool, len(s.Cat.Tables))
	for _, t := range s.Cat.Tables {
		live[t.ID] = true
	}
	files, err := os.ReadDir(filepath.Join(s.Dir, "tables"))
	if err != nil {
		return
	}
	for _, de := range files {
		var id int64
		name := de.Name()
		// t<6位ID>.dat（页文件）/ t<6位ID>.snp（旧版快照，见到就顺手清掉）
		if _, err := fmt.Sscanf(name, "t%d.dat", &id); err != nil {
			if _, err := fmt.Sscanf(name, "t%d.snp", &id); err != nil {
				continue
			}
		}
		if !live[id] {
			_ = os.Remove(filepath.Join(s.Dir, "tables", name))
		}
	}
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
