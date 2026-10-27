package storage

import (
	"sort"
	"testing"
	"time"

	"godb/types"
)

// 保序编码的核心契约：字符串比较的结果和 types.Compare 一致。
func TestOrderKeyMatchesCompare(t *testing.T) {
	cases := [][]types.Value{
		{types.Int(-100), types.Int(-1), types.Int(0), types.Int(1), types.Int(9), types.Int(10), types.Int(100)},
		{types.Int(-9223372036854775808), types.Int(9223372036854775807)},
		{types.Float(-1e9), types.Float(-1.5), types.Float(-0.0001), types.Float(0), types.Float(0.0001), types.Float(1.5), types.Float(1e9)},
		{types.Bool(false), types.Bool(true)},
		{types.Text(""), types.Text("a"), types.Text("ab"), types.Text("abc"), types.Text("b"), types.Text("中文")},
		{types.Time(time.Unix(0, 0)), types.Time(time.Unix(1000, 0)), types.Time(time.Unix(0, 1e9))},
	}
	for gi, group := range cases {
		// 用 types.Compare 排一遍，拿到期望顺序
		want := append([]types.Value(nil), group...)
		sort.Slice(want, func(i, j int) bool {
			c, err := types.Compare(want[i], want[j])
			if err != nil {
				t.Fatalf("compare: %v", err)
			}
			return c < 0
		})
		// 编码后按字符串排序，顺序必须一致
		type kv struct {
			k string
			v types.Value
		}
		var enc []kv
		for _, v := range group {
			enc = append(enc, kv{OrderKey([]types.Value{v}), v})
		}
		sort.Slice(enc, func(i, j int) bool { return enc[i].k < enc[j].k })
		for i := range want {
			if !types.Same(want[i], enc[i].v) {
				t.Fatalf("第 %d 组第 %d 位：编码序是 %v，值序应为 %v", gi, i, enc[i].v.TextForm(), want[i].TextForm())
			}
		}
	}
}

// -0.0 和 0.0 相等，编码必须相同（否则范围扫描会把它们当成两个键）。
func TestOrderKeyNegativeZero(t *testing.T) {
	zero := types.Float(0)
	neg := types.Float(-0.0)
	if OrderKey([]types.Value{zero}) != OrderKey([]types.Value{neg}) {
		t.Fatal("-0.0 与 0.0 的键应该相同")
	}
}

// 文本里的 0x00 要转义，不能和终止符混淆。
func TestOrderKeyNulBytes(t *testing.T) {
	a := OrderKey([]types.Value{types.Text("a")})
	b := OrderKey([]types.Value{types.Text("a\x00b")})
	c := OrderKey([]types.Value{types.Text("ab")})
	if a >= b {
		t.Fatal(`"a" 应该小于 "a\0b"`)
	}
	if b >= c {
		t.Fatal(`"a\0b" 应该小于 "ab"`)
	}
	// 多列拼接不能有歧义：("a","bc") vs ("ab","c")
	x := OrderKey([]types.Value{types.Text("a"), types.Text("bc")})
	y := OrderKey([]types.Value{types.Text("ab"), types.Text("c")})
	if x >= y {
		t.Fatal(`("a","bc") 应小于 ("ab","c")`)
	}
}

// 多列键：先比第一列，第一列相同再比第二列。
func TestOrderKeyMultiColumn(t *testing.T) {
	rows := [][]types.Value{
		{types.Int(1), types.Text("b")},
		{types.Int(1), types.Text("a")},
		{types.Int(2), types.Text("a")},
		{types.Int(1), types.Text("c")},
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = OrderKey(r)
	}
	sort.Strings(keys)
	want := []string{
		OrderKey([]types.Value{types.Int(1), types.Text("a")}),
		OrderKey([]types.Value{types.Int(1), types.Text("b")}),
		OrderKey([]types.Value{types.Int(1), types.Text("c")}),
		OrderKey([]types.Value{types.Int(2), types.Text("a")}),
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("第 %d 位排序不对", i)
		}
	}
}

// 前缀区间必须恰好覆盖以该前缀开头的所有文本。
func TestTextPrefixRange(t *testing.T) {
	lo, hi := TextPrefixRange("abc")
	inside := []string{"abc", "abcd", "abc-zzz", "abc中文"}
	outside := []string{"ab", "abca" /* 还在里面 */, "abd", "ab", "abz"}
	_ = outside
	for _, s := range inside {
		k := OrderKey([]types.Value{types.Text(s)})
		if k < lo || k >= hi {
			t.Fatalf("%q 应该落在前缀区间内", s)
		}
	}
	for _, s := range []string{"ab", "abd", "abz", "ac", "b", ""} {
		k := OrderKey([]types.Value{types.Text(s)})
		if k >= lo && k < hi {
			t.Fatalf("%q 不该落在前缀区间内", s)
		}
	}
}

// 行上取键：含 NULL 或缺列的行不入索引。
func TestOrderKeyOfRow(t *testing.T) {
	row := types.Row{types.Int(1), types.Null, types.Text("x")}
	if _, ok := OrderKeyOfRow(row, []int{1}); ok {
		t.Fatal("NULL 列不该产生键")
	}
	if _, ok := OrderKeyOfRow(row, []int{5}); ok {
		t.Fatal("越界列不该产生键")
	}
	k, ok := OrderKeyOfRow(row, []int{0, 2})
	if !ok || k != OrderKey([]types.Value{row[0], row[2]}) {
		t.Fatalf("多列取键不对: %q", k)
	}
}
