package types

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal 是精确十进制（NUMERIC）。Value 里存规范化文本（无指数、无多余尾零），
// 运算用 math/big.Rat，保证「0.1+0.2 == 0.3」而不是 float64 的 0.30000000000000004。
//
// 规范化规则：去掉前导零（整数部分留一个 0）、去掉尾部小数零、"-.5" → "-0.5"、
// "5." → "5"。空串与非法输入返回错误。

// ParseDecimal 解析并规范化十进制文本。
func ParseDecimal(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("invalid input syntax for type numeric: %q", s)
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return "", fmt.Errorf("invalid input syntax for type numeric: %q", s)
	}
	// 只允许 digits 和最多一个小数点（不支持 1e5 这种写法——那不是 numeric 字面量）
	dot := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if dot >= 0 {
				return "", fmt.Errorf("invalid input syntax for type numeric: %q", s)
			}
			dot = i
			continue
		}
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid input syntax for type numeric: %q", s)
		}
	}
	intPart, fracPart := s, ""
	if dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	// 去掉整数部分前导零
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	// 去掉小数部分尾零
	fracPart = strings.TrimRight(fracPart, "0")
	if neg && (intPart != "0" || fracPart != "") {
		if fracPart == "" {
			return "-" + intPart, nil
		}
		return "-" + intPart + "." + fracPart, nil
	}
	if fracPart == "" {
		return intPart, nil
	}
	return intPart + "." + fracPart, nil
}

// DecimalRat 把规范十进制文本转成 big.Rat。
func DecimalRat(s string) *big.Rat {
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		r.SetInt64(0)
	}
	return r
}

// RatDecimal 把 big.Rat 渲染成规范十进制文本（不用指数）。
func RatDecimal(r *big.Rat) string {
	if r == nil {
		return "0"
	}
	s := r.FloatString(decimalScale(r))
	// FloatString 会补足 scale 位，再按规范化去掉尾零
	out, err := ParseDecimal(s)
	if err != nil {
		return strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return out
}

// decimalScale 决定渲染时保留几位：分母只含 2/5 因子时要足够多位才精确。
func decimalScale(r *big.Rat) int {
	den := r.Denom()
	if den.Cmp(big.NewInt(1)) == 0 {
		return 0
	}
	// 10 的幂次：den = 2^a * 5^b → scale = max(a,b)
	s := 0
	d := new(big.Int).Set(den)
	two := big.NewInt(2)
	five := big.NewInt(5)
	for {
		q, m := new(big.Int).DivMod(d, two, new(big.Int))
		if m.Sign() != 0 {
			break
		}
		d = q
		s++
	}
	for {
		q, m := new(big.Int).DivMod(d, five, new(big.Int))
		if m.Sign() != 0 {
			break
		}
		d = q
		s++
	}
	if d.Cmp(big.NewInt(1)) == 0 {
		if s < 1 {
			s = 1
		}
		if s > 20 {
			s = 20
		}
		return s
	}
	// 循环小数：保留 10 位（够 NUMERIC 用）
	return 10
}

// Decimal 构造规范十进制值（非法输入回 0）。
func Decimal(s string) Value {
	n, err := ParseDecimal(s)
	if err != nil {
		return Value{K: KDecimal, S: "0"}
	}
	return Value{K: KDecimal, S: n}
}

// DecimalMantissaFast 尝试用 int64 解析尾数（零分配快速路径）。
// 返回 (尾数, 小数位, true)；超出 int64 范围或解析失败返回 false。
func DecimalMantissaFast(s string) (int64, int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s, ""
	if dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	fracPart = strings.TrimRight(fracPart, "0")
	scale := len(fracPart)
	digits := intPart + fracPart
	if digits == "" {
		return 0, scale, true // "0" 或 "."
	}
	var m int64
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, 0, false
		}
		d := int64(c - '0')
		if m > (9223372036854775807-d)/10 {
			return 0, 0, false // 溢出
		}
		m = m*10 + d
	}
	if neg {
		m = -m
	}
	return m, scale, true
}

// DecimalMantissa 把规范十进制文本拆成 (尾数, 小数位)：`123.45` → (12345, 2)。
// 比 big.Rat.SetString 快得多，供 sum/avg 累加用。
func DecimalMantissa(s string) (*big.Int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, 0, fmt.Errorf("invalid numeric %q", s)
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	intPart, fracPart := s, ""
	if dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	// 去掉尾零，缩小小数位
	fracPart = strings.TrimRight(fracPart, "0")
	scale := len(fracPart)
	digits := intPart + fracPart
	if digits == "" {
		digits = "0"
	}
	m := new(big.Int)
	if _, ok := m.SetString(digits, 10); !ok {
		return nil, 0, fmt.Errorf("invalid numeric %q", s)
	}
	if neg {
		m.Neg(m)
	}
	return m, scale, nil
}

// MantissaDecimal 把 (尾数, 小数位) 渲染成规范十进制文本。
func MantissaDecimal(m *big.Int, scale int) string {
	if m == nil {
		return "0"
	}
	s := m.String()
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for len(s) <= scale {
		s = "0" + s
	}
	if scale == 0 {
		if neg {
			return "-" + s
		}
		return s
	}
	out := s[:len(s)-scale] + "." + s[len(s)-scale:]
	out = strings.TrimRight(out, "0")
	out = strings.TrimRight(out, ".")
	if out == "" || out == "-" {
		return "0"
	}
	if neg {
		return "-" + out
	}
	return out
}

// CompareDecimal 比较两个十进制文本。
func CompareDecimal(a, b string) int {
	ra, rb := DecimalRat(a), DecimalRat(b)
	return ra.Cmp(rb)
}

// AddDecimal / SubDecimal / MulDecimal / DivDecimal 做精确四则。
func AddDecimal(a, b string) (string, error) {
	r := new(big.Rat).Add(DecimalRat(a), DecimalRat(b))
	return RatDecimal(r), nil
}

func SubDecimal(a, b string) (string, error) {
	r := new(big.Rat).Sub(DecimalRat(a), DecimalRat(b))
	return RatDecimal(r), nil
}

func MulDecimal(a, b string) (string, error) {
	r := new(big.Rat).Mul(DecimalRat(a), DecimalRat(b))
	return RatDecimal(r), nil
}

func DivDecimal(a, b string) (string, error) {
	rb := DecimalRat(b)
	if rb.Sign() == 0 {
		return "", fmt.Errorf("division by zero")
	}
	r := new(big.Rat).Quo(DecimalRat(a), rb)
	return RatDecimal(r), nil
}
