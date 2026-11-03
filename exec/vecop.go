package exec

import (
	"errors"
	"fmt"

	"godb/sql"
	"godb/storage"
	"godb/types"
)

// 向量距离算子（pgvector 命名，越小越近）：
//
//	<->  L2 欧氏距离
//	<=>  余弦距离（1 - cos）
//	<#>  负内积（-dot，越负越近）
//
// 三者都返回 double precision，可以直接进 ORDER BY / 比较 / 聚合。
func isVecDistOp(op string) bool {
	return op == "<->" || op == "<=>" || op == "<#>"
}

// VecColumn 与 VecSource 定义在 storage（那里才知道文件怎么放）。
// 这里只做类型别名：执行层写 VecColumn{...} 时不必关心它来自哪个包。
type VecColumn = storage.VecColumn
type VecSource = storage.VecSource

var errVecNull = fmt.Errorf("vector is NULL")

// vecArg 是距离运算的一侧。
//
// 三条取值路径，按代价从低到高：
//   - 编译期就能确定的常量（`'[1,2,3]'`、`'[1,2,3]'::vector`）：只解析一次；
//   - 行里的内联向量（值本身带文本）：每次解析，但同一段文本只解析一次；
//   - 行里的槽位引用：交给 VecSource 读列文件，复用 buf。
type vecArg struct {
	ev  Eval
	src VecSource

	// col 非 nil 表示这一侧直接来自某个向量列。
	col *VecColumn

	// 编译期常量：konst 为非 nil 时不再求值。
	konst []float32

	// 内联文本解析缓存（值按行变化时只命中同一段文本）。
	lastText string
	lastVec  []float32

	// 槽位读取的复用缓冲与列句柄（第一次用到时解析，之后直连）。
	buf []float32
	h   storage.VecHandle
	// colIdx 是列在输入行里的下标：直接取，跳过 ev.Eval 的接口调用。
	colIdx int
}

// load 返回这一侧的向量。ok=false 表示值为 NULL（距离结果也应为 NULL）。
func (a *vecArg) load(row types.Row) (vec []float32, ok bool, err error) {
	if a.konst != nil {
		return a.konst, true, nil
	}
	var v types.Value
	if a.colIdx >= 0 {
		// 列引用：直接从行里取，省掉一次接口调用与类型断言。
		if a.colIdx >= len(row) {
			return nil, false, fmt.Errorf("column index %d out of range", a.colIdx)
		}
		v = row[a.colIdx]
	} else {
		var err error
		v, err = a.ev.Eval(row)
		if err != nil {
			return nil, false, err
		}
	}
	if v.IsNull() {
		return nil, false, nil
	}
	if v.K != types.KVector {
		return nil, false, fmt.Errorf("vector distance requires vector operands, got %s", v.K.String())
	}
	if v.IsVecInline() {
		// 同一段文本复用上一次的结果：T4 接线之前列值都是内联的，
		// 一行一次 ParseVector 会让 1000 万行多花十几秒。
		if a.lastVec != nil && a.lastText == v.S {
			return a.lastVec, true, nil
		}
		vec, err := types.ParseVector(v.S)
		if err != nil {
			return nil, false, err
		}
		a.lastText, a.lastVec = v.S, vec
		return vec, true, nil
	}
	id, _ := v.VecID()
	if a.src == nil || a.col == nil {
		return nil, false, errVecUnreadable(v.VecDim())
	}
	dim := a.col.Dim
	if dim <= 0 {
		dim = v.VecDim()
	}
	if cap(a.buf) < dim {
		a.buf = make([]float32, dim)
	}
	if a.h == nil {
		if h, ok := a.src.VecHandle(*a.col); ok {
			a.h = h
		}
	}
	if a.h != nil {
		return readVecHandle(a.h, id, a.buf[:dim])
	}
	vec, err = a.src.VecAt(*a.col, id, a.buf[:dim])
	if err != nil {
		// 槽位越界 = 这一行的向量没了，按 NULL 处理，不要让整个查询挂掉；
		// 列文件没打开是另一回事，必须报出来（见 storage.ErrVecNoFile 的注释）。
		if errors.Is(err, storage.ErrVecMissing) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return vec, true, nil
}

// readVecHandle 读一个槽位，把「槽位越界」翻译成 NULL。
//
// 优先取零拷贝视图：向量列文件是整文件映射的，取值等于直接从页缓存读，
// 十万行省下十万次内存拷贝（实测每行 425 ns → 40 ns，见 storage 包的基准）。
func readVecHandle(h storage.VecHandle, id int64, dst []float32) ([]float32, bool, error) {
	if vec, ok := h.View(id); ok {
		return vec, true, nil
	}
	vec, err := h.Read(id, dst)
	if err != nil {
		if errors.Is(err, storage.ErrVecMissing) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return vec, true, nil
}

// errVecUnreadable 在「向量存在列文件里、但当前上下文拿不到列」时报错。
//
// 这里必须报错而不是返回零向量：零向量算出来的距离是「和原点的距离」，
// 排序结果看起来完全正常，用户没有机会发现它在骗人。
func errVecUnreadable(dim int) error {
	return fmt.Errorf("vector of dimension %d is stored in a column file but this query "+
		"has no vector column bound to it (vector search is not wired to storage yet)", dim)
}

// compileVecArg 把距离运算的一侧编译成 vecArg。
func compileVecArg(e sql.Expr, c *compileCtx) (*vecArg, error) {
	ev, err := compile(e, c)
	if err != nil {
		return nil, err
	}
	t := ev.Type()
	if t.Kind != types.KVector {
		// 文本常量隐式当作文向量：`'[1,2,3]' <-> '[...]'` 是 pgvector 最常见
		// 的写法（那边靠 unknown 字面量走隐式转换）。只对「常量」放宽 ——
		// 文本列里的 `'[1,2]'` 不该被悄悄当成向量，那是两回事。
		if v, cerr := CompileConst(e); cerr == nil && v.K == types.KText {
			if vec, perr := types.ParseVector(v.S); perr == nil {
				return &vecArg{konst: vec}, nil
			}
		}
		return nil, fmt.Errorf("vector distance requires vector operands, got %s", typeName(t))
	}

	a := &vecArg{ev: ev, src: c.vec, colIdx: -1}

	// 编译期常量：一次解析，之后每行都直接用。
	// 用 CompileConst 而不是手工判字面量，是为了让 `'[1,2,3]'::vector`
	// 这类「带转换的常量」也能走快速路径。
	if v, cerr := CompileConst(e); cerr == nil && v.K == types.KVector && v.IsVecInline() {
		vec, perr := types.ParseVector(v.S)
		if perr != nil {
			return nil, perr
		}
		if t.Dim() > 0 && len(vec) != t.Dim() {
			return nil, fmt.Errorf("vector dimension mismatch: column is %d, value has %d", t.Dim(), len(vec))
		}
		a.konst = vec
		return a, nil
	}

	// 列引用：记下表的 ID 与列 ID，运行时按它找列文件。
	if cr, ok := e.(*sql.ColumnRef); ok && c.sc != nil {
		if idx, _, rerr := c.sc.Resolve(cr.Table, cr.Name); rerr == nil && idx < len(c.sc) {
			ci := c.sc[idx]
			// 行里这一列的下标固定是 idx，直接取值比走一遍 ev.Eval 便宜。
			a.colIdx = idx
			if ci.TableID != 0 && ci.ColID != 0 {
				a.col = &VecColumn{
					TableOID: ci.TableOID,
					TableID:  ci.TableID,
					ColumnID: ci.ColID,
					Dim:      t.Dim(),
					AttNum:   ci.AttNum,
				}
			}
		}
	}
	return a, nil
}

// typeName 给错误信息用的类型名。
func typeName(t types.Type) string {
	if t.Name != "" {
		if t.Dim() > 0 {
			return fmt.Sprintf("%s(%d)", t.Name, t.Dim())
		}
		return t.Name
	}
	return t.Kind.String()
}

// vecDistEval 求两侧向量的距离。
type vecDistEval struct {
	op   string
	l, r *vecArg
	t    types.Type
}

func (e *vecDistEval) Type() types.Type { return e.t }

func (e *vecDistEval) Eval(row types.Row) (types.Value, error) {
	a, aok, err := e.l.load(row)
	if err != nil {
		return types.Null, err
	}
	if !aok {
		return types.Null, nil
	}
	b, bok, err := e.r.load(row)
	if err != nil {
		return types.Null, err
	}
	if !bok {
		return types.Null, nil
	}
	if len(a) != len(b) {
		return types.Null, fmt.Errorf("vector dimension mismatch: %d vs %d", len(a), len(b))
	}
	var d float32
	switch e.op {
	case "<->":
		d = types.DistL2(a, b)
	case "<=>":
		d = types.DistCosine(a, b)
	case "<#>":
		d = types.DistNegIP(a, b)
	}
	return types.Float(float64(d)), nil
}

// compileVecDist 编译一个距离表达式。
func compileVecDist(n *sql.Binary, c *compileCtx) (Eval, error) {
	l, err := compileVecArg(n.L, c)
	if err != nil {
		return nil, err
	}
	r, err := compileVecArg(n.R, c)
	if err != nil {
		return nil, err
	}
	// 两侧维度必须在编译期就能对上：运行期才发现「列是 768 维、查询是 128 维」
	// 等于每行白算一次距离再报错。
	ld, rd := l.dim(), r.dim()
	if ld > 0 && rd > 0 && ld != rd {
		return nil, fmt.Errorf("vector dimension mismatch: %d vs %d", ld, rd)
	}
	return &vecDistEval{op: n.Op, l: l, r: r,
		t: types.Type{Kind: types.KFloat, Name: "double precision"}}, nil
}

// dim 返回这一侧已知的维度，未知返回 0。
func (a *vecArg) dim() int {
	if a.konst != nil {
		return len(a.konst)
	}
	if a.col != nil && a.col.Dim > 0 {
		return a.col.Dim
	}
	return a.ev.Type().Dim()
}

// CompileWithVec 在给定 schema 与向量来源下编译表达式。
//
// Compile 保持原签名（src 为 nil）不变：大部分表达式不需要读列文件，
// 让所有调用点都多传一个参数只会制造噪音。接线时在 Planner 里统一注入。
func CompileWithVec(e sql.Expr, sc Schema, src VecSource) (Eval, error) {
	return compile(e, &compileCtx{sc: sc, vec: src})
}
