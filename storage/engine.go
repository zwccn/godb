// Package storage 是内存行存 + WAL/快照持久化的存储引擎，并实现事务写集。
package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"godb/catalog"
	"godb/types"
)

// Ref 定位一行：New 表示事务内新插入的行，否则是已提交行在堆中的行号。
type Ref struct {
	New  bool
	Slot int
}

// Store 持有全部表数据与 WAL。
//
// 并发约定：
//   - SchemaMu：语句执行期间持读锁，DDL 持写锁。加锁顺序永远是 SchemaMu → CommitMu。
//   - CommitMu：串行化提交（写集应用 + WAL fsync + checkpoint）。
//   - txSeq 用原子自增，Begin 可并发调用。
type Store struct {
	Dir  string
	Cat  *catalog.Catalog
	Data map[string]*TableData

	SchemaMu sync.RWMutex
	CommitMu sync.Mutex

	wal     *Wal
	commits int
	txSeq   int64
}

// Open 打开数据目录：加载快照 → 重放 WAL → 立刻 checkpoints 并截断 WAL。
func Open(dir string, cat *catalog.Catalog) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Cat: cat, Data: map[string]*TableData{}}
	for _, t := range cat.Tables {
		s.Data[cleanName(t.Name)] = newTableData(t.Name)
	}
	if err := s.loadSnapshot(); err != nil {
		return nil, err
	}
	if err := s.replayWAL(); err != nil {
		return nil, err
	}
	s.rebuildAll()
	if err := s.Checkpoint(); err != nil {
		return nil, err
	}
	w, err := OpenWAL(dir)
	if err != nil {
		return nil, err
	}
	s.wal = w
	return s, nil
}

// Close 关闭 WAL（写快照由 Checkpoint 负责）。
func (s *Store) Close() error {
	if s.wal != nil {
		return s.wal.Close()
	}
	return nil
}

// Table 按名字取表数据。
func (s *Store) Table(name string) *TableData {
	return s.Data[cleanName(name)]
}

// AddTable 为新建的表准备空数据。
func (s *Store) AddTable(name string) {
	s.Data[cleanName(name)] = newTableData(name)
}

// DropTable 删除表数据。
func (s *Store) DropTable(name string) {
	delete(s.Data, cleanName(name))
}

// RenameTable 重命名表数据。
func (s *Store) RenameTable(oldName, newName string) {
	td := s.Data[cleanName(oldName)]
	if td == nil {
		return
	}
	delete(s.Data, cleanName(oldName))
	td.Name = newName
	s.Data[cleanName(newName)] = td
}

// RebuildIndex 重建某张表的主键索引。
func (s *Store) RebuildIndex(name string) {
	td := s.Table(name)
	if td == nil {
		return
	}
	if t, ok := s.Cat.Get(name); ok {
		td.RebuildIndex(t)
	}
}

func (s *Store) rebuildAll() {
	for _, t := range s.Cat.Tables {
		td := s.Table(t.Name)
		if td == nil {
			td = newTableData(t.Name)
			s.Data[cleanName(t.Name)] = td
		}
		td.RebuildIndex(t)
	}
}

// RowCount 返回已提交的行数。
func (s *Store) RowCount(name string) int {
	td := s.Table(name)
	if td == nil {
		return 0
	}
	return len(td.Rows)
}

// Begin 开启一个事务。事务 ID 原子自增，可并发调用。
func (s *Store) Begin() *Tx {
	id := atomic.AddInt64(&s.txSeq, 1)
	return &Tx{
		ID:  id,
		st:  s,
		ov:  map[string]*overlay{},
		seq: map[string]int64{},
	}
}

// ---------- 事务 ----------

type overlay struct {
	ins    []types.Row
	insUpd map[int]types.Row
	insDel map[int]bool
	upd    map[int]types.Row
	del    map[int]bool
}

func newOverlay() *overlay {
	return &overlay{
		insUpd: map[int]types.Row{},
		insDel: map[int]bool{},
		upd:    map[int]types.Row{},
		del:    map[int]bool{},
	}
}

func (o *overlay) clone() *overlay {
	c := newOverlay()
	c.ins = append([]types.Row(nil), o.ins...)
	for k, v := range o.insUpd {
		c.insUpd[k] = v
	}
	for k, v := range o.insDel {
		c.insDel[k] = v
	}
	for k, v := range o.upd {
		c.upd[k] = v
	}
	for k, v := range o.del {
		c.del[k] = v
	}
	return c
}

// Tx 是事务上下文：所有写操作先落到私有写集，提交时一次性应用到存储并写 WAL。
type Tx struct {
	ID  int64
	st  *Store
	ov  map[string]*overlay
	seq map[string]int64
	n   int
}

// Changes 返回事务内已修改的行数。
func (tx *Tx) Changes() int { return tx.n }

// Store 返回底层存储。
func (tx *Tx) Store() *Store { return tx.st }

func (tx *Tx) ovl(table string) *overlay {
	k := cleanName(table)
	o, ok := tx.ov[k]
	if !ok {
		o = newOverlay()
		tx.ov[k] = o
	}
	return o
}

// Scan 遍历表在事务视角下的全部行（已提交 + 本事务未提交改动）。
func (tx *Tx) Scan(table string, fn func(ref Ref, row types.Row) error) error {
	td := tx.st.Table(table)
	if td == nil {
		return fmt.Errorf("relation %q does not exist", table)
	}
	o := tx.ov[cleanName(table)]
	for i, row := range td.Rows {
		if o != nil {
			if o.del[i] {
				continue
			}
			if nr, ok := o.upd[i]; ok {
				row = nr
			}
		}
		if err := fn(Ref{Slot: i}, row); err != nil {
			return err
		}
	}
	if o != nil {
		for i, row := range o.ins {
			if o.insDel[i] {
				continue
			}
			if nr, ok := o.insUpd[i]; ok {
				row = nr
			}
			if err := fn(Ref{New: true, Slot: i}, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// Get 按引用取一行（事务视角）。
func (tx *Tx) Get(table string, ref Ref) (types.Row, bool) {
	td := tx.st.Table(table)
	if td == nil {
		return nil, false
	}
	o := tx.ov[cleanName(table)]
	if ref.New {
		if o == nil || ref.Slot >= len(o.ins) || o.insDel[ref.Slot] {
			return nil, false
		}
		if nr, ok := o.insUpd[ref.Slot]; ok {
			return nr, true
		}
		return o.ins[ref.Slot], true
	}
	if ref.Slot >= len(td.Rows) {
		return nil, false
	}
	if o != nil {
		if o.del[ref.Slot] {
			return nil, false
		}
		if nr, ok := o.upd[ref.Slot]; ok {
			return nr, true
		}
	}
	return td.Rows[ref.Slot], true
}

// Insert 在事务内插入一行。
func (tx *Tx) Insert(table string, row types.Row) (Ref, error) {
	if tx.st.Table(table) == nil {
		return Ref{}, fmt.Errorf("relation %q does not exist", table)
	}
	o := tx.ovl(table)
	o.ins = append(o.ins, row)
	tx.n++
	return Ref{New: true, Slot: len(o.ins) - 1}, nil
}

// Update 在事务内更新一行。
func (tx *Tx) Update(table string, ref Ref, row types.Row) error {
	o := tx.ovl(table)
	if ref.New {
		if _, existed := o.insUpd[ref.Slot]; !existed {
			tx.n++
		}
		o.insUpd[ref.Slot] = row
		return nil
	}
	if _, existed := o.upd[ref.Slot]; !existed {
		tx.n++
	}
	o.upd[ref.Slot] = row
	return nil
}

// Delete 在事务内删除一行。
func (tx *Tx) Delete(table string, ref Ref) error {
	o := tx.ovl(table)
	if ref.New {
		if o.insDel[ref.Slot] {
			return nil
		}
		o.insDel[ref.Slot] = true
	} else {
		if o.del[ref.Slot] {
			return nil
		}
		o.del[ref.Slot] = true
	}
	tx.n++
	return nil
}

// NextSeq 分配一个自增值。
func (tx *Tx) NextSeq(table string) int64 {
	k := cleanName(table)
	v, ok := tx.seq[k]
	if !ok {
		td := tx.st.Table(table)
		if td != nil {
			v = td.Seq
		}
	}
	v++
	tx.seq[k] = v
	return v
}

// ResetSeq 把自增序列重置为 0（TRUNCATE 用）。
func (tx *Tx) ResetSeq(table string) {
	k := cleanName(table)
	tx.seq[k] = 0
}

// OverlayInsLen 返回本事务在表 t 上已插入的行数。
// 语句开始前取一次，配合 FindUnique 的 scanTo 参数，可以只扫描「之前语句」插入的行，
// 语句自身插入的重复由调用方的局部集合负责，从而把大批量 INSERT 从 O(n²) 降到 O(n)。
func (tx *Tx) OverlayInsLen(table string) int {
	o := tx.ov[cleanName(table)]
	if o == nil {
		return 0
	}
	return len(o.ins)
}

// FindUnique 检查某列上是否已存在该值（排除 self 指向的行）。
// scanTo 限制扫描 overlay 插入行的范围（只扫 [0,scanTo)），传 -1 表示全扫。
func (tx *Tx) FindUnique(t *catalog.Table, col int, val types.Value, self *Ref, scanTo int) (bool, error) {
	if col < 0 || val.IsNull() {
		return false, nil
	}
	key := types.Key([]types.Value{val})
	td := tx.st.Table(t.Name)
	if td == nil {
		return false, fmt.Errorf("relation %q does not exist", t.Name)
	}
	o := tx.ov[cleanName(t.Name)]

	if idx, ok := td.Index[key]; ok {
		if col == t.PKIdx() {
			deleted := o != nil && o.del[idx]
			if !deleted {
				conflict := true
				if o != nil {
					if nr, isUpd := o.upd[idx]; isUpd {
						if nr[col].K != val.K || types.Key([]types.Value{nr[col]}) != key {
							conflict = false
						}
					}
				}
				if self != nil && !self.New && self.Slot == idx {
					conflict = false
				}
				if conflict {
					return true, nil
				}
			}
		}
	}
	if o == nil {
		return false, nil
	}
	limit := len(o.ins)
	if scanTo >= 0 && scanTo < limit {
		limit = scanTo
	}
	for i := 0; i < limit; i++ {
		row := o.ins[i]
		if col >= len(row) {
			continue
		}
		if o.insDel[i] {
			continue
		}
		if !types.Same(row[col], val) {
			continue
		}
		if self != nil && self.New && self.Slot == i {
			continue
		}
		return true, nil
	}
	for idx, row := range o.upd {
		if col >= len(row) {
			continue
		}
		if !types.Same(row[col], val) {
			continue
		}
		if self != nil && !self.New && self.Slot == idx {
			continue
		}
		if o.del[idx] {
			continue
		}
		return true, nil
	}
	// 非主键的唯一列：全表扫描（含已提交行）
	if col != t.PKIdx() {
		var conflict bool
		err := tx.Scan(t.Name, func(ref Ref, row types.Row) error {
			if col >= len(row) {
				return nil
			}
			if self != nil && ref.New == self.New && ref.Slot == self.Slot {
				return nil
			}
			if types.Same(row[col], val) {
				conflict = true
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		if conflict {
			return true, nil
		}
	}
	return false, nil
}

// Rollback 丢弃整个事务。
func (tx *Tx) Rollback() {
	tx.ov = map[string]*overlay{}
	tx.seq = map[string]int64{}
	tx.n = 0
}

// Commit 写 WAL（fsync）后把写集应用到内存，最后按需 checkpoint。
// 提交临界区用 CommitMu 串行化：不同表的事务可以并行执行，但提交必须排队。
func (tx *Tx) Commit() error {
	if tx.n == 0 && len(tx.seq) == 0 {
		return nil
	}
	tx.st.CommitMu.Lock()
	defer tx.st.CommitMu.Unlock()
	names := make([]string, 0, len(tx.ov))
	for k := range tx.ov {
		names = append(names, k)
	}
	sort.Strings(names)

	type change struct {
		table  string
		rows   []types.Row
		seq    int64
		seqSet bool
	}
	var (
		changes []*change
		recs    []walRec
	)
	for _, name := range names {
		o := tx.ov[name]
		td := tx.st.Table(name)
		if td == nil {
			return fmt.Errorf("relation %q does not exist", name)
		}
		newRows := make([]types.Row, 0, len(td.Rows)+len(o.ins))
		for i, row := range td.Rows {
			if o.del[i] {
				recs = append(recs, walRec{Tx: tx.ID, Op: "del", Table: td.Name, Old: encodeRow(row)})
				continue
			}
			if nr, ok := o.upd[i]; ok {
				recs = append(recs, walRec{Tx: tx.ID, Op: "upd", Table: td.Name, Old: encodeRow(row), New: encodeRow(nr)})
				newRows = append(newRows, nr)
				continue
			}
			newRows = append(newRows, row)
		}
		for i, row := range o.ins {
			if o.insDel[i] {
				continue
			}
			if nr, ok := o.insUpd[i]; ok {
				row = nr
			}
			recs = append(recs, walRec{Tx: tx.ID, Op: "ins", Table: td.Name, New: encodeRow(row)})
			newRows = append(newRows, row)
		}
		c := &change{table: name, rows: newRows}
		if v, ok := tx.seq[name]; ok {
			c.seq, c.seqSet = v, true
		}
		changes = append(changes, c)
	}

	recs = append(recs, walRec{Tx: tx.ID, Op: "commit"})
	if len(recs) > 1 {
		if err := tx.st.wal.Append(recs); err != nil {
			return fmt.Errorf("写 WAL 失败: %w", err)
		}
	}

	for _, c := range changes {
		td := tx.st.Table(c.table)
		if td == nil {
			continue
		}
		td.Rows = c.rows
		if c.seqSet {
			if c.seq == 0 {
				td.Seq = 0
			} else if c.seq > td.Seq {
				td.Seq = c.seq
			}
		}
		if t, ok := tx.st.Cat.Get(c.table); ok {
			td.RebuildIndex(t)
		}
	}

	tx.ov = map[string]*overlay{}
	tx.seq = map[string]int64{}
	tx.n = 0

	tx.st.commits++
	if tx.st.commits >= 128 {
		return tx.st.checkpointLocked()
	}
	return nil
}

// ---------- 事务内语句回滚 ----------

// TxSnapshot 是写集的浅拷贝（行按不可变方式使用，拷贝即可靠）。
type TxSnapshot struct {
	ov  map[string]*overlay
	seq map[string]int64
	n   int
}

// Mark 记录语句开始前的状态。
func (tx *Tx) Mark() *TxSnapshot {
	m := &TxSnapshot{ov: make(map[string]*overlay, len(tx.ov)), seq: map[string]int64{}, n: tx.n}
	for k, o := range tx.ov {
		m.ov[k] = o.clone()
	}
	for k, v := range tx.seq {
		m.seq[k] = v
	}
	return m
}

// RestoreTo 回滚到某个语句前状态（用于语句级原子性）。
func (tx *Tx) RestoreTo(m *TxSnapshot) {
	tx.ov = m.ov
	tx.seq = m.seq
	tx.n = m.n
}

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return filepath.Clean(s.Dir) }
