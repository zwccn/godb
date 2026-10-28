package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// tryFastAgg 处理「无 GROUP BY / HAVING」的简单聚合：
//
//	SELECT count(*) FROM t;
//	SELECT min(id), max(id) FROM t;
//
// count(*) 直接用行数（含本事务 overlay 净变化）；min/max 命中索引首列时
// 走树端点。命中不了就返回 false，回落到通用聚合。
func (p *Planner) tryFastAgg(s *sql.Select, items []*sql.SelectItem, sc Schema) (Operator, bool) {
	if p.Tx == nil || len(s.GroupBy) > 0 || s.Having != nil || s.Distinct {
		return nil, false
	}
	if len(s.OrderBy) > 0 {
		return nil, false // ORDER BY 聚合结果走通用路径
	}
	if len(s.From) != 1 {
		return nil, false
	}
	bt, ok := s.From[0].(*sql.BaseTable)
	if !ok || p.lookupCTE(bt.Table) != nil {
		return nil, false
	}
	t, ok := p.Cat.Get(bt.Table)
	if !ok {
		return nil, false
	}
	qual := bt.Alias
	if qual == "" {
		qual = t.Name
	}
	qual = strings.ToLower(qual)

	// 只认「纯聚合」输出列；带表达式（count(*)+1 之类）走通用路径
	var slots []fastAggSlot
	for _, it := range items {
		if it.Star {
			return nil, false
		}
		fc, isFn := it.Expr.(*sql.FuncCall)
		if !isFn {
			return nil, false
		}
		name := strings.ToLower(fc.Name)
		if name == "count" {
			// 只有 count(*) / count(1) 可走快路径（count(col) 要跳过 NULL）
			if !fc.Star && !(len(fc.Args) == 1 && isConstOne(fc.Args[0])) {
				return nil, false
			}
			slots = append(slots, fastAggSlot{fn: "count", name: itemName(it), typ: types.Type{Kind: types.KInt, Name: "bigint"}})
			continue
		}
		if name != "min" && name != "max" {
			return nil, false
		}
		if len(fc.Args) != 1 {
			return nil, false
		}
		ref, isCol := fc.Args[0].(*sql.ColumnRef)
		if !isCol {
			return nil, false
		}
		ci, _, err := sc.Resolve(ref.Table, ref.Name)
		if err != nil {
			return nil, false
		}
		// 必须是这张表的列（不是表达式别名）
		if !strings.EqualFold(sc[ci].Qualifier, qual) {
			return nil, false
		}
		cIdx, found := t.ColumnIdx(ref.Name)
		if !found {
			return nil, false
		}
		ixName, ok := endpointIndex(t, cIdx)
		if !ok {
			return nil, false
		}
		slots = append(slots, fastAggSlot{fn: name, col: cIdx, ixName: ixName, max: name == "max", name: itemName(it), typ: sc[ci].Type})
	}

	// 有 WHERE 就不算「快」——交给通用聚合（流式扫也够快）
	if s.Where != nil {
		return nil, false
	}

	outSc := make(Schema, len(slots))
	for i, sl := range slots {
		outSc[i] = ColInfo{Qualifier: qual, Name: sl.name, Type: sl.typ}
	}
	return &fastAggOp{
		tx: p.Tx, t: t, slots: slots, sc: outSc,
	}, true
}

// fastAggSlot 是快聚合里的一个输出列。
type fastAggSlot struct {
	fn    string // count | min | max
	col   int    // min/max 的列位置；count 忽略
	ixName string // min/max 用的索引名（空 = 主键）
	max   bool
	name  string
	typ   types.Type
}

// isConstOne 判断参数是否是字面量 1（count(1)）。
func isConstOne(e sql.Expr) bool {
	lit, ok := e.(*sql.Literal)
	return ok && lit.Val.K == types.KInt && lit.Val.I == 1
}

// endpointIndex 找能给 min/max(col) 当端点的索引：col 是索引首列（含主键）。
func endpointIndex(t *catalog.Table, col int) (string, bool) {
	if pk := t.PKIdxes(); len(pk) > 0 && pk[0] == col {
		return "", true // 主键
	}
	for _, ix := range t.Indexes {
		if len(ix.Columns) == 0 {
			continue
		}
		_, idx, ok := t.Col(ix.Columns[0])
		if ok && idx == col {
			return ix.Name, true
		}
	}
	return "", false
}

// fastAggOp 在 Open 时算出聚合结果，Next 只吐一行。
type fastAggOp struct {
	tx    *storage.Tx
	t     *catalog.Table
	slots []fastAggSlot
	sc    Schema
	done bool
	row  types.Row
}

func (o *fastAggOp) Schema() Schema { return o.sc }
func (o *fastAggOp) Children() []Operator {
	return nil
}
func (o *fastAggOp) Close() error    { return nil }
func (o *fastAggOp) Describe() string { return "Fast Aggregate on " + o.t.Name }

func (o *fastAggOp) Open() error {
	o.done = false
	o.row = make(types.Row, len(o.slots))
	for i, sl := range o.slots {
		switch sl.fn {
		case "count":
			o.row[i] = types.Int(o.tx.CountLive(o.t.Name))
		case "min", "max":
			v, ok, err := o.tx.IndexEndpoint(o.t, sl.ixName, sl.col, sl.max)
			if err != nil {
				return err
			}
			if !ok {
				o.row[i] = types.Null
			} else {
				o.row[i] = v
			}
		default:
			return fmt.Errorf("fast aggregate: unsupported fn %q", sl.fn)
		}
	}
	return nil
}

func (o *fastAggOp) Next() (types.Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	o.done = true
	return o.row, true, nil
}

// ExplainDetail 说明走了快路径。
func (o *fastAggOp) ExplainDetail() []string {
	parts := make([]string, 0, len(o.slots))
	for _, sl := range o.slots {
		if sl.fn == "count" {
			parts = append(parts, "count(*) → 行数")
		} else {
			parts = append(parts, sl.fn+"("+sl.name+") → 索引端点")
		}
	}
	return []string{"快聚合：" + strings.Join(parts, "、")}
}
