package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"godb/types"
)

const snapshotFile = "snapshot.json"

type tableSnap struct {
	Rows [][]interface{} `json:"rows"`
	Seq  int64           `json:"seq,omitempty"`
}

type snapFile struct {
	Tables map[string]*tableSnap `json:"tables"`
}

func (s *Store) snapshotPath() string { return filepath.Join(s.Dir, snapshotFile) }

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
	for name, ts := range snap.Tables {
		td := s.Table(name)
		if td == nil {
			// 快照里有、catalog 里没有的表（例如崩溃在 DROP 之后）：忽略
			continue
		}
		t, ok := s.Cat.Get(name)
		if !ok {
			continue
		}
		rows := make([]types.Row, 0, len(ts.Rows))
		for _, raw := range ts.Rows {
			row, err := decodeRow(t, raw)
			if err != nil {
				return fmt.Errorf("表 %s 的快照行解码失败: %w", name, err)
			}
			rows = append(rows, row)
		}
		td.Rows = rows
		td.Seq = ts.Seq
	}
	return nil
}

// Checkpoint 把当前内存数据整体写成快照，然后清空 WAL。
func (s *Store) Checkpoint() error {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	return s.checkpointLocked()
}

// checkpointLocked 在已持有 CommitMu 时执行 checkpoint。
func (s *Store) checkpointLocked() error {
	snap := snapFile{Tables: map[string]*tableSnap{}}
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
	s.commits = 0
	if s.wal != nil {
		return s.wal.Truncate()
	}
	if err := os.Remove(filepath.Join(s.Dir, walFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
