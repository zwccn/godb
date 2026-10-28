package exec

import (
	"fmt"
	"math/big"
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
	// decimal 累加：优先 int64 定标度快路径（零分配），溢出/变标度才回退 big.Int
	dinit  bool  // 是否已有第一个 decimal 值
	di     int64 // int64 尾数累加（dfast=true 时有效）
	dscale int   // 当前小数位
	dfast  bool  // true = int64 路径；false = big.Int 路径
	dm     *big.Int
	seen   map[string]bool
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
		// 先走 int64 快速解析（零分配），失败才退回 big.Int
		if mi, sc, ok := types.DecimalMantissaFast(v.TextForm()); ok {
			return a.addMantissaInt64(mi, sc)
		}
		m, sc, err := types.DecimalMantissa(v.TextForm())
		if err != nil {
			return err
		}
		return a.addMantissa(m, sc)
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

// addMantissaInt64 用 int64 尾数累加（零分配路径）。
func (a *sumAcc) addMantissaInt64(mi int64, sc int) error {
	if !a.dinit {
		a.dinit = true
		a.di = mi
		a.dscale = sc
		a.dfast = true
		return nil
	}
	if a.dfast && sc == a.dscale {
		s := a.di + mi
		if (a.di > 0 && mi > 0 && s < 0) || (a.di < 0 && mi < 0 && s >= 0) {
			// 溢出：切到 big.Int
			a.dm = big.NewInt(a.di)
			a.dfast = false
			a.dm.Add(a.dm, big.NewInt(mi))
			return nil
		}
		a.di = s
		return nil
	}
	// 标度不同或已在 big.Int 模式：走通用路径
	return a.addMantissa(big.NewInt(mi), sc)
}

// addMantissa 累加一个（尾数, 标度）对。优先 int64 快路径，溢出或变标度时回退 big.Int。
func (a *sumAcc) addMantissa(m *big.Int, sc int) error {
	if !a.dinit {
		// 第一个值
		a.dinit = true
		if m.IsInt64() {
			a.di = m.Int64()
			a.dscale = sc
			a.dfast = true
			return nil
		}
		a.dm = m
		a.dscale = sc
		a.dfast = false
		return nil
	}
	if a.dfast {
		if sc == a.dscale {
			// 同标度：int64 直接加
			mi := m.Int64()
			if !m.IsInt64() {
				// 溢出 int64：切到 big.Int
				a.bigFromInt64()
				a.dm.Add(a.dm, m)
				return nil
			}
			s := a.di + mi
			if (a.di > 0 && mi > 0 && s < 0) || (a.di < 0 && mi < 0 && s >= 0) {
				// 溢出：切到 big.Int
				a.bigFromInt64()
				a.dm.Add(a.dm, m)
				return nil
			}
			a.di = s
			return nil
		}
		// 标度不同：先切到 big.Int 再对齐
		a.bigFromInt64()
	}
	// big.Int 路径
	if sc > a.dscale {
		pow := pow10(sc - a.dscale)
		a.dm.Mul(a.dm, pow)
		a.dscale = sc
	} else if sc < a.dscale {
		pow := pow10(a.dscale - sc)
		m = new(big.Int).Mul(m, pow)
	}
	a.dm.Add(a.dm, m)
	return nil
}

// bigFromInt64 把当前 int64 累加状态迁移到 big.Int。
func (a *sumAcc) bigFromInt64() {
	a.dm = big.NewInt(a.di)
	a.dfast = false
}

// pow10 返回 10^n（缓存常用值，避免每次分配）。
func pow10(n int) *big.Int {
	if n >= 0 && n < len(pow10Cache) {
		return pow10Cache[n]
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

var pow10Cache = func() []*big.Int {
	c := make([]*big.Int, 21)
	for i := range c {
		c[i] = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(i)), nil)
	}
	return c
}()

func (a *sumAcc) result() types.Value {
	if !a.has {
		return types.Null
	}
	if a.isDec {
		if a.dfast {
			return types.Decimal(types.MantissaDecimal(big.NewInt(a.di), a.dscale))
		}
		if a.dm == nil {
			return types.Decimal("0")
		}
		return types.Decimal(types.MantissaDecimal(a.dm, a.dscale))
	}
	if a.isFloat {
		return types.Float(a.f)
	}
	return types.Int(a.i)
}

type avgAcc struct {
	f        float64
	n        int64
	dinit    bool
	di       int64
	dm       *big.Int
	dscale   int
	dfast    bool
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
		// 先走 int64 快速解析（零分配），失败才退回 big.Int
		if mi, sc, ok := types.DecimalMantissaFast(v.TextForm()); ok {
			a.addMantissaInt64(mi, sc)
			a.n++
			return nil
		}
		m, sc, err := types.DecimalMantissa(v.TextForm())
		if err != nil {
			return err
		}
		a.addMantissa(m, sc)
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

// addMantissaInt64 与 sumAcc 相同的 int64 快速路径。
func (a *avgAcc) addMantissaInt64(mi int64, sc int) {
	if !a.dinit {
		a.dinit = true
		a.di = mi
		a.dscale = sc
		a.dfast = true
		return
	}
	if a.dfast && sc == a.dscale {
		s := a.di + mi
		if (a.di > 0 && mi > 0 && s < 0) || (a.di < 0 && mi < 0 && s >= 0) {
			a.dm = big.NewInt(a.di)
			a.dfast = false
			a.dm.Add(a.dm, big.NewInt(mi))
			return
		}
		a.di = s
		return
	}
	a.addMantissa(big.NewInt(mi), sc)
}

// addMantissa 与 sumAcc 相同的 int64 快路径。
func (a *avgAcc) addMantissa(m *big.Int, sc int) {
	if !a.dinit {
		a.dinit = true
		if m.IsInt64() {
			a.di = m.Int64()
			a.dscale = sc
			a.dfast = true
			return
		}
		a.dm = m
		a.dscale = sc
		a.dfast = false
		return
	}
	if a.dfast {
		if sc == a.dscale {
			mi := m.Int64()
			if !m.IsInt64() {
				a.dm = big.NewInt(a.di)
				a.dfast = false
				a.dm.Add(a.dm, m)
				return
			}
			s := a.di + mi
			if (a.di > 0 && mi > 0 && s < 0) || (a.di < 0 && mi < 0 && s >= 0) {
				a.dm = big.NewInt(a.di)
				a.dfast = false
				a.dm.Add(a.dm, m)
				return
			}
			a.di = s
			return
		}
		a.dm = big.NewInt(a.di)
		a.dfast = false
	}
	if sc > a.dscale {
		pow := pow10(sc - a.dscale)
		a.dm.Mul(a.dm, pow)
		a.dscale = sc
	} else if sc < a.dscale {
		pow := pow10(a.dscale - sc)
		m = new(big.Int).Mul(m, pow)
	}
	a.dm.Add(a.dm, m)
}

func (a *avgAcc) result() types.Value {
	if a.n == 0 {
		return types.Null
	}
	if a.isDec {
		var dm *big.Int
		if a.dfast {
			dm = big.NewInt(a.di)
		} else if a.dm != nil {
			dm = a.dm
		} else {
			return types.Decimal("0")
		}
		// 尾数乘 10^6 再除 n，保留 6 位小数后规范化
		num := new(big.Int).Set(dm)
		num.Mul(num, big.NewInt(1_000_000))
		num.Quo(num, big.NewInt(a.n))
		return types.Decimal(types.MantissaDecimal(num, a.dscale+6))
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
