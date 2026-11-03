package types

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MaxVectorDim 是向量列允许的最大维度。
//
// 上限定在 4096 而不是更大：1536 维 × 1000 万行已经是 57 GB，普通机器放不下；
// 再往上加维度只会让「内存不够」变成一种更难解释的慢。
const MaxVectorDim = 4096

// ParseVector 解析 pgvector 风格的向量文本：`[1,2,3]`。
//
// 允许元素间有空格与换行，不接受空元素、NaN、Inf —— 这些一旦进了列，
// 距离计算会静默返回 NaN 并污染整个 Top-K 排序，不如在入口就报错。
func ParseVector(s string) ([]float32, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, fmt.Errorf("invalid input syntax for type vector: %q", s)
	}
	body := t[1 : len(t)-1]
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("vector must have at least 1 dimension")
	}
	// 按逗号数预分配：10 万行的导入里，每行多一次 grow 就是一次拷贝
	n := 1
	for i := 0; i < len(body); i++ {
		if body[i] == ',' {
			n++
		}
	}
	out := make([]float32, 0, n)
	i := 0
	for {
		for i < len(body) && isVecSpace(body[i]) {
			i++
		}
		if i >= len(body) {
			// 走到这里只有一种情况：末尾多了一个逗号（"[1,2,]"）
			return nil, fmt.Errorf("invalid input syntax for type vector: %q", s)
		}
		start := i
		for i < len(body) && body[i] != ',' {
			i++
		}
		tok := strings.TrimSpace(body[start:i])
		if tok == "" {
			return nil, fmt.Errorf("invalid input syntax for type vector: %q", s)
		}
		f, err := strconv.ParseFloat(tok, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid input syntax for type vector: %q", s)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("vector element must be finite, got %q", tok)
		}
		out = append(out, float32(f))
		if len(out) > MaxVectorDim {
			return nil, fmt.Errorf("vector dimension %d exceeds the maximum of %d", len(out), MaxVectorDim)
		}
		if i < len(body) {
			i++ // 跳过逗号
			continue
		}
		break
	}
	return out, nil
}

func isVecSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// AppendVector 按 pgvector 的文本格式追加一个向量：`[0.1,0.2]`。
//
// 用 32 位最短往返表示（'g' + bitSize 32），和 pgvector 的输出一致：
// 1.0 打成 "1"、0.1 打成 "0.1"，不会出现 "0.10000000149011612"。
func AppendVector(b []byte, v []float32) []byte {
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(f), 'g', -1, 32)
	}
	return append(b, ']')
}

// VectorText 把向量格式化成文本。
func VectorText(v []float32) string {
	return string(AppendVector(make([]byte, 0, len(v)*8+2), v))
}

// DotProduct 计算点积。
//
// 四个累加器不是可选项：单累加器有 5 cycle 的 FMA 依赖链，实测慢 1.3 倍
// （本机单核 1.6 → 2.45 GFLOPS）。这是整条检索路径最热的循环。
func DotProduct(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// Norm2 计算 L2 模长。
func Norm2(a []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(a)
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * a[i]
		s1 += a[i+1] * a[i+1]
		s2 += a[i+2] * a[i+2]
		s3 += a[i+3] * a[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * a[i]
	}
	return float32(math.Sqrt(float64((s0 + s1) + (s2 + s3))))
}

// DistL2 是 `<->`：欧氏距离。
func DistL2(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		d0 := a[i] - b[i]
		d1 := a[i+1] - b[i+1]
		d2 := a[i+2] - b[i+2]
		d3 := a[i+3] - b[i+3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	for ; i < n; i++ {
		d := a[i] - b[i]
		s0 += d * d
	}
	return float32(math.Sqrt(float64((s0 + s1) + (s2 + s3))))
}

// DistCosine 是 `<=>`：余弦距离 1 - cos(a,b)。
//
// 任一侧是零向量时返回 NaN（与 pgvector 一致）。调用方必须把 NaN 当成
// 「不可比较」—— 表达式层会把它折成 NULL，否则 NaN 会以「既不大于也不小于」
// 的方式沉到排序结果最前面，看起来像是召回了奇怪的近邻。
func DistCosine(a, b []float32) float32 {
	na, nb := Norm2(a), Norm2(b)
	if na == 0 || nb == 0 {
		return float32(math.NaN())
	}
	d := DotProduct(a, b) / (na * nb)
	// 浮点误差可能让 d 略微超出 [-1,1]，夹一下，避免 acos 类的后续计算出 NaN
	if d > 1 {
		d = 1
	} else if d < -1 {
		d = -1
	}
	return 1 - d
}

// DistNegIP 是 `<#>`：负内积 -a·b（越小越相似，和 pgvector 同语义）。
func DistNegIP(a, b []float32) float32 {
	return -DotProduct(a, b)
}
