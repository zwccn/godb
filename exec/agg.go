package exec

import (
	"fmt"
	"strconv"
	"strings"

	"godb/types"
)

// aggSpec 描述一个聚合调用。
type aggSpec struct {
	Fn string // count | sum | avg | min | max | string_agg
	// Separator 是 string_agg 的分隔符。要求是常量：客户端与 psql 都是写字面量，
	// 支持非常量需要让累加器拿到整行，收益不大。
	Separator string
	Arg       Eval // nil 表示 count(*)
	Star      bool
	Distinct  bool
	Label     string
	Type      types.Type
	key       string // 规范文本，用于去重与下标映射
}

func (a *aggSpec) String() string {
	return a.Label
}

type accum interface {
	add(v types.Value) error
	result() types.Value
}

type countAcc struct {
	n        int64
	distinct bool
	seen     map[string]bool
}

func (a *countAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.distinct {
		k := types.Key([]types.Value{v})
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	a.n++
	return nil
}
func (a *countAcc) result() types.Value { return types.Int(a.n) }

type sumAcc struct {
	isFloat  bool
	isDec    bool
	i        int64
	f        float64
	dec      string // 精确十进制累加
	seen     map[string]bool
	distinct bool
	has      bool
}

func (a *sumAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.distinct {
		k := types.Key([]types.Value{v})
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	a.has = true
	if a.isDec {
		s := v.TextForm()
		if a.dec == "" {
			a.dec = s
			return nil
		}
		out, err := types.AddDecimal(a.dec, s)
		if err != nil {
			return err
		}
		a.dec = out
		return nil
	}
	if a.isFloat {
		f, err := toFloat(v)
		if err != nil {
			return err
		}
		a.f += f
		return nil
	}
	n, err := toInt(v)
	if err != nil {
		return err
	}
	a.i += n
	return nil
}

func (a *sumAcc) result() types.Value {
	if !a.has {
		return types.Null
	}
	if a.isDec {
		return types.Decimal(a.dec)
	}
	if a.isFloat {
		return types.Float(a.f)
	}
	return types.Int(a.i)
}

type avgAcc struct {
	f        float64
	n        int64
	dec      string
	isDec    bool
	distinct bool
	seen     map[string]bool
}

func (a *avgAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.distinct {
		k := types.Key([]types.Value{v})
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	if a.isDec {
		s := v.TextForm()
		if a.dec == "" {
			a.dec = s
		} else {
			out, err := types.AddDecimal(a.dec, s)
			if err != nil {
				return err
			}
			a.dec = out
		}
		a.n++
		return nil
	}
	f, err := toFloat(v)
	if err != nil {
		return err
	}
	a.f += f
	a.n++
	return nil
}

func (a *avgAcc) result() types.Value {
	if a.n == 0 {
		return types.Null
	}
	if a.isDec {
		out, err := types.DivDecimal(a.dec, strconv.FormatInt(a.n, 10))
		if err != nil {
			return types.Null
		}
		return types.Decimal(out)
	}
	return types.Float(a.f / float64(a.n))
}

type minMaxAcc struct {
	isMin    bool
	v        types.Value
	has      bool
	distinct bool
	seen     map[string]bool
}

func (a *minMaxAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.distinct {
		k := types.Key([]types.Value{v})
		if a.seen[k] {
			return nil
		}
		a.seen[k] = true
	}
	if !a.has {
		a.v = v
		a.has = true
		return nil
	}
	c, err := types.Compare(v, a.v)
	if err != nil {
		return err
	}
	if (a.isMin && c < 0) || (!a.isMin && c > 0) {
		a.v = v
	}
	return nil
}

func (a *minMaxAcc) result() types.Value {
	if !a.has {
		return types.Null
	}
	return a.v
}

func newAccum(spec *aggSpec) accum {
	switch spec.Fn {
	case "count":
		a := &countAcc{distinct: spec.Distinct}
		if spec.Distinct {
			a.seen = map[string]bool{}
		}
		return a
	case "sum":
		a := &sumAcc{isFloat: spec.Type.Kind == types.KFloat, isDec: spec.Type.Kind == types.KDecimal, distinct: spec.Distinct}
		if spec.Distinct {
			a.seen = map[string]bool{}
		}
		return a
	case "avg":
		a := &avgAcc{isDec: spec.Type.Kind == types.KDecimal, distinct: spec.Distinct}
		if spec.Distinct {
			a.seen = map[string]bool{}
		}
		return a
	case "string_agg":
		return &strAggAcc{sep: spec.Separator, distinct: spec.Distinct, seen: map[string]bool{}}
	case "min", "max":
		a := &minMaxAcc{isMin: spec.Fn == "min", distinct: spec.Distinct}
		if spec.Distinct {
			a.seen = map[string]bool{}
		}
		return a
	}
	return &countAcc{}
}

// aggOp 是哈希聚合算子：输出 schema 为 [组键..., 聚合结果...]。
type aggOp struct {
	child      Operator
	groupEvals []Eval
	groupTypes []types.Type
	aggs       []*aggSpec
	sc         Schema
	rows       []types.Row
	i          int
}

func (o *aggOp) Schema() Schema       { return o.sc }
func (o *aggOp) Children() []Operator { return []Operator{o.child} }
func (o *aggOp) Close() error         { return o.child.Close() }

func (o *aggOp) Describe() string {
	parts := make([]string, 0, len(o.aggs))
	for _, a := range o.aggs {
		parts = append(parts, a.Label)
	}
	if len(o.groupEvals) > 0 {
		return fmt.Sprintf("HashAggregate (group by %d keys, %s)", len(o.groupEvals), strings.Join(parts, ", "))
	}
	return fmt.Sprintf("Aggregate (%s)", strings.Join(parts, ", "))
}

type aggGroup struct {
	keys []types.Value
	accs []accum
}

func (o *aggOp) Open() error {
	if err := o.child.Open(); err != nil {
		return err
	}
	groups := map[string]*aggGroup{}
	var order []string

	for {
		row, ok, err := o.child.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		keyVals := make([]types.Value, len(o.groupEvals))
		for i, e := range o.groupEvals {
			v, err := e.Eval(row)
			if err != nil {
				return err
			}
			keyVals[i] = v
		}
		k := types.Key(keyVals)
		g, exists := groups[k]
		if !exists {
			g = &aggGroup{keys: keyVals, accs: make([]accum, len(o.aggs))}
			for i, spec := range o.aggs {
				g.accs[i] = newAccum(spec)
			}
			groups[k] = g
			order = append(order, k)
		}
		for i, spec := range o.aggs {
			if spec.Star {
				if err := g.accs[i].add(types.Int(1)); err != nil {
					return err
				}
				continue
			}
			v, err := spec.Arg.Eval(row)
			if err != nil {
				return err
			}
			if err := g.accs[i].add(v); err != nil {
				return err
			}
		}
	}

	// 无 GROUP BY 时，即使没有输入行也要输出一行（count=0、sum=NULL）
	if len(o.groupEvals) == 0 && len(order) == 0 {
		g := &aggGroup{accs: make([]accum, len(o.aggs))}
		for i, spec := range o.aggs {
			g.accs[i] = newAccum(spec)
		}
		groups[""] = g
		order = append(order, "")
	}

	o.rows = make([]types.Row, 0, len(order))
	for _, k := range order {
		g := groups[k]
		out := make(types.Row, 0, len(g.keys)+len(g.accs))
		out = append(out, g.keys...)
		for _, a := range g.accs {
			out = append(out, a.result())
		}
		o.rows = append(o.rows, out)
	}
	o.i = 0
	return nil
}

func (o *aggOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

// strAggAcc 实现 string_agg(expr, sep)：把值拼成一个字符串。
type strAggAcc struct {
	parts    []string
	sep      string
	distinct bool
	seen     map[string]bool
	has      bool
}

func (a *strAggAcc) add(v types.Value) error {
	if v.IsNull() {
		// PostgreSQL 的 string_agg 跳过 NULL
		return nil
	}
	s := v.TextForm()
	if a.distinct {
		if a.seen[s] {
			return nil
		}
		a.seen[s] = true
	}
	a.parts = append(a.parts, s)
	a.has = true
	return nil
}

func (a *strAggAcc) result() types.Value {
	if !a.has {
		return types.Null
	}
	return types.Text(strings.Join(a.parts, a.sep))
}
