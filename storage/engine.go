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

	// cpMu 只串行化「写快照文件」这一段（在 CommitMu 之外拿）。
	// cpBusy 保证同一时刻只有一个 checkpoint 在拍快照 + 写盘：
	// 后来的直接跳过，等下一轮 —— 否则两个 checkpoint 可能乱序覆盖文件。
	cpMu   sync.Mutex
	cpBusy atomic.Bool
	// dirtyRows 是「自上次落盘以来这张表改了多少行」。
	// 只改一两行就重写整张表的代价太大（实测 2 万行的表改一行要写 0.97 MB），
	// 所以攒够比例再落盘，中间靠 WAL 兜底。
	dirtyRows map[string]int64
	// written 是累计写盘字节数（给 CHECKPOINT / 状态页看）
	written atomic.Int64
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
		dirtyRows: map[string]int64{},
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
			return td.Live // 槽位数组可能带空洞，Live 才是真实行数
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

	// User 是发起本事务的登录名（current_user 回显；空串表示未知）。
	User string

	// lastSeq / hasLastSeq 记录本事务最后一次 nextval 分配的值（lastval() 用）。
	lastSeq    int64
	hasLastSeq bool
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
	it := tx.ScanIter(table)
	for {
		ref, row, ok := it.Next()
		if !ok {
			return it.Err()
		}
		if err := fn(ref, row); err != nil {
			return err
		}
	}
}

// RowIter 是表扫描的游标：上层要一行才走一步，配合 LIMIT 可以提前停，
// 不必把整张表读进内存再扔掉（60 万行的表 LIMIT 100 会从 40ms 降到亚毫秒）。
type RowIter struct {
	td   *TableData
	o    *overlay
	slot int  // 已提交部分的槽位游标
	ins  int  // overlay 插入部分的游标；-1 表示还没开始 / 已结束
	done bool
	err  error
}

// ScanIter 打开一个流式扫描游标。
func (tx *Tx) ScanIter(table string) *RowIter {
	td := tx.st.Table(table)
	it := &RowIter{td: td, ins: -1}
	if td == nil {
		it.err = fmt.Errorf("relation %q does not exist", table)
		it.done = true
		return it
	}
	it.o = tx.ov[cleanName(table)]
	return it
}

// Err 返回扫描过程中遇到的错误（表不存在）。
func (it *RowIter) Err() error { return it.err }

// Next 取下一行；ok=false 表示结束。
func (it *RowIter) Next() (Ref, types.Row, bool) {
	if it.done || it.err != nil {
		return Ref{}, nil, false
	}
	// 1) 已提交的槽位数组
	for it.td != nil && it.slot < len(it.td.Rows) {
		i := it.slot
		it.slot++
		row := it.td.Rows[i]
		if row == nil {
			continue // 空槽（被删除后还没被复用）
		}
		if it.o != nil {
			if it.o.del[i] {
				continue
			}
			if nr, ok := it.o.upd[i]; ok {
				row = nr
			}
		}
		return Ref{Slot: i}, row, true
	}
	// 2) 本事务 overlay 里新插入的行
	if it.o != nil {
		if it.ins < 0 {
			it.ins = 0
		}
		for it.ins < len(it.o.ins) {
			i := it.ins
			it.ins++
			if it.o.insDel[i] {
				continue
			}
			row := it.o.ins[i]
			if nr, ok := it.o.insUpd[i]; ok {
				row = nr
			}
			return Ref{New: true, Slot: i}, row, true
		}
	}
	it.done = true
	return Ref{}, nil, false
}

// HasOverlay 表上是否有本事务的写集。覆盖索引扫描用它判断能不能免归并。
func (tx *Tx) HasOverlay(table string) bool {
	return tx.ov[cleanName(table)] != nil
}

// CountLive 返回表在事务视角下的行数（已提交 Live + 本事务 overlay 净变化）。
// `SELECT count(*) FROM t` 无 WHERE 时直接用它，不扫表。
func (tx *Tx) CountLive(table string) int64 {
	td := tx.st.Table(table)
	if td == nil {
		return 0
	}
	n := int64(td.Live)
	o := tx.ov[cleanName(table)]
	if o != nil {
		for i := range o.ins {
			if !o.insDel[i] {
				n++
			}
		}
		for slot := range o.del {
			if slot < len(td.Rows) && td.Rows[slot] != nil {
				n--
			}
		}
	}
	return n
}

// IndexEndpoint 取索引端点（max=false 最小，max=true 最大）上某列的值。
// 用于 `min(col)` / `max(col)`：col 必须是索引首列，树序即列序；
// 会与本事务 overlay 里新插/改过的行合并比较。找不到返回 ok=false（空表或全 NULL）。
func (tx *Tx) IndexEndpoint(t *catalog.Table, ixName string, col int, max bool) (types.Value, bool, error) {
	td := tx.st.Table(t.Name)
	if td == nil {
		return types.Null, false, fmt.Errorf("relation %q does not exist", t.Name)
	}
	tree := td.pk
	if ixName != "" {
		tree = td.IndexTree(ixName)
	}
	var best types.Value
	var has bool
	consider := func(v types.Value) {
		if v.IsNull() {
			return
		}
		if !has {
			best, has = v, true
			return
		}
		c, err := types.Compare(v, best)
		if err != nil {
			return
		}
		if (!max && c < 0) || (max && c > 0) {
			best = v
		}
	}
	if tree != nil {
		cur := tree.first()
		if max {
			cur = tree.last()
		}
		for i := 0; i < 64; i++ {
			var slot int
			var ok bool
			if max {
				_, slot, ok = cur.prev()
			} else {
				_, slot, ok = cur.next()
			}
			if !ok {
				break
			}
			row, exists := tx.Get(t.Name, Ref{Slot: slot})
			if !exists {
				continue
			}
			if col < 0 || col >= len(row) {
				break
			}
			consider(row[col])
			break // 索引首列有序，第一个存活行就是端点
		}
	}
	// overlay 里的插入/更新可能更优
	o := tx.ov[cleanName(t.Name)]
	if o != nil {
		for i, row := range o.ins {
			if o.insDel[i] {
				continue
			}
			if nr, ok := o.insUpd[i]; ok {
				row = nr
			}
			if col < len(row) {
				consider(row[col])
			}
		}
		for slot, row := range o.upd {
			if o.del[slot] {
				continue
			}
			if col < len(row) {
				consider(row[col])
			}
		}
	}
	return best, has, nil
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
	if td.Rows[ref.Slot] == nil {
		return nil, false // 已删除的空槽
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
	tx.lastSeq, tx.hasLastSeq = v, true
	return v
}

// SeqCurrent 返回自增计数器的当前值（不分配新值），
// ok=false 表示这张表没有自增列。currval() 用它。
func (tx *Tx) SeqCurrent(table string) (int64, bool) {
	k := cleanName(table)
	// 优先读本事务已分配但还没提交的值
	if v, ok := tx.seq[k]; ok {
		return v, true
	}
	td := tx.st.Table(table)
	if td == nil {
		return 0, false
	}
	return td.Seq, true
}

// LastSeqValue 返回本事务最后一次 nextval 分配的值。lastval() 用它。
func (tx *Tx) LastSeqValue() (int64, bool) {
	return tx.lastSeq, tx.hasLastSeq
}

// SetLastSeq 把一个会话级的「最后一次序列值」带进新事务。
// lastval() 在 PostgreSQL 里是会话级的，跨语句有效，所以每次开事务都要继承。
func (tx *Tx) SetLastSeq(v int64) {
	tx.lastSeq, tx.hasLastSeq = v, true
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

// IndexHit 是索引查找命中的一行。Ref 是事务视角的（New 表示本事务新插入的行），
// 判重时要拿它和 self 比较。
type IndexHit struct {
	Ref Ref
	Row types.Row
}

// IndexLookup 用用户建的二级索引查一个键对应的全部行。
//
// 索引里存的是「已提交态」的行号，所以必须逐个用 tx.Get 取事务视角的行，
// 再重新校验一次键值：本事务可能已经把这行改成了别的值。反过来，本事务
// 新插入或改了键的行不在索引里，要单独从 overlay 补一遍。少了任何一边都会
// 出现「事务里刚插的行自己查不到」或「已删的行又被索引捞回来」。
func (tx *Tx) IndexLookup(t *catalog.Table, ixName string, cols []int, key string) []IndexHit {
	td := tx.st.Table(t.Name)
	if td == nil {
		return nil
	}
	var out []IndexHit
	// seen 记录「已经按堆索引处理过」的行号，避免 upd 分支重复产出。
	seen := make(map[int]bool, 4)
	if tree := td.IndexTree(ixName); tree != nil {
		for _, slot := range tree.Get(key) {
			seen[slot] = true
			row, ok := tx.Get(t.Name, Ref{Slot: slot})
			if !ok {
				continue
			}
			if !groupKeyEqual(row, cols, key) {
				continue
			}
			out = append(out, IndexHit{Ref: Ref{Slot: slot}, Row: row})
		}
	}
	o := tx.ov[cleanName(t.Name)]
	if o == nil {
		return out
	}
	for i, row := range o.ins {
		if o.insDel[i] {
			continue
		}
		if nr, ok := o.insUpd[i]; ok {
			row = nr
		}
		if !groupKeyEqual(row, cols, key) {
			continue
		}
		out = append(out, IndexHit{Ref: Ref{New: true, Slot: i}, Row: row})
	}
	// 被本事务改了键的行：新键在堆索引里查不到，只能从 overlay 补。
	var slots []int
	for slot := range o.upd {
		if !seen[slot] {
			slots = append(slots, slot)
		}
	}
	if len(slots) > 1 {
		sort.Ints(slots) // map 遍历顺序随机，排一下让结果可复现
	}
	for _, slot := range slots {
		if o.del[slot] {
			continue
		}
		row := o.upd[slot]
		if !groupKeyEqual(row, cols, key) {
			continue
		}
		out = append(out, IndexHit{Ref: Ref{Slot: slot}, Row: row})
	}
	return out
}

// IndexRange 用索引扫一个键区间，返回事务视角的行（`BETWEEN`、`>`、`LIKE 'x%'` 都走它）。
func (tx *Tx) IndexRange(t *catalog.Table, ixName string, cols []int, r KeyRange) []IndexHit {
	return tx.IndexRanges(t, ixName, cols, nil, []KeyRange{r})
}

// IndexScanner 是索引区间扫描的**游标**：按索引键升序逐行返回事务视角的行。
//
// 和 IndexRanges 的区别只在「什么时候算」：IndexRanges 一次把整个区间扫完塞进切片，
// 游标是调用方要一行才走一步。带 LIMIT 的查询必须用游标 —— 取够 10 行就撒手，
// 两万行的区间只碰前面几个叶子，而不是全物化出来扔掉 19990 行。
//
// 事务里有写集时退回物化：本事务插入/改键的行要和树结果归并，边扫边归并的
// 状态机不值得为「很少发生的场景」写。自动提交的查询（绝大多数）没有写集，
// 走的是纯游标路径。
type IndexScanner struct {
	tx   *Tx
	tbl  *catalog.Table
	cols []int
	rs   []KeyRange
	ri   int // 当前区间下标
	desc bool
	cur  *btreeCursor
	cr   KeyRange // 当前区间的副本（cur 换区间时要跟着换）
	seen map[int]bool
	tree *btree
	fini bool

	// 物化路径（事务有写集）：一次取完，逐行吐
	hits []IndexHit
	hi   int
}

// ScanIndex 打开一个游标，desc 决定方向。调用方用完要 Close（只是释放引用）。
func (tx *Tx) ScanIndex(t *catalog.Table, ixName string, cols, orderCols []int, rs []KeyRange, desc bool) *IndexScanner {
	s := &IndexScanner{tx: tx, tbl: t, cols: cols, desc: desc}
	if desc {
		s.rs = orderedRangesDesc(rs)
	} else {
		s.rs = orderedRanges(rs)
	}
	st := tx.st
	td := st.Table(t.Name)
	if td == nil || len(rs) == 0 {
		s.fini = true
		return s
	}
	s.tree = td.pk
	if ixName != "" {
		s.tree = td.IndexTree(ixName)
	}
	if s.tree == nil {
		s.fini = true
		return s
	}
	if tx.ov[cleanName(t.Name)] != nil {
		// 事务里有写集：物化一次（要归并本事务插入/改键的行），反序时倒过来
		s.hits = tx.IndexRanges(t, ixName, cols, orderCols, rs)
		if desc {
			for i, j := 0, len(s.hits)-1; i < j; i, j = i+1, j-1 {
				s.hits[i], s.hits[j] = s.hits[j], s.hits[i]
			}
		}
		s.tree = nil
		return s
	}
	if len(s.rs) > 1 {
		s.seen = make(map[int]bool, 16) // 多区间可能重叠，同一个槽位只出一次
	}
	return s
}

// Next 取下一行。返回 ok=false 表示扫完了（之后再调也还是 false）。
func (s *IndexScanner) Next() (IndexHit, bool) {
	if s.hits != nil {
		if s.hi >= len(s.hits) {
			return IndexHit{}, false
		}
		h := s.hits[s.hi]
		s.hi++
		return h, true
	}
	for !s.fini {
		_, slot, ok := s.nextEntry()
		if !ok {
			s.fini = true
			break
		}
		if s.seen != nil {
			if s.seen[slot] {
				continue
			}
			s.seen[slot] = true
		}
		row, exists := s.tx.Get(s.tbl.Name, Ref{Slot: slot})
		if !exists {
			continue // 槽位已空：本事务删掉的行，或延迟合并留下的空槽
		}
		return IndexHit{Ref: Ref{Slot: slot}, Row: row}, true
	}
	return IndexHit{}, false
}

// NextKeyed 返回索引键原文与槽位，**不做回表**。覆盖索引扫描用它：
// 调用方从键还原列值。本事务 overlay 改过键的行不在这里出现，调用方需自行归并。
func (s *IndexScanner) NextKeyed() (string, Ref, bool) {
	if s.fini {
		return "", Ref{}, false
	}
	for {
		key, slot, ok := s.nextEntry()
		if !ok {
			s.fini = true
			return "", Ref{}, false
		}
		if s.seen != nil {
			if s.seen[slot] {
				continue
			}
			s.seen[slot] = true
		}
		return key, Ref{Slot: slot}, true
	}
}

// nextEntry 在树上走一步（按 desc 决定方向），跨区间时自动切到下一段。
func (s *IndexScanner) nextEntry() (string, int, bool) {
	for {
		if s.cur == nil {
			if s.ri >= len(s.rs) {
				return "", 0, false
			}
			s.cr = s.rs[s.ri]
			switch {
			case s.desc && s.cr.HasHi:
				// 从区间右端开始：闭区间取最后一个 <= hi，开区间取最后一个 < hi
				if s.cr.HiInc {
					s.cur = s.tree.revCursorLe(s.cr.Hi)
				} else {
					s.cur = s.tree.revCursorLt(s.cr.Hi)
				}
			case s.desc:
				s.cur = s.tree.last()
			case s.cr.HasLo:
				s.cur = s.tree.cursor(s.cr.Lo)
			default:
				s.cur = s.tree.first()
			}
		}
		var key string
		var slot int
		var ok bool
		if s.desc {
			key, slot, ok = s.cur.prev()
		} else {
			key, slot, ok = s.cur.next()
		}
		if !ok {
			s.cur = nil
			s.ri++
			continue
		}
		if s.desc {
			if s.cr.HasLo && (key < s.cr.Lo || (!s.cr.LoInc && key == s.cr.Lo)) {
				s.cur = nil // 过了本区间左端：换下一段
				s.ri++
				continue
			}
			if s.cr.HasHi && !s.cr.HiInc && key >= s.cr.Hi {
				continue // 开区间上界：等于 hi 的条目不要
			}
			return key, slot, true
		}
		if s.cr.HasLo && !s.cr.LoInc && key == s.cr.Lo {
			continue // 开区间下界：等于 lo 的条目不要
		}
		if s.cr.HasHi && (key > s.cr.Hi || (!s.cr.HiInc && key == s.cr.Hi)) {
			// 越过本区间上界：换下一段（重新定位，因为中间那段用不上）
			s.cur = nil
			s.ri++
			continue
		}
		return key, slot, true
	}
}

// Close 释放游标持有的引用。
func (s *IndexScanner) Close() {
	s.cur = nil
	s.tree = nil
	s.hits = nil
	s.seen = nil
	s.fini = true
}

// IndexRanges 一次扫多个键区间，返回**按索引键升序**、按槽位去重的行。
//
// 多个区间是给 OR 用的：`WHERE a = 1 OR a = 2` 拆成两个精确区间，一次查完。
// 结果必须有序 —— ORDER BY 免排序靠的就是这个顺序；区间按起点排好序后逐段扫，
// 重叠部分由 seen 去掉，剩下的仍然递增（后一段的输出要么已被前一段覆盖，要么更大）。
//
// 和 IndexLookup 一样要合并两边：已提交索引给候选（按索引序），再用 tx.Get 过滤
// 掉本事务删掉的、重新校验键是否还在区间内（本事务可能把键改出去了）；overlay
// 里新插入和改键进来的行按 key 归并进来。
// orderCols 是**索引的完整列**（用于给 overlay 行定排序位置）。cols 只是「条件列」，
// 往往只有首列；同首列内部还要按后面的索引列排，才能和树的键序对齐。
func (tx *Tx) IndexRanges(t *catalog.Table, ixName string, cols, orderCols []int, rs []KeyRange) []IndexHit {
	if len(rs) == 0 {
		return nil
	}
	td := tx.st.Table(t.Name)
	if td == nil {
		return nil
	}
	tree := td.pk
	if ixName != "" {
		tree = td.IndexTree(ixName)
	}
	if tree == nil {
		return nil
	}
	var out []IndexHit
	var keys []string // 与 out 对齐：合并 overlay 时要按 key 归并
	// 没有写集且只有一个区间时不必记录 seen，也不必重新校验键（省一次编码/行）
	var seen map[int]bool
	if tx.ov[cleanName(t.Name)] != nil || len(rs) > 1 {
		seen = make(map[int]bool, 16)
	}
	for _, r := range orderedRanges(rs) {
		run := r
		tree.Range(run, func(key string, slot int) bool {
			if seen != nil {
				if seen[slot] {
					return true // 多个区间重叠时同一个槽位只出一次
				}
				seen[slot] = true
			}
			row, ok := tx.Get(t.Name, Ref{Slot: slot})
			if !ok {
				return true
			}
			if seen != nil && !rowInKeyRanges(row, cols, rs) {
				return true
			}
			out = append(out, IndexHit{Ref: Ref{Slot: slot}, Row: row})
			keys = append(keys, key)
			return true
		})
	}
	o := tx.ov[cleanName(t.Name)]
	if o == nil {
		return out
	}
	// overlay 补的行要靠索引完整列定位：只按条件列排的话，同一首列下的
	// 多行顺序和树的键序对不上，ORDER BY a, b 会输出错序。
	sortCols := orderCols
	if len(sortCols) == 0 {
		sortCols = cols
	}
	// overlay 补的行：key 单独收集，最后和已排序的主结果归并
	var extra []IndexHit
	var extraKeys []string
	push := func(key string, hit IndexHit) {
		extra = append(extra, hit)
		extraKeys = append(extraKeys, key)
	}
	for i, row := range o.ins {
		if o.insDel[i] {
			continue
		}
		if nr, ok := o.insUpd[i]; ok {
			row = nr
		}
		if !rowInKeyRanges(row, cols, rs) {
			continue
		}
		k, _ := OrderKeyOfRow(row, sortCols)
		push(k, IndexHit{Ref: Ref{New: true, Slot: i}, Row: row})
	}
	var slots []int
	for slot := range o.upd {
		if !seen[slot] {
			slots = append(slots, slot)
		}
	}
	if len(slots) > 1 {
		sort.Ints(slots)
	}
	for _, slot := range slots {
		if o.del[slot] {
			continue
		}
		row := o.upd[slot]
		if !rowInKeyRanges(row, cols, rs) {
			continue
		}
		k, _ := OrderKeyOfRow(row, sortCols)
		push(k, IndexHit{Ref: Ref{Slot: slot}, Row: row})
	}
	return mergeByKey(out, keys, extra, extraKeys)
}

// orderedRanges 按区间起点排序。
//
// 排序之后逐个区间扫描，重叠部分被去重，剩下的输出仍然全局递增：后一段的起点
// 不小于前一段，落在前一段里的键已经输出过了，新输出的键必然更大。
func orderedRanges(rs []KeyRange) []KeyRange {
	if len(rs) < 2 {
		return rs
	}
	out := make([]KeyRange, len(rs))
	copy(out, rs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HasLo != b.HasLo {
			return !a.HasLo // 没有下界 = 从最小开始
		}
		if a.Lo != b.Lo {
			return a.Lo < b.Lo
		}
		return !a.LoInc && b.LoInc
	})
	return out
}

// orderedRangesDesc 按区间**右端**从大到小排序（反序扫描用）。
//
// 和正序同理：排好序逐段扫、重叠靠去重，剩下的输出仍然全局递减。
func orderedRangesDesc(rs []KeyRange) []KeyRange {
	if len(rs) < 2 {
		return rs
	}
	out := make([]KeyRange, len(rs))
	copy(out, rs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HasHi != b.HasHi {
			return !a.HasHi // 没有上界 = 从最大开始
		}
		if a.Hi != b.Hi {
			return a.Hi > b.Hi
		}
		return a.HiInc && !b.HiInc // 闭区间覆盖更广，先扫
	})
	return out
}

// mergeByKey 把两段 (hit, key) 归并成一段升序。
//
// a 来自索引扫描，本来就按 key 升序；b 是 overlay 补的行（插入顺序、按槽位顺序），
// 必须在这里先排一次 —— 少了这一步，本事务里改过键的行就会以「键 2 排在键 2 前面
// 但顺序反了」的方式混进结果，ORDER BY 免排序会直接输出错序。
func mergeByKey(a []IndexHit, ak []string, b []IndexHit, bk []string) []IndexHit {
	if len(b) == 0 {
		return a
	}
	if len(b) > 1 {
		order := make([]int, len(b))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(i, j int) bool { return bk[order[i]] < bk[order[j]] })
		nb := make([]IndexHit, len(b))
		nk := make([]string, len(b))
		for i, idx := range order {
			nb[i], nk[i] = b[idx], bk[idx]
		}
		b, bk = nb, nk
	}
	if len(a) == 0 {
		return b
	}
	out := make([]IndexHit, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if ak[i] <= bk[j] {
			out = append(out, a[i])
			i++
			continue
		}
		out = append(out, b[j])
		j++
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// rowInKeyRanges 判断一行在指定列上的键是否落在任何一个区间内。
func rowInKeyRanges(row types.Row, cols []int, rs []KeyRange) bool {
	k, ok := OrderKeyOfRow(row, cols)
	if !ok {
		return false
	}
	for _, r := range rs {
		if keyInRange(k, r) {
			return true
		}
	}
	return false
}

// rowInKeyRange 判断一行在指定列上的键是否落在区间内。
func rowInKeyRange(row types.Row, cols []int, r KeyRange) bool {
	k, ok := OrderKeyOfRow(row, cols)
	if !ok {
		return false
	}
	return keyInRange(k, r)
}

func keyInRange(k string, r KeyRange) bool {
	if r.HasLo {
		if r.LoInc && k < r.Lo {
			return false
		}
		if !r.LoInc && k <= r.Lo {
			return false
		}
	}
	if r.HasHi {
		if r.HiInc && k > r.Hi {
			return false
		}
		if !r.HiInc && k >= r.Hi {
			return false
		}
	}
	return true
}

// PKLookup 用主键哈希索引按完整主键值取一行。
func (tx *Tx) PKLookup(t *catalog.Table, key string) (IndexHit, bool) {
	cols := t.PKIdxes()
	if len(cols) == 0 {
		return IndexHit{}, false
	}
	td := tx.st.Table(t.Name)
	if td == nil {
		return IndexHit{}, false
	}
	for _, slot := range td.pk.Get(key) {
		if row, exists := tx.Get(t.Name, Ref{Slot: slot}); exists && groupKeyEqual(row, cols, key) {
			return IndexHit{Ref: Ref{Slot: slot}, Row: row}, true
		}
	}
	o := tx.ov[cleanName(t.Name)]
	if o == nil {
		return IndexHit{}, false
	}
	for i, row := range o.ins {
		if o.insDel[i] {
			continue
		}
		if nr, ok := o.insUpd[i]; ok {
			row = nr
		}
		if groupKeyEqual(row, cols, key) {
			return IndexHit{Ref: Ref{New: true, Slot: i}, Row: row}, true
		}
	}
	best := -1
	for slot, row := range o.upd {
		if o.del[slot] {
			continue
		}
		if groupKeyEqual(row, cols, key) && (best < 0 || slot < best) {
			best = slot
		}
	}
	if best >= 0 {
		return IndexHit{Ref: Ref{Slot: best}, Row: o.upd[best]}, true
	}
	return IndexHit{}, false
}

// FindUniqueGroup 检查一组列的组合值是否已存在（主键可能是复合的）。
//
// 复合主键必须按「整组」判重：(a,b) 主键下 a 单独重复是合法的。
// 主键组走内存哈希索引；非主键的 UNIQUE 组只能全表扫描。
// FindUniqueGroup 判断这组值是否与已有行冲突（唯一约束校验用）。
func (tx *Tx) FindUniqueGroup(t *catalog.Table, cols []int, vals []types.Value, self *Ref, scanTo int) (bool, error) {
	_, ok, err := tx.FindUniqueGroupRef(t, cols, vals, self, scanTo)
	return ok, err
}

// FindUniqueGroupRef 和 FindUniqueGroup 一样，但顺带返回冲突行的引用。
// INSERT ... ON CONFLICT DO UPDATE 要靠它定位要被覆盖的那一行。
func (tx *Tx) FindUniqueGroupRef(t *catalog.Table, cols []int, vals []types.Value, self *Ref, scanTo int) (Ref, bool, error) {
	if len(cols) == 0 || len(cols) != len(vals) {
		return Ref{}, false, nil
	}
	// 含 NULL 的组合不参与唯一性判断（和 SQL 一致）
	for _, v := range vals {
		if v.IsNull() {
			return Ref{}, false, nil
		}
	}
	key := OrderKey(vals)
	td := tx.st.Table(t.Name)
	if td == nil {
		return Ref{}, false, fmt.Errorf("relation %q does not exist", t.Name)
	}
	o := tx.ov[cleanName(t.Name)]
	isPK := sameCols(cols, t.PKIdxes())

	if isPK {
		for _, idx := range td.pk.Get(key) {
			deleted := o != nil && o.del[idx]
			if deleted {
				continue
			}
			if self != nil && !self.New && self.Slot == idx {
				continue
			}
			if o != nil {
				if nr, isUpd := o.upd[idx]; isUpd && !groupKeyEqual(nr, cols, key) {
					continue
				}
			}
			return Ref{Slot: idx}, true, nil
		}
	}
	// 本事务的写集里也可能藏着冲突。注意这里不能提前返回：事务外的一条
	// INSERT 没有写集（o == nil），但已提交行里的重复照样要查 —— 之前
	// 直接 return false，导致列级 UNIQUE 在自动提交路径上形同虚设。
	if o != nil {
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
			return Ref{New: true, Slot: i}, true, nil
		}
		for idx, row := range o.upd {
			if o.del[idx] || !groupKeyEqual(row, cols, key) {
				continue
			}
			if self != nil && !self.New && self.Slot == idx {
				continue
			}
			return Ref{Slot: idx}, true, nil
		}
	}
	// 非主键的唯一组合：查 UNIQUE 列的哈希索引（已提交态）。
	// 本事务自己的插入/更新上面已经处理过，这里只补「已提交行」这一边。
	if !isPK {
		if tree := td.UniqueTree(colsSig(cols)); tree != nil {
			for _, slot := range tree.Get(key) {
				if o != nil && o.del[slot] {
					continue
				}
				if self != nil && !self.New && self.Slot == slot {
					continue
				}
				// 本事务把这行的这一组改成了别的值，就不算冲突
				if o != nil {
					if nr, changed := o.upd[slot]; changed && !groupKeyEqual(nr, cols, key) {
						continue
					}
				}
				return Ref{Slot: slot}, true, nil
			}
			return Ref{}, false, nil
		}
		// 兜底：表结构刚变、哈希表还没重建时照样全表扫一遍，不能漏判
		var found Ref
		var conflict bool
		err := tx.Scan(t.Name, func(ref Ref, row types.Row) error {
			if self != nil && ref.New == self.New && ref.Slot == self.Slot {
				return nil
			}
			if groupKeyEqual(row, cols, key) {
				found, conflict = ref, true
			}
			return nil
		})
		if err != nil {
			return Ref{}, false, err
		}
		if conflict {
			return found, true, nil
		}
	}
	return Ref{}, false, nil
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
	return OrderKey(vals) == key
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
		table   string
		del     []int       // 要删除的槽位
		upd     []int       // 要原地替换的槽位
		updRows []types.Row // 与 upd 对齐的新行
		ins     []types.Row // 要分配新槽位的行
		dirty   int64
		seq     int64
		seqSet  bool
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
		c := &change{table: name}
		// 只遍历写集，不再扫全表：这是「提交成本与表大小无关」的关键。
		if len(o.del) > 0 {
			for slot := range o.del {
				if slot < 0 || slot >= len(td.Rows) || td.Rows[slot] == nil {
					continue
				}
				recs = append(recs, walRec{Tx: tx.ID, Op: opDel, Table: td.Name, Old: td.Rows[slot]})
				c.del = append(c.del, slot)
			}
			sort.Ints(c.del) // map 遍历无序，排一下让 WAL 记录可复现
		}
		if len(o.upd) > 0 {
			slots := make([]int, 0, len(o.upd))
			for slot := range o.upd {
				slots = append(slots, slot)
			}
			sort.Ints(slots)
			for _, slot := range slots {
				// 既删又改：删优先（否则会给一个马上要消失的槽位换行）
				if o.del[slot] || slot < 0 || slot >= len(td.Rows) || td.Rows[slot] == nil {
					continue
				}
				nr := o.upd[slot]
				recs = append(recs, walRec{Tx: tx.ID, Op: opUpd, Table: td.Name, Old: td.Rows[slot], New: nr})
				c.upd = append(c.upd, slot)
				c.updRows = append(c.updRows, nr)
			}
		}
		for i, row := range o.ins {
			if o.insDel[i] {
				continue
			}
			if nr, ok := o.insUpd[i]; ok {
				row = nr
			}
			recs = append(recs, walRec{Tx: tx.ID, Op: opIns, Table: td.Name, New: row})
			c.ins = append(c.ins, row)
		}
		c.dirty = int64(len(c.del) + len(c.upd) + len(c.ins))
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
		t, ok := tx.st.Cat.Get(c.table)
		if !ok {
			t = nil
		}
		// 增量应用：只碰写集里那几行，索引也只动那几个键。
		// 换行（原地替换）不改变槽位，所以索引里的槽位号一直有效 ——
		// 这是「提交成本与表大小无关」的另一半。
		for _, slot := range c.del {
			if slot >= len(td.Rows) || td.Rows[slot] == nil {
				continue
			}
			td.IndexDelete(t, slot, td.Rows[slot])
			td.Rows[slot] = nil
			td.Live--
			td.release(slot)
		}
		for i, slot := range c.upd {
			if slot >= len(td.Rows) || td.Rows[slot] == nil {
				continue
			}
			td.IndexUpdate(t, slot, td.Rows[slot], c.updRows[i])
			td.Rows[slot] = c.updRows[i]
		}
		for _, row := range c.ins {
			slot := td.alloc()
			td.IndexInsert(t, slot, row)
			td.Rows[slot] = row
			td.Live++
		}
		// 整表清空（TRUNCATE / 全表 DELETE）后把槽位数组一起放掉：
		// 否则空数组会一直占着 len(Rows) 那么大的指针空间，扫描也白跑一遍。
		if td.Live == 0 && len(td.Rows) > 0 {
			td.Rows = nil
			td.free = nil
		}
		if c.seqSet {
			if c.seq == 0 {
				td.Seq = 0
			} else if c.seq > td.Seq {
				td.Seq = c.seq
			}
		}
		key := cleanName(c.table)
		tx.st.loadMu.Lock()
		tx.st.dirty[key] = true
		tx.st.dirtyRows[key] += c.dirty
		tx.st.tableRows[key] = td.Live
		tx.st.loadMu.Unlock()
	}

	tx.ov = map[string]*overlay{}
	tx.seq = map[string]int64{}
	tx.n = 0

	tx.st.commits++
	tx.st.cpSince.Add(1)
	if tx.st.needCheckpointLocked() {
		done, err := tx.st.tryCheckpointLocked()
		if err != nil {
			return 0, err
		}
		if done {
			// checkpoint 已经把数据写进快照，这批记录不需要再等 fsync
			return 0, nil
		}
		// 撞上别人正在写盘：这一批还在 WAL 里没落盘，得照常等 fsync
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

// Clone 复制一份快照。SAVEPOINT 用它：ROLLBACK TO 之后同一个保存点还能再回滚
// 一次（PostgreSQL 允许），所以快照不能被子类「消费」掉。
func (m *TxSnapshot) Clone() *TxSnapshot {
	c := &TxSnapshot{ov: make(map[string]*overlay, len(m.ov)), seq: make(map[string]int64, len(m.seq)), n: m.n}
	for k, o := range m.ov {
		c.ov[k] = o.clone()
	}
	for k, v := range m.seq {
		c.seq[k] = v
	}
	return c
}

// DataDir 返回数据目录。
func (s *Store) DataDir() string { return filepath.Clean(s.Dir) }
