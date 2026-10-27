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

	// 分表快照的状态。key 一律是 cleanName(表名)。
	tableGen  map[string]int64 // 该表的数据文件已包含到哪个 WAL 代次
	tableRows map[string]int   // 该表文件里的行数（未加载时用它回答行数查询）
	hasSnap   map[string]bool  // 是否已经有数据文件
	dirty     map[string]bool  // 上一次 checkpoint 之后有没有改动
	loaded    map[string]bool  // 行是否已经在内存里
	metaGen   int64
	snapBytes int64
	loadMu    sync.RWMutex
	// loadErr 记录读取表文件时的错误（不为 nil 时启动日志会提示）。
	loadErr error

	// appliedGen 是"已经应用到内存的最高 WAL 代次"，snapshotGen 是磁盘快照
	// 覆盖到的代次。重放时只应用代次更大的记录，避免和快照重复。
	appliedGen  int64
	snapshotGen int64
	// Repaired 是启动时为对齐列结构而修复的单元格数（正常应为 0）。
	Repaired int64

	// 落盘策略与统计（可在运行中热改，见 policy.go）。
	cpCommits  atomic.Int64
	cpSeconds  atomic.Int64
	cpWALBytes atomic.Int64
	cpLastNs   atomic.Int64
	cpSince    atomic.Int64
	cpRuns     atomic.Int64
	cpStop     chan struct{}
	cpOnce     sync.Once
	cpWG       sync.WaitGroup
}

// Open 打开数据目录：加载快照 → 重放 WAL → 立刻 checkpoints 并截断 WAL。
func Open(dir string, cat *catalog.Catalog) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		Dir:       dir,
		Cat:       cat,
		Data:      map[string]*TableData{},
		tableGen:  map[string]int64{},
		tableRows: map[string]int{},
		hasSnap:   map[string]bool{},
		dirty:     map[string]bool{},
		loaded:    map[string]bool{},
	}
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
	// 先打开旧 WAL（不清空），写完快照后再轮转：
	// 这样即使 Open 之后立刻崩溃，旧记录仍在，重放不会丢数据。
	w, err := openWAL(dir, s.appliedGen+1)
	if err != nil {
		return nil, err
	}
	s.wal = w
	if err := s.Checkpoint(); err != nil {
		return nil, err
	}
	// 落盘策略在 main 里按配置文件覆盖；这里先装出厂默认，
	// 并启动「每隔 N 秒」的后台定时器。
	s.SetCheckpointPolicy(DefaultPolicy())
	s.resetCheckpointClock()
	s.StartAutoCheckpoint()
	return s, nil
}

// Close 停掉后台落盘定时器并关闭 WAL（写快照由 Checkpoint 负责）。
func (s *Store) Close() error {
	s.StopAutoCheckpoint()
	if s.wal != nil {
		return s.wal.Close()
	}
	return nil
}

// Table 按名字取表数据；行还没加载就先从磁盘读进来（按需加载）。
//
// 并发说明：加载在 loadMu 下完成，且 ensureLoaded 之后 Rows 不会再被写，
// 所以并发读同一个表不会看到半加载状态。
func (s *Store) Table(name string) *TableData {
	key := cleanName(name)
	s.loadMu.RLock()
	td := s.Data[key]
	ready := td == nil || s.loaded[key]
	s.loadMu.RUnlock()
	if ready {
		return td
	}
	s.ensureLoaded(key)
	s.loadMu.RLock()
	td = s.Data[key]
	s.loadMu.RUnlock()
	return td
}

// LoadError 返回加载表文件时遇到的错误（没有则为 nil）。
func (s *Store) LoadError() error {
	s.loadMu.RLock()
	defer s.loadMu.RUnlock()
	return s.loadErr
}

// isLoaded 判断某张表的行是否已经在内存里。
func (s *Store) isLoaded(name string) bool {
	key := cleanName(name)
	s.loadMu.RLock()
	defer s.loadMu.RUnlock()
	return s.loaded[key]
}

// ensureLoaded 把一张表的行从磁盘读进内存（幂等）。
func (s *Store) ensureLoaded(key string) {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.ensureLoadedLocked(key)
}

func (s *Store) ensureLoadedLocked(key string) {
	if s.loaded[key] {
		return
	}
	td := s.Data[key]
	if td == nil {
		return
	}
	t, ok := s.Cat.Get(td.Name)
	if !ok {
		s.loaded[key] = true
		return
	}
	rows, gen, repaired, err := readTableSnap(s.Dir, t.ID, t)
	if err != nil {
		// 文件坏了：当作空表，但保留错误信息给启动日志
		s.loadErr = err
		rows = nil
	}
	if gen > s.tableGen[key] {
		s.tableGen[key] = gen
	}
	s.Repaired += int64(repaired)
	td.Rows = rows
	s.tableRows[key] = len(rows)
	// 必须重建主键索引：否则重启后第一次写入看不到已有行，
	// 重复主键会被放过去（索引要等到下一次提交才重建）。
	td.RebuildIndex(t)
	// 自增序列也要恢复：快照里只存行，不存计数器，所以取现有数据的最大值。
	// 不恢复的话重启后 SERIAL 从 1 重新发号，会撞上已有主键。
	if si := t.SerialIdx(); si >= 0 {
		var max int64
		for _, r := range rows {
			if si < len(r) && r[si].K == types.KInt && r[si].I > max {
				max = r[si].I
			}
		}
		if max > td.Seq {
			td.Seq = max
		}
	}
	s.loaded[key] = true
}

// LoadTables 预加载一批表（语句执行前调用，避免在持 schema 锁时做磁盘 IO）。
func (s *Store) LoadTables(names []string) {
	for _, n := range names {
		key := cleanName(n)
		s.loadMu.RLock()
		need := !s.loaded[key] && s.Data[key] != nil
		s.loadMu.RUnlock()
		if need {
			s.ensureLoaded(key)
		}
	}
}

// AddTable 为新建的表准备空数据（新表没有任何行，直接算已加载）。
func (s *Store) AddTable(name string) {
	key := cleanName(name)
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.Data[key] = newTableData(name)
	s.loaded[key] = true
	s.tableRows[key] = 0
}

// DropTable 删除表数据。
func (s *Store) DropTable(name string) {
	key := cleanName(name)
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	delete(s.Data, key)
	delete(s.loaded, key)
	delete(s.tableGen, key)
	delete(s.tableRows, key)
	delete(s.hasSnap, key)
	delete(s.dirty, key)
}

// RenameTable 重命名表数据。表文件的命名基于稳定 ID，所以不需要搬文件。
func (s *Store) RenameTable(oldName, newName string) {
	oldKey, newKey := cleanName(oldName), cleanName(newName)
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	td := s.Data[oldKey]
	if td == nil {
		return
	}
	delete(s.Data, oldKey)
	td.Name = newName
	s.Data[newKey] = td
	move := func(m map[string]bool) {
		if v, ok := m[oldKey]; ok {
			m[newKey] = v
			delete(m, oldKey)
		}
	}
	if s.loaded[oldKey] {
		s.loaded[newKey] = true
		delete(s.loaded, oldKey)
	}
	if v, ok := s.tableGen[oldKey]; ok {
		s.tableGen[newKey] = v
		delete(s.tableGen, oldKey)
	}
	if v, ok := s.tableRows[oldKey]; ok {
		s.tableRows[newKey] = v
		delete(s.tableRows, oldKey)
	}
	move(s.hasSnap)
	if s.dirty[oldKey] {
		s.dirty[newKey] = true
		delete(s.dirty, oldKey)
	}
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

// rebuildAll 为已加载的表重建主键索引（未加载的表等它被读进来时再说，
// 那时 ensureLoaded 之后不需要索引也能正确扫描，索引在首次写入前会重建）。
func (s *Store) rebuildAll() {
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		if !s.isLoaded(key) {
			continue
		}
		td := s.Data[key]
		if td == nil {
			td = newTableData(t.Name)
			s.Data[key] = td
			s.loaded[key] = true
		}
		td.RebuildIndex(t)
	}
}

// RowCount 返回已提交的行数。表还没加载时直接用文件头里的行数，
// 这样 pg_class.reltuples 之类的元数据查询不会把整库拉进内存。
func (s *Store) RowCount(name string) int {
	key := cleanName(name)
	s.loadMu.RLock()
	defer s.loadMu.RUnlock()
	if s.loaded[key] {
		if td := s.Data[key]; td != nil {
			return len(td.Rows)
		}
		return 0
	}
	return s.tableRows[key]
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
	return tx.FindUniqueGroup(t, []int{col}, []types.Value{val}, self, scanTo)
}

// FindUniqueGroup 检查一组列的组合值是否已存在（主键可能是复合的）。
//
// 复合主键必须按「整组」判重：(a,b) 主键下 a 单独重复是合法的。
// 主键组走内存哈希索引；非主键的 UNIQUE 组只能全表扫描。
func (tx *Tx) FindUniqueGroup(t *catalog.Table, cols []int, vals []types.Value, self *Ref, scanTo int) (bool, error) {
	if len(cols) == 0 || len(cols) != len(vals) {
		return false, nil
	}
	// 含 NULL 的组合不参与唯一性判断（和 SQL 一致）
	for _, v := range vals {
		if v.IsNull() {
			return false, nil
		}
	}
	key := types.Key(vals)
	td := tx.st.Table(t.Name)
	if td == nil {
		return false, fmt.Errorf("relation %q does not exist", t.Name)
	}
	o := tx.ov[cleanName(t.Name)]
	isPK := sameCols(cols, t.PKIdxes())

	if isPK {
		if idx, ok := td.Index[key]; ok {
			deleted := o != nil && o.del[idx]
			if !deleted {
				conflict := true
				if o != nil {
					if nr, isUpd := o.upd[idx]; isUpd {
						if !groupKeyEqual(nr, cols, key) {
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
		if o.insDel[i] {
			continue
		}
		if !groupKeyEqual(row, cols, key) {
			continue
		}
		if self != nil && self.New && self.Slot == i {
			continue
		}
		return true, nil
	}
	for idx, row := range o.upd {
		if o.del[idx] || !groupKeyEqual(row, cols, key) {
			continue
		}
		if self != nil && !self.New && self.Slot == idx {
			continue
		}
		return true, nil
	}
	// 非主键的唯一组合：全表扫描（含已提交行）
	if !isPK {
		var conflict bool
		err := tx.Scan(t.Name, func(ref Ref, row types.Row) error {
			if self != nil && ref.New == self.New && ref.Slot == self.Slot {
				return nil
			}
			if groupKeyEqual(row, cols, key) {
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

// groupKeyEqual 判断某行的这组列编码后是否等于 key。
func groupKeyEqual(row types.Row, cols []int, key string) bool {
	vals := make([]types.Value, len(cols))
	for i, ci := range cols {
		if ci >= len(row) {
			return false
		}
		vals[i] = row[ci]
		if vals[i].IsNull() {
			return false
		}
	}
	return types.Key(vals) == key
}

// sameCols 判断两个列位置列表是否相同（顺序敏感）。
func sameCols(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Rollback 丢弃整个事务。
func (tx *Tx) Rollback() {
	tx.ov = map[string]*overlay{}
	tx.seq = map[string]int64{}
	tx.n = 0
}

// Commit 提交事务：写 WAL → 应用写集 → （必要时 checkpoint）→ 等 fsync。
//
// 关键点：fsync **不在** CommitMu 里做。这样并发事务可以先把日志写进文件、
// 释放锁，再由第一个等到的人做一次 fsync 覆盖所有人的记录（组提交）。
// 语义上等价于 PostgreSQL 的 synchronous_commit=on：客户端收到成功时数据已落盘。
func (tx *Tx) Commit() error {
	if tx.n == 0 && len(tx.seq) == 0 {
		return nil
	}
	tx.st.CommitMu.Lock()
	seq, err := tx.commitLocked()
	tx.st.CommitMu.Unlock()
	if err != nil {
		return err
	}
	if seq <= 0 || tx.st.wal == nil {
		return nil
	}
	return tx.st.wal.SyncTo(seq)
}

// commitLocked 在持有 CommitMu 时完成"写日志 + 改内存"。
// 返回本批记录的 WAL 序号（0 表示没有需要落盘的记录）。
func (tx *Tx) commitLocked() (int64, error) {
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
			return 0, fmt.Errorf("relation %q does not exist", name)
		}
		newRows := make([]types.Row, 0, len(td.Rows)+len(o.ins))
		for i, row := range td.Rows {
			if o.del[i] {
				recs = append(recs, walRec{Tx: tx.ID, Op: opDel, Table: td.Name, Old: row})
				continue
			}
			if nr, ok := o.upd[i]; ok {
				recs = append(recs, walRec{Tx: tx.ID, Op: opUpd, Table: td.Name, Old: row, New: nr})
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
			recs = append(recs, walRec{Tx: tx.ID, Op: opIns, Table: td.Name, New: row})
			newRows = append(newRows, row)
		}
		c := &change{table: name, rows: newRows}
		if v, ok := tx.seq[name]; ok {
			c.seq, c.seqSet = v, true
		}
		changes = append(changes, c)
	}

	recs = append(recs, walRec{Tx: tx.ID, Op: opCommit})
	var seq int64
	if len(recs) > 1 && tx.st.wal != nil {
		s, err := tx.st.wal.Append(recs)
		if err != nil {
			return 0, err
		}
		seq = s
		tx.st.appliedGen = tx.st.wal.Gen()
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
		key := cleanName(c.table)
		tx.st.loadMu.Lock()
		tx.st.dirty[key] = true
		tx.st.tableRows[key] = len(c.rows)
		tx.st.loadMu.Unlock()
	}

	tx.ov = map[string]*overlay{}
	tx.seq = map[string]int64{}
	tx.n = 0

	tx.st.commits++
	tx.st.cpSince.Add(1)
	if tx.st.needCheckpointLocked() {
		if err := tx.st.checkpointLocked(); err != nil {
			return 0, err
		}
		tx.st.noteCheckpointLocked()
		// checkpoint 已经把数据写进快照，这批记录不需要再等 fsync
		return 0, nil
	}
	return seq, nil
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
