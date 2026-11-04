package types

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
)

func TestParseVectorOK(t *testing.T) {
	cases := []struct {
		in   string
		want []float32
	}{
		{"[1,2,3]", []float32{1, 2, 3}},
		{" [ 1 , -2.5 , 3e2 ] ", []float32{1, -2.5, 300}},
		{"[0.1]", []float32{0.1}},
		{"[\n 1,\n 2\n]", []float32{1, 2}},
		{"[-0.0,0]", []float32{0, 0}},
	}
	for _, c := range cases {
		got, err := ParseVector(c.in)
		if err != nil {
			t.Fatalf("ParseVector(%q) 报错: %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("ParseVector(%q) 维度 %d，期望 %d", c.in, len(got), len(c.want))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("ParseVector(%q)[%d] = %v，期望 %v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseVectorErrors(t *testing.T) {
	bad := []string{
		"", "[]", "[ ]", "1,2", "[1,2", "1,2]", "[1,,2]", "[1,2,]",
		"[,1]", "[1,a]", "[1,2]x", "[NaN]", "[Inf]", "[-Infinity]",
	}
	for _, in := range bad {
		if _, err := ParseVector(in); err == nil {
			t.Fatalf("ParseVector(%q) 应该报错，但通过了", in)
		}
	}
	// 维度上限
	huge := make([]string, MaxVectorDim+1)
	for i := range huge {
		huge[i] = "1"
	}
	if _, err := ParseVector("[" + strings.Join(huge, ",") + "]"); err == nil {
		t.Fatalf("超过 %d 维应该报错", MaxVectorDim)
	}
}

// AppendVector 必须能和 pgvector 一样做「最短往返」输出，否则同一份数据
// 在 psql 里看到的和写进去的不是一个字符串。
func TestAppendVectorRoundTrip(t *testing.T) {
	cases := []struct {
		in   []float32
		want string
	}{
		{[]float32{1, 2, 3}, "[1,2,3]"},
		{[]float32{0.1}, "[0.1]"},
		{[]float32{-0.5, 0}, "[-0.5,0]"},
		{[]float32{}, "[]"},
	}
	for _, c := range cases {
		got := VectorText(c.in)
		if got != c.want {
			t.Fatalf("VectorText(%v) = %q，期望 %q", c.in, got, c.want)
		}
		// 空向量只用于显示（不是合法值），不参与回解析
		if len(c.in) == 0 {
			continue
		}
		// 再解析回来必须一模一样
		back, err := ParseVector(got)
		if err != nil {
			t.Fatalf("回解析 %q 失败: %v", got, err)
		}
		if len(back) != len(c.in) {
			t.Fatalf("回解析维度不对: %d vs %d", len(back), len(c.in))
		}
		for i := range back {
			if back[i] != c.in[i] {
				t.Fatalf("回解析 [%d] = %v，期望 %v", i, back[i], c.in[i])
			}
		}
	}
}

func TestParseVectorType(t *testing.T) {
	ty, err := ParseType("vector", []int{768})
	if err != nil {
		t.Fatalf("ParseType 报错: %v", err)
	}
	if ty.Kind != KVector || ty.Len != 768 || ty.Name != "vector(768)" {
		t.Fatalf("解析结果不对: %+v", ty)
	}
	if ty.String() != "vector(768)" {
		t.Fatalf("String() = %q", ty.String())
	}
	for _, args := range [][]int{nil, {}, {0}, {-1}, {MaxVectorDim + 1}} {
		if _, err := ParseType("vector", args); err == nil {
			t.Fatalf("ParseType(vector, %v) 应该报错", args)
		}
	}
}

func TestCastToVector(t *testing.T) {
	ty, _ := ParseType("vector", []int{3})

	v, err := Cast(Text("[1,2,3]"), ty)
	if err != nil {
		t.Fatalf("Cast 失败: %v", err)
	}
	if v.K != KVector || v.VecDim() != 3 || !v.IsVecInline() {
		t.Fatalf("转换结果不对: %+v", v)
	}
	if v.TextForm() != "[1,2,3]" {
		t.Fatalf("TextForm = %q", v.TextForm())
	}

	// 维度不匹配必须报错，而不是悄悄截断/补零
	if _, err := Cast(Text("[1,2]"), ty); err == nil {
		t.Fatal("维度不匹配应该报错")
	} else if !strings.Contains(err.Error(), "dimension mismatch") {
		t.Fatalf("错误信息不对: %v", err)
	}

	// 已落盘的向量转文本：明确拒绝（内容只有存储层知道）
	if _, err := Cast(VectorRef(7, 3), Type{Kind: KText, Name: "text"}); err == nil {
		t.Fatal("已落盘向量转 text 应该报错")
	}
	// 内联字面量可以转文本
	if got, err := Cast(VectorLiteral("[1,2,3]", 3), Type{Kind: KText, Name: "text"}); err != nil || got.S != "[1,2,3]" {
		t.Fatalf("内联向量转文本失败: %v %q", err, got.S)
	}
	// NULL 保持 NULL
	if got, err := Cast(Null, ty); err != nil || !got.IsNull() {
		t.Fatalf("NULL 转 vector 不对: %v %v", err, got)
	}
}

func TestVectorValueShape(t *testing.T) {
	ref := VectorRef(42, 768)
	if id, ok := ref.VecID(); !ok || id != 42 {
		t.Fatalf("VecID = %d,%v", id, ok)
	}
	if ref.IsVecInline() {
		t.Fatal("VectorRef 不该是内联")
	}
	// 落盘向量拿不到内容，TextForm 要给一个扎眼的兜底
	if got := ref.TextForm(); got != "[vector:42]" {
		t.Fatalf("兜底文本 = %q", got)
	}
	if got := TypeOf(ref); got.Name != "vector(768)" || got.Len != 768 {
		t.Fatalf("TypeOf = %+v", got)
	}

	lit := VectorLiteral("[1,2]", 2)
	if _, ok := lit.VecID(); ok {
		t.Fatal("内联字面量不该有槽位号")
	}
	if lit.TextForm() != "[1,2]" {
		t.Fatalf("内联文本 = %q", lit.TextForm())
	}

	// 快照 JSON 必须 round-trip，否则恢复出来的向量引用全丢
	for _, v := range []Value{ref, lit} {
		b, err := v.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON 失败: %v", err)
		}
		var back Value
		if err := back.UnmarshalJSON(b); err != nil {
			t.Fatalf("UnmarshalJSON(%s) 失败: %v", b, err)
		}
		if back.K != KVector || back.VecDim() != v.VecDim() || !Same(back, v) {
			t.Fatalf("round-trip 不一致: %s -> %+v", b, back)
		}
	}

	// 直接比大小没有语义，必须拒绝
	if _, err := Compare(VectorLiteral("[1,2]", 2), VectorLiteral("[1,2]", 2)); err == nil {
		t.Fatal("向量直接比较应该报错")
	}
}

func TestDistances(t *testing.T) {
	a := []float32{0, 0}
	b := []float32{3, 4}
	if got := DistL2(a, b); math.Abs(float64(got)-5) > 1e-6 {
		t.Fatalf("L2 = %v，期望 5", got)
	}
	// 正交单位向量：余弦距离 = 1
	if got := DistCosine([]float32{1, 0}, []float32{0, 1}); math.Abs(float64(got)-1) > 1e-6 {
		t.Fatalf("余弦距离 = %v，期望 1", got)
	}
	// 同向：余弦距离 = 0
	if got := DistCosine([]float32{1, 2, 3}, []float32{2, 4, 6}); math.Abs(float64(got)) > 1e-6 {
		t.Fatalf("同向余弦距离 = %v，期望 0", got)
	}
	// 零向量：NaN（调用方折成 NULL）
	if got := DistCosine([]float32{0, 0}, []float32{1, 1}); !math.IsNaN(float64(got)) {
		t.Fatalf("零向量余弦距离应为 NaN，得到 %v", got)
	}
	// 负内积
	if got := DistNegIP([]float32{1, 2}, []float32{3, 4}); got != -11 {
		t.Fatalf("负内积 = %v，期望 -11", got)
	}
}

// 四路累加器的结果必须和朴素单累加器在同一量级（不是逐位相等，浮点加法有结合律问题）。
func TestDotProductMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{1, 3, 4, 5, 17, 768} {
		a := make([]float32, n)
		b := make([]float32, n)
		var want float64
		for i := 0; i < n; i++ {
			a[i] = float32(rng.NormFloat64())
			b[i] = float32(rng.NormFloat64())
			want += float64(a[i]) * float64(b[i])
		}
		got := float64(DotProduct(a, b))
		scale := math.Abs(want)
		if scale < 1 {
			scale = 1
		}
		if math.Abs(got-want)/scale > 1e-4 {
			t.Fatalf("n=%d 点积 %v，期望 ≈%v", n, got, want)
		}
	}
}

// 向量不能充当等值/分组键：内联字面量也不行。
//
// 内联值本可以按文本当身份，但同一句查询里「有的行内联、有的行已落盘」是可能的
// （事务内还没物化），两种身份混用会让去重结果取决于物理时机。所以一致拒绝。
func TestVectorNotKeyable(t *testing.T) {
	inline := VectorLiteral("[1,2,3]", 3)
	if _, err := Key([]Value{inline}); !errors.Is(err, ErrVectorNotKeyable) {
		t.Fatalf("内联向量编码键应报 ErrVectorNotKeyable，实际 %v", err)
	}
	stored := VectorRef(7, 3)
	if _, err := Key([]Value{stored}); !errors.Is(err, ErrVectorNotKeyable) {
		t.Fatalf("落盘向量编码键应报 ErrVectorNotKeyable，实际 %v", err)
	}
	// 混在正常列里同样拒绝：一个键里只要有一列是向量就没有正确的身份。
	if _, err := Key([]Value{Int(1), inline}); !errors.Is(err, ErrVectorNotKeyable) {
		t.Fatalf("混合值里的向量也应报错，实际 %v", err)
	}
	// 普通值的编码行为不变。
	k, err := Key([]Value{Int(1), Text("a"), Float(0)})
	if err != nil || k == "" {
		t.Fatalf("普通值编码键失败：%q %v", k, err)
	}
}
