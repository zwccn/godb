package pgwire

import (
	"fmt"
	"strings"
)

// compat 拦下引擎不支持的会话语句（SET / SHOW）。
// 其它探针（version()、current_schema()、set_config() 等）现在由
// exec 里的兼容函数直接执行，走正常查询路径，所以不在这里处理。
//
// 返回 (消息列表, 是否已处理)。
func (c *conn) compat(sqlText string) ([][]byte, bool) {
	norm := normalize(sqlText)
	if norm == "" {
		return nil, false
	}

	// SET xxx = yyy：客户端用来设会话参数，我们直接确认
	if strings.HasPrefix(norm, "set ") || strings.HasPrefix(norm, "set\t") {
		return [][]byte{encodeCommandComplete("SET")}, true
	}

	// SHOW xxx：返回单行单列
	if strings.HasPrefix(norm, "show ") {
		name := strings.TrimSpace(strings.TrimPrefix(norm, "show "))
		val, ok := c.showParam(name)
		if !ok {
			val = ""
		}
		return [][]byte{
			encodeRowDescription([]field{{Name: name, TypeOID: 25, TypeLen: -1}}),
			encodeDataRow([][]byte{[]byte(val)}),
			encodeCommandComplete("SHOW"),
		}, true
	}

	return nil, false
}

func (c *conn) dbName() string { return "godb" }

func (c *conn) cannedText(col, val string) [][]byte {
	return [][]byte{
		encodeRowDescription([]field{{Name: col, TypeOID: 25, TypeLen: -1}}),
		encodeDataRow([][]byte{[]byte(val)}),
		encodeCommandComplete("SELECT 1"),
	}
}

func (c *conn) serverVersionLine() string {
	return fmt.Sprintf("PostgreSQL %s (godb %s) on x86_64-pc-windows, compiled by Go", pgServerVersion, godbVersion)
}

// showParam 回应 SHOW 语句。
func (c *conn) showParam(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSuffix(name, ";")) {
	case "server_version":
		return pgServerVersion, true
	case "server_encoding", "client_encoding":
		return "UTF8", true
	case "datestyle":
		return "ISO, MDY", true
	case "timezone":
		return "UTC", true
	case "standard_conforming_strings":
		return "on", true
	case "integer_datetimes":
		return "on", true
	case "search_path":
		return "public", true
	case "transaction_isolation":
		return "serializable", true
	case "application_name":
		return "", true
	}
	return "", false
}

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
