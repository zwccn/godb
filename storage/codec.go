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

// decodeRow 按表结构解码一行。
func decodeRow(t *catalog.Table, raw []interface{}) (types.Row, error) {
	if len(raw) != len(t.Columns) {
		return nil, fmt.Errorf("行宽不匹配（期望 %d 列，实际 %d 列）", len(t.Columns), len(raw))
	}
	row := make(types.Row, len(raw))
	for i, v := range raw {
		val, err := decodeValue(v, t.Columns[i].Type())
		if err != nil {
			return nil, fmt.Errorf("列 %s: %w", t.Columns[i].Name, err)
		}
		row[i] = val
	}
	return row, nil
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
