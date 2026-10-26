package exec

import (
	"fmt"
	"strings"

	"godb/sql"
	"godb/types"
)

// joinOp 实现 INNER / LEFT / RIGHT / CROSS 连接。
// 实现方式是“物化两侧 + 哈希连接/嵌套循环”，对这个规模的库来说足够快。
type joinOp struct {
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
	if o.i >= len(o.out) {
		return nil, false, nil
	}
	r := o.out[o.i]
	o.i++
	return r, true, nil
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
