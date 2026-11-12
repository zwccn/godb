package db

import (
	"fmt"
	"strings"
	"testing"
)

// substr 的位置语义要和 PG 一致：1-based，而且 **start < 1 时缺失的那几格仍然占长度**。
//
// 原来的实现把起点 clamp 到 1、长度却原样保留，于是 substr('abcdef', 0, 3) 给出
// 'abc'（PG 是 'ab'）—— 截取差一位不报错，只会在某天发现「名字少了一个字」。
func TestSubstrPositionSemantics(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	cases := []struct{ sql, want string }{
		{"SELECT substr('abcdef', 1, 3)", "abc"},
		{"SELECT substr('abcdef', 0, 3)", "ab"},   // 位置 0 占掉一格长度
		{"SELECT substr('abcdef', -1, 3)", "a"},   // 缺口比长度还长 → 只剩一个字符
		{"SELECT substr('abcdef', 0)", "abcdef"},  // 无长度：从 1 截到结尾
		{"SELECT substr('abcdef', 3, 2)", "cd"},
		{"SELECT substr('abcdef', 2, 100)", "bcdef"},
		{"SELECT substr('abcdef', 99, 2)", ""},
		{"SELECT substr('abcdef', 7, 2)", ""}, // 刚好越界
		{"SELECT substr('你好世界', 0, 3)", "你好"},
		{"SELECT substring('abcdef' from 0 for 3)", "ab"},
	}
	for _, c := range cases {
		res := run(t, d, s, c.sql)
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.want {
			t.Errorf("%s = %q，期望 %q", c.sql, got, c.want)
		}
	}

	// 负长度在 PG 里是 22011，不是「返回空串」
	if err := runErr(t, d, s, "SELECT substr('abcdef', 1, -1)"); !strings.Contains(err.Error(), "negative substring length") {
		t.Errorf("负长度应报 negative substring length，实际: %v", err)
	}
}
