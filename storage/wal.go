package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"godb/types"
)

const walFile = "wal.log"

// walRec 是一条 WAL 记录。行操作记录旧/新行镜像，提交记录作为事务边界。
type walRec struct {
	Tx    int64         `json:"tx"`
	Op    string        `json:"op"` // ins | upd | del | commit
	Table string        `json:"table,omitempty"`
	Old   []interface{} `json:"old,omitempty"`
	New   []interface{} `json:"new,omitempty"`
}

// Wal 是追加写的日志文件，每条记录一行 JSON，提交时 fsync。
type Wal struct {
	path string
	f    *os.File
	enc  *json.Encoder
}

// OpenWAL 打开（或创建）WAL 文件用于追加。
func OpenWAL(dir string) (*Wal, error) {
	path := filepath.Join(dir, walFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &Wal{path: path, f: f, enc: enc}, nil
}

// Append 追加一批记录并落盘（一次 fsync 覆盖整批）。
func (w *Wal) Append(recs []walRec) error {
	for i := range recs {
		if err := w.enc.Encode(&recs[i]); err != nil {
			return err
		}
	}
	return w.f.Sync()
}

// Close 关闭文件。
func (w *Wal) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Truncate 清空日志（checkpoint 之后调用）。
func (w *Wal) Truncate() error {
	if err := w.Close(); err != nil {
		return err
	}
	if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	nw, err := OpenWAL(filepath.Dir(w.path))
	if err != nil {
		return err
	}
	w.f, w.enc, w.path = nw.f, nw.enc, nw.path
	return nil
}

// replayWAL 重放日志：只有带 commit 标记的事务会被应用，未提交事务直接丢弃。
func (s *Store) replayWAL() error {
	path := filepath.Join(s.Dir, walFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.UseNumber()

	type pendingTx struct {
		recs []walRec
	}
	pending := map[int64]*pendingTx{}
	var order []int64

	for {
		var rec walRec
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// 尾部写坏（崩溃时只写了一部分）：丢弃剩余内容
			break
		}
		if rec.Op == "commit" {
			if p, ok := pending[rec.Tx]; ok {
				for _, r := range p.recs {
					if err := s.applyRec(r); err != nil {
						return fmt.Errorf("重放 WAL 失败: %w", err)
					}
				}
				delete(pending, rec.Tx)
			}
			continue
		}
		if _, ok := pending[rec.Tx]; !ok {
			pending[rec.Tx] = &pendingTx{}
			order = append(order, rec.Tx)
		}
		pending[rec.Tx].recs = append(pending[rec.Tx].recs, rec)
	}
	// 未提交事务不应用（原子性）
	_ = order
	return nil
}

func (s *Store) applyRec(rec walRec) error {
	td := s.Table(rec.Table)
	if td == nil {
		return nil
	}
	t, ok := s.Cat.Get(rec.Table)
	if !ok {
		return nil
	}
	switch rec.Op {
	case "ins":
		row, err := decodeRow(t, rec.New)
		if err != nil {
			return err
		}
		td.Rows = append(td.Rows, row)
	case "del":
		row, err := decodeRow(t, rec.Old)
		if err != nil {
			return err
		}
		for i := range td.Rows {
			if types.RowSame(td.Rows[i], row) {
				td.Rows = append(td.Rows[:i], td.Rows[i+1:]...)
				break
			}
		}
	case "upd":
		oldRow, err := decodeRow(t, rec.Old)
		if err != nil {
			return err
		}
		newRow, err := decodeRow(t, rec.New)
		if err != nil {
			return err
		}
		for i := range td.Rows {
			if types.RowSame(td.Rows[i], oldRow) {
				td.Rows[i] = newRow
				break
			}
		}
	}
	return nil
}
