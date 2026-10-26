package pgwire

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// substituteParams 把我们解析器不认识的 $1/$2 占位符替换成字面量。
//
// 说明：godb 的 SQL 解析器不支持参数占位符，而 PG 客户端（Navicat、JDBC）几乎都用
// 扩展协议带参数查询。这里在协议层把参数值就地转成字面量再交给引擎 —— 值是我们自己
// 按类型转义拼的，不是用户拼接，能避免注入。
func substituteParams(query string, params [][]byte, oids []int32, formats []int16) (string, error) {
	if !strings.ContainsRune(query, '$') {
		if len(params) == 0 {
			return query, nil
		}
		return query, nil
	}

	var b strings.Builder
	runes := []rune(query)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// 单引号字符串：原样复制，'' 视为转义
		if r == '\'' {
			b.WriteRune(r)
			for i++; i < len(runes); i++ {
				b.WriteRune(runes[i])
				if runes[i] == '\'' {
					if i+1 < len(runes) && runes[i+1] == '\'' {
						i++
						b.WriteRune(runes[i])
						continue
					}
					break
				}
			}
			continue
		}
		// 双引号标识符
		if r == '"' {
			b.WriteRune(r)
			for i++; i < len(runes); i++ {
				b.WriteRune(runes[i])
				if runes[i] == '"' {
					break
				}
			}
			continue
		}
		// 占位符
		if r == '$' && i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
			j := i + 1
			for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(string(runes[i+1 : j]))
			if err != nil || n < 1 || n > len(params) {
				return "", fmt.Errorf("there is no parameter $%d", n)
			}
			oid := int32(0)
			if n-1 < len(oids) {
				oid = oids[n-1]
			}
			format := int16(0)
			switch {
			case len(formats) == 1:
				format = formats[0]
			case len(formats) > 1 && n-1 < len(formats):
				format = formats[n-1]
			}
			lit, err := paramLiteral(params[n-1], oid, format)
			if err != nil {
				return "", err
			}
			b.WriteString(lit)
			i = j - 1
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// paramLiteral 把一个参数值转成 SQL 字面量。
func paramLiteral(raw []byte, oid int32, format int16) (string, error) {
	if raw == nil {
		return "NULL", nil
	}
	if format == 1 { // 二进制格式
		return binaryLiteral(raw, oid)
	}
	text := string(raw)
	switch oid {
	case 20, 21, 23, 26, 700, 701, 1700: // 数值类：原样输出，但要确认确实是数字
		if _, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err != nil {
			return "", fmt.Errorf("invalid input syntax for numeric parameter: %q", text)
		}
		return strings.TrimSpace(text), nil
	case 16: // bool
		if text == "t" || text == "true" || text == "1" {
			return "TRUE", nil
		}
		return "FALSE", nil
	}
	return quoteLiteral(text), nil
}

func binaryLiteral(raw []byte, oid int32) (string, error) {
	switch oid {
	case 21:
		if len(raw) != 2 {
			return "", fmt.Errorf("bad int2 length %d", len(raw))
		}
		return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(raw))), 10), nil
	case 23:
		if len(raw) != 4 {
			return "", fmt.Errorf("bad int4 length %d", len(raw))
		}
		return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(raw))), 10), nil
	case 20:
		if len(raw) != 8 {
			return "", fmt.Errorf("bad int8 length %d", len(raw))
		}
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(raw)), 10), nil
	case 700:
		if len(raw) != 4 {
			return "", fmt.Errorf("bad float4 length %d", len(raw))
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), 'g', -1, 64), nil
	case 701:
		if len(raw) != 8 {
			return "", fmt.Errorf("bad float8 length %d", len(raw))
		}
		return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(raw)), 'g', -1, 64), nil
	case 16:
		if len(raw) != 1 {
			return "", fmt.Errorf("bad bool length %d", len(raw))
		}
		if raw[0] != 0 {
			return "TRUE", nil
		}
		return "FALSE", nil
	}
	return quoteLiteral(string(raw)), nil
}

// quoteLiteral 生成带单引号的标准字符串字面量，内部单引号翻倍。
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// replaceParamsWithNull 把 $1/$2 一律换成 NULL，仅用于 Describe 阶段推断列定义
// （不解参数值，也不执行）。这样带占位符的查询也能被解析出输出列。
func replaceParamsWithNull(query string) string {
	if !strings.ContainsRune(query, '$') {
		return query
	}
	var b strings.Builder
	runes := []rune(query)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\'' || r == '"' {
			quote := r
			b.WriteRune(r)
			for i++; i < len(runes); i++ {
				b.WriteRune(runes[i])
				if runes[i] == quote {
					if quote == '\'' && i+1 < len(runes) && runes[i+1] == '\'' {
						i++
						b.WriteRune(runes[i])
						continue
					}
					break
				}
			}
			continue
		}
		if r == '$' && i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
			for i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
				i++
			}
			b.WriteString("NULL")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
