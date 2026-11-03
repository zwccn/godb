package exec

import (
	"strings"
	"testing"

	"godb/sql"
	"godb/storage"
	"godb/types"
)

// vecSchema 是一张带向量列的 schema：id int、e vector(3)。
func vecSchema() Schema {
	return Schema{
		{Name: "id", Type: types.Type{Kind: types.KInt, Name: "integer"}, TableOID: 16384, AttNum: 1, TableID: 1, ColID: 1},
		{Name: "e", Type: types.Type{Kind: types.KVector, Name: "vector(3)", Len: 3}, TableOID: 16384, AttNum: 2, TableID: 1, ColID: 2},
	}
}

func mustParse(t *testing.T, src string) sql.Expr {
	t.Helper()
	e, err := sql.ParseExpr(src)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", src, err)
	}
	return e
}

// 三种距离的数值：和手算对得上。
func TestVecDistOperators(t *testing.T) {
	cases := []struct {
		src  string
		want float64
	}{
		// L2：sqrt((3-1)^2 + (4-2)^2) = sqrt(8) = 2.828427
		{"'[1,2]' <-> '[3,4]'", 2.8284271247461903},
		// 余弦距离：正交 → 1；相同方向 → 0；反向 → 2
		{"'[1,0]' <=> '[0,1]'", 1},
		{"'[1,0]' <=> '[1,0]'", 0},
		{"'[1,0]' <=> '[-1,0]'", 2},
		// 负内积：-dot
		{"'[1,2,3]' <#> '[4,5,6]'", -32},
		{"'[1,2,3]' <#> '[0,0,0]'", 0},
	}
	for _, c := range cases {
		ev, err := Compile(mustParse(t, c.src), nil)
		if err != nil {
			t.Fatalf("编译 %q 失败: %v", c.src, err)
		}
		if got := ev.Type(); got.Kind != types.KFloat || got.Name != "double precision" {
			t.Fatalf("%q 的类型是 %v，期望 double precision", c.src, got)
		}
		v, err := ev.Eval(nil)
		if err != nil {
			t.Fatalf("求值 %q 失败: %v", c.src, err)
		}
		if v.K != types.KFloat {
			t.Fatalf("%q 返回 %v，期望 float", c.src, v.K)
		}
		if diff := v.Float() - c.want; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("%q = %v，期望 %v", c.src, v.Float(), c.want)
		}
	}
}

// 维度不匹配必须在编译期就报出来，而不是每行算一次再报。
func TestVecDistDimMismatch(t *testing.T) {
	for _, src := range []string{
		"'[1,2]' <-> '[1,2,3]'",
		"'[1,2,3]' <-> '[1,2]'",
	} {
		_, err := Compile(mustParse(t, src), nil)
		if err == nil {
			t.Fatalf("%q 应报维度不匹配", src)
		}
		if !strings.Contains(err.Error(), "dimension mismatch") {
			t.Fatalf("%q 的错误信息是 %q，期望提到 dimension mismatch", src, err)
		}
	}
}

// 非向量操作数：报错而不是静默算 0。
func TestVecDistTypeError(t *testing.T) {
	for _, src := range []string{
		"'abc' <-> '[1,2]'",
		"1 <-> 2",
		"'[1,2]' <-> 3",
	} {
		if _, err := Compile(mustParse(t, src), nil); err == nil {
			t.Fatalf("%q 应报类型错误", src)
		}
	}
}

// NULL 传播：任一侧为 NULL，结果为 NULL（与 pgvector 一致）。
func TestVecDistNullPropagates(t *testing.T) {
	sc := vecSchema()
	e := mustParse(t, "e <-> '[1,2,3]'")
	ev, err := Compile(e, sc)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Row{types.Int(1), types.Null}
	v, err := ev.Eval(row)
	if err != nil {
		t.Fatalf("NULL 参与距离计算不应报错: %v", err)
	}
	if !v.IsNull() {
		t.Fatalf("NULL 参与距离计算应返回 NULL，实际 %v", v.TextForm())
	}
}

// 列里的内联向量能直接算（T4 接线前的形态）。
func TestVecDistInlineColumn(t *testing.T) {
	sc := vecSchema()
	ev, err := Compile(mustParse(t, "e <-> '[0,0,0]'"), sc)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Row{types.Int(1), types.VectorLiteral("[3,4,0]", 3)}
	v, err := ev.Eval(row)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Float(); got < 4.9999 || got > 5.0001 {
		t.Fatalf("L2 = %v，期望 5", got)
	}
}

// 编译期常量只解析一次：把同一段文本用 10 万行喂进去，
// 应当明显快于「每行 ParseVector」。这里只验证语义不变 + 不崩，
// 耗时断言在 CI 上不可靠。
func TestVecDistConstParsedOnce(t *testing.T) {
	ev, err := Compile(mustParse(t, "'[1,2,3]' <-> '[4,5,6]'"), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ev.Eval(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		v, err := ev.Eval(nil)
		if err != nil {
			t.Fatal(err)
		}
		if v.Float() != first.Float() {
			t.Fatalf("第 %d 次求值结果漂移: %v vs %v", i, v.Float(), first.Float())
		}
	}
}

// fakeVecSource 是最小的 VecSource：槽位号就是下标。
type fakeVecSource struct {
	cols map[VecColumn][][]float32
	hit  int
	// viewOK 模拟「列文件有映射视图」。false 时 View 一律返回 false，
	// 于是所有用例都走 Read 退路；打开它才走零拷贝快路径。
	viewOK bool
}

func (f *fakeVecSource) VecAt(col VecColumn, id int64, dst []float32) ([]float32, error) {
	f.hit++
	return f.read(col, id, dst)
}

// VecHandle 让句柄路径也走同一份数据。
func (f *fakeVecSource) VecHandle(col VecColumn) (storage.VecHandle, bool) {
	if _, ok := f.cols[col]; !ok {
		return nil, false
	}
	return fakeVecHandle{f: f, col: col}, true
}

type fakeVecHandle struct {
	f   *fakeVecSource
	col VecColumn
}

func (h fakeVecHandle) Read(id int64, dst []float32) ([]float32, error) {
	h.f.hit++
	return h.f.read(h.col, id, dst)
}

// View 走零拷贝：直接把底层切片交出去，不经过 dst。
func (h fakeVecHandle) View(id int64) ([]float32, bool) {
	if !h.f.viewOK {
		return nil, false
	}
	rows := h.f.cols[h.col]
	if id < 0 || int(id) >= len(rows) {
		return nil, false
	}
	return rows[id], true
}

func (f *fakeVecSource) read(col VecColumn, id int64, dst []float32) ([]float32, error) {
	rows := f.cols[col]
	if id < 0 || int(id) >= len(rows) {
		// 与真实 VecFile 一致：越界是「这一行没有向量」，不是「整列不可用」。
		return nil, storage.ErrVecMissing
	}
	src := rows[id]
	if cap(dst) < len(src) {
		dst = make([]float32, len(src))
	}
	dst = dst[:len(src)]
	copy(dst, src)
	return dst, nil
}

// 有 VecSource 时，槽位引用能读出来并参与距离计算。
func TestVecDistFromVecSource(t *testing.T) {
	sc := vecSchema()
	col := VecColumn{TableOID: 16384, TableID: 1, ColumnID: 2, Dim: 3, AttNum: 2}
	src := &fakeVecSource{cols: map[VecColumn][][]float32{
		col: {{0, 0, 0}, {3, 4, 0}},
	}}
	ev, err := CompileWithVec(mustParse(t, "e <-> '[0,0,0]'"), sc, src)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Row{types.Int(1), types.VectorRef(1, 3)}
	v, err := ev.Eval(row)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Float(); got < 4.9999 || got > 5.0001 {
		t.Fatalf("L2 = %v，期望 5", got)
	}
	if src.hit != 1 {
		t.Fatalf("VecSource 被调用 %d 次，期望 1", src.hit)
	}
	// 常量侧不应该去读列文件
	if src.hit != 1 {
		t.Fatalf("常量侧也读了列文件（hit=%d）", src.hit)
	}
}

// 零拷贝视图路径与 Read 退路必须给出完全一样的结果。
//
// 两条路径的差别只在「数据怎么来」，一旦有一边算错（比如视图少拷一位、
// 边界多读一格），排序结果会整体偏掉 —— 这种错不会报错，只会让人觉得
// 「近似搜索本来就不太准」。所以逐行对账。
func TestVecDistViewMatchesRead(t *testing.T) {
	sc := vecSchema()
	col := VecColumn{TableOID: 16384, TableID: 1, ColumnID: 2, Dim: 3, AttNum: 2}
	rows := [][]float32{{0, 0, 0}, {3, 4, 0}, {-1, 2, 2}, {0.5, -0.25, 1e-3}}
	base := &fakeVecSource{cols: map[VecColumn][][]float32{col: rows}}
	view := &fakeVecSource{cols: map[VecColumn][][]float32{col: rows}, viewOK: true}
	evA, err := CompileWithVec(mustParse(t, "e <-> '[1,1,1]'"), sc, base)
	if err != nil {
		t.Fatal(err)
	}
	evB, err := CompileWithVec(mustParse(t, "e <-> '[1,1,1]'"), sc, view)
	if err != nil {
		t.Fatal(err)
	}
	for id := 0; id < len(rows); id++ {
		row := types.Row{types.Int(int64(id)), types.VectorRef(int64(id), 3)}
		a, err := evA.Eval(row)
		if err != nil {
			t.Fatalf("Read 路径第 %d 行: %v", id, err)
		}
		b, err := evB.Eval(row)
		if err != nil {
			t.Fatalf("View 路径第 %d 行: %v", id, err)
		}
		if a.Float() != b.Float() {
			t.Fatalf("第 %d 行两条路径结果不同: Read=%v View=%v", id, a.Float(), b.Float())
		}
	}
	// 槽位越界：View 说「没有」，Read 说 ErrVecMissing，最终都必须是 NULL。
	row := types.Row{types.Int(99), types.VectorRef(99, 3)}
	if v, err := evB.Eval(row); err != nil || !v.IsNull() {
		t.Fatalf("越界槽位在 View 路径上应为 NULL，得到 %v / %v", v, err)
	}
}

// 没有 VecSource 时读槽位引用要报错，不能返回零向量。
func TestVecDistMissingSource(t *testing.T) {
	sc := vecSchema()
	ev, err := Compile(mustParse(t, "e <-> '[0,0,0]'"), sc)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Row{types.Int(1), types.VectorRef(7, 3)}
	_, err = ev.Eval(row)
	if err == nil {
		t.Fatal("没有 VecSource 时读槽位引用应报错")
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Fatalf("错误信息应提到 vector，实际 %q", err)
	}
}

// 词法：三个算子必须整体成一个 token，不能被拆成 <= + >。
func TestVecDistLexer(t *testing.T) {
	for _, op := range []string{"<->", "<=>", "<#>"} {
		src := "'[1]' " + op + " '[2]'"
		toks, err := sql.Lex(src)
		if err != nil {
			t.Fatalf("%q 词法失败: %v", src, err)
		}
		found := false
		for _, tk := range toks {
			if tk.Text == op {
				found = true
			}
			if tk.Text == "<=" || tk.Text == "<#" {
				t.Fatalf("%q 被拆成了 %q", op, tk.Text)
			}
		}
		if !found {
			t.Fatalf("%q 里找不到完整算子 %q", src, op)
		}
	}
}
