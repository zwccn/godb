package pgwire

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"godb/types"
)

// 二进制结果集（Bind 里 resultFormats = 1）。
//
// 为什么要做：Npgsql 及其上的 EFCore / Dapper 在扩展协议里**默认请求二进制结果**，
// 以前我们一律按文本回，客户端按二进制去解就全错位了 —— 表现为
// “Unknown message code: 76”（字节错位后 payload 里的字符被当成消息类型读）、
// 类型加载失败（pg_type 的 oid 列 4 字节被当成 2 字节文本）、
// GetInt32/GetDecimal 全部报 “Can't cast database type .<unknown>”。
//
// 注意：这里的输入是 exec.Result 里的 interface{}（int64 / float64 / bool / string），
// 时间在那一层已经格式化成 "2006-01-02 15:04:05.999999" 了，所以按字符串再解析回来。

// encodeBinaryValue 按 PG 二进制格式编码一个结果值。
// oid 是 RowDescription 里报给客户端的类型 OID，必须和 Describe 用的同一套映射。
func encodeBinaryValue(v interface{}, oid int32) ([]byte, error) {
	if v == nil {
		return nil, nil // NULL：DataRow 里写 -1 长度，不是空字节
	}
	switch oid {
	case types.OIDBool:
		b, ok := boolOf(v)
		if !ok {
			return nil, fmt.Errorf("cannot encode %T as boolean", v)
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil

	case types.OIDInt2, types.OIDInt4, types.OIDInt8:
		n, ok := int64Of(v)
		if !ok {
			return nil, fmt.Errorf("cannot encode %T as integer", v)
		}
		switch oid {
		case types.OIDInt2:
			return binary.BigEndian.AppendUint16(nil, uint16(int16(n))), nil
		case types.OIDInt4:
			return binary.BigEndian.AppendUint32(nil, uint32(int32(n))), nil
		}
		return binary.BigEndian.AppendUint64(nil, uint64(n)), nil

	// 各种「整数当 OID 用」的类型：长度都是 4 字节无符号。
	case types.OIDOid, types.OIDRegproc, types.OIDRegclass, types.OIDRegtype,
		types.OIDXid, types.OIDCid, types.OIDTid:
		if n, ok := int64Of(v); ok {
			return binary.BigEndian.AppendUint32(nil, uint32(n)), nil
		}
		// regproc 这类列我们内部存的是函数名（文本格式下本来也该显示名字）。
		// 二进制格式下 PG 只发 OID，我们手上没有「名字 → OID」的反查表，
		// 与其让整条查询报错（客户端会认为连接不可用），不如发 0。
		// 会 SELECT typinput/typsubscript 这类列并要二进制的客户端极少。
		return make([]byte, 4), nil

	case types.OIDFloat4:
		f, ok := float64Of(v)
		if !ok {
			return nil, fmt.Errorf("cannot encode %T as real", v)
		}
		return binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(f))), nil

	case types.OIDFloat8:
		f, ok := float64Of(v)
		if !ok {
			return nil, fmt.Errorf("cannot encode %T as double precision", v)
		}
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(f)), nil

	case types.OIDNumeric:
		return numericBinaryOf(v)

	case types.OIDText, types.OIDVarchar, types.OIDBpchar, types.OIDChar,
		types.OIDName, types.OIDJson, types.OIDXml, types.OIDUnknown:
		// 这些类型的二进制编码就是 UTF-8 字节本身
		return []byte(textOf(v)), nil

	case types.OIDBytea:
		return byteaBinaryOf(v)

	case types.OIDDate:
		t, ok := timeOf(textOf(v))
		if !ok {
			return nil, fmt.Errorf("cannot encode %q as date", textOf(v))
		}
		days := int32(civilUTC(t).Sub(pgEpoch) / (24 * time.Hour))
		return binary.BigEndian.AppendUint32(nil, uint32(days)), nil

	case types.OIDTime:
		t, ok := timeOf(textOf(v))
		if !ok {
			return nil, fmt.Errorf("cannot encode %q as time", textOf(v))
		}
		us := int64(t.Hour())*3600e6 + int64(t.Minute())*60e6 +
			int64(t.Second())*1e6 + int64(t.Nanosecond())/1e3
		return binary.BigEndian.AppendUint64(nil, uint64(us)), nil

	case types.OIDTimetz:
		t, ok := timeOf(textOf(v))
		if !ok {
			return nil, fmt.Errorf("cannot encode %q as timetz", textOf(v))
		}
		us := int64(t.Hour())*3600e6 + int64(t.Minute())*60e6 +
			int64(t.Second())*1e6 + int64(t.Nanosecond())/1e3
		out := binary.BigEndian.AppendUint64(nil, uint64(us))
		_, off := t.Zone()
		return binary.BigEndian.AppendUint32(out, uint32(-int32(off))), nil

	case types.OIDTimestamp, types.OIDTimestamptz:
		t, ok := timeOf(textOf(v))
		if !ok {
			return nil, fmt.Errorf("cannot encode %q as timestamp", textOf(v))
		}
		us := microsFromEpoch(t.UTC())
		if oid == types.OIDTimestamp {
			// timestamp without time zone：把墙上时间当成 UTC 再数微秒
			us = microsFromEpoch(civilUTC(t))
		}
		return binary.BigEndian.AppendUint64(nil, uint64(us)), nil

	case types.OIDUuid:
		s := strings.ReplaceAll(strings.TrimSpace(textOf(v)), "-", "")
		raw, err := hex.DecodeString(s)
		if err != nil || len(raw) != 16 {
			return nil, fmt.Errorf("cannot encode %q as uuid", textOf(v))
		}
		return raw, nil

	case types.OIDJsonb:
		// 首字节是版本号（目前只有 1），其余是 JSON 文本
		return append([]byte{1}, textOf(v)...), nil
	}
	return nil, fmt.Errorf("binary result of type OID %d is not supported", oid)
}

// microsSincePG 返回自 2000-01-01 UTC 起的微秒数。
func microsFromEpoch(t time.Time) int64 {
	return int64(t.Sub(pgEpoch) / time.Microsecond)
}

// civilUTC 取「墙上时间」并标记成 UTC：PG 的 timestamp/date 不带时区，
// 内部存的就是这串年月日时分秒，不能做时区换算。
func civilUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// timeOf 解析结果层给过来的时间文本。带不带小数秒、带不带时区都认。
func timeOf(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// numericBinaryOf 生成 PG 的 numeric 二进制格式：
//
//	int16 ndigits | int16 weight | int16 sign | int16 dscale | ndigits × int16
//
// 每段是 0–9999 的四位十进制块。内部值只有 float64，所以先按最短往返表示
// 转成十进制字符串（'f' 格式，绝不出现指数），再切块 —— 不用 float 做小数
// 累积，免得 0.1 之类出现 0.0999999 的尾巴。
func numericBinaryOf(v interface{}) ([]byte, error) {
	var s string
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) {
			return numericSpecial(0xC000), nil
		}
		if math.IsInf(x, 1) {
			return numericSpecial(0xD000), nil
		}
		if math.IsInf(x, -1) {
			return numericSpecial(0xF000), nil
		}
		s = strconv.FormatFloat(math.Abs(x), 'f', -1, 64)
		if x < 0 {
			s = "-" + s
		}
	case int64:
		s = strconv.FormatInt(x, 10)
	case string:
		s = strings.TrimSpace(x)
	default:
		return nil, fmt.Errorf("cannot encode %T as numeric", v)
	}
	if strings.EqualFold(s, "nan") {
		return numericSpecial(0xC000), nil
	}
	if strings.EqualFold(s, "infinity") || strings.EqualFold(s, "+infinity") {
		return numericSpecial(0xD000), nil
	}
	if strings.EqualFold(s, "-infinity") {
		return numericSpecial(0xF000), nil
	}

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}

	// 整数部分从右往左每 4 位一段，小数部分从左往右每 4 位一段
	var groups []int
	intPadded := intPart
	if pad := (4 - len(intPart)%4) % 4; pad > 0 {
		intPadded = strings.Repeat("0", pad) + intPart
	}
	for i := 0; i < len(intPadded); i += 4 {
		n, err := strconv.Atoi(intPadded[i : i+4])
		if err != nil {
			return nil, fmt.Errorf("invalid numeric %q", s)
		}
		groups = append(groups, n)
	}
	weight := len(intPadded)/4 - 1

	if fracPart != "" {
		fracPadded := fracPart
		if pad := (4 - len(fracPart)%4) % 4; pad > 0 {
			fracPadded += strings.Repeat("0", pad)
		}
		for i := 0; i < len(fracPadded); i += 4 {
			n, err := strconv.Atoi(fracPadded[i : i+4])
			if err != nil {
				return nil, fmt.Errorf("invalid numeric %q", s)
			}
			groups = append(groups, n)
		}
	}

	// 去掉高位、低位的 0 段：weight 指向第一段，去掉几段 weight 就减几
	for len(groups) > 0 && groups[0] == 0 {
		groups = groups[1:]
		weight--
	}
	for len(groups) > 0 && groups[len(groups)-1] == 0 {
		groups = groups[:len(groups)-1]
	}
	if len(groups) == 0 {
		weight = 0
	}

	sign := uint16(0)
	if neg && len(groups) > 0 {
		sign = 0x4000
	}
	out := binary.BigEndian.AppendUint16(nil, uint16(len(groups)))
	out = binary.BigEndian.AppendUint16(out, uint16(int16(weight)))
	out = binary.BigEndian.AppendUint16(out, sign)
	out = binary.BigEndian.AppendUint16(out, uint16(len(fracPart)))
	for _, g := range groups {
		out = binary.BigEndian.AppendUint16(out, uint16(g))
	}
	return out, nil
}

// numericSpecial 生成 NaN / ±Infinity 的 numeric 二进制（只有 8 字节头）。
func numericSpecial(sign uint16) []byte {
	out := binary.BigEndian.AppendUint16(nil, 0) // ndigits
	out = binary.BigEndian.AppendUint16(out, 0)  // weight
	out = binary.BigEndian.AppendUint16(out, sign)
	return binary.BigEndian.AppendUint16(out, 0) // dscale
}

func byteaBinaryOf(v interface{}) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		// 文本表示是 \x 开头的十六进制（见 params.go 里 bytea 的反向处理）
		if strings.HasPrefix(x, `\x`) {
			raw, err := hex.DecodeString(x[2:])
			if err != nil {
				return nil, fmt.Errorf("invalid bytea %q", x)
			}
			return raw, nil
		}
		return []byte(x), nil
	}
	return nil, fmt.Errorf("cannot encode %T as bytea", v)
}

// ---------- 取值辅助 ----------

func boolOf(v interface{}) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int64:
		return x != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "t", "true", "1", "y", "yes", "on":
			return true, true
		case "f", "false", "0", "n", "no", "off":
			return false, true
		}
	}
	return false, false
}

func int64Of(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func float64Of(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func textOf(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "t"
		}
		return "f"
	}
	return fmt.Sprint(v)
}
