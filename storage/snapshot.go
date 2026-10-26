package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"godb/types"
)

const snapshotFile = "snapshot.json"

type tableSnap struct {
	Rows [][]interface{} `json:"rows"`
	Seq  int64           `json:"seq,omitempty"`
}

type snapFile struct {
	// Generation 是这份快照覆盖到的 WAL 代次：代次 <= Generation 的记录
	// 都已经体现在 Rows 里，重放时必须跳过。
	Generation int64                 `json:"generation"`
	Tables     map[string]*tableSnap `json:"tables"`
}

func (s *Store) snapshotPath() string { return filepath.Join(s.Dir, snapshotFile) }

// loadSnapshot 读快照。行宽和当前 catalog 不一致时按 castRow 的规则修复，
// 不再让整个库因为一个 ALTER TABLE 之后的小概率崩溃而打不开。
func (s *Store) loadSnapshot() error {
	f, err := os.Open(s.snapshotPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.UseNumber()
	var snap snapFile
	if err := dec.Decode(&snap); err != nil {
		return fmt.Errorf("解析快照失败: %w", err)
	}
	s.snapshotGen = snap.Generation
	s.appliedGen = snap.Generation
	for name, ts := range snap.Tables {
		td := s.Table(name)
		if td == nil {
			// 快照里有、catalog 里没有的表（崩溃在 DROP TABLE 之后）：忽略
			continue
		}
		t, ok := s.Cat.Get(name)
		if !ok {
			continue
		}
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
		td.Seq = ts.Seq
	}
	return nil
}

// Checkpoint 把当前内存数据整体写成快照，然后轮转 WAL。
func (s *Store) Checkpoint() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	return s.checkpointLocked()
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
	return s.checkpointLocked()
}

// checkpointLocked 在已持有 CommitMu 时执行 checkpoint。
//
// 顺序很关键：先把快照（含 Generation）原子落盘，再轮转 WAL。
// 崩溃在两者之间也没关系——新 WAL 里残留的旧代次记录会被重放逻辑按
// Generation 过滤掉，不会重复应用。
func (s *Store) checkpointLocked() error {
	gen := s.appliedGen
	snap := snapFile{Generation: gen, Tables: map[string]*tableSnap{}}
	for name, td := range s.Data {
		snap.Tables[name] = &tableSnap{Rows: encodeRows(td.Rows), Seq: td.Seq}
	}
	b, err := json.Marshal(&snap)
	if err != nil {
		return err
	}
	path := s.snapshotPath()
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	s.snapshotGen = gen
	s.commits = 0
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
}

// Info 返回持久化层的当前状态。
func (s *Store) Info() SnapshotInfo {
	info := SnapshotInfo{Generation: s.snapshotGen, Repaired: s.Repaired}
	for _, td := range s.Data {
		info.Rows += len(td.Rows)
	}
	if st, err := os.Stat(s.snapshotPath()); err == nil {
		info.Bytes = st.Size()
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

func parseRFC3339(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

var _ = filepath.Join
