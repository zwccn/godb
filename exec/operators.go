package exec

import (
	"fmt"
	"sort"
	"strings"

	"godb/catalog"
	"godb/storage"
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

// ExplainPlan 把算子树渲染成文本。
func ExplainPlan(op Operator) []string {
	var out []string
	var walk func(o Operator, depth int)
	walk = func(o Operator, depth int) {
		label := fmt.Sprintf("%T", o)
		var kids []Operator
		if pn, ok := o.(planNode); ok {
			label = pn.Describe()
			kids = pn.Children()
		}
		out = append(out, strings.Repeat("  ", depth)+label)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(op, 0)
	return out
}

// ---------- 全表扫描 ----------

type scanOp struct {
	tx    *storage.Tx
	table string
	qual  string
	sc    Schema
	rows  []types.Row
	i     int
}

func newScan(tx *storage.Tx, t *catalog.Table, qual string) *scanOp {
	return &scanOp{tx: tx, table: t.Name, qual: qual, sc: tableSchema(t, qual)}
}

func (o *scanOp) Schema() Schema { return o.sc }

func (o *scanOp) Open() error {
	o.rows = o.rows[:0]
	o.i = 0
	return o.tx.Scan(o.table, func(_ storage.Ref, row types.Row) error {
		o.rows = append(o.rows, row)
		return nil
	})
}

func (o *scanOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

func (o *scanOp) Close() error { o.rows = nil; return nil }

func (o *scanOp) Describe() string {
	return fmt.Sprintf("Seq Scan on %s", o.table)
}
func (o *scanOp) Children() []Operator { return nil }

// tableSchema 按表定义生成 FROM 子树的列布局。
func tableSchema(t *catalog.Table, qual string) Schema {
	sc := make(Schema, len(t.Columns))
	for i, c := range t.Columns {
		sc[i] = ColInfo{Qualifier: strings.ToLower(qual), Name: strings.ToLower(c.Name), Type: c.Type()}
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
	tx     *storage.Tx
	tbl    *catalog.Table
	ixName string // 空表示走主键哈希索引
	cols   []int
	vals   []types.Value // 已按列类型归一
	sc     Schema
	rows   []types.Row
	i      int
}

func newIndexScan(tx *storage.Tx, t *catalog.Table, ixName string, cols []int, vals []types.Value, qual string) *indexScanOp {
	return &indexScanOp{
		tx: tx, tbl: t, ixName: ixName, cols: cols, vals: vals,
		sc: tableSchema(t, qual),
	}
}

func (o *indexScanOp) Schema() Schema { return o.sc }

func (o *indexScanOp) Open() error {
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
		return nil
	}
	for _, hit := range o.tx.IndexLookup(o.tbl, o.ixName, o.cols, key) {
		o.rows = append(o.rows, hit.Row)
	}
	return nil
}

func (o *indexScanOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
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

// indexRangeScanOp 用索引扫一个键区间（`BETWEEN`、`>`、`LIKE 'x%'`）。
//
// 和等值扫描一样，它只负责缩小候选集，WHERE 仍由上层 filterOp 再判一遍。
type indexRangeScanOp struct {
	tx     *storage.Tx
	tbl    *catalog.Table
	ixName string // 空表示主键
	cols   []int
	r      storage.KeyRange
	sc     Schema
	rows   []types.Row
	i      int
}

func newIndexRangeScan(tx *storage.Tx, t *catalog.Table, ixName string, cols []int, r storage.KeyRange, qual string) *indexRangeScanOp {
	return &indexRangeScanOp{
		tx: tx, tbl: t, ixName: ixName, cols: cols, r: r,
		sc: tableSchema(t, qual),
	}
}

func (o *indexRangeScanOp) Schema() Schema { return o.sc }

func (o *indexRangeScanOp) Open() error {
	o.rows = o.rows[:0]
	o.i = 0
	for _, h := range o.tx.IndexRange(o.tbl, o.ixName, o.cols, o.r) {
		o.rows = append(o.rows, h.Row)
	}
	return nil
}

func (o *indexRangeScanOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

func (o *indexRangeScanOp) Close() error {
	o.rows = nil
	o.i = 0
	return nil
}

func (o *indexRangeScanOp) Describe() string {
	name := o.ixName
	if name == "" {
		name = o.tbl.Name + "_pkey"
	}
	return fmt.Sprintf("Index Range Scan using %s on %s", name, o.tbl.Name)
}

func (o *indexRangeScanOp) Children() []Operator { return nil }

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

type sortOp struct {
	child Operator
	keys  []sortKey
	rows  []types.Row
	i     int
	sc    Schema
	err   error
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
	return "Sort (" + strings.Join(parts, ", ") + ")"
}

func (o *sortOp) Open() error {
	if err := o.child.Open(); err != nil {
		return err
	}
	o.rows = o.rows[:0]
	o.i = 0
	o.err = nil
	for {
		row, ok, err := o.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		o.rows = append(o.rows, row)
	}
	keys := o.keys
	rows := o.rows
	sort.SliceStable(rows, func(i, j int) bool {
		if o.err != nil {
			return false
		}
		for _, k := range keys {
			av, err := k.eval.Eval(rows[i])
			if err != nil {
				o.err = err
				return false
			}
			bv, err := k.eval.Eval(rows[j])
			if err != nil {
				o.err = err
				return false
			}
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
			if k.desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	return o.err
}

func (o *sortOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
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
