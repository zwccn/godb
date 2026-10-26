// Package exec 把 AST 变成算子树并执行。
package exec

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"godb/sql"
	"godb/types"
)

// ColInfo 是输出/中间结果中的一列。
type ColInfo struct {
	Qualifier string
	Name      string
	Type      types.Type
}

// Schema 是一组列。
type Schema []ColInfo

// Resolve 解析列引用，返回列位置与类型。
func (s Schema) Resolve(table, name string) (int, types.Type, error) {
	name = strings.ToLower(name)
	table = strings.ToLower(table)
	found := -1
	for i, c := range s {
		if c.Name != name {
			continue
		}
		if table != "" && c.Qualifier != table {
			continue
		}
		if found >= 0 {
			if table != "" {
				return 0, types.Type{}, fmt.Errorf("column reference %q is ambiguous", name)
			}
			return 0, types.Type{}, fmt.Errorf("column reference %q is ambiguous", name)
		}
		found = i
	}
	if found < 0 {
		if table != "" {
			return 0, types.Type{}, fmt.Errorf("column %s.%s does not exist", table, name)
		}
		return 0, types.Type{}, fmt.Errorf("column %q does not exist", name)
	}
	return found, s[found].Type, nil
}

// Contains 判断 schema 中是否有某列（用于 JOIN 侧判定）。
func (s Schema) Contains(table, name string) bool {
	_, _, err := s.Resolve(table, name)
	return err == nil
}

// Eval 是可求值表达式。
type Eval interface {
	Eval(row types.Row) (types.Value, error)
	Type() types.Type
}

// compileCtx 携带编译期上下文（普通模式 / 聚合后模式）。
type compileCtx struct {
	sc      Schema
	postAgg bool
	keys    map[string]int // GROUP BY 表达式规范文本 → 输出列下标
	aggs    map[string]int // 聚合表达式规范文本 → 输出列下标
	groupN  int            // 组键数量（输出 schema 前缀长度）
	scOut   Schema         // 聚合输出 schema
}

func (c *compileCtx) aggregateAt(e sql.Expr) (int, bool) {
	if !c.postAgg || c.aggs == nil {
		return 0, false
	}
	idx, ok := c.aggs[sql.Canonical(e)]
	return idx, ok
}

func (c *compileCtx) groupAt(e sql.Expr) (int, bool) {
	if !c.postAgg || c.keys == nil {
		return 0, false
	}
	idx, ok := c.keys[sql.Canonical(e)]
	return idx, ok
}

// Compile 在给定 schema 下编译表达式。
func Compile(e sql.Expr, sc Schema) (Eval, error) {
	return compile(e, &compileCtx{sc: sc})
}

func compile(e sql.Expr, c *compileCtx) (Eval, error) {
	if e == nil {
		return nil, fmt.Errorf("empty expression")
	}
	// 聚合后模式：先尝试按整棵子树匹配组键或聚合结果
	if c.postAgg {
		if idx, ok := c.groupAt(e); ok {
			return &colEval{idx: idx, t: c.scOut[idx].Type}, nil
		}
		if fc, ok := e.(*sql.FuncCall); ok && sql.IsAggregate(fc.Name) {
			if idx, ok := c.aggregateAt(e); ok {
				return &colEval{idx: idx, t: c.scOut[idx].Type}, nil
			}
			return nil, fmt.Errorf("aggregate %s is not available here", fc.Name)
		}
	}

	switch n := e.(type) {
	case *sql.Literal:
		return &constEval{v: n.Val, t: types.TypeOf(n.Val)}, nil

	case *sql.ColumnRef:
		idx, t, err := c.sc.Resolve(n.Table, n.Name)
		if err != nil {
			if c.postAgg {
				return nil, fmt.Errorf("column %q must appear in the GROUP BY clause or be used in an aggregate function", n.Name)
			}
			return nil, err
		}
		return &colEval{idx: idx, t: t}, nil

	case *sql.Binary:
		l, err := compile(n.L, c)
		if err != nil {
			return nil, err
		}
		r, err := compile(n.R, c)
		if err != nil {
			return nil, err
		}
		return &binEval{op: n.Op, l: l, r: r, t: binType(n.Op, l.Type(), r.Type())}, nil

	case *sql.Unary:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		switch n.Op {
		case "not":
			return &notEval{x: x}, nil
		case "-":
			return &negEval{x: x, t: x.Type()}, nil
		case "+":
			return x, nil
		}
		return nil, fmt.Errorf("unsupported unary operator %q", n.Op)

	case *sql.IsNull:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		return &isNullEval{x: x, not: n.Not}, nil

	case *sql.In:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		list := make([]Eval, 0, len(n.List))
		for _, item := range n.List {
			v, err := compile(item, c)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return &inEval{x: x, list: list, not: n.Not}, nil

	case *sql.Between:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		lo, err := compile(n.Lo, c)
		if err != nil {
			return nil, err
		}
		hi, err := compile(n.Hi, c)
		if err != nil {
			return nil, err
		}
		return &betweenEval{x: x, lo: lo, hi: hi, not: n.Not}, nil

	case *sql.Like:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		p, err := compile(n.Pattern, c)
		if err != nil {
			return nil, err
		}
		return &likeEval{x: x, pat: p, not: n.Not, ic: n.IgnoreCase}, nil

	case *sql.Cast:
		x, err := compile(n.X, c)
		if err != nil {
			return nil, err
		}
		t, err := types.ParseType(n.TypeName, n.TypeArgs)
		if err != nil {
			return nil, err
		}
		return &castEval{x: x, t: t}, nil

	case *sql.Case:
		return compileCase(n, c)

	case *sql.FuncCall:
		if sql.IsAggregate(n.Name) {
			if c.postAgg {
				return nil, fmt.Errorf("aggregate %s used in an invalid context", n.Name)
			}
			return nil, fmt.Errorf("aggregate functions are not allowed here")
		}
		return compileScalar(n, c)

	case *sql.DefaultVal:
		return nil, fmt.Errorf("DEFAULT is not allowed here")
	}
	return nil, fmt.Errorf("unsupported expression")
}

// CompileConst 编译“不含任何列引用”的常量表达式。
func CompileConst(e sql.Expr) (types.Value, error) {
	if e == nil {
		return types.Null, nil
	}
	ev, err := Compile(e, nil)
	if err != nil {
		return types.Null, err
	}
	return ev.Eval(nil)
}

// ---------- 基本求值节点 ----------

type constEval struct {
	v types.Value
	t types.Type
}

func (e *constEval) Eval(types.Row) (types.Value, error) { return e.v, nil }
func (e *constEval) Type() types.Type                    { return e.t }

type colEval struct {
	idx int
	t   types.Type
}

func (e *colEval) Eval(row types.Row) (types.Value, error) {
	if e.idx >= len(row) {
		return types.Null, fmt.Errorf("column index %d out of range", e.idx)
	}
	return row[e.idx], nil
}
func (e *colEval) Type() types.Type { return e.t }

type binEval struct {
	op   string
	l, r Eval
	t    types.Type
}

func (e *binEval) Type() types.Type { return e.t }

func (e *binEval) Eval(row types.Row) (types.Value, error) {
	switch e.op {
	case "and", "or":
		return e.evalLogic(row)
	}
	lv, err := e.l.Eval(row)
	if err != nil {
		return types.Null, err
	}
	rv, err := e.r.Eval(row)
	if err != nil {
		return types.Null, err
	}
	switch e.op {
	case "=", "<>", "<", "<=", ">", ">=":
		if lv.IsNull() || rv.IsNull() {
			return types.Null, nil
		}
		c, err := types.Compare(lv, rv)
		if err != nil {
			return types.Null, err
		}
		switch e.op {
		case "=":
			return types.Bool(c == 0), nil
		case "<>":
			return types.Bool(c != 0), nil
		case "<":
			return types.Bool(c < 0), nil
		case "<=":
			return types.Bool(c <= 0), nil
		case ">":
			return types.Bool(c > 0), nil
		case ">=":
			return types.Bool(c >= 0), nil
		}
	case "is":
		// NULL 安全比较（x IS TRUE / x IS FALSE）
		if lv.IsNull() || rv.IsNull() {
			return types.Bool(lv.IsNull() && rv.IsNull()), nil
		}
		c, err := types.Compare(lv, rv)
		if err != nil {
			return types.Null, err
		}
		return types.Bool(c == 0), nil
	case "||":
		if lv.IsNull() || rv.IsNull() {
			return types.Null, nil
		}
		return types.Text(lv.TextForm() + rv.TextForm()), nil
	}

	// 算术
	if lv.IsNull() || rv.IsNull() {
		return types.Null, nil
	}
	if e.t.Kind == types.KInt {
		a, err := toInt(lv)
		if err != nil {
			return types.Null, err
		}
		b, err := toInt(rv)
		if err != nil {
			return types.Null, err
		}
		switch e.op {
		case "+":
			return types.Int(a + b), nil
		case "-":
			return types.Int(a - b), nil
		case "*":
			return types.Int(a * b), nil
		case "/":
			if b == 0 {
				return types.Null, fmt.Errorf("division by zero")
			}
			return types.Int(a / b), nil
		case "%":
			if b == 0 {
				return types.Null, fmt.Errorf("division by zero")
			}
			return types.Int(a % b), nil
		}
	}
	a, err := toFloat(lv)
	if err != nil {
		return types.Null, err
	}
	b, err := toFloat(rv)
	if err != nil {
		return types.Null, err
	}
	switch e.op {
	case "+":
		return types.Float(a + b), nil
	case "-":
		return types.Float(a - b), nil
	case "*":
		return types.Float(a * b), nil
	case "/":
		if b == 0 {
			return types.Null, fmt.Errorf("division by zero")
		}
		return types.Float(a / b), nil
	case "%":
		if b == 0 {
			return types.Null, fmt.Errorf("division by zero")
		}
		return types.Float(math.Mod(a, b)), nil
	}
	return types.Null, fmt.Errorf("unsupported operator %q", e.op)
}

func (e *binEval) evalLogic(row types.Row) (types.Value, error) {
	lv, err := e.l.Eval(row)
	if err != nil {
		return types.Null, err
	}
	lb, lnull, err := asBool(lv)
	if err != nil {
		return types.Null, err
	}
	if e.op == "and" && !lnull && !lb {
		return types.Bool(false), nil
	}
	if e.op == "or" && !lnull && lb {
		return types.Bool(true), nil
	}
	rv, err := e.r.Eval(row)
	if err != nil {
		return types.Null, err
	}
	rb, rnull, err := asBool(rv)
	if err != nil {
		return types.Null, err
	}
	if e.op == "and" {
		if !rnull && !rb {
			return types.Bool(false), nil
		}
		if lnull || rnull {
			return types.Null, nil
		}
		return types.Bool(true), nil
	}
	if !rnull && rb {
		return types.Bool(true), nil
	}
	if lnull || rnull {
		return types.Null, nil
	}
	return types.Bool(false), nil
}

type notEval struct{ x Eval }

func (e *notEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }
func (e *notEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	b, isNull, err := asBool(v)
	if err != nil || isNull {
		return types.Null, err
	}
	return types.Bool(!b), nil
}

type negEval struct {
	x Eval
	t types.Type
}

func (e *negEval) Type() types.Type { return e.t }
func (e *negEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	if v.IsNull() {
		return types.Null, nil
	}
	if v.K == types.KInt {
		return types.Int(-v.I), nil
	}
	f, err := toFloat(v)
	if err != nil {
		return types.Null, err
	}
	return types.Float(-f), nil
}

type isNullEval struct {
	x   Eval
	not bool
}

func (e *isNullEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }
func (e *isNullEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	isNull := v.IsNull()
	if e.not {
		return types.Bool(!isNull), nil
	}
	return types.Bool(isNull), nil
}

type inEval struct {
	x    Eval
	list []Eval
	not  bool
}

func (e *inEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }
func (e *inEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	if v.IsNull() {
		return types.Null, nil
	}
	anyNull := false
	for _, item := range e.list {
		iv, err := item.Eval(row)
		if err != nil {
			return types.Null, err
		}
		if iv.IsNull() {
			anyNull = true
			continue
		}
		c, err := types.Compare(v, iv)
		if err != nil {
			return types.Null, err
		}
		if c == 0 {
			return types.Bool(!e.not), nil
		}
	}
	if anyNull {
		return types.Null, nil
	}
	return types.Bool(e.not), nil
}

type betweenEval struct {
	x, lo, hi Eval
	not       bool
}

func (e *betweenEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }
func (e *betweenEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	lo, err := e.lo.Eval(row)
	if err != nil {
		return types.Null, err
	}
	hi, err := e.hi.Eval(row)
	if err != nil {
		return types.Null, err
	}
	if v.IsNull() || lo.IsNull() || hi.IsNull() {
		return types.Null, nil
	}
	c1, err := types.Compare(v, lo)
	if err != nil {
		return types.Null, err
	}
	c2, err := types.Compare(v, hi)
	if err != nil {
		return types.Null, err
	}
	ok := c1 >= 0 && c2 <= 0
	if e.not {
		ok = !ok
	}
	return types.Bool(ok), nil
}

type likeEval struct {
	x, pat Eval
	not    bool
	ic     bool
}

func (e *likeEval) Type() types.Type { return types.Type{Kind: types.KBool, Name: "boolean"} }
func (e *likeEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	p, err := e.pat.Eval(row)
	if err != nil {
		return types.Null, err
	}
	if v.IsNull() || p.IsNull() {
		return types.Null, nil
	}
	ok := likeMatch(p.TextForm(), v.TextForm(), e.ic)
	if e.not {
		ok = !ok
	}
	return types.Bool(ok), nil
}

type castEval struct {
	x Eval
	t types.Type
}

func (e *castEval) Type() types.Type { return e.t }
func (e *castEval) Eval(row types.Row) (types.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return types.Null, err
	}
	return types.Cast(v, e.t)
}

type caseEval struct {
	operand Eval
	whens   []struct {
		cond, result Eval
	}
	els Eval
	t   types.Type
}

func (e *caseEval) Type() types.Type { return e.t }
func (e *caseEval) Eval(row types.Row) (types.Value, error) {
	var opv types.Value
	if e.operand != nil {
		v, err := e.operand.Eval(row)
		if err != nil {
			return types.Null, err
		}
		opv = v
	}
	for _, w := range e.whens {
		cv, err := w.cond.Eval(row)
		if err != nil {
			return types.Null, err
		}
		hit := false
		if e.operand != nil {
			if !opv.IsNull() && !cv.IsNull() {
				c, err := types.Compare(opv, cv)
				if err != nil {
					return types.Null, err
				}
				hit = c == 0
			}
		} else {
			b, isNull, err := asBool(cv)
			if err != nil {
				return types.Null, err
			}
			hit = !isNull && b
		}
		if hit {
			return w.result.Eval(row)
		}
	}
	if e.els != nil {
		return e.els.Eval(row)
	}
	return types.Null, nil
}

func compileCase(n *sql.Case, c *compileCtx) (Eval, error) {
	ce := &caseEval{}
	if n.Operand != nil {
		op, err := compile(n.Operand, c)
		if err != nil {
			return nil, err
		}
		ce.operand = op
	}
	var outType types.Type
	haveType := false
	for _, w := range n.Whens {
		cond, err := compile(w.Cond, c)
		if err != nil {
			return nil, err
		}
		res, err := compile(w.Result, c)
		if err != nil {
			return nil, err
		}
		ce.whens = append(ce.whens, struct {
			cond, result Eval
		}{cond, res})
		if !haveType && res.Type().Kind != types.KNull {
			outType, haveType = res.Type(), true
		}
	}
	if n.Else != nil {
		e, err := compile(n.Else, c)
		if err != nil {
			return nil, err
		}
		ce.els = e
		if !haveType && e.Type().Kind != types.KNull {
			outType, haveType = e.Type(), true
		}
	}
	if !haveType {
		outType = types.Type{Kind: types.KText, Name: "text"}
	}
	ce.t = outType
	return ce, nil
}

// ---------- 标量函数 ----------

type funcEval struct {
	name string
	args []Eval
	t    types.Type
}

func (e *funcEval) Type() types.Type { return e.t }

func (e *funcEval) Eval(row types.Row) (types.Value, error) {
	vals := make([]types.Value, len(e.args))
	for i, a := range e.args {
		v, err := a.Eval(row)
		if err != nil {
			return types.Null, err
		}
		vals[i] = v
	}
	switch e.name {
	case "coalesce":
		for _, v := range vals {
			if !v.IsNull() {
				return v, nil
			}
		}
		return types.Null, nil
	case "nullif":
		if len(vals) != 2 {
			return types.Null, fmt.Errorf("nullif requires 2 arguments")
		}
		if vals[0].IsNull() {
			return types.Null, nil
		}
		if vals[1].IsNull() {
			return vals[0], nil
		}
		c, err := types.Compare(vals[0], vals[1])
		if err != nil {
			return types.Null, err
		}
		if c == 0 {
			return types.Null, nil
		}
		return vals[0], nil
	case "concat":
		var b strings.Builder
		for _, v := range vals {
			if v.IsNull() {
				continue
			}
			b.WriteString(v.TextForm())
		}
		return types.Text(b.String()), nil
	case "now", "current_timestamp":
		if len(vals) > 0 {
			return vals[0], nil
		}
		return types.Time(time.Now()), nil
	case "greatest", "least":
		if len(vals) == 0 {
			return types.Null, nil
		}
		best := vals[0]
		for _, v := range vals[1:] {
			if v.IsNull() {
				continue
			}
			if best.IsNull() {
				best = v
				continue
			}
			c, err := types.Compare(v, best)
			if err != nil {
				return types.Null, err
			}
			if (e.name == "greatest" && c > 0) || (e.name == "least" && c < 0) {
				best = v
			}
		}
		return best, nil
	}
	if len(vals) > 0 && vals[0].IsNull() && e.name != "length" {
		return types.Null, nil
	}
	v := vals[0]
	switch e.name {
	case "lower":
		return types.Text(strings.ToLower(v.TextForm())), nil
	case "upper":
		return types.Text(strings.ToUpper(v.TextForm())), nil
	case "length", "char_length", "character_length":
		if v.IsNull() {
			return types.Null, nil
		}
		return types.Int(int64(len([]rune(v.TextForm())))), nil
	case "trim", "btrim":
		return types.Text(strings.TrimSpace(v.TextForm())), nil
	case "ltrim":
		return types.Text(strings.TrimLeft(v.TextForm(), " ")), nil
	case "rtrim":
		return types.Text(strings.TrimRight(v.TextForm(), " ")), nil
	case "abs":
		if v.K == types.KInt {
			if v.I < 0 {
				return types.Int(-v.I), nil
			}
			return v, nil
		}
		f, err := toFloat(v)
		if err != nil {
			return types.Null, err
		}
		return types.Float(math.Abs(f)), nil
	case "ceil", "ceiling":
		f, err := toFloat(v)
		if err != nil {
			return types.Null, err
		}
		return types.Float(math.Ceil(f)), nil
	case "floor":
		f, err := toFloat(v)
		if err != nil {
			return types.Null, err
		}
		return types.Float(math.Floor(f)), nil
	case "round":
		f, err := toFloat(v)
		if err != nil {
			return types.Null, err
		}
		digits := 0
		if len(vals) > 1 && !vals[1].IsNull() {
			d, err := toInt(vals[1])
			if err != nil {
				return types.Null, err
			}
			digits = int(d)
		}
		p := math.Pow(10, float64(digits))
		r := math.Round(f*p) / p
		if v.K == types.KInt && digits == 0 {
			return types.Int(int64(r)), nil
		}
		return types.Float(r), nil
	case "replace":
		if len(vals) != 3 {
			return types.Null, fmt.Errorf("replace requires 3 arguments")
		}
		if vals[1].IsNull() || vals[2].IsNull() {
			return types.Null, nil
		}
		return types.Text(strings.ReplaceAll(v.TextForm(), vals[1].TextForm(), vals[2].TextForm())), nil
	case "substr", "substring":
		if len(vals) < 2 {
			return types.Null, fmt.Errorf("substring requires at least 2 arguments")
		}
		if vals[1].IsNull() {
			return types.Null, nil
		}
		r := []rune(v.TextForm())
		start, err := toInt(vals[1])
		if err != nil {
			return types.Null, err
		}
		from := int(start) - 1
		if from < 0 {
			from = 0
		}
		if from >= len(r) {
			return types.Text(""), nil
		}
		if len(vals) >= 3 {
			if vals[2].IsNull() {
				return types.Null, nil
			}
			n, err := toInt(vals[2])
			if err != nil {
				return types.Null, err
			}
			to := from + int(n)
			if to > len(r) {
				to = len(r)
			}
			if to < from {
				return types.Text(""), nil
			}
			return types.Text(string(r[from:to])), nil
		}
		return types.Text(string(r[from:])), nil
	}
	return types.Null, fmt.Errorf("function %s does not exist", e.name)
}

func compileScalar(n *sql.FuncCall, c *compileCtx) (Eval, error) {
	name := strings.ToLower(n.Name)
	if n.Star {
		return nil, fmt.Errorf("function %s(*) does not exist", name)
	}
	args := make([]Eval, 0, len(n.Args))
	for _, a := range n.Args {
		ev, err := compile(a, c)
		if err != nil {
			return nil, err
		}
		args = append(args, ev)
	}
	fe := &funcEval{name: name, args: args}
	switch name {
	case "lower", "upper", "trim", "btrim", "ltrim", "rtrim", "replace", "substr", "substring", "concat", "text":
		fe.t = types.Type{Kind: types.KText, Name: "text"}
	case "length", "char_length", "character_length":
		fe.t = types.Type{Kind: types.KInt, Name: "integer"}
	case "now", "current_timestamp":
		fe.t = types.Type{Kind: types.KTime, Name: "timestamp"}
	case "abs", "round", "ceil", "ceiling", "floor", "greatest", "least", "coalesce", "nullif":
		if len(args) > 0 {
			fe.t = args[0].Type()
		} else {
			fe.t = types.Type{Kind: types.KText, Name: "text"}
		}
	default:
		return nil, fmt.Errorf("function %s does not exist", name)
	}
	return fe, nil
}

// ---------- 辅助 ----------

func binType(op string, l, r types.Type) types.Type {
	switch op {
	case "=", "<>", "<", "<=", ">", ">=", "and", "or", "is":
		return types.Type{Kind: types.KBool, Name: "boolean"}
	case "||":
		return types.Type{Kind: types.KText, Name: "text"}
	}
	if l.Kind == types.KInt && r.Kind == types.KInt {
		return types.Type{Kind: types.KInt, Name: "integer"}
	}
	return types.Type{Kind: types.KFloat, Name: "double precision"}
}

func toInt(v types.Value) (int64, error) {
	switch v.K {
	case types.KInt:
		return v.I, nil
	case types.KFloat:
		return int64(v.F), nil
	case types.KBool:
		if v.B {
			return 1, nil
		}
		return 0, nil
	}
	c, err := types.Cast(v, types.Type{Kind: types.KInt, Name: "integer"})
	if err != nil {
		return 0, err
	}
	return c.I, nil
}

func toFloat(v types.Value) (float64, error) {
	switch v.K {
	case types.KInt:
		return float64(v.I), nil
	case types.KFloat:
		return v.F, nil
	}
	c, err := types.Cast(v, types.Type{Kind: types.KFloat, Name: "double precision"})
	if err != nil {
		return 0, err
	}
	return c.F, nil
}

// asBool 把值解释成布尔（NULL 返回 isNull=true）。
func asBool(v types.Value) (val bool, isNull bool, err error) {
	if v.IsNull() {
		return false, true, nil
	}
	if v.K == types.KBool {
		return v.B, false, nil
	}
	c, err := types.Cast(v, types.Type{Kind: types.KBool, Name: "boolean"})
	if err != nil {
		return false, false, fmt.Errorf("argument must be type boolean")
	}
	return c.B, false, nil
}

// likeMatch 实现 SQL LIKE 的 % 与 _ 通配（\ 转义）。
func likeMatch(pattern, s string, ignoreCase bool) bool {
	pat := []rune(pattern)
	str := []rune(s)
	pi, si := 0, 0
	star := -1
	mark := 0
	for si < len(str) {
		if pi < len(pat) {
			ch := pat[pi]
			switch {
			case ch == '%':
				star = pi
				mark = si
				pi++
				continue
			case ch == '_':
				pi++
				si++
				continue
			case ch == '\\' && pi+1 < len(pat):
				if charEq(pat[pi+1], str[si], ignoreCase) {
					pi += 2
					si++
					continue
				}
			case charEq(ch, str[si], ignoreCase):
				pi++
				si++
				continue
			}
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pat) && pat[pi] == '%' {
		pi++
	}
	return pi == len(pat)
}

func charEq(a, b rune, ignoreCase bool) bool {
	if ignoreCase {
		return unicode.ToLower(a) == unicode.ToLower(b)
	}
	return a == b
}
