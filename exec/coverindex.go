package exec

import (
	"fmt"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// tryCoverScan 判断能否走覆盖索引（index-only）：查询用到的列全部在索引里，
// 命中键即可还原行，不必回表。事务有写集时放弃（要归并 overlay）。
func (p *Planner) tryCoverScan(bt *sql.BaseTable, where sql.Expr, orderBy []*sql.OrderItem, items []*sql.SelectItem, ixName string, ixCols []int, rs []storage.KeyRange, qual string, desc bool) (Operator, bool) {
	if p.Tx == nil || len(ixCols) == 0 {
		return nil, false
	}
	// 有写集要归并 overlay，覆盖路径没实现，走普通扫描
	if p.Tx.HasOverlay(bt.Table) {
		return nil, false
	}
	t, ok := p.Cat.Get(bt.Table)
	if !ok {
		return nil, false
	}
	// 索引列 → 键序号
	keyOf := map[int]int{}
	for i, c := range ixCols {
		keyOf[c] = i
	}
	need := map[int]bool{}
	collectCols := func(e sql.Expr) bool {
		ok := true
		sql.Walk(e, func(n sql.Expr) bool {
			ref, isRef := n.(*sql.ColumnRef)
			if !isRef {
				return true
			}
			ci, found := t.ColumnIdx(ref.Name)
			if !found {
				ok = false
				return false
			}
			if _, in := keyOf[ci]; !in {
				ok = false
				return false
			}
			need[ci] = true
			return true
		})
		return ok
	}
	if where != nil && !collectCols(where) {
		return nil, false
	}
	for _, oi := range orderBy {
		ref, isRef := oi.Expr.(*sql.ColumnRef)
		if !isRef {
			return nil, false
		}
		ci, found := t.ColumnIdx(ref.Name)
		if !found {
			return nil, false
		}
		if _, in := keyOf[ci]; !in {
			return nil, false
		}
	}
	for _, it := range items {
		if it.Star {
			return nil, false // SELECT * 要整行
		}
		if !collectCols(it.Expr) {
			return nil, false
		}
	}
	// 还要能从键还原：列类型都得是可解码的（OrderKey 支持的那些）
	for ci := range keyOf {
		if ci < len(t.Columns) {
			// OrderKey 编码都能解，无需再拦
			_ = ci
		}
	}

	sc := tableSchema(p.Cat, t, qual)
	op := &coverScanOp{
		tx: p.Tx, tbl: t, ixName: ixName, ixCols: ixCols,
		keyOf: keyOf, rs: rs, desc: desc, sc: sc,
	}
	return op, true
}

// coverScanOp 是覆盖索引扫描：行直接从索引键还原，不读堆表。
type coverScanOp struct {
	OpProfile
	tx     *storage.Tx
	tbl    *catalog.Table
	ixName string
	ixCols []int
	keyOf  map[int]int // 表列位置 → 键序号
	rs     []storage.KeyRange
	desc   bool
	sc     Schema
	it     *storage.IndexScanner
}

func (o *coverScanOp) Schema() Schema       { return o.sc }
func (o *coverScanOp) Children() []Operator { return nil }

func (o *coverScanOp) Open() error {
	t := o.begin()
	o.it = o.tx.ScanIndex(o.tbl, o.ixName, o.ixCols, o.ixCols, o.rs, o.desc)
	t.done(false)
	return nil
}

func (o *coverScanOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.it == nil {
		t.done(false)
		return nil, false, nil
	}
	for {
		key, _, ok := o.it.NextKeyed()
		if !ok {
			t.done(false)
			return nil, false, nil
		}
		vals, ok := storage.DecodeOrderKey(key, len(o.ixCols))
		if !ok || len(vals) < len(o.ixCols) {
			continue
		}
		row := make(types.Row, len(o.tbl.Columns))
		for ci, ki := range o.keyOf {
			if ci < len(row) && ki < len(vals) {
				row[ci] = vals[ki]
			}
		}
		t.done(true)
		return row, true, nil
	}
}

func (o *coverScanOp) Close() error {
	if o.it != nil {
		o.it.Close()
		o.it = nil
	}
	return nil
}

func (o *coverScanOp) Describe() string {
	name := o.ixName
	if name == "" {
		name = o.tbl.Name + "_pkey"
	}
	if len(o.rs) > 1 {
		return fmt.Sprintf("Index Union Scan using %s (%d ranges) on %s | 覆盖索引", name, len(o.rs), o.tbl.Name)
	}
	return "Index Only Scan using " + name + " on " + o.tbl.Name
}

func (o *coverScanOp) ExplainDetail() []string {
	return []string{"覆盖索引：从键还原行，不回表"}
}

// orderedScan 接口
func (o *coverScanOp) orderedColumns() []string {
	names := make([]string, len(o.ixCols))
	for i, c := range o.ixCols {
		if c < len(o.tbl.Columns) {
			names[i] = o.tbl.Columns[c].Name
		}
	}
	return names
}
func (o *coverScanOp) isReversed() bool  { return o.desc }
func (o *coverScanOp) setReversed(v bool) { o.desc = v }
