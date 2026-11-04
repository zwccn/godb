package exec

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"godb/catalog"
	"godb/storage"
	"godb/syscat"
	"godb/types"
)

// Operator 是火山模型算子。
type Operator interface {
	Schema() Schema
	Open() error
	Next() (types.Row, bool, error)
	Close() error
}

// planNode 供 EXPLAIN 输出使用。
type planNode interface {
	Describe() string
	Children() []Operator
}

// ---------- 全表扫描 ----------

type scanOp struct {
	OpProfile
	tx    *storage.Tx
	table string
	qual  string
	sc    Schema
	it    *storage.RowIter
	// scanned 是已经从堆表里读过的行数。它和 Profile 里的「输出行数」
	// 是两回事：一次全表扫可能只往上吐一行。
	scanned int
	// notes 是规划器留下的解释（为什么没走索引、下推了什么条件）。
	notes []string
}

func newScan(tx *storage.Tx, t *catalog.Table, qual string, cat *catalog.Catalog) *scanOp {
	return &scanOp{tx: tx, table: t.Name, qual: qual, sc: tableSchema(cat, t, qual)}
}

func (o *scanOp) Schema() Schema { return o.sc }

func (o *scanOp) Open() error {
	// 没有事务就扫表说明调用方在「只编译」的模式下真去执行了计划。
	// 直接报错，别让空指针把整个进程带走（Describe 阶段的 CTE 踩过这个坑）。
	if o.tx == nil {
		return fmt.Errorf("internal error: table scan %q requires a transaction (plan was opened without one)", o.table)
	}
	t := o.begin()
	// 流式游标：上层要一行才走一步。`LIMIT 100` 取满就 Close，
	// 不会把 60 万行全读进内存（旧实现 Open 就物化全表）。
	o.it = o.tx.ScanIter(o.table)
	o.scanned = 0
	syscat.NoteSeqScan(o.table)
	t.done(false)
	return nil
}

func (o *scanOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.it == nil {
		t.done(false)
		return nil, false, nil
	}
	_, row, ok := o.it.Next()
	if !ok {
		if err := o.it.Err(); err != nil {
			t.done(false)
			return nil, false, err
		}
		t.done(false)
		return nil, false, nil
	}
	o.scanned++
	o.tx.NoteScan()
	t.done(true)
	return row, true, nil
}

// ExplainDetail 说明这次扫描读了多少行 —— 「扫 20 万行只出 1 行」一眼就能看出来。
func (o *scanOp) ExplainDetail() []string {
	out := []string{fmt.Sprintf("已扫描 %d 行", o.scanned)}
	if len(o.notes) > 0 {
		out = append(out, o.notes...)
	}
	return out
}

// ScanRows 把「从堆里读了多少行」交给结构化计划（探测执行的「读取 N 行」）。
// 它和 profile 的 rows 是两回事：读了 5 万行、上层过滤后只吐 1 行，差额就是白读的。
func (o *scanOp) ScanRows() int64 { return int64(o.scanned) }

func (o *scanOp) Close() error { o.it = nil; return nil }

func (o *scanOp) Describe() string {
	return fmt.Sprintf("Seq Scan on %s", o.table)
}
func (o *scanOp) Children() []Operator { return nil }

// tableSchema 按表定义生成 FROM 子树的列布局。
// cat 只用来算表 OID（PG 协议里要回给客户端，客户端据此判断能不能回写）；
// 传 nil 也能用，只是 OID 为 0。
func tableSchema(cat *catalog.Catalog, t *catalog.Table, qual string) Schema {
	sc := make(Schema, len(t.Columns))
	oid, _ := syscat.RelationOID(cat, t.Name)
	for i, c := range t.Columns {
		sc[i] = ColInfo{
			Qualifier: strings.ToLower(qual),
			Name:      strings.ToLower(c.Name),
			Type:      c.Type(),
			TableOID:  oid,
			AttNum:    int16(i + 1),
			TableID:   t.ID,
			ColID:     c.ID,
		}
	}
	return sc
}

// ---------- 索引扫描 ----------

// indexScanOp 用索引取出等值匹配的候选行。
//
// 它只负责把「整表扫描」缩成「几行」，WHERE 里的谓词仍然由上层 filterOp
// 再判一遍。重复判一次的代价可以忽略，换来的是：就算这里对类型或常量的
// 假设有偏差，也不会返回不该出现的行（漏行则由下面的选索引规则兜住 ——
// 只有常量能安全 Cast 到列类型时才会用索引）。
type indexScanOp struct {
	OpProfile
	tx      *storage.Tx
	tbl     *catalog.Table
	ixName  string // 空表示走主键索引
	cols    []int
	ixCols  []int         // 索引的完整列，ORDER BY 前缀匹配用
	ixNames []string      // 同上，换成列名（等值扫描恒按这些列有序）
	vals    []types.Value // 已按列类型归一
	sc      Schema
	rows    []types.Row
	i       int
	fetched int // Open 时从索引取回的行数（EXPLAIN 用）
}

func newIndexScan(tx *storage.Tx, t *catalog.Table, ixName string, cols, ixCols []int, vals []types.Value, qual string, cat *catalog.Catalog) *indexScanOp {
	return &indexScanOp{
		tx: tx, tbl: t, ixName: ixName, cols: cols, ixCols: ixCols,
		ixNames: colNames(t, ixCols), vals: vals,
		sc: tableSchema(cat, t, qual),
	}
}

// colNames 把列位置翻成小写列名（ORDER BY 比对用）。
func colNames(t *catalog.Table, idxes []int) []string {
	if len(idxes) == 0 {
		return nil
	}
	out := make([]string, 0, len(idxes))
	for _, ci := range idxes {
		if ci < 0 || ci >= len(t.Columns) {
			return nil
		}
		out = append(out, strings.ToLower(t.Columns[ci].Name))
	}
	return out
}

func (o *indexScanOp) Schema() Schema { return o.sc }

func (o *indexScanOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	o.rows = o.rows[:0]
	o.i = 0
	// 键里有 NULL 就不可能等值命中（`col = NULL` 恒为未知）
	for _, v := range o.vals {
		if v.IsNull() {
			return nil
		}
	}
	key := storage.OrderKey(o.vals)
	if o.ixName == "" {
		if hit, ok := o.tx.PKLookup(o.tbl, key); ok {
			o.rows = append(o.rows, hit.Row)
		}
	} else {
		for _, hit := range o.tx.IndexLookup(o.tbl, o.ixName, o.cols, key) {
			o.rows = append(o.rows, hit.Row)
		}
	}
	o.fetched = len(o.rows)
	return nil
}

func (o *indexScanOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.i >= len(o.rows) {
		t.done(false)
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	o.tx.NoteScan()
	t.done(true)
	return r, true, nil
}

func (o *indexScanOp) ExplainDetail() []string {
	return []string{fmt.Sprintf("索引取回 %d 行（键 = %s）", o.fetched, keyDigest(o.vals))}
}

func (o *indexScanOp) Close() error {
	o.rows = nil
	o.i = 0
	return nil
}

func (o *indexScanOp) Describe() string {
	name := o.ixName
	if name == "" {
		name = o.tbl.Name + "_pkey"
	}
	return fmt.Sprintf("Index Scan using %s on %s", name, o.tbl.Name)
}

func (o *indexScanOp) Children() []Operator { return nil }

// indexRangeScanOp 用索引扫键区间（`BETWEEN`、`>`、`LIKE 'x%'`，以及 OR 合并出来的多段）。
//
// 和等值扫描一样，它只负责缩小候选集，WHERE 仍由上层 filterOp 再判一遍。
// 输出按索引键升序（存储层保证），所以 `ORDER BY` 命中索引前缀时可以省掉排序；
// desc 只影响遍历方向，扫描的仍是同一批行。
//
// 正反两个方向都走**游标**：上层要一行才走一步，所以 `LIMIT 10` 只碰前面（或者
// 最后面）几个叶子。叶子链是双向的，反序从右端往回走，不需要先物化。
type indexRangeScanOp struct {
	OpProfile
	tx      *storage.Tx
	tbl     *catalog.Table
	ixName  string // 空表示主键
	cols    []int
	ixCols  []int // 索引的完整列，ORDER BY 前缀匹配用
	rs      []storage.KeyRange
	desc    bool
	sc      Schema
	scanner *storage.IndexScanner
	ixNames []string // 同上，换成列名
}

func newIndexRangeScan(tx *storage.Tx, t *catalog.Table, ixName string, cols, ixCols []int, rs []storage.KeyRange, qual string, cat *catalog.Catalog) *indexRangeScanOp {
	return &indexRangeScanOp{
		tx: tx, tbl: t, ixName: ixName, cols: cols, ixCols: ixCols, rs: rs,
		ixNames: colNames(t, ixCols),
		sc:      tableSchema(cat, t, qual),
	}
}

func (o *indexRangeScanOp) Schema() Schema { return o.sc }

func (o *indexRangeScanOp) Open() error {
	t := o.begin()
	o.scanner = o.tx.ScanIndex(o.tbl, o.ixName, o.cols, o.ixCols, o.rs, o.desc)
	syscat.NoteIdxScan(o.tbl.Name)
	t.done(false)
	return nil
}

func (o *indexRangeScanOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.scanner == nil {
		t.done(false)
		return nil, false, nil
	}
	h, ok := o.scanner.Next()
	if !ok {
		t.done(false)
		return nil, false, nil
	}
	o.tx.NoteScan()
	t.done(true)
	return h.Row, true, nil
}

func (o *indexRangeScanOp) Close() error {
	if o.scanner != nil {
		o.scanner.Close()
		o.scanner = nil
	}
	return nil
}

func (o *indexRangeScanOp) Describe() string {
	name := o.ixName
	if name == "" {
		name = o.tbl.Name + "_pkey"
	}
	var s string
	if len(o.rs) > 1 {
		s = fmt.Sprintf("Index Union Scan using %s (%d ranges) on %s", name, len(o.rs), o.tbl.Name)
	} else {
		s = fmt.Sprintf("Index Range Scan using %s on %s", name, o.tbl.Name)
	}
	if o.desc {
		s += " (backward)"
	}
	return s
}

func (o *indexRangeScanOp) Children() []Operator { return nil }

// 两个索引扫描算子都能按索引键序输出，ORDER BY 前缀匹配时省掉排序。
func (o *indexScanOp) orderedColumns() []string { return o.ixNames }
func (o *indexScanOp) isReversed() bool         { return false }
func (o *indexScanOp) setReversed(bool)         {}

func (o *indexRangeScanOp) orderedColumns() []string { return o.ixNames }
func (o *indexRangeScanOp) isReversed() bool         { return o.desc }
func (o *indexRangeScanOp) setReversed(v bool)       { o.desc = v }

// ---------- 单行（无 FROM 的 SELECT） ----------

type oneRowOp struct {
	OpProfile
	done bool
}

func (o *oneRowOp) Schema() Schema { return nil }
func (o *oneRowOp) Open() error {
	t := o.begin()
	o.done = false
	t.done(false)
	return nil
}
func (o *oneRowOp) Close() error   { return nil }
func (o *oneRowOp) Describe() string     { return "Result (no FROM)" }
func (o *oneRowOp) Children() []Operator { return nil }
func (o *oneRowOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.done {
		t.done(false)
		return nil, false, nil
	}
	o.done = true
	t.done(true)
	return types.Row{}, true, nil
}

// valuesOp 是 `VALUES (…),(…)` 查询源。
// PG 里它就是一个查询，所以既能独立执行，也能当派生表 `FROM (VALUES …) x(a,b)`，
// 后面还能接 UNION。列名是 column1、column2……，由外层的别名列表改名。
type valuesOp struct {
	OpProfile
	rows [][]Eval
	sc   Schema
	i    int
}

func (o *valuesOp) Schema() Schema { return o.sc }
func (o *valuesOp) Open() error {
	t := o.begin()
	o.i = 0
	t.done(false)
	return nil
}
func (o *valuesOp) Close() error         { return nil }
func (o *valuesOp) Describe() string     { return fmt.Sprintf("Values (%d rows)", len(o.rows)) }
func (o *valuesOp) Children() []Operator { return nil }

func (o *valuesOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.i >= len(o.rows) {
		t.done(false)
		return nil, false, nil
	}
	evs := o.rows[o.i]
	o.i++
	row := make(types.Row, len(evs))
	for j, ev := range evs {
		v, err := ev.Eval(nil)
		if err != nil {
			t.done(false)
			return nil, false, err
		}
		row[j] = v
	}
	t.done(true)
	return row, true, nil
}

// ---------- 过滤 ----------

type filterOp struct {
	OpProfile
	child Operator
	pred  Eval
	label string
}

func (o *filterOp) Schema() Schema { return o.child.Schema() }
func (o *filterOp) Open() error {
	// 计时必须包住 child.Open()：父节点的总时长要含整棵子树，
	// 否则 SelfNs = 父 - 子 会算出负数，根节点还会显示成 0。
	t := o.begin()
	defer func() { t.done(false) }()
	return o.child.Open()
}
func (o *filterOp) Close() error         { return o.child.Close() }
func (o *filterOp) Children() []Operator { return []Operator{o.child} }
func (o *filterOp) Describe() string {
	if o.label != "" {
		return "Filter (" + o.label + ")"
	}
	return "Filter"
}

func (o *filterOp) Next() (types.Row, bool, error) {
	t := o.begin()
	for {
		row, ok, err := o.child.Next()
		if err != nil || !ok {
			t.done(false)
			return nil, ok, err
		}
		et := o.evalTimer()
		v, err := o.pred.Eval(row)
		o.evalDone(et)
		if err != nil {
			t.done(false)
			return nil, false, err
		}
		b, isNull, err := asBool(v)
		if err != nil {
			t.done(false)
			return nil, false, err
		}
		if !isNull && b {
			t.done(true)
			return row, true, nil
		}
	}
}

// ---------- 投影 ----------

type projectOp struct {
	OpProfile
	child Operator
	exprs []Eval
	sc    Schema
}

func (o *projectOp) Schema() Schema { return o.sc }
func (o *projectOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	return o.child.Open()
}
func (o *projectOp) Close() error         { return o.child.Close() }
func (o *projectOp) Children() []Operator { return []Operator{o.child} }

func (o *projectOp) Describe() string {
	names := make([]string, len(o.sc))
	for i, c := range o.sc {
		names[i] = c.Name
	}
	return "Project (" + strings.Join(names, ", ") + ")"
}

func (o *projectOp) Next() (types.Row, bool, error) {
	t := o.begin()
	row, ok, err := o.child.Next()
	if err != nil || !ok {
		t.done(false)
		return nil, ok, err
	}
	out := make(types.Row, len(o.exprs))
	et := o.evalTimer()
	for i, e := range o.exprs {
		v, err := e.Eval(row)
		if err != nil {
			t.done(false)
			return nil, false, err
		}
		out[i] = v
	}
	o.evalDone(et)
	t.done(true)
	return out, true, nil
}

// ---------- 去重 ----------

type distinctOp struct {
	OpProfile
	child Operator
	seen  map[string]bool
	buf   []types.Row
	i     int
	sc    Schema
}

func (o *distinctOp) Schema() Schema       { return o.sc }
func (o *distinctOp) Close() error         { return o.child.Close() }
func (o *distinctOp) Children() []Operator { return []Operator{o.child} }
func (o *distinctOp) Describe() string     { return "Distinct" }

// MemRows 是去重缓冲的行数（buf）。seen 里还有一份序列化后的键，没有计入 ——
// 面板只说「攒了多少行」，字节口径由 ExplainTree 统一折算。
func (o *distinctOp) MemRows() int64 { return int64(len(o.buf)) }

func (o *distinctOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	if err := o.child.Open(); err != nil {
		return err
	}
	o.seen = map[string]bool{}
	o.buf = o.buf[:0]
	o.i = 0
	for {
		row, ok, err := o.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		k, err := types.Key(row)
		if err != nil {
			return fmt.Errorf("SELECT DISTINCT needs to compare rows: %w", err)
		}
		if o.seen[k] {
			continue
		}
		o.seen[k] = true
		o.buf = append(o.buf, row)
	}
	return nil
}

func (o *distinctOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.i >= len(o.buf) {
		t.done(false)
		return nil, false, nil
	}
	r := o.buf[o.i]
	o.i++
	t.done(true)
	return r, true, nil
}

// ---------- 排序 ----------

type sortKey struct {
	eval       Eval
	desc       bool
	nullsFirst bool
}

// sortEntry 是一行及其预计算的排序键（一次 Eval，比较时不再反复求值）。
type sortEntry struct {
	row  types.Row
	keys []types.Value
}

type sortOp struct {
	OpProfile
	child Operator
	keys  []sortKey
	rows  []sortEntry
	i     int
	sc    Schema
	err   error
	// topN > 0 时用「大小为 N 的最大堆」只保留前 N 行（含 OFFSET），
	// 不把整段结果物化下来再全排。分页 `ORDER BY … LIMIT n` 靠它。
	topN int
	// fastKey 表示「只有一个排序键，且类型是 double precision」。
	//
	// 向量 Top-K 正是这个形状：`ORDER BY v <=> q LIMIT k` 的键是 double。
	// 每一行都要比一次，省掉 types.Compare 里的类型分支与 NULL 检查，
	// 100 万行能少一次函数调用 + 一次 switch。
	fastKey bool
	// scratch 是 topN 路径求值排序键的暂存区。
	//
	// 关键点：被淘汰的行不该为「用不上的键数组」分配内存。原来的写法每行都
	// make([]types.Value)，100 万行就是 100 万次小分配 —— 稳态下这里改成
	// 「先算进 scratch，只有进堆/换入时才复用一条已存在的键数组」。
	scratch []types.Value
}

func (o *sortOp) Schema() Schema       { return o.sc }
func (o *sortOp) Close() error         { return o.child.Close() }
func (o *sortOp) Children() []Operator { return []Operator{o.child} }
func (o *sortOp) Describe() string {
	parts := make([]string, len(o.keys))
	for i, k := range o.keys {
		d := "ASC"
		if k.desc {
			d = "DESC"
		}
		parts[i] = d
	}
	name := "Sort (" + strings.Join(parts, ", ") + ")"
	if o.topN > 0 {
		name = "Top-N " + name + fmt.Sprintf(" (n=%d)", o.topN)
	}
	return name
}

// MemRows 是排序物化下来的行数。排序是全量物化的 —— 输入多少攒多少，
// 哪怕调用方只取前 10 行；只有走了 Top-N（带 LIMIT）才真的只留 N 行。
func (o *sortOp) MemRows() int64 { return int64(len(o.rows)) }

// ExplainDetail 说明这一步是否把排序键的求值并行化了。
//
// 这句话等于解释「为什么 100 万行 × 128 维的 Top-K 只用了几十分之一秒」——
// 没有它，计划里只有一串耗时，看不出并行这件事。
func (o *sortOp) ExplainDetail() []string {
	nw := o.parallelWorkers()
	if nw == 0 {
		return nil
	}
	return []string{fmt.Sprintf("排序键并行求值（%d 个 worker，每批 %d 行）", nw, sortParBatch)}
}

// lessEntry 按排序键比较两行：true 表示 a 应该排在 b 前面。
func (o *sortOp) lessEntry(a, b sortEntry) bool {
	// 单键 double 的快路径：向量距离、时间差这类排序都落在这里。
	// 两边都是非 NULL 浮点时直接比，绕开 types.Compare 的类型 switch。
	if o.fastKey {
		ak, bk := a.keys[0], b.keys[0]
		if ak.K == types.KFloat && bk.K == types.KFloat {
			af, bf := ak.Float(), bk.Float()
			if af == bf {
				return false
			}
			if o.keys[0].desc {
				return af > bf
			}
			return af < bf
		}
	}
	for i, k := range o.keys {
		av, bv := a.keys[i], b.keys[i]
		c := 0
		switch {
		case av.IsNull() && bv.IsNull():
			continue
		case av.IsNull():
			// 空值位置与排序方向无关（NULLS FIRST / LAST 是绝对位置）
			return k.nullsFirst
		case bv.IsNull():
			return !k.nullsFirst
		default:
			cmp, err := types.Compare(av, bv)
			if err != nil {
				o.err = err
				return false
			}
			c = cmp
		}
		if c == 0 {
			continue // 这一列相等，看下一列；别在这里就 return false
		}
		if k.desc {
			return c > 0
		}
		return c < 0
	}
	return false
}

// beatsWorst 判断新行的键是否优于堆顶（当前最差）。
//
// 单独抽出来是为了避开 sortEntry 的 40 字节按值传递：Top-N 里每行都要问一次
// 这个问题，而绝大多数答案都是「不优」。单键 double 时直接比浮点。
func (o *sortOp) beatsWorst(cand, worst []types.Value) bool {
	if o.fastKey {
		ak, bk := cand[0], worst[0]
		if ak.K == types.KFloat && bk.K == types.KFloat {
			if o.keys[0].desc {
				return ak.Float() > bk.Float()
			}
			return ak.Float() < bk.Float()
		}
	}
	return o.lessEntry(sortEntry{keys: cand}, sortEntry{keys: worst})
}

// topNHeap 是「保留最好的 N 个」用的大顶堆：堆顶是当前最差的那行（要被淘汰的）。
type topNHeap struct {
	op    *sortOp
	items []sortEntry
}

// 下面三个方法手写，不用 container/heap：
//
// 那个每步都要经 any 装箱 + 接口调用（Push/Pop/Less/Swap 四次断言），100 万行
// 的 Top-K 里这层开销能测出来。堆只用到两种操作 —— 追加、替换堆顶，
// 所以只需要 siftUp / siftDown。
//
// 这里是「堆顶最差」的大顶堆：比较用的是排序序（worse 表示在结果里更靠后）。
func (h *topNHeap) worse(i, j int) bool {
	return h.op.lessEntry(h.items[j], h.items[i])
}

func (h *topNHeap) siftUp(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.worse(i, p) {
			return
		}
		h.items[i], h.items[p] = h.items[p], h.items[i]
		i = p
	}
}

func (h *topNHeap) siftDown(i int) {
	n := len(h.items)
	for {
		l := i*2 + 1
		if l >= n {
			return
		}
		pick := l
		if r := l + 1; r < n && h.worse(r, l) {
			pick = r
		}
		if !h.worse(pick, i) {
			return
		}
		h.items[i], h.items[pick] = h.items[pick], h.items[i]
		i = pick
	}
}

func (h *topNHeap) push(e sortEntry) {
	h.items = append(h.items, e)
	h.siftUp(len(h.items) - 1)
}

// replaceWorst 换掉堆顶（当前最差）并重新下沉。
func (h *topNHeap) replaceWorst(e sortEntry) {
	h.items[0] = e
	h.siftDown(0)
}

func (o *sortOp) Open() error {
	t := o.begin()
	// 排序的活全在 Open 里（物化 + 比较），计时必须包住整段；
	// 只计 Next 的话，一次几十万行的排序会被算成「0 微秒」。
	defer func() { t.done(false) }()
	if err := o.child.Open(); err != nil {
		return err
	}
	o.rows = o.rows[:0]
	o.i = 0
	o.err = nil
	// 单键 double 的排序（向量距离的典型形状）走比较快路径。
	o.fastKey = len(o.keys) == 1 && o.keys[0].eval.Type().Kind == types.KFloat

	// 预计算排序键：每行只 Eval 一次，比较时直接用值。
	// 单独计一段 evalNs：`ORDER BY v <=> q` 的距离计算全在这里，不单列的话
	// 会被当成「排序慢」（见 OpProfile.evalNs）。
	if o.topN > 0 {
		// 只有「一个排序键、且这个键能复制出独立求值器」时才并行。
		// 典型就是 `ORDER BY v <=> q LIMIT k`：距离计算是纯算术，能分给多核；
		// 而堆的维护必须串行（要保持行序，否则并列值的取舍会变）。
		if nw := o.parallelWorkers(); nw > 0 {
			if err := o.topNParallel(nw); err != nil {
				return err
			}
		} else if err := o.topNSerial(); err != nil {
			return err
		}
	} else {
		for {
			row, ok, err := o.child.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			e, err := o.entry(row)
			if err != nil {
				return err
			}
			o.rows = append(o.rows, e)
		}
	}
	// 堆里是「按最差优先」的无序序列，最后再整体排成正确顺序
	entries := o.rows
	sort.SliceStable(entries, func(i, j int) bool {
		if o.err != nil {
			return false
		}
		return o.lessEntry(entries[i], entries[j])
	})
	return o.err
}

// entry 算一行的排序键（含求值计时）。
//
// 全量排序用：每一行都要留在结果里，键必须各自持有一份。
func (o *sortOp) entry(row types.Row) (sortEntry, error) {
	ks := make([]types.Value, len(o.keys))
	if err := o.entryInto(row, ks); err != nil {
		return sortEntry{}, err
	}
	return sortEntry{row: row, keys: ks}, nil
}

// entryInto 把排序键算进调用方给的数组（不分配）。Top-N 路径用它写 scratch。
func (o *sortOp) entryInto(row types.Row, dst []types.Value) error {
	et := o.evalTimer()
	for i, k := range o.keys {
		v, err := k.eval.Eval(row)
		if err != nil {
			o.evalDone(et)
			return err
		}
		dst[i] = v
	}
	o.evalDone(et)
	return nil
}

// topNSerial 是 Top-N 的串行版本（不并行的所有情形都走这里）。
//
// 分配策略：先算进 scratch，只有真的进堆或换入时才动键数组。
// 稳态下被淘汰的行一次分配都不做，换入时复用被换掉那条的数组。
func (o *sortOp) topNSerial() error {
	h := &topNHeap{op: o}
	if cap(o.scratch) < len(o.keys) {
		o.scratch = make([]types.Value, len(o.keys))
	}
	scratch := o.scratch[:len(o.keys)]
	for {
		row, ok, err := o.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if err := o.entryInto(row, scratch); err != nil {
			return err
		}
		if len(h.items) < o.topN {
			keys := make([]types.Value, len(scratch))
			copy(keys, scratch)
			h.push(sortEntry{row: row, keys: keys})
			continue
		}
		if !o.beatsWorst(scratch, h.items[0].keys) {
			continue // 没挤进前 N：行和键都不要了，零分配
		}
		keys := h.items[0].keys // 复用被换掉那一条的键数组
		copy(keys, scratch)
		h.replaceWorst(sortEntry{row: row, keys: keys})
		if o.err != nil {
			return o.err
		}
	}
	o.rows = h.items
	return o.err
}

// parKeyEval 是「可以把求值复制给多个 goroutine」的排序键。
//
// 这条路径只为向量距离准备：它只依赖只读的查询向量 + 只读映射的列文件，
// 每个 worker 一份自己的缓冲就能并行。任意表达式（子查询、聚合）不满足，返回 nil。
type parKeyEval interface {
	parallelWorker() Eval
}

// sortParBatch 是并行求值的一批行数。
//
// 太小 → goroutine 调度/同步开销显影；太大 → 内存里多攒一份行指针，
// 中止响应也变慢。1024 行 × 8 字节指针 = 8KB，可以忽略。
const sortParBatch = 1024

// parallelWorkers 返回并行度；0 表示走串行。
func (o *sortOp) parallelWorkers() int {
	if o.topN <= 0 || len(o.keys) != 1 {
		return 0
	}
	if _, ok := o.keys[0].eval.(parKeyEval); !ok {
		return 0
	}
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		// 距离计算是纯算术，8 核之后基本被内存带宽卡住，再多只是抢核。
		n = 8
	}
	if n < 2 {
		return 0
	}
	return n
}

// topNParallel 与 topNSerial 语义完全相同，只是把「算排序键」按批分给 nw 个 worker。
//
// 入堆顺序仍是原始行序 —— 这样并列值在 Top-K 边界上的取舍与串行版一致，
// 并行只改变「什么时候算」，不改变「谁进堆」。
func (o *sortOp) topNParallel(nw int) error {
	proto := o.keys[0].eval.(parKeyEval)
	workers := make([]Eval, nw)
	for i := range workers {
		w := proto.parallelWorker()
		if w == nil {
			// parallelWorkers 说能并行、复制时又不行了：退回串行，别报错。
			return o.topNSerial()
		}
		workers[i] = w
	}
	h := &topNHeap{op: o}
	rows := make([]types.Row, sortParBatch)
	errs := make([]error, nw)
	for {
		n := 0
		for n < len(rows) {
			row, ok, err := o.child.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			rows[n] = row
			n++
		}
		if n == 0 {
			break
		}

		keys := make([]types.Value, n)
		chunk := (n + nw - 1) / nw
		et := o.evalTimer()
		var wg sync.WaitGroup
		for w := 0; w < nw; w++ {
			lo, hi := w*chunk, (w+1)*chunk
			if lo >= n {
				break
			}
			if hi > n {
				hi = n
			}
			wg.Add(1)
			go func(w int, ev Eval, lo, hi int) {
				defer wg.Done()
				for i := lo; i < hi; i++ {
					v, err := ev.Eval(rows[i])
					if err != nil {
						errs[w] = err
						return
					}
					keys[i] = v
				}
			}(w, workers[w], lo, hi)
		}
		wg.Wait()
		o.evalDone(et)

		// 报错按行序挑第一个（worker 顺序就是行序），和串行版一致。
		var firstErr error
		for _, e := range errs {
			if e != nil {
				firstErr = e
				break
			}
		}
		for i := range errs {
			errs[i] = nil
		}
		if firstErr != nil {
			return firstErr
		}

		for i := 0; i < n; i++ {
			if o.err != nil {
				return o.err
			}
			// keys[i:i+1] 共享这一批的键数组：整批只分配一次，
			// 只有留在堆里的那几个 entry 会把这个数组留在内存里。
			k := keys[i : i+1]
			if len(h.items) < o.topN {
				h.push(sortEntry{row: rows[i], keys: k})
			} else if o.beatsWorst(k, h.items[0].keys) {
				h.replaceWorst(sortEntry{row: rows[i], keys: k})
			}
			rows[i] = nil
		}
	}
	o.rows = h.items
	return o.err
}

func (o *sortOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.i >= len(o.rows) {
		t.done(false)
		return nil, false, nil
	}
	r := o.rows[o.i].row
	o.i++
	t.done(true)
	return r, true, nil
}

// ---------- LIMIT / OFFSET ----------

type limitOp struct {
	OpProfile
	child  Operator
	limit  int64
	offset int64
	seen   int64
	emited int64
}

func (o *limitOp) Schema() Schema { return o.child.Schema() }
func (o *limitOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	o.seen, o.emited = 0, 0
	return o.child.Open()
}
func (o *limitOp) Close() error         { return o.child.Close() }
func (o *limitOp) Children() []Operator { return []Operator{o.child} }
func (o *limitOp) Describe() string {
	if o.offset > 0 {
		return fmt.Sprintf("Limit (limit=%d, offset=%d)", o.limit, o.offset)
	}
	return fmt.Sprintf("Limit (limit=%d)", o.limit)
}

func (o *limitOp) Next() (types.Row, bool, error) {
	t := o.begin()
	for o.emited < o.limit {
		row, ok, err := o.child.Next()
		if err != nil || !ok {
			t.done(false)
			return nil, ok, err
		}
		if o.seen < o.offset {
			o.seen++
			continue
		}
		o.emited++
		t.done(true)
		return row, true, nil
	}
	t.done(false)
	return nil, false, nil
}
