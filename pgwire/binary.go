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
		// 越界直接报错，别截断：声明成 int4 却装不下时，截断会把 9223372036854775807
		// 静默变成 -1，客户端拿到的是一本正经的错数字。
		switch oid {
		case types.OIDInt2:
			if n < math.MinInt16 || n > math.MaxInt16 {
				return nil, fmt.Errorf("smallint out of range")
			}
			return binary.BigEndian.AppendUint16(nil, uint16(int16(n))), nil
		case types.OIDInt4:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return nil, fmt.Errorf("integer out of range")
			}
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

	case types.OIDInterval:
		return intervalBinaryOf(v)

	// 数组：元素类型由数组 OID 决定，编码见 arrayBinaryOf。
	case 1000, 1005, 1007, 1009, 1015, 1016, 1021, 1022,
		1028, 1115, 1182, 1183, 1185, 1187, 1231, 2951:
		elem, ok := arrayElemOID(oid)
		if !ok {
			return nil, fmt.Errorf("binary result of type OID %d is not supported", oid)
		}
		return arrayBinaryOf(v, elem)
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

// ---------- interval ----------

// intervalBinaryOf 把 interval 的 PG 文本形式（"1 year 2 mons 3 days 04:05:06.5"）
// 转成二进制：int64 微秒 + int32 天 + int32 月。
// 三段分开是 PG 的约定；我们内部存的是「月数 + 纳秒」，天和时分秒都在纳秒里，
// 这里按文本原样拆开，保证客户端读回去和写进来一致。
func intervalBinaryOf(v interface{}) ([]byte, error) {
	months, days, micros, err := parseIntervalText(textOf(v))
	if err != nil {
		return nil, err
	}
	out := binary.BigEndian.AppendUint64(nil, uint64(micros))
	out = binary.BigEndian.AppendUint32(out, uint32(int32(days)))
	out = binary.BigEndian.AppendUint32(out, uint32(int32(months)))
	return out, nil
}

// parseIntervalText 解析 intervalText 生成的文本。单位支持 year/mon/day/hour/min/sec
// 的单复数形式，以及 HH:MM:SS[.ffffff] 时钟段；整体可以带一个前导负号。
func parseIntervalText(s string) (months, days, micros int64, err error) {
	orig := strings.TrimSpace(s)
	s = orig
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = strings.TrimSpace(s[1:])
	}
	sign := func(n int64) int64 {
		if neg {
			return -n
		}
		return n
	}
	fields := strings.Fields(s)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.ContainsRune(f, ':') {
			h, m, sec, cerr := parseClock(f)
			if cerr != nil {
				return 0, 0, 0, fmt.Errorf("cannot encode %q as interval", orig)
			}
			micros += sign(h*3600e6 + m*60e6 + int64(sec*1e6))
			continue
		}
		n, perr := strconv.ParseInt(f, 10, 64)
		if perr != nil || i+1 >= len(fields) {
			return 0, 0, 0, fmt.Errorf("cannot encode %q as interval", orig)
		}
		unit := strings.ToLower(fields[i+1])
		i++
		switch strings.TrimSuffix(unit, "s") {
		case "year", "yr":
			months += sign(n * 12)
		case "mon", "month":
			months += sign(n)
		case "day":
			days += sign(n)
		case "hour", "hr":
			micros += sign(n * 3600e6)
		case "min", "minute":
			micros += sign(n * 60e6)
		case "sec", "second":
			micros += sign(int64(float64(n) * 1e6))
		default:
			return 0, 0, 0, fmt.Errorf("cannot encode %q as interval", orig)
		}
	}
	return months, days, micros, nil
}

func parseClock(f string) (h, m int64, sec float64, err error) {
	parts := strings.Split(f, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("bad clock %q", f)
	}
	if h, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if m, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if sec, err = strconv.ParseFloat(parts[2], 64); err != nil {
		return 0, 0, 0, err
	}
	return h, m, sec, nil
}

// ---------- 数组 ----------

// arrayBinaryOf 生成 PG 数组二进制格式：
//
//	int32 ndim | int32 hasnull | int32 elemOID
//	ndim × (int32 dimlen | int32 lowerbound)
//	每个元素：int32 len + 内容（NULL 写 -1）
//
// 只支持一维：我们的数组值就是一段文本（'{1,2,3}'），没有多维的构造路径。
func arrayBinaryOf(v interface{}, elemOID int32) ([]byte, error) {
	elems, nulls, err := parseArrayLiteral(textOf(v))
	if err != nil {
		return nil, err
	}
	hasNull := 0
	for _, n := range nulls {
		if n {
			hasNull = 1
			break
		}
	}
	ndim := 0
	if len(elems) > 0 {
		ndim = 1
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(ndim))
	out = binary.BigEndian.AppendUint32(out, uint32(hasNull))
	out = binary.BigEndian.AppendUint32(out, uint32(elemOID))
	if ndim == 0 {
		return out, nil
	}
	out = binary.BigEndian.AppendUint32(out, uint32(len(elems)))
	out = binary.BigEndian.AppendUint32(out, 1) // 下标从 1 开始
	for i, e := range elems {
		if nulls[i] {
			out = binary.BigEndian.AppendUint32(out, 0xFFFFFFFF)
			continue
		}
		b, err := encodeBinaryValue(e, elemOID)
		if err != nil {
			return nil, err
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(b)))
		out = append(out, b...)
	}
	return out, nil
}

// parseArrayLiteral 把 PG 数组文本（{a,b,"c,d",NULL}）拆成元素。
// 处理双引号包裹、反斜杠转义、元素为 NULL（未加引号时）三种情况；
// 多维数组直接报错，不猜。
func parseArrayLiteral(s string) ([]string, []bool, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' {
		return nil, nil, fmt.Errorf("cannot encode %q as array", s)
	}
	body := t[1 : len(t)-1]
	if strings.TrimSpace(body) == "" {
		return nil, nil, nil
	}
	var (
		elems []string
		nulls []bool
		cur   strings.Builder
		inQ   bool
		esc   bool
		wasQ  bool
	)
	flush := func() error {
		raw := cur.String()
		cur.Reset()
		null := !wasQ && strings.EqualFold(strings.TrimSpace(raw), "NULL")
		if !wasQ {
			raw = strings.TrimSpace(raw)
		}
		if raw == "" && !null && !wasQ {
			return fmt.Errorf("cannot encode %q as array", s)
		}
		elems = append(elems, raw)
		nulls = append(nulls, null)
		wasQ = false
		return nil
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case esc:
			cur.WriteByte(c)
			esc = false
		case c == '\\':
			esc = true
		case c == '"':
			inQ = !inQ
			wasQ = true
		case c == '{' && !inQ:
			return nil, nil, fmt.Errorf("nested arrays are not supported")
		case c == ',' && !inQ:
			if err := flush(); err != nil {
				return nil, nil, err
			}
		default:
			cur.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return elems, nulls, nil
}
