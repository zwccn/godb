package storage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"godb/catalog"
	"godb/types"
)

// encodeRow 把行编码成可 JSON 化的切片（Value 自带 MarshalJSON）。
func encodeRow(row types.Row) []interface{} {
	out := make([]interface{}, len(row))
	for i, v := range row {
		out[i] = v
	}
	return out
}

func encodeRows(rows []types.Row) [][]interface{} {
	out := make([][]interface{}, len(rows))
	for i, r := range rows {
		out[i] = encodeRow(r)
	}
	return out
}

// decodeRow 按表结构解码一行，返回 (行, 需要修复的单元格数)。
//
// 这里**刻意不做严格校验**：磁盘上的行可能和当前 catalog 的列数不一致——
// ALTER TABLE ADD/DROP COLUMN 是"先改 catalog.json、再写快照"的两步操作，
// 中间崩溃就会留下旧行宽的快照。以前这种不一致会让整个库打不开
// （"行宽不匹配"直接让 Open 失败），现在改成按当前结构补齐/截断：
//   - 少列（ADD COLUMN 崩在中间）→ 缺的补 NULL，语义正好等于"新列默认 NULL"
//   - 多列（DROP COLUMN 崩在中间）→ 多余的丢弃，语义正好等于"这一列已删除"
//   - 单值解码失败（ALTER TYPE 崩在中间）→ 该格置 NULL，而不是让库打不开
//
// 修复数量会累加到 Store.Repaired，启动时可以据此提示用户。
func decodeRow(t *catalog.Table, raw []interface{}) (types.Row, int) {
	n := len(t.Columns)
	row := make(types.Row, n)
	repaired := 0
	if len(raw) != n {
		if len(raw) > n {
			repaired = len(raw) - n
		} else {
			repaired = n - len(raw)
		}
	}
	for i := 0; i < n; i++ {
		if i >= len(raw) {
			row[i] = types.Null
			continue
		}
		val, err := decodeValue(raw[i], t.Columns[i].Type())
		if err != nil {
			row[i] = types.Null
			repaired++
			continue
		}
		row[i] = val
	}
	return row, repaired
}

// castRow 把一串值按当前表结构对齐：少则补 NULL、多则截断、单值类型不兼容则置 NULL。
// 返回 (行, 修复的单元格数)。
func castRow(t *catalog.Table, vals []types.Value) (types.Row, int) {
	n := len(t.Columns)
	row := make(types.Row, n)
	repaired := 0
	if len(vals) != n {
		if len(vals) > n {
			repaired = len(vals) - n
		} else {
			repaired = n - len(vals)
		}
	}
	for i := 0; i < n; i++ {
		if i >= len(vals) {
			row[i] = types.Null
			continue
		}
		v, err := types.Cast(vals[i], t.Columns[i].Type())
		if err != nil {
			row[i] = types.Null
			repaired++
			continue
		}
		row[i] = v
	}
	return row, repaired
}

func decodeValue(v interface{}, typ types.Type) (types.Value, error) {
	if v == nil {
		return types.Null, nil
	}
	switch typ.Kind {
	case types.KInt:
		switch x := v.(type) {
		case json.Number:
			return types.Int(mustInt(x)), nil
		case float64:
			return types.Int(int64(x)), nil
		case string:
			i, err := strconv.ParseInt(x, 10, 64)
			if err != nil {
				return types.Null, err
			}
			return types.Int(i), nil
		}
	case types.KFloat:
		switch x := v.(type) {
		case json.Number:
			f, err := x.Float64()
			if err != nil {
				return types.Null, err
			}
			return types.Float(f), nil
		case float64:
			return types.Float(x), nil
		}
	case types.KText:
		if s, ok := v.(string); ok {
			return types.Text(s), nil
		}
		return types.Text(fmt.Sprint(v)), nil
	case types.KBool:
		switch x := v.(type) {
		case bool:
			return types.Bool(x), nil
		case string:
			return types.Bool(x == "true"), nil
		}
	case types.KTime:
		switch x := v.(type) {
		case string:
			t, err := time.Parse(time.RFC3339Nano, x)
			if err != nil {
				return types.Null, err
			}
			return types.Time(t.Local()), nil
		case map[string]interface{}:
			if s, ok := x["$ts"].(string); ok {
				t, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return types.Null, err
				}
				return types.Time(t.Local()), nil
			}
		}
	}
	return types.Null, fmt.Errorf("无法解码值 %v", v)
}

func mustInt(n json.Number) int64 {
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}
