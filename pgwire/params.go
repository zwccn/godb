package pgwire

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
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

	// 以下这些类型的二进制编码**就等于** UTF-8 文本，直接当字符串用
	case 18, 19, 25, 1042, 1043, 705, 114, 142: // char / name / text / bpchar / varchar / unknown / json / xml
		return quoteLiteral(string(raw)), nil

	case 1700: // numeric
		return numericBinaryText(raw)
	case 1114: // timestamp
		return timestampBinaryText(raw, false)
	case 1184: // timestamptz
		return timestampBinaryText(raw, true)
	case 1082: // date
		return dateBinaryText(raw)
	case 1083: // time
		return timeBinaryText(raw)
	case 2950: // uuid
		return uuidBinaryText(raw)
	case 17: // bytea：内存里就是原始字节，文本表示用 \x 十六进制
		return quoteLiteral(`\x` + hex.EncodeToString(raw)), nil
	case 3802: // jsonb：首字节是版本号（目前只有 1），其余是 JSON 文本
		if len(raw) < 1 {
			return "", fmt.Errorf("bad jsonb length 0")
		}
		if raw[0] != 1 {
			return "", fmt.Errorf("unsupported jsonb version %d", raw[0])
		}
		return quoteLiteral(string(raw[1:])), nil
	}

	// 兜底：宁可明确报错，也不要把二进制字节当文本拼进去 ——
	// 以前就是这么干的：错误信息里全是乱码，jsonb / uuid 这类还会「写成功但数据是错的」。
	return "", fmt.Errorf("binary parameter of type OID %d is not supported (use a text-format parameter instead)", oid)
}

// numericBinaryText 解 PG 的 numeric 二进制格式：
//
//	int16 ndigits | int16 weight | int16 sign | int16 dscale | ndigits × int16
//
// 每段是 0–9999 的四位十进制块，值 = Σ digits[i] × 10000^(weight-i)，
// 小数点后固定 dscale 位。sign 0x4000 = 负，0xC000 = NaN。
func numericBinaryText(raw []byte) (string, error) {
	if len(raw) < 8 {
		return "", fmt.Errorf("bad numeric length %d", len(raw))
	}
	ndigits := int(binary.BigEndian.Uint16(raw[0:2]))
	weight := int(int16(binary.BigEndian.Uint16(raw[2:4])))
	sign := binary.BigEndian.Uint16(raw[4:6])
	dscale := int(binary.BigEndian.Uint16(raw[6:8]))
	if len(raw) < 8+ndigits*2 {
		return "", fmt.Errorf("bad numeric payload: %d bytes for %d digits", len(raw), ndigits)
	}
	if sign == 0xC000 {
		return quoteLiteral("NaN"), nil
	}
	digits := make([]int, ndigits)
	for i := 0; i < ndigits; i++ {
		digits[i] = int(binary.BigEndian.Uint16(raw[8+i*2:]))
	}

	var b strings.Builder
	if sign == 0x4000 {
		b.WriteByte('-')
	}
	if weight < 0 {
		b.WriteByte('0')
	} else {
		for i := 0; i <= weight; i++ {
			d := 0
			if i < ndigits {
				d = digits[i]
			}
			if i == 0 {
				b.WriteString(strconv.Itoa(d))
			} else {
				fmt.Fprintf(&b, "%04d", d)
			}
		}
	}
	if dscale > 0 {
		b.WriteByte('.')
		start := weight + 1
		frac := make([]byte, 0, dscale+4)
		for i := 0; i < -start; i++ { // 权重太小，前面补整块的 0
			frac = append(frac, '0', '0', '0', '0')
		}
		for i := start; i < ndigits; i++ {
			if i < 0 {
				continue
			}
			frac = append(frac, fmt.Sprintf("%04d", digits[i])...)
		}
		s := string(frac)
		if len(s) < dscale {
			s += strings.Repeat("0", dscale-len(s))
		}
		b.WriteString(s[:dscale])
	}
	return b.String(), nil
}

// pgEpoch 是 PG 二进制时间类型的基准：2000-01-01 00:00:00（不是 Unix 纪元）。
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// microsSinceEpoch 把微秒数加到基准时间上。拆成秒+微秒再加，避免大数值溢出。
func microsSinceEpoch(us int64) time.Time {
	return pgEpoch.Add(time.Duration(us/1e6)*time.Second + time.Duration(us%1e6)*time.Microsecond)
}

func timestampBinaryText(raw []byte, withTZ bool) (string, error) {
	if len(raw) != 8 {
		return "", fmt.Errorf("bad timestamp length %d", len(raw))
	}
	t := microsSinceEpoch(int64(binary.BigEndian.Uint64(raw)))
	if withTZ {
		// 带时区的用 RFC3339，SQL 层的 ParseTime 认识这个写法
		return quoteLiteral(t.Format("2006-01-02T15:04:05.999999Z07:00")), nil
	}
	return quoteLiteral(t.Format("2006-01-02 15:04:05.999999")), nil
}

func dateBinaryText(raw []byte) (string, error) {
	if len(raw) != 4 {
		return "", fmt.Errorf("bad date length %d", len(raw))
	}
	days := int32(binary.BigEndian.Uint32(raw))
	return quoteLiteral(pgEpoch.AddDate(0, 0, int(days)).Format("2006-01-02")), nil
}

func timeBinaryText(raw []byte) (string, error) {
	if len(raw) != 8 {
		return "", fmt.Errorf("bad time length %d", len(raw))
	}
	us := int64(binary.BigEndian.Uint64(raw))
	t := pgEpoch.Add(time.Duration(us) * time.Microsecond)
	return quoteLiteral(t.Format("15:04:05.999999")), nil
}

func uuidBinaryText(raw []byte) (string, error) {
	if len(raw) != 16 {
		return "", fmt.Errorf("bad uuid length %d", len(raw))
	}
	h := hex.EncodeToString(raw)
	return quoteLiteral(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]), nil
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
