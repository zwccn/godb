package types

import "testing"

// 整数字面量按值域提升：int32 边界内是 integer，超出就是 bigint。
// 声明成 int4 却装不下时，客户端按 4 字节解码会把值截断（9223372036854775807 → -1）。
func TestLiteralType(t *testing.T) {
	cases := []struct {
		in   Value
		want string
	}{
		{Int(0), "integer"},
		{Int(2147483647), "integer"},
		{Int(-2147483648), "integer"},
		{Int(2147483648), "bigint"},
		{Int(-2147483649), "bigint"},
		{Int(9223372036854775807), "bigint"},
		{Text("x"), "text"},
		{Bool(true), "boolean"},
		{Float(1.5), "double precision"},
		{Null, "unknown"},
	}
	for _, c := range cases {
		if got := LiteralType(c.in).Name; got != c.want {
			t.Errorf("LiteralType(%v).Name = %q, 期望 %q", c.in.TextForm(), got, c.want)
		}
	}
}
