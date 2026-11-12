package db

import (
	"fmt"
	"testing"
)

// numeric 除法的结果精度要和 PG 对齐。
//
// 原来循环小数只留 10 位（注释写的是「够 NUMERIC 用」），而 PG 的 numeric
// 除法至少给 16 位、实测 20 位：1::numeric/3 在 godb 里是 0.3333333333，
// 在 PG 里是 0.33333333333333333333。金额类算到分够用，但做同比/环比、
// 或者把结果再乘回去时，误差会被放大。
func TestNumericDivisionScale(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	cases := []struct{ sql, want string }{
		{"SELECT 1::numeric / 3", "0.33333333333333333333"},
		{"SELECT 2::numeric / 3", "0.66666666666666666667"},
		// 分母只含 2/5 时是有限小数，不该被补成 20 位
		{"SELECT 1::numeric / 8", "0.125"},
		{"SELECT 10::numeric / 4", "2.5"},
	}
	for _, c := range cases {
		res := run(t, d, s, c.sql)
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.want {
			t.Errorf("%s = %s，期望 %s", c.sql, got, c.want)
		}
	}

	// 带精度的列仍按声明标度落盘（返回值精度高，不代表存储要跟着变宽）
	run(t, d, s, "CREATE TABLE nd (v numeric(6,3))")
	run(t, d, s, "INSERT INTO nd VALUES (1::numeric / 3)")
	res := run(t, d, s, "SELECT v FROM nd")
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0.333" {
		t.Errorf("numeric(6,3) 列应存 0.333，实际 %s", got)
	}
}

// 小数字面量按 PG 的规则解析：**普通小数是 numeric，带指数的才是 double**。
//
// 以前两者都按 float64 解析，于是 0.1 + 0.2 = 0.30000000000000004、
// `0.1 + 0.2 = 0.3` 为 false —— 金额/比率类计算静默带误差。
func TestDecimalLiteralTyping(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	cases := []struct{ sql, wantType, wantVal string }{
		{"SELECT 0.1 + 0.2", "numeric", "0.3"},
		{"SELECT 123.45", "numeric", "123.45"},
		{"SELECT 1.5 * 2", "numeric", "3"},
		{"SELECT 3.0 / 2", "numeric", "1.5"},
		{"SELECT round(2.5)", "numeric", "3"},
		{"SELECT 1e3", "double precision", "1000"},
		{"SELECT 1.5::float8", "double precision", "1.5"},
	}
	for _, c := range cases {
		res := run(t, d, s, c.sql)
		if got := res[0].Columns[0].Type; got != c.wantType {
			t.Errorf("%s 类型 = %q，期望 %q", c.sql, got, c.wantType)
		}
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.wantVal {
			t.Errorf("%s = %s，期望 %s", c.sql, got, c.wantVal)
		}
	}

	// 等值比较必须成立 —— 这是浮点字面量最容易露馅的地方
	res := run(t, d, s, "SELECT 0.1 + 0.2 = 0.3")
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "true" {
		t.Errorf("0.1 + 0.2 = 0.3 应为 true，实际 %s", got)
	}
}
