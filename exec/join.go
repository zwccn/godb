package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// joinOp 实现 INNER / LEFT / RIGHT / CROSS 连接。
// 实现方式是“物化两侧 + 哈希连接/嵌套循环”，对这个规模的库来说足够快。
type joinOp struct {
	OpProfile
	left  Operator
	right Operator
	typ   sql.JoinType
	lsc   Schema
	rsc   Schema
	sc    Schema
	leftN int

	leftKeys  []Eval
	rightKeys []Eval
	residual  Eval
	cond      string

	leftRows  []types.Row
	rightRows []types.Row
	out       []types.Row
	i         int

	// details 是规划器写下的决策说明（为什么用/不用索引连接、谁做构建侧）。
	details []string
}

const maxJoinOutput = 2000000

func (o *joinOp) Schema() Schema       { return o.sc }
func (o *joinOp) Children() []Operator { return []Operator{o.left, o.right} }

func (o *joinOp) Describe() string {
	kind := "Inner"
	switch o.typ {
	case sql.JoinLeft:
		kind = "Left"
	case sql.JoinRight:
		kind = "Right"
	case sql.JoinCross:
		kind = "Cross"
	}
	algo := "Nested Loop"
	if len(o.leftKeys) > 0 {
		algo = "Hash"
	}
	if o.cond != "" {
		return fmt.Sprintf("%s %s Join (on: %s)", algo, kind, o.cond)
	}
	return fmt.Sprintf("%s %s Join", algo, kind)
}

func (o *joinOp) Close() error {
	if err := o.left.Close(); err != nil {
		return err
	}
	return o.right.Close()
}

func (o *joinOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	if err := o.left.Open(); err != nil {
		return err
	}
	o.leftRows = o.leftRows[:0]
	for {
		r, ok, err := o.left.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		o.leftRows = append(o.leftRows, r)
	}
	if err := o.right.Open(); err != nil {
		return err
	}
	o.rightRows = o.rightRows[:0]
	for {
		r, ok, err := o.right.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		o.rightRows = append(o.rightRows, r)
	}
	o.leftN = len(o.lsc)
	return o.buildOutput()
}

func (o *joinOp) Next() (types.Row, bool, error) {
	t := o.begin()
	if o.i >= len(o.out) {
		t.done(false)
		return nil, false, nil
	}
	r := o.out[o.i]
	o.i++
	t.done(true)
	return r, true, nil
}

func (o *joinOp) ExplainDetail() []string {
	out := append([]string{}, o.details...)
	out = append(out,
		fmt.Sprintf("左 %d 行 / 右 %d 行 → 输出 %d 行", len(o.leftRows), len(o.rightRows), len(o.out)))
	return out
}

func (o *joinOp) nullLeft() types.Row  { return make(types.Row, len(o.lsc)) }
func (o *joinOp) nullRight() types.Row { return make(types.Row, len(o.rsc)) }

func combine(l, r types.Row) types.Row {
	out := make(types.Row, 0, len(l)+len(r))
	out = append(out, l...)
	out = append(out, r...)
	return out
}

func (o *joinOp) buildOutput() error {
	o.out = o.out[:0]
	o.i = 0

	// CROSS 或没有等值条件：嵌套循环
	equi := len(o.leftKeys) > 0 && o.typ != sql.JoinCross

	if equi {
		// 构建侧：INNER/LEFT 用右表，RIGHT 用左表
		useRight := o.typ != sql.JoinRight
		buildRows := o.rightRows
		buildKeys := o.rightKeys
		if !useRight {
			buildRows = o.leftRows
			buildKeys = o.leftKeys
		}
		index := make(map[string][]int, len(buildRows))
		for i, row := range buildRows {
			k, hasNull, err := evalKey(buildKeys, row)
			if err != nil {
				return err
			}
			if hasNull {
				continue
			}
			index[k] = append(index[k], i)
		}
		if useRight {
			for _, lrow := range o.leftRows {
				k, hasNull, err := evalKey(o.leftKeys, lrow)
				if err != nil {
					return err
				}
				if hasNull {
					if o.typ == sql.JoinLeft {
						if err := o.push(combine(lrow, o.nullRight())); err != nil {
							return err
						}
					}
					continue
				}
				hits := index[k]
				matched := false
				for _, hi := range hits {
					row := combine(lrow, o.rightRows[hi])
					ok, err := o.pass(row)
					if err != nil {
						return err
					}
					if ok {
						matched = true
						if err := o.push(row); err != nil {
							return err
						}
					}
				}
				if !matched && o.typ == sql.JoinLeft {
					if err := o.push(combine(lrow, o.nullRight())); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, rrow := range o.rightRows {
			k, hasNull, err := evalKey(o.rightKeys, rrow)
			if err != nil {
				return err
			}
			if hasNull {
				if err := o.push(combine(o.nullLeft(), rrow)); err != nil {
					return err
				}
				continue
			}
			hits := index[k]
			matched := false
			for _, hi := range hits {
				row := combine(o.leftRows[hi], rrow)
				ok, err := o.pass(row)
				if err != nil {
					return err
				}
				if ok {
					matched = true
					if err := o.push(row); err != nil {
						return err
					}
				}
			}
			if !matched {
				if err := o.push(combine(o.nullLeft(), rrow)); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// 嵌套循环
	if o.typ == sql.JoinRight {
		matchedRow := make([]bool, len(o.leftRows))
		for _, rrow := range o.rightRows {
			matched := false
			for li, lrow := range o.leftRows {
				row := combine(lrow, rrow)
				ok, err := o.pass(row)
				if err != nil {
					return err
				}
				if ok {
					matched = true
					matchedRow[li] = true
					if err := o.push(row); err != nil {
						return err
					}
				}
			}
			if !matched {
				if err := o.push(combine(o.nullLeft(), rrow)); err != nil {
					return err
				}
			}
		}
		_ = matchedRow
		return nil
	}

	for _, lrow := range o.leftRows {
		matched := false
		for _, rrow := range o.rightRows {
			row := combine(lrow, rrow)
			ok, err := o.pass(row)
			if err != nil {
				return err
			}
			if ok {
				matched = true
				if err := o.push(row); err != nil {
					return err
				}
			}
		}
		if !matched && o.typ == sql.JoinLeft {
			if err := o.push(combine(lrow, o.nullRight())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *joinOp) pass(row types.Row) (bool, error) {
	if o.residual == nil {
		return true, nil
	}
	v, err := o.residual.Eval(row)
	if err != nil {
		return false, err
	}
	b, isNull, err := asBool(v)
	if err != nil {
		return false, err
	}
	return !isNull && b, nil
}

func (o *joinOp) push(row types.Row) error {
	if len(o.out) >= maxJoinOutput {
		return fmt.Errorf("连接结果超过 %d 行，请加上更严格的连接条件", maxJoinOutput)
	}
	o.out = append(o.out, row)
	return nil
}

func evalKey(evals []Eval, row types.Row) (string, bool, error) {
	vals := make([]types.Value, len(evals))
	for i, e := range evals {
		v, err := e.Eval(row)
		if err != nil {
			return "", false, err
		}
		if v.IsNull() {
			// SQL 语义：NULL 不参与等值连接
			return "", true, nil
		}
		vals[i] = v
	}
	return types.Key(vals), false, nil
}

// exprRefs 收集表达式中的列引用。
func exprRefs(e sql.Expr) []*sql.ColumnRef {
	var out []*sql.ColumnRef
	sql.Walk(e, func(n sql.Expr) bool {
		if cr, ok := n.(*sql.ColumnRef); ok {
			out = append(out, cr)
		}
		return true
	})
	return out
}

// sideOf 判断表达式只依赖哪一侧。
func sideOf(e sql.Expr, lsc, rsc Schema) (onlyLeft, onlyRight bool, err error) {
	refs := exprRefs(e)
	if len(refs) == 0 {
		return false, false, nil
	}
	onlyLeft, onlyRight = true, true
	sawLeft, sawRight := false, false
	for _, r := range refs {
		inL := lsc.Contains(r.Table, r.Name)
		inR := rsc.Contains(r.Table, r.Name)
		if inL && inR {
			return false, false, fmt.Errorf("column reference %q is ambiguous", r.Name)
		}
		if !inL && !inR {
			return false, false, fmt.Errorf("column %q does not exist", r.Name)
		}
		if inR {
			onlyLeft = false
			sawRight = true
		}
		if inL {
			onlyRight = false
			sawLeft = true
		}
	}
	if !sawLeft && !sawRight {
		return false, false, nil
	}
	return onlyLeft, onlyRight, nil
}

// splitConjuncts 把 a AND b AND c 拆成 [a, b, c]。
func splitConjuncts(e sql.Expr) []sql.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*sql.Binary); ok && b.Op == "and" {
		return append(splitConjuncts(b.L), splitConjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

func andAll(list []sql.Expr) sql.Expr {
	if len(list) == 0 {
		return nil
	}
	out := list[0]
	for _, e := range list[1:] {
		out = &sql.Binary{Op: "and", L: out, R: e}
	}
	return out
}

// joinCondText 生成 ON 条件的可读文本。
func joinCondText(conjuncts []sql.Expr) string {
	parts := make([]string, 0, len(conjuncts))
	for _, c := range conjuncts {
		parts = append(parts, sql.String(c))
	}
	return strings.Join(parts, " AND ")
}

// ---------- 索引连接（索引嵌套循环） ----------

// indexJoinOp：外表逐行驱动，内表按索引点查。
//
// 和 joinOp 的区别在「内表要不要物化」。哈希连接必须把两侧都读进来；索引连接
// 只读外表，内表每行按索引取。所以「外表很小、内表很大且有索引」时差一个数量级
// —— 典型的就是按主键过滤维表、再拿它去捞事实表。
//
// 内表的连接列必须是**某个索引的完整列**（顺序一致）。部分前缀也能做（键区间），
// 但那要多一条代码路径，而 99% 的关联条件都是完整主键/唯一键，先只做这一种。
type indexJoinOp struct {
	OpProfile
	tx    *storage.Tx
	outer Operator
	inner *catalog.Table
	typ   sql.JoinType
	osc   Schema
	isc   Schema
	sc    Schema
	// swap 为 true 表示内表是左表（外表是右表）：拼行时顺序要反过来，
	// 因为输出的列顺序永远是 left++right。只有 INNER 会交换。
	swap bool

	// outerKeys 在外表行上求值，结果就是内表的索引键
	outerKeys []Eval
	ixName    string
	ixCols    []int
	ixNames   []string
	residual  Eval
	cond      string
	details   []string

	outerRows []types.Row
	oi        int
	matched   []types.Row
	mi        int
	vals      []types.Value // 键值缓冲，按行复用
}

func (o *indexJoinOp) Schema() Schema { return o.sc }

func (o *indexJoinOp) Children() []Operator { return []Operator{o.outer} }

func (o *indexJoinOp) Describe() string {
	kind := "Inner"
	if o.typ == sql.JoinLeft {
		kind = "Left"
	}
	name := o.ixName
	if name == "" {
		name = o.inner.Name + "_pkey"
	}
	s := fmt.Sprintf("Index %s Join using %s on %s", kind, name, o.inner.Name)
	if o.cond != "" {
		s += " (on: " + o.cond + ")"
	}
	return s
}

func (o *indexJoinOp) Open() error {
	t := o.begin()
	defer func() { t.done(false) }()
	if err := o.outer.Open(); err != nil {
		return err
	}
	o.outerRows = o.outerRows[:0]
	for {
		r, ok, err := o.outer.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		o.outerRows = append(o.outerRows, r)
	}
	o.oi, o.mi = 0, 0
	o.matched = o.matched[:0]
	return nil
}

func (o *indexJoinOp) Next() (types.Row, bool, error) {
	t := o.begin()
	for {
		if o.mi < len(o.matched) {
			r := o.matched[o.mi]
			o.mi++
			t.done(true)
			return r, true, nil
		}
		if o.oi >= len(o.outerRows) {
			t.done(false)
			return nil, false, nil
		}
		orow := o.outerRows[o.oi]
		o.oi++
		if err := o.fetch(orow); err != nil {
			t.done(false)
			return nil, false, err
		}
		if len(o.matched) == 0 && o.typ == sql.JoinLeft {
			o.matched = append(o.matched, o.join(o.nullInner(), orow))
		}
	}
}

func (o *indexJoinOp) Close() error { return o.outer.Close() }

func (o *indexJoinOp) join(irow, orow types.Row) types.Row {
	if o.swap {
		return combine(irow, orow)
	}
	return combine(orow, irow)
}

func (o *indexJoinOp) nullInner() types.Row { return make(types.Row, len(o.isc)) }

// fetch 用当前外行去查内表索引，把命中的行填进 matched。
func (o *indexJoinOp) fetch(orow types.Row) error {
	o.matched = o.matched[:0]
	o.mi = 0
	if o.vals == nil || len(o.vals) != len(o.outerKeys) {
		o.vals = make([]types.Value, len(o.outerKeys))
	}
	for i, e := range o.outerKeys {
		v, err := e.Eval(orow)
		if err != nil {
			return err
		}
		if v.IsNull() {
			return nil // NULL 不参与等值连接
		}
		// 连接两侧的列类型可能不同（int4 对 int8），键要和索引里存的一致
		cv, err := types.Cast(v, o.inner.Columns[o.ixCols[i]].Type())
		if err != nil {
			return nil
		}
		o.vals[i] = cv
	}
	key := storage.OrderKey(o.vals)
	if o.ixName == "" {
		if hit, ok := o.tx.PKLookup(o.inner, key); ok {
			o.push(hit.Row, orow)
		}
		return nil
	}
	for _, hit := range o.tx.IndexLookup(o.inner, o.ixName, o.ixCols, key) {
		o.push(hit.Row, orow)
	}
	return nil
}

func (o *indexJoinOp) push(irow, orow types.Row) {
	row := o.join(irow, orow)
	if o.residual != nil {
		v, err := o.residual.Eval(row)
		if err != nil {
			return
		}
		b, isNull, err := asBool(v)
		if err != nil || isNull || !b {
			return
		}
	}
	o.matched = append(o.matched, row)
}

func (o *indexJoinOp) ExplainDetail() []string {
	out := append([]string{}, o.details...)
	side := "右"
	if o.swap {
		side = "左"
	}
	out = append(out, fmt.Sprintf("驱动 %d 行，内表（%s）按索引点查", len(o.outerRows), side))
	return out
}
