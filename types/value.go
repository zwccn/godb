// Package types 定义值（Datum）与 SQL 数据类型。
package types

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind 是值/类型的运行时类别。
type Kind uint8

const (
	KNull Kind = iota
	KInt
	KFloat
	KText
	KBool
	KTime
	KDecimal  // NUMERIC：S 存规范十进制文本，运算精确
	KInterval // I = 纳秒（天/时/分/秒），z = 月数（含年*12）
)

func (k Kind) String() string {
	switch k {
	case KInt:
		return "integer"
	case KFloat:
		return "double precision"
	case KText:
		return "text"
	case KBool:
		return "boolean"
	case KTime:
		return "timestamp"
	case KDecimal:
		return "numeric"
	case KInterval:
		return "interval"
	case KNull:
		return "unknown"
	}
	return "unknown"
}

// Value 是一个 SQL 值。
//
// 内存布局是刻意压小的：行数据全驻内存，一个 Value 的大小直接乘上行数。
// 早期版本把 int64/float64/bool/time.Time 各占一个字段，一个 Value 要 72 字节，
// 4 列的表一行就要 300 多字节。现在只保留 I 一个 8 字节槽位，按 Kind 复用：
//
//	KInt      I = 整数值
//	KFloat    I = float64 的位模式（math.Float64bits）
//	KBool     I = 0 / 1
//	KTime     I = UnixNano，z = 时区偏移（分钟）
//	KText     S
//
// 结果：32 字节（K 1 + z 2 + 对齐 5 + I 8 + S 16），比原来小一半以上。
type Value struct {
	K Kind
	z int16
	I int64
	S string
}

var Null = Value{K: KNull}

func Int(i int64) Value     { return Value{K: KInt, I: i} }
func Float(f float64) Value { return Value{K: KFloat, I: int64(math.Float64bits(f))} }
func Text(s string) Value   { return Value{K: KText, S: s} }
func Bool(b bool) Value {
	if b {
		return Value{K: KBool, I: 1}
	}
	return Value{K: KBool}
}
func Time(t time.Time) Value {
	_, offSec := t.Zone()
	return Value{K: KTime, I: t.UnixNano(), z: int16(offSec / 60)}
}

// Interval 构造 interval：months 月 + nanos 纳秒。
func Interval(months, nanos int64) Value {
	return Value{K: KInterval, I: nanos, z: int16(months)}
}

// Months 返回 interval 的月数部分。
func (v Value) Months() int64 {
	if v.K != KInterval {
		return 0
	}
	return int64(v.z)
}

func (v Value) IsNull() bool    { return v.K == KNull }
func (v Value) IsNumeric() bool { return v.K == KInt || v.K == KFloat || v.K == KDecimal }

// Float 返回数值形态（非数值返回 NaN）。KDecimal 有损转换，仅在必须用 float 时用。
func (v Value) Float() float64 {
	switch v.K {
	case KInt:
		return float64(v.I)
	case KFloat:
		return math.Float64frombits(uint64(v.I))
	case KDecimal:
		f, err := strconv.ParseFloat(v.S, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

// Bool 返回布尔值（非布尔类型返回 false）。
func (v Value) Bool() bool { return v.K == KBool && v.I != 0 }

// Time 返回时间值（非时间类型返回零值）。
func (v Value) Time() time.Time {
	if v.K != KTime {
		return time.Time{}
	}
	t := time.Unix(0, v.I)
	if v.z == 0 {
		return t.UTC()
	}
	return t.In(time.FixedZone("", int(v.z)*60))
}

// TextForm 是值的文本表现形式，用于字符串拼接、LIKE、显示。
func (v Value) TextForm() string {
	switch v.K {
	case KInt:
		return strconv.FormatInt(v.I, 10)
	case KFloat:
		return formatFloat(v.Float())
	case KBool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case KTime:
		// 带微秒：timestamp 的小数秒不能被截掉（客户端写进来 06:07:08.123456，
		// 读回去只剩 06:07:08 算数据丢失）。.999999 会去掉尾随的 0，
		// 整秒仍输出 "…:08"，和 PG 的文本格式一致。
		return v.Time().Format("2006-01-02 15:04:05.999999")
	case KDecimal:
		return v.S
	case KInterval:
		return intervalText(v.Months(), v.I)
	case KText:
		return v.S
	}
	return "NULL"
}

// intervalText 渲染 interval 为 PG 风格文本（如 "1 mon 2 days 03:00:00"）。
func intervalText(months, nanos int64) string {
	var parts []string
	if months != 0 {
		y, m := months/12, months%12
		if y != 0 {
			parts = append(parts, fmt.Sprintf("%d year%s", abs64(y), plural(y)))
		}
		if m != 0 {
			parts = append(parts, fmt.Sprintf("%d mon%s", abs64(m), plural(m)))
		}
	}
	neg := nanos < 0
	if neg {
		nanos = -nanos
	}
	days := nanos / (24 * 3600 * 1e9)
	nanos %= 24 * 3600 * 1e9
	h := nanos / (3600 * 1e9)
	nanos %= 3600 * 1e9
	mi := nanos / (60 * 1e9)
	nanos %= 60 * 1e9
	sec := float64(nanos) / 1e9
	if days != 0 {
		parts = append(parts, fmt.Sprintf("%d day%s", days, plural(days)))
	}
	if h != 0 || mi != 0 || sec != 0 || (len(parts) == 0 && months == 0) {
		if sec == float64(int64(sec)) {
			parts = append(parts, fmt.Sprintf("%02d:%02d:%02d", h, mi, int64(sec)))
		} else {
			parts = append(parts, fmt.Sprintf("%02d:%02d:%09.6f", h, mi, sec))
		}
	}
	s := strings.Join(parts, " ")
	if neg && s != "" {
		return "-" + s
	}
	if s == "" {
		return "00:00:00"
	}
	return s
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func plural(v int64) string {
	if v == 1 || v == -1 {
		return ""
	}
	return "s"
}

func formatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Compare 按 SQL 语义比较两个非 NULL 值。
// 若两侧类别不同，会尝试把文本侧解释为目标类型（模拟 PG 的 unknown 字面量）。
func Compare(a, b Value) (int, error) {
	if a.K == KNull || b.K == KNull {
		return 0, fmt.Errorf("cannot compare NULL")
	}
	if a.K == b.K {
		switch a.K {
		case KInt:
			return cmpInt(a.I, b.I), nil
		case KFloat:
			return cmpFloat(a.Float(), b.Float()), nil
		case KDecimal:
			return CompareDecimal(a.S, b.S), nil
		case KText:
			return strings.Compare(a.S, b.S), nil
		case KBool:
			return cmpInt(b2i(a.Bool()), b2i(b.Bool())), nil
		case KTime:
			at, bt := a.Time(), b.Time()
			switch {
			case at.Before(bt):
				return -1, nil
			case at.After(bt):
				return 1, nil
			}
			return 0, nil
		}
	}
	if a.IsNumeric() && b.IsNumeric() {
		// 有 decimal 参与时走精确比较，避免 float 精度把 0.1+0.2 判成不等于 0.3
		if a.K == KDecimal || b.K == KDecimal {
			return CompareDecimal(a.TextForm(), b.TextForm()), nil
		}
		return cmpFloat(a.Float(), b.Float()), nil
	}
	if a.K == KText {
		if c, err := Cast(a, Type{Kind: b.K, Name: b.K.String()}); err == nil {
			return Compare(c, b)
		}
	}
	if b.K == KText {
		if c, err := Cast(b, Type{Kind: a.K, Name: a.K.String()}); err == nil {
			return Compare(a, c)
		}
	}
	return 0, fmt.Errorf("operator does not exist: %s = %s", a.K, b.K)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Same 判断两个值是否完全相同（NULL 与 NULL 视为相同），用于回放日志时的行定位。
func Same(a, b Value) bool {
	if a.K != b.K {
		return false
	}
	switch a.K {
	case KNull:
		return true
	case KInt:
		return a.I == b.I
	case KFloat:
		return a.Float() == b.Float()
	case KText:
		return a.S == b.S
	case KBool:
		return a.Bool() == b.Bool()
	case KTime:
		return a.I == b.I && a.z == b.z
	case KDecimal:
		return a.S == b.S
	}
	return false
}

// RowSame 判断两行内容是否完全一致。
func RowSame(a, b Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Same(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Row 是一行数据，位置与表定义中的列顺序一致。
type Row []Value

// Key 把若干值编码成可做 map key 的字符串（无二义性、无分隔符冲突）。
func Key(vals []Value) string {
	var b strings.Builder
	for _, v := range vals {
		AppendKey(&b, v)
	}
	return b.String()
}

func AppendKey(b *strings.Builder, v Value) {
	b.WriteByte(byte(v.K) + 'a')
	switch v.K {
	case KNull:
	case KInt:
		b.WriteString(strconv.FormatInt(v.I, 10))
	case KFloat:
		f := v.Float()
		// -0.0 与 0.0 是相等的两个值，编码必须一致，否则哈希索引会把它们
		// 当成两个键（主键/唯一索引会放过重复值，等值查找还会漏行）。
		if f == 0 {
			f = 0
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case KBool:
		if v.Bool() {
			b.WriteByte('t')
		} else {
			b.WriteByte('f')
		}
	case KText:
		b.WriteString(strconv.Itoa(len(v.S)))
		b.WriteByte(':')
		b.WriteString(v.S)
	case KTime:
		b.WriteString(strconv.FormatInt(v.I, 10))
	case KDecimal:
		b.WriteString(v.S)
	}
	b.WriteByte('|')
}

// MarshalJSON 用于持久化：时间用 {"$ts":...} 包装，避免与文本字面量混淆。
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.K {
	case KNull:
		return []byte("null"), nil
	case KInt:
		return []byte(strconv.FormatInt(v.I, 10)), nil
	case KFloat:
		f := v.Float()
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return json.Marshal(map[string]string{"$f": formatFloat(f)})
		}
		return []byte(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case KBool:
		return json.Marshal(v.Bool())
	case KText:
		return json.Marshal(v.S)
	case KTime:
		return json.Marshal(map[string]string{"$ts": v.Time().UTC().Format(time.RFC3339Nano)})
	case KDecimal:
		// 用字符串持久化：float 会丢精度，{"$n":"..."} 保证 round-trip 精确
		return json.Marshal(map[string]string{"$n": v.S})
	}
	return []byte("null"), nil
}

func (v *Value) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*v = Null
		return nil
	}
	if strings.HasPrefix(s, "{") {
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if ts, ok := m["$ts"]; ok {
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil {
				return err
			}
			*v = Time(t)
			return nil
		}
		if f, ok := m["$f"]; ok {
			fv, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return err
			}
			*v = Float(fv)
			return nil
		}
		if n, ok := m["$n"]; ok {
			*v = Decimal(n)
			return nil
		}
		return fmt.Errorf("cannot decode value %s", s)
	}
	if s == "true" || s == "false" {
		*v = Bool(s == "true")
		return nil
	}
	if strings.HasPrefix(s, "\"") {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*v = Text(str)
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		*v = Int(i)
		return nil
	}
	fv, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*v = Float(fv)
	return nil
}

// Interface 返回适合直接编码成 JSON 给前端的形式。
func (v Value) Interface() interface{} {
	switch v.K {
	case KNull:
		return nil
	case KInt:
		return v.I
	case KFloat:
		f := v.Float()
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return formatFloat(f)
		}
		return f
	case KBool:
		return v.Bool()
	case KText:
		return v.S
	case KTime:
		// 带微秒：timestamp 的小数秒不能被截掉（客户端写进来 06:07:08.123456，
		// 读回去只剩 06:07:08 算数据丢失）。.999999 会去掉尾随的 0，
		// 整秒仍输出 "…:08"，和 PG 的文本格式一致。
		return v.Time().Format("2006-01-02 15:04:05.999999")
	case KDecimal:
		// 保持精确文本：转 float 会丢精度/出科学计数法（1234567890123.45 → 1.23e+12）
		return v.S
	case KInterval:
		return intervalText(v.Months(), v.I)
	}
	return nil
}
