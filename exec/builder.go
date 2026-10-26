package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// Planner 负责把 SELECT 语句变成算子树。
type Planner struct {
	Tx  *storage.Tx
	Cat *catalog.Catalog

	// outerScopes 是编译子查询时压入的外层作用域（见 buildSubquery）。
	outerScopes []outerScope
}

// BuildSelect 构建查询计划。
func (p *Planner) BuildSelect(s *sql.Select) (Operator, error) {
	// 集合运算节点是个空壳：SetLeft/SetRight 才是两个操作数。
	if s.SetOp != "" {
		return p.buildSetOp(s)
	}
	op, sc, err := p.buildFrom(s.From)
	if err != nil {
		return nil, err
	}

	if s.Where != nil {
		pred, err := p.compile(s.Where, sc)
		if err != nil {
			return nil, err
		}
		op = &filterOp{child: op, pred: pred, label: sql.String(s.Where)}
	}

	items, err := expandStars(s.Items, sc)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("SELECT must have at least one output column")
	}

	aggMode := len(s.GroupBy) > 0 || s.Having != nil
	for _, it := range items {
		if sql.ContainsAggregate(it.Expr) {
			aggMode = true
		}
	}
	for _, oi := range s.OrderBy {
		if sql.ContainsAggregate(oi.Expr) {
			aggMode = true
		}
	}

	if aggMode {
		return p.buildAggregate(s, items, op, sc)
	}
	return p.buildSimple(s, items, op, sc)
}

// buildFrom 构建 FROM 子树。
func (p *Planner) buildFrom(from []sql.TableRef) (Operator, Schema, error) {
	if len(from) == 0 || from[0] == nil {
		return &oneRowOp{}, nil, nil
	}
	return p.buildTableRef(from[0])
}

func (p *Planner) buildTableRef(r sql.TableRef) (Operator, Schema, error) {
	switch n := r.(type) {
	case *sql.BaseTable:
		t, ok := p.Cat.Get(n.Table)
		if !ok {
			// 不是业务表就看看是不是系统目录（pg_catalog / information_schema）
			if op, sc, vok := p.virtualTable(n.Schema, n.Table, n.Alias); vok {
				return op, sc, nil
			}
			return nil, nil, fmt.Errorf("relation %q does not exist", n.Table)
		}
		qual := n.Alias
		if qual == "" {
			qual = t.Name
		}
		op := newScan(p.Tx, t, qual)
		return op, op.Schema(), nil

	case *sql.SubTable:
		return p.buildSubTable(n)

	case *sql.FuncTable:
		return p.buildFuncTable(n)

	case *sql.ParenJoin:
		op, sc, err := p.buildTableRef(n.Inner)
		if err != nil {
			return nil, nil, err
		}
		if n.Alias == "" {
			return op, sc, nil
		}
		// 别名覆盖内部所有列的限定符：外面写 x.col 就能取到内部任意列，
		// 而内部别名（a、b）在括号外不再可见。
		ns := make(Schema, len(sc))
		copy(ns, sc)
		for i := range ns {
			ns[i].Qualifier = n.Alias
		}
		return &aliasOp{inner: op, sc: ns}, ns, nil

	case *sql.Join:
		lop, lsc, err := p.buildTableRef(n.Left)
		if err != nil {
			return nil, nil, err
		}
		rop, rsc, err := p.buildTableRef(n.Right)
		if err != nil {
			return nil, nil, err
		}
		sc := make(Schema, 0, len(lsc)+len(rsc))
		sc = append(sc, lsc...)
		sc = append(sc, rsc...)
		j := &joinOp{left: lop, right: rop, typ: n.Type, lsc: lsc, rsc: rsc, sc: sc}
		if n.On != nil {
			j.cond = sql.String(n.On)
			conjuncts := splitConjuncts(n.On)
			var residual []sql.Expr
			for _, c := range conjuncts {
				matched := false
				if b, ok := c.(*sql.Binary); ok && b.Op == "=" {
					ll, lr, err1 := sideOf(b.L, lsc, rsc)
					rl, rr, err2 := sideOf(b.R, lsc, rsc)
					if err1 == nil && err2 == nil {
						if ll && rr {
							le, err := p.compile(b.L, lsc)
							if err != nil {
								return nil, nil, err
							}
							re, err := p.compile(b.R, rsc)
							if err != nil {
								return nil, nil, err
							}
							j.leftKeys = append(j.leftKeys, le)
							j.rightKeys = append(j.rightKeys, re)
							matched = true
						} else if rl && lr {
							le, err := p.compile(b.R, lsc)
							if err != nil {
								return nil, nil, err
							}
							re, err := p.compile(b.L, rsc)
							if err != nil {
								return nil, nil, err
							}
							j.leftKeys = append(j.leftKeys, le)
							j.rightKeys = append(j.rightKeys, re)
							matched = true
						}
					}
				}
				if !matched {
					residual = append(residual, c)
				}
			}
			if len(residual) > 0 {
				re, err := p.compile(andAll(residual), sc)
				if err != nil {
					return nil, nil, err
				}
				j.residual = re
			}
		}
		return j, sc, nil
	}
	return nil, nil, fmt.Errorf("unsupported FROM clause")
}

// expandStars 展开 * 与 table.*。
func expandStars(items []*sql.SelectItem, sc Schema) ([]*sql.SelectItem, error) {
	var out []*sql.SelectItem
	for _, it := range items {
		if !it.Star {
			out = append(out, it)
			continue
		}
		matched := false
		for _, c := range sc {
			if it.StarTable != "" && c.Qualifier != strings.ToLower(it.StarTable) {
				continue
			}
			matched = true
			out = append(out, &sql.SelectItem{Expr: &sql.ColumnRef{Table: c.Qualifier, Name: c.Name}})
		}
		if !matched {
			if it.StarTable != "" {
				return nil, fmt.Errorf("table %q does not exist in FROM clause", it.StarTable)
			}
			return nil, fmt.Errorf("SELECT * requires a FROM clause")
		}
	}
	return out, nil
}

func itemName(it *sql.SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	switch e := it.Expr.(type) {
	case *sql.ColumnRef:
		return strings.ToLower(e.Name)
	case *sql.FuncCall:
		return strings.ToLower(e.Name)
	case *sql.Cast:
		return itemName(&sql.SelectItem{Expr: e.X})
	}
	return "?column?"
}

// buildSimple 处理不含聚合的 SELECT。
func (p *Planner) buildSimple(s *sql.Select, items []*sql.SelectItem, op Operator, sc Schema) (Operator, error) {
	exprs := make([]Eval, len(items))
	out := make(Schema, len(items))
	for i, it := range items {
		ev, err := p.compile(it.Expr, sc)
		if err != nil {
			return nil, err
		}
		exprs[i] = ev
		out[i] = ColInfo{Name: itemName(it), Type: ev.Type()}
	}

	if len(s.OrderBy) > 0 {
		keys := make([]sortKey, 0, len(s.OrderBy))
		for _, oi := range s.OrderBy {
			ev, err := p.orderKey(oi, items, exprs, out, &compileCtx{sc: sc})
			if err != nil {
				return nil, err
			}
			nullsFirst := oi.Desc
			if oi.NullsFirst != nil {
				nullsFirst = *oi.NullsFirst
			}
			keys = append(keys, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
		}
		op = &sortOp{child: op, keys: keys, sc: sc}
	}

	op = &projectOp{child: op, exprs: exprs, sc: out}
	if s.Distinct {
		op = &distinctOp{child: op, sc: out}
	}
	return p.applyLimit(s, op)
}

// buildSetOp 构建 UNION / INTERSECT / EXCEPT。
func (p *Planner) buildSetOp(s *sql.Select) (Operator, error) {
	left, err := p.BuildSelect(s.SetLeft)
	if err != nil {
		return nil, err
	}
	right, err := p.BuildSelect(s.SetRight)
	if err != nil {
		return nil, err
	}
	if s.SetAll && s.SetOp != "union" {
		return nil, fmt.Errorf("%s ALL is not supported", strings.ToUpper(s.SetOp))
	}
	sc := left.Schema()
	var op Operator = &setOp{left: left, right: right, op: s.SetOp, all: s.SetAll, sc: sc}

	// ORDER BY / LIMIT 属于整条查询，作用在集合运算的结果上。
	// 结果列名取自左操作数，所以排序键按这份 schema 编译。
	if len(s.OrderBy) > 0 {
		keys := make([]sortKey, 0, len(s.OrderBy))
		for _, oi := range s.OrderBy {
			ev, err := p.compile(oi.Expr, sc)
			if err != nil {
				return nil, err
			}
			nullsFirst := oi.Desc
			if oi.NullsFirst != nil {
				nullsFirst = *oi.NullsFirst
			}
			keys = append(keys, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
		}
		op = &sortOp{child: op, keys: keys, sc: sc}
	}
	return p.applyLimit(s, op)
}

// buildAggregate 处理含聚合/分组/HAVING 的 SELECT。
func (p *Planner) buildAggregate(s *sql.Select, items []*sql.SelectItem, op Operator, sc Schema) (Operator, error) {
	groupEvals := make([]Eval, len(s.GroupBy))
	groupTypes := make([]types.Type, len(s.GroupBy))
	for i, ge := range s.GroupBy {
		ev, err := p.compile(ge, sc)
		if err != nil {
			return nil, err
		}
		groupEvals[i] = ev
		groupTypes[i] = ev.Type()
	}

	col := &aggCollector{byKey: map[string]int{}, sc: sc,
		ctx: &compileCtx{sc: sc, planner: p, outerScopes: p.outerScopes}}
	for _, it := range items {
		if err := col.collect(it.Expr); err != nil {
			return nil, err
		}
	}
	if s.Having != nil {
		if err := col.collect(s.Having); err != nil {
			return nil, err
		}
	}
	for _, oi := range s.OrderBy {
		if err := col.collect(oi.Expr); err != nil {
			return nil, err
		}
	}

	outSc := make(Schema, 0, len(groupEvals)+len(col.aggs))
	keys := map[string]int{}
	for i, ge := range s.GroupBy {
		name := "?column?"
		qual := ""
		if cr, ok := ge.(*sql.ColumnRef); ok {
			name = strings.ToLower(cr.Name)
			qual = strings.ToLower(cr.Table)
		}
		keys[sql.Canonical(ge)] = len(outSc)
		outSc = append(outSc, ColInfo{Qualifier: qual, Name: name, Type: groupTypes[i]})
	}
	aggIdx := map[string]int{}
	for i, spec := range col.aggs {
		aggIdx[spec.key] = len(outSc)
		outSc = append(outSc, ColInfo{Name: fmt.Sprintf("__agg_%d", i), Type: spec.Type})
	}

	agg := &aggOp{child: op, groupEvals: groupEvals, groupTypes: groupTypes, aggs: col.aggs, sc: outSc}
	ctx := &compileCtx{postAgg: true, keys: keys, aggs: aggIdx, groupN: len(groupEvals), scOut: outSc, sc: outSc}

	if s.Having != nil {
		pred, err := compile(s.Having, ctx)
		if err != nil {
			return nil, err
		}
		op = &filterOp{child: agg, pred: pred, label: sql.String(s.Having)}
	} else {
		op = agg
	}

	exprs := make([]Eval, len(items))
	finalSc := make(Schema, len(items))
	for i, it := range items {
		ev, err := compile(it.Expr, ctx)
		if err != nil {
			return nil, err
		}
		exprs[i] = ev
		finalSc[i] = ColInfo{Name: itemName(it), Type: ev.Type()}
	}

	if len(s.OrderBy) > 0 {
		keysOut := make([]sortKey, 0, len(s.OrderBy))
		for _, oi := range s.OrderBy {
			ev, err := p.orderKey(oi, items, exprs, finalSc, ctx)
			if err != nil {
				return nil, err
			}
			nullsFirst := oi.Desc
			if oi.NullsFirst != nil {
				nullsFirst = *oi.NullsFirst
			}
			keysOut = append(keysOut, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
		}
		op = &sortOp{child: op, keys: keysOut, sc: outSc}
	}

	op = &projectOp{child: op, exprs: exprs, sc: finalSc}
	if s.Distinct {
		op = &distinctOp{child: op, sc: finalSc}
	}
	return p.applyLimit(s, op)
}

// orderKey 解析 ORDER BY 的一项：支持序号、输出别名、普通表达式。
func (p *Planner) orderKey(oi *sql.OrderItem, items []*sql.SelectItem, targetExprs []Eval, out Schema, ctx *compileCtx) (Eval, error) {
	// 序号：ORDER BY 2
	if lit, ok := oi.Expr.(*sql.Literal); ok && lit.Val.K == types.KInt {
		n := lit.Val.I
		if n >= 1 && int(n) <= len(items) {
			return targetExprs[n-1], nil
		}
		return nil, fmt.Errorf("ORDER BY position %d is not in select list", n)
	}
	// 别名
	if cr, ok := oi.Expr.(*sql.ColumnRef); ok && cr.Table == "" {
		for i, it := range items {
			if it.Alias != "" && strings.EqualFold(it.Alias, cr.Name) {
				return targetExprs[i], nil
			}
		}
	}
	return compile(oi.Expr, ctx)
}

func (p *Planner) applyLimit(s *sql.Select, op Operator) (Operator, error) {
	if s.Limit == nil && s.Offset == nil {
		return op, nil
	}
	limit := int64(-1)
	offset := int64(0)
	if s.Limit != nil {
		v, err := CompileConst(s.Limit)
		if err != nil {
			return nil, err
		}
		n, err := toInt(v)
		if err != nil || v.IsNull() {
			return nil, fmt.Errorf("LIMIT must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("LIMIT must not be negative")
		}
		limit = n
	}
	if s.Offset != nil {
		v, err := CompileConst(s.Offset)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return nil, fmt.Errorf("OFFSET must be an integer")
		}
		n, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf("OFFSET must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("OFFSET must not be negative")
		}
		offset = n
	}
	if limit < 0 {
		limit = int64(1) << 62
	}
	return &limitOp{child: op, limit: limit, offset: offset}, nil
}

// aggCollector 收集查询中出现的聚合调用（同一聚合只保留一份）。
type aggCollector struct {
	aggs  []*aggSpec
	byKey map[string]int
	sc    Schema
	// ctx 用于把 planner / 外层作用域传给聚合参数的编译（子查询要用）。
	ctx *compileCtx
}

func (c *aggCollector) collect(e sql.Expr) error {
	var firstErr error
	sql.Walk(e, func(n sql.Expr) bool {
		if firstErr != nil {
			return false
		}
		fc, ok := n.(*sql.FuncCall)
		if !ok || !sql.IsAggregate(fc.Name) {
			return true
		}
		key := sql.Canonical(fc)
		if _, exists := c.byKey[key]; exists {
			return false
		}
		spec, err := buildAggSpec(fc, c.sc, c.ctx)
		if err != nil {
			firstErr = err
			return false
		}
		spec.key = key
		c.byKey[key] = len(c.aggs)
		c.aggs = append(c.aggs, spec)
		return false
	})
	return firstErr
}

func buildAggSpec(fc *sql.FuncCall, sc Schema, parent *compileCtx) (*aggSpec, error) {
	name := strings.ToLower(fc.Name)
	spec := &aggSpec{Fn: name, Distinct: fc.Distinct, Label: sql.String(fc)}
	if fc.Star {
		if name != "count" {
			return nil, fmt.Errorf("%s(*) is not supported", name)
		}
		spec.Star = true
		spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
		return spec, nil
	}
	if len(fc.Args) != 1 {
		return nil, fmt.Errorf("aggregate %s requires exactly one argument", name)
	}
	sub := &compileCtx{sc: sc, postAgg: parent.postAgg, keys: parent.keys, aggs: parent.aggs,
		groupN: parent.groupN, scOut: parent.scOut, planner: parent.planner, outerScopes: parent.outerScopes}
	arg, err := compile(fc.Args[0], sub)
	if err != nil {
		return nil, err
	}
	spec.Arg = arg
	switch name {
	case "count":
		spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
	case "sum":
		if arg.Type().Kind == types.KInt {
			spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
		} else {
			spec.Type = types.Type{Kind: types.KFloat, Name: "double precision"}
		}
	case "avg":
		spec.Type = types.Type{Kind: types.KFloat, Name: "double precision"}
	case "min", "max":
		spec.Type = arg.Type()
	}
	return spec, nil
}

// aliasOp 给整棵子树的列换一个限定符，用于括号联接组起别名。
type aliasOp struct {
	inner Operator
	sc    Schema
}

func (o *aliasOp) Schema() Schema                 { return o.sc }
func (o *aliasOp) Open() error                    { return o.inner.Open() }
func (o *aliasOp) Next() (types.Row, bool, error) { return o.inner.Next() }
func (o *aliasOp) Close() error                   { return o.inner.Close() }
func (o *aliasOp) Describe() string               { return "Alias(" + o.sc[0].Qualifier + ")" }
func (o *aliasOp) Children() []Operator           { return []Operator{o.inner} }

// compile 编译表达式，并允许其中出现子查询（标量子查询）。
func (p *Planner) compile(e sql.Expr, sc Schema) (Eval, error) {
	return compile(e, &compileCtx{sc: sc, planner: p, outerScopes: p.outerScopes})
}

// buildSubquery 在「当前外层作用域」下构建子查询计划。
//
// outerRow 指向调用方 subqueryEval 里的当前行，编译出来的 outerRef
// 会持有这个指针，从而支持相关子查询（例如 WHERE inhparent = c.oid）。
func (p *Planner) buildSubquery(s *sql.Select, outer Schema, outerRow *types.Row) (Operator, error) {
	p.outerScopes = append(p.outerScopes, outerScope{sc: outer, row: outerRow})
	defer func() {
		p.outerScopes = p.outerScopes[:len(p.outerScopes)-1]
	}()
	return p.BuildSelect(s)
}
