package pgwire

import (
	"strings"
)

// SET / SHOW 现在由 SQL 层统一处理（sql.Show、sql.SetStmt + exec.ShowResult），
// 这样网页控制台和 PG 协议两条入口的行为不会分叉。
// 这个文件只剩 normalize：在「不执行语句就推断输出列」时（Describe）按前缀匹配用。

// normalize 压成便于前缀匹配的形式：小写、去多余空白。
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// 去掉结尾分号，避免 "select 1;" 匹配不上
	s = strings.TrimRight(strings.TrimSpace(s), ";")
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
