package pgwire

import (
	"fmt"
	"strings"
)

// compat 处理 PG 客户端连上来后自动发出的一批“探针语句”。
// 我们不是真的 PostgreSQL，没有 pg_catalog / information_schema，
// 这些语句如果不拦，客户端会在连接测试阶段就直接报错。
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

	// 版本号探针
	if strings.HasPrefix(norm, "select version()") || strings.HasPrefix(norm, "select pg_catalog.version()") ||
		strings.HasPrefix(norm, "select pg_catalog.version() as version") {
		return c.cannedText("version", c.serverVersionLine()), true
	}

	if strings.HasPrefix(norm, "select current_schema()") {
		return c.cannedText("current_schema", "public"), true
	}
	if strings.HasPrefix(norm, "select current_database()") {
		return c.cannedText("current_database", c.dbName()), true
	}
	if norm == "select current_user" || norm == "select user" || strings.HasPrefix(norm, "select current_user(") {
		return c.cannedText("current_user", c.user), true
	}
	if strings.HasPrefix(norm, "select pg_backend_pid()") {
		return c.cannedText("pg_backend_pid", "1"), true
	}
	if strings.HasPrefix(norm, "select txid_current()") {
		return c.cannedText("txid_current", "1"), true
	}

	// 客户端（Navicat / JDBC / psql 的部分路径）会用它来改 search_path 等
	if strings.HasPrefix(norm, "select pg_catalog.set_config") {
		return c.cannedText("set_config", ""), true
	}
	if strings.HasPrefix(norm, "select current_setting(") {
		return c.cannedText("current_setting", ""), true
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
