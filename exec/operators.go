package exec

import (
	"container/heap"
	"fmt"
	"sort"
	"strings"

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
	done bool
}

func (o *oneRowOp) Schema() Schema       { return nil }
func (o *oneRowOp) Open() error          { o.done = false; return nil }
func (o *oneRowOp) Close() error         { return nil }
func (o *oneRowOp) Describe() string     { return "Result (no FROM)" }
func (o *oneRowOp) Children() []Operator { return nil }
func (o *oneRowOp) Next() (types.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	return types.Row{}, true, nil
}

// valuesOp 是 `VALUES (…),(…)` 查询源。
// PG 里它就是一个查询，所以既能独立执行，也能当派生表 `FROM (VALUES …) x(a,b)`，
// 后面还能接 UNION。列名是 column1、column2……，由外层的别名列表改名。
type valuesOp struct {
	rows [][]Eval
	sc   Schema
	i    int
}

func (o *valuesOp) Schema() Schema       { return o.sc }
func (o *valuesOp) Open() error          { o.i = 0; return nil }
func (o *valuesOp) Close() error         { return nil }
func (o *valuesOp) Describe() string     { return fmt.Sprintf("Values (%d rows)", len(o.rows)) }
func (o *valuesOp) Children() []Operator { return nil }

func (o *valuesOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	evs := o.rows[o.i]
	o.i++
	row := make(types.Row, len(evs))
	for j, ev := range evs {
		v, err := ev.Eval(nil)
		if err != nil {
			return nil, false, err
		}
		row[j] = v
	}
	return row, true, nil
}

// ---------- 过滤 ----------

type filterOp struct {
	child Operator
	pred  Eval
	label string
}

func (o *filterOp) Schema() Schema       { return o.child.Schema() }
func (o *filterOp) Open() error          { return o.child.Open() }
func (o *filterOp) Close() error         { return o.child.Close() }
func (o *filterOp) Children() []Operator { return []Operator{o.child} }
func (o *filterOp) Describe() string {
	if o.label != "" {
		return "Filter (" + o.label + ")"
	}
	return "Filter"
}

func (o *filterOp) Next() (types.Row, bool, error) {
	for {
		row, ok, err := o.child.Next()
		if err != nil || !ok {
			return nil, ok, err
		}
		v, err := o.pred.Eval(row)
		if err != nil {
			return nil, false, err
		}
		b, isNull, err := asBool(v)
		if err != nil {
			return nil, false, err
		}
		if !isNull && b {
			return row, true, nil
		}
	}
}

// ---------- 投影 ----------

type projectOp struct {
	child Operator
	exprs []Eval
	sc    Schema
}

func (o *projectOp) Schema() Schema       { return o.sc }
func (o *projectOp) Open() error          { return o.child.Open() }
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
	row, ok, err := o.child.Next()
	if err != nil || !ok {
		return nil, ok, err
	}
	out := make(types.Row, len(o.exprs))
	for i, e := range o.exprs {
		v, err := e.Eval(row)
		if err != nil {
			return nil, false, err
		}
		out[i] = v
	}
	return out, true, nil
}

// ---------- 去重 ----------

type distinctOp struct {
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

func (o *distinctOp) Open() error {
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
		k := types.Key(row)
		if o.seen[k] {
			continue
		}
		o.seen[k] = true
		o.buf = append(o.buf, row)
	}
	return nil
}

func (o *distinctOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.buf) {
		return nil, false, nil
	}
	r := o.buf[o.i]
	o.i++
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
	child Operator
	keys  []sortKey
	rows  []sortEntry
	i     int
	sc    Schema
	err   error
	// topN > 0 时用「大小为 N 的最大堆」只保留前 N 行（含 OFFSET），
	// 不把整段结果物化下来再全排。分页 `ORDER BY … LIMIT n` 靠它。
	topN int
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

// lessEntry 按排序键比较两行：true 表示 a 应该排在 b 前面。
func (o *sortOp) lessEntry(a, b sortEntry) bool {
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

// topNHeap 是「保留最好的 N 个」用的最大堆：堆顶是当前最差的那行（要被淘汰的）。
// container/heap 是小顶堆，所以 Less 表示「更差、更该丢」。
type topNHeap struct {
	op    *sortOp
	items []sortEntry
}

func (h *topNHeap) Len() int { return len(h.items) }
func (h *topNHeap) Less(i, j int) bool {
	// 堆顶 = 最差 = 排序序里最大（ASC 时键值最大）
	return h.op.lessEntry(h.items[j], h.items[i])
}
func (h *topNHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *topNHeap) Push(x any)    { h.items = append(h.items, x.(sortEntry)) }
func (h *topNHeap) Pop() any {
	n := len(h.items)
	it := h.items[n-1]
	h.items = h.items[:n-1]
	return it
}

func (o *sortOp) Open() error {
	if err := o.child.Open(); err != nil {
		return err
	}
	o.rows = o.rows[:0]
	o.i = 0
	o.err = nil

	// 预计算排序键：每行只 Eval 一次，比较时直接用值
	makeEntry := func(row types.Row) (sortEntry, error) {
		ks := make([]types.Value, len(o.keys))
		for i, k := range o.keys {
			v, err := k.eval.Eval(row)
			if err != nil {
				return sortEntry{}, err
			}
			ks[i] = v
		}
		return sortEntry{row: row, keys: ks}, nil
	}

	if o.topN > 0 {
		h := &topNHeap{op: o}
		for {
			row, ok, err := o.child.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			e, err := makeEntry(row)
			if err != nil {
				return err
			}
			if h.Len() < o.topN {
				heap.Push(h, e)
			} else if o.lessEntry(e, h.items[0]) {
				// e 比堆顶（当前最差）更好 → 换掉堆顶
				h.items[0] = e
				heap.Fix(h, 0)
			}
			if o.err != nil {
				return o.err
			}
		}
		o.rows = h.items
	} else {
		for {
			row, ok, err := o.child.Next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			e, err := makeEntry(row)
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

func (o *sortOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i].row
	o.i++
	return r, true, nil
}

// ---------- LIMIT / OFFSET ----------

type limitOp struct {
	child  Operator
	limit  int64
	offset int64
	seen   int64
	emited int64
}

func (o *limitOp) Schema() Schema       { return o.child.Schema() }
func (o *limitOp) Open() error          { o.seen, o.emited = 0, 0; return o.child.Open() }
func (o *limitOp) Close() error         { return o.child.Close() }
func (o *limitOp) Children() []Operator { return []Operator{o.child} }
func (o *limitOp) Describe() string {
	if o.offset > 0 {
		return fmt.Sprintf("Limit (limit=%d, offset=%d)", o.limit, o.offset)
	}
	return fmt.Sprintf("Limit (limit=%d)", o.limit)
}

func (o *limitOp) Next() (types.Row, bool, error) {
	for o.emited < o.limit {
		row, ok, err := o.child.Next()
		if err != nil || !ok {
			return nil, ok, err
		}
		if o.seen < o.offset {
			o.seen++
			continue
		}
		o.emited++
		return row, true, nil
	}
	return nil, false, nil
}
