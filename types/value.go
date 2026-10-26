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
	case KNull:
		return "unknown"
	}
	return "unknown"
}

// Value 是一个 SQL 值。使用 Kind 判别当前哪个字段有效，避免重复的类型断言。
type Value struct {
	K Kind
	I int64
	F float64
	S string
	B bool
	T time.Time
}

var Null = Value{K: KNull}

func Int(i int64) Value      { return Value{K: KInt, I: i} }
func Float(f float64) Value  { return Value{K: KFloat, F: f} }
func Text(s string) Value    { return Value{K: KText, S: s} }
func Bool(b bool) Value      { return Value{K: KBool, B: b} }
func Time(t time.Time) Value { return Value{K: KTime, T: t} }

func (v Value) IsNull() bool    { return v.K == KNull }
func (v Value) IsNumeric() bool { return v.K == KInt || v.K == KFloat }

// Float 返回数值形态（非数值返回 NaN）。
func (v Value) Float() float64 {
	switch v.K {
	case KInt:
		return float64(v.I)
	case KFloat:
		return v.F
	}
	return math.NaN()
}

// TextForm 是值的文本表现形式，用于字符串拼接、LIKE、显示。
func (v Value) TextForm() string {
	switch v.K {
	case KInt:
		return strconv.FormatInt(v.I, 10)
	case KFloat:
		return formatFloat(v.F)
	case KBool:
		if v.B {
			return "true"
		}
		return "false"
	case KTime:
		return v.T.Format("2006-01-02 15:04:05")
	case KText:
		return v.S
	}
	return "NULL"
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
			return cmpFloat(a.F, b.F), nil
		case KText:
			return strings.Compare(a.S, b.S), nil
		case KBool:
			return cmpInt(b2i(a.B), b2i(b.B)), nil
		case KTime:
			switch {
			case a.T.Before(b.T):
				return -1, nil
			case a.T.After(b.T):
				return 1, nil
			}
			return 0, nil
		}
	}
	if a.IsNumeric() && b.IsNumeric() {
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
		return a.F == b.F
	case KText:
		return a.S == b.S
	case KBool:
		return a.B == b.B
	case KTime:
		return a.T.Equal(b.T)
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
		b.WriteString(strconv.FormatFloat(v.F, 'g', -1, 64))
	case KBool:
		if v.B {
			b.WriteByte('t')
		} else {
			b.WriteByte('f')
		}
	case KText:
		b.WriteString(strconv.Itoa(len(v.S)))
		b.WriteByte(':')
		b.WriteString(v.S)
	case KTime:
		b.WriteString(strconv.FormatInt(v.T.UnixNano(), 10))
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
		if math.IsInf(v.F, 0) || math.IsNaN(v.F) {
			return json.Marshal(map[string]string{"$f": formatFloat(v.F)})
		}
		return []byte(strconv.FormatFloat(v.F, 'g', -1, 64)), nil
	case KBool:
		return json.Marshal(v.B)
	case KText:
		return json.Marshal(v.S)
	case KTime:
		return json.Marshal(map[string]string{"$ts": v.T.UTC().Format(time.RFC3339Nano)})
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
		if math.IsInf(v.F, 0) || math.IsNaN(v.F) {
			return formatFloat(v.F)
		}
		return v.F
	case KBool:
		return v.B
	case KText:
		return v.S
	case KTime:
		return v.T.Format("2006-01-02 15:04:05")
	}
	return nil
}
