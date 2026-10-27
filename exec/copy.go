package exec

import (
	"fmt"
	"strings"

	"godb/sql"
	"godb/storage"
	"godb/types"
)

// COPY 用 PostgreSQL 的文本格式：字段以 tab 分隔、行以换行分隔、
// \N 表示 NULL，反斜杠用于转义。只支持 STDIN/STDOUT（见 sql.parseCopy）。

// CopyFromText 把 COPY 文本数据导入表里。
//
// 复用 execInsert：默认值、自增、NOT NULL、唯一性、CHECK、外键全都一致生效，
// 不用在这里重复一遍规则。
func (e *Engine) CopyFromText(tx *storage.Tx, c *sql.Copy, data []byte) (int64, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var rows [][]sql.Expr
	for _, line := range lines {
		if line == "" {
			continue
		}
		// psql 约定：单独一行 \. 表示数据结束
		if line == `\.` {
			break
		}
		fields := strings.Split(line, "\t")
		exprs := make([]sql.Expr, len(fields))
		for i, f := range fields {
			if f == `\N` {
				exprs[i] = &sql.Literal{Val: types.Null}
				continue
			}
			exprs[i] = &sql.Literal{Val: types.Text(unescapeCopy(f))}
		}
		rows = append(rows, exprs)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	res, err := e.execInsert(tx, &sql.Insert{Table: c.Table, Columns: c.Cols, Rows: rows})
	if err != nil {
		return 0, err
	}
	return int64(res.Affected), nil
}

// CopyOutText 把整表（或指定列）导出成 COPY 文本行。
func (e *Engine) CopyOutText(tx *storage.Tx, c *sql.Copy) ([]string, int64, error) {
	t, ok := e.Cat.Get(c.Table)
	if !ok {
		return nil, 0, fmt.Errorf("relation %q does not exist", c.Table)
	}
	idx := make([]int, 0, len(t.Columns))
	names := make([]string, 0, len(t.Columns))
	if len(c.Cols) == 0 {
		for i, col := range t.Columns {
			idx = append(idx, i)
			names = append(names, col.Name)
		}
	} else {
		for _, name := range c.Cols {
			ci, ok := t.ColumnIdx(name)
			if !ok {
				return nil, 0, fmt.Errorf("column %q of relation %q does not exist", name, t.Name)
			}
			idx = append(idx, ci)
			names = append(names, t.Columns[ci].Name)
		}
	}
	_ = names

	var out []string
	var n int64
	err := tx.Scan(t.Name, func(ref storage.Ref, row types.Row) error {
		parts := make([]string, len(idx))
		for i, ci := range idx {
			if ci >= len(row) || row[ci].IsNull() {
				parts[i] = `\N`
				continue
			}
			parts[i] = escapeCopy(row[ci].TextForm())
		}
		out = append(out, strings.Join(parts, "\t"))
		n++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, n, nil
}

// escapeCopy 转义 COPY 文本里的特殊字符。
func escapeCopy(s string) string {
	if !strings.ContainsAny(s, "\\\t\n\r") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unescapeCopy 还原 COPY 文本的转义。
func unescapeCopy(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
