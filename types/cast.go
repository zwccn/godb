package types

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// hasPrefixFold 是不分配内存的前缀比较（strings.HasPrefix(strings.ToLower(...))
// 每调一次就分配一个字符串；解码路径上每个值都要过一遍）。
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}

// Cast 把值显式/隐式转换为目标类型。NULL 始终保持 NULL。
func Cast(v Value, t Type) (Value, error) {
	if v.K == KNull {
		return Null, nil
	}
	// 数组：内部统一用 PG 的数组文本（'{1,2,3}'）表示，不做逐元素转换。
	// 必须放在下面的前缀匹配之前 —— "numeric[]" 也以 "numeric" 开头，
	// 不先返回就会被当成标量 numeric，拿整个 "{1,2}" 去解析而报错。
	// 注意这里**不要** ToLower：方括号不是字母，大小写无关的只有类型的字面名，
	// 而 ToLower 每行每列都会分配一个新字符串 —— 换入的行走一次 Cast，
	// 20 万行就是上百万次分配（这里是解码路径最大的单点开销）。
	if strings.HasSuffix(t.Name, "[]") {
		if v.K == KText {
			return v, nil
		}
		return Null, fmt.Errorf("cannot cast type %s to %s", v.K.String(), t.Name)
	}
	// interval：月数进 z、天/时/分/秒进 I（纳秒），timestamp 加减用日历月
	if strings.EqualFold(t.Name, "interval") {
		switch v.K {
		case KInterval:
			return v, nil
		case KInt:
			return Interval(0, v.I), nil
		case KFloat:
			return Interval(0, int64(math.Round(v.Float()))), nil
		case KText:
			months, ns, err := ParseIntervalParts(v.S)
			if err != nil {
				return Null, err
			}
			return Interval(months, ns), nil
		}
	}
	// NUMERIC / DECIMAL：精确十进制（KDecimal）
	if hasPrefixFold(t.Name, "numeric") || hasPrefixFold(t.Name, "decimal") {
		return castDecimal(v, t)
	}
	switch t.Kind {
	case KInt:
		switch v.K {
		case KInt:
			return v, nil
		case KFloat:
			return Int(int64(math.Round(v.Float()))), nil
		case KDecimal:
			f, err := strconv.ParseFloat(v.S, 64)
			if err != nil {
				return Null, err
			}
			return Int(int64(math.Round(f))), nil
		case KBool:
			if v.Bool() {
				return Int(1), nil
			}
			return Int(0), nil
		case KText:
			s := strings.TrimSpace(v.S)
			i, err := strconv.ParseInt(s, 10, 64)
			if err == nil {
				return Int(i), nil
			}
			f, ferr := strconv.ParseFloat(s, 64)
			if ferr != nil {
				return Null, fmt.Errorf("invalid input syntax for type integer: %q", v.S)
			}
			return Int(int64(math.Round(f))), nil
		}
	case KFloat:
		switch v.K {
		case KFloat:
			return v, nil
		case KInt:
			return Float(float64(v.I)), nil
		case KDecimal:
			return Float(v.Float()), nil
		case KText:
			f, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64)
			if err != nil {
				return Null, fmt.Errorf("invalid input syntax for type double precision: %q", v.S)
			}
			return Float(f), nil
		case KBool:
			if v.Bool() {
				return Float(1), nil
			}
			return Float(0), nil
		}
	case KText:
		s := v.TextForm()
		// 字符数用 utf8.RuneCountInString 数，不要 []rune(s)：
		// 后者按字符数分配一个 rune 切片，varchar(n) 列每行每值都要分一次
		// （换入的行必然过一次 Cast，20 万行就是几十万次无谓分配）。
		if t.Len > 0 && utf8.RuneCountInString(s) > t.Len {
			return Null, fmt.Errorf("value too long for type %s", t.String())
		}
		return Text(s), nil
	case KBool:
		switch v.K {
		case KBool:
			return v, nil
		case KInt:
			return Bool(v.I != 0), nil
		case KText:
			switch strings.ToLower(strings.TrimSpace(v.S)) {
			case "t", "true", "yes", "y", "on", "1":
				return Bool(true), nil
			case "f", "false", "no", "n", "off", "0":
				return Bool(false), nil
			}
			return Null, fmt.Errorf("invalid input syntax for type boolean: %q", v.S)
		}
	case KTime:
		switch v.K {
		case KTime:
			return v, nil
		case KText:
			tm, err := ParseTime(v.S)
			if err != nil {
				return Null, fmt.Errorf("invalid input syntax for type timestamp: %q", v.S)
			}
			return Time(tm), nil
		}
	}
	return Null, fmt.Errorf("cannot cast type %s to %s", v.K, t.String())
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	time.RFC3339Nano,
	time.RFC3339,
}

// ParseTime 解析常见的时间字面量（本地时区）。
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	switch low {
	case "now":
		return time.Now(), nil
	case "today":
		n := time.Now()
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()), nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format")
}

// castDecimal 转成精确十进制。int 直接格式化，float 用最短往返再规范，
// 文本按 numeric 解析。t.Scale > 0 时才按标度四舍五入。
func castDecimal(v Value, t Type) (Value, error) {
	scale := t.Scale
	if scale < 0 {
		scale = 0
	}
	// 快速路径：文本已是目标标度的小数，跳过 big.Rat 舍入（批量写每行都要走这里）。
	tryFast := func(s string) (Value, bool) {
		if scale == 0 {
			return Decimal(s), true
		}
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			return Null, false // 整数字符串，需要补小数位
		}
		frac := len(s) - dot - 1
		if frac == scale {
			return Decimal(s), true
		}
		return Null, false
	}
	toDecimal := func(s string) (Value, error) {
		s, err := ParseDecimal(s)
		if err != nil {
			return Null, err
		}
		if v, ok := tryFast(s); ok {
			return v, nil
		}
		if scale == 0 {
			return Decimal(s), nil
		}
		// 放大 10^scale 后四舍五入到整数，再缩小
		r := DecimalRat(s)
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
		scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow))
		num, den := scaled.Num(), scaled.Denom()
		q, m := new(big.Int).QuoRem(num, den, new(big.Int))
		twice := new(big.Int).Abs(m)
		twice.Lsh(twice, 1)
		if twice.Cmp(den) >= 0 {
			if num.Sign() >= 0 {
				q.Add(q, big.NewInt(1))
			} else {
				q.Sub(q, big.NewInt(1))
			}
		}
		out := new(big.Rat).SetFrac(q, pow)
		return Decimal(RatDecimal(out)), nil
	}
	switch v.K {
	case KDecimal:
		if v2, ok := tryFast(v.S); ok {
			return v2, nil
		}
		return toDecimal(v.S)
	case KInt:
		return Decimal(strconv.FormatInt(v.I, 10)), nil
	case KFloat:
		return toDecimal(strconv.FormatFloat(v.Float(), 'f', -1, 64))
	case KText:
		s, err := ParseDecimal(v.S)
		if err != nil {
			return Null, fmt.Errorf("invalid input syntax for type numeric: %q", v.S)
		}
		if v2, ok := tryFast(s); ok {
			return v2, nil
		}
		if scale == 0 {
			return Decimal(s), nil
		}
		return toDecimal(s)
	}
	return Null, fmt.Errorf("cannot cast type %s to %s", v.K, t.String())
}

// ParseInterval 解析 PG 风格的 interval 文本。
// 返回 (months, nanos)：月/年进 months（timestamp 加减用日历月），其余进 nanos。
//
// 支持：`1 day` / `2 days` / `1 d` / `01:30:00` / `1 day 02:03:04` /
// `3 months` / `1 year` / `2 weeks` / `1.5 seconds` / 组合。
func ParseIntervalParts(s string) (int64, int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
	}
	var months, nanos int64
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		// HH:MM:SS 时间段（可能带前导天数，如 "1 02:03:04"）
		if j := parseTimeOfDay(s[i:]); j > 0 {
			ns, n, err := parseTimeOfDayVal(s[i : i+j])
			if err != nil {
				return 0, 0, err
			}
			nanos += ns
			i += n
			continue
		}
		// 数字 + 单位
		j := i
		for j < len(s) && (s[j] == '+' || s[j] == '-' || s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		if j == i {
			return 0, 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
		}
		numStr := s[i:j]
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
		}
		for j < len(s) && s[j] == ' ' {
			j++
		}
		k := j
		for k < len(s) && (s[k] >= 'a' && s[k] <= 'z') {
			k++
		}
		unit := s[j:k]
		if unit == "" {
			return 0, 0, fmt.Errorf("missing unit in interval: %q", s)
		}
		switch unit {
		case "usec", "us", "microsecond", "microseconds":
			nanos += int64(f * 1_000)
		case "msec", "ms", "millisecond", "milliseconds":
			nanos += int64(f * 1_000_000)
		case "s", "sec", "secs", "second", "seconds":
			nanos += int64(f * 1_000_000_000)
		case "m", "min", "mins", "minute", "minutes":
			nanos += int64(f * 60 * 1_000_000_000)
		case "h", "hr", "hrs", "hour", "hours":
			nanos += int64(f * 3600 * 1_000_000_000)
		case "d", "day", "days":
			nanos += int64(f * 24 * 3600 * 1_000_000_000)
		case "w", "week", "weeks":
			nanos += int64(f * 7 * 24 * 3600 * 1_000_000_000)
		case "mon", "mons", "month", "months":
			months += int64(f)
		case "y", "year", "years":
			months += int64(f * 12)
		case "decade", "decades":
			months += int64(f * 120)
		case "century", "centuries":
			months += int64(f * 1200)
		default:
			return 0, 0, fmt.Errorf("invalid interval unit %q", unit)
		}
		i = k
	}
	return months, nanos, nil
}

// ParseInterval 解析 interval 文本为纳秒（月/年按 30/365 天近似，仅用于 interval±interval 的显示）。
// 时间加减请用 ParseIntervalParts + AddMonthsNanos。
func ParseInterval(s string) (int64, error) {
	months, nanos, err := ParseIntervalParts(s)
	if err != nil {
		return 0, err
	}
	// 近似：1 月 = 30 天，1 年已在 ParseIntervalParts 里算成月
	nanos += months * 30 * 24 * 3600 * 1_000_000_000
	return nanos, nil
}

// AddMonthsNanos 按日历月/日把 interval 加到时间上：先加月（超出月末则夹到月末），
// 再加纳秒。`2024-01-31 + 1 month = 2024-02-29`（PG 语义，不是 3 月 2 日）。
func AddMonthsNanos(t time.Time, months, nanos int64) time.Time {
	if months != 0 {
		t = addMonthsClamp(t, months)
	}
	if nanos != 0 {
		t = t.Add(time.Duration(nanos))
	}
	return t
}

// addMonthsClamp 加月并把日期夹到目标月的最后一天。
func addMonthsClamp(t time.Time, months int64) time.Time {
	year, month, day := t.Date()
	total := int(month) - 1 + int(months)
	y := year + total/12
	m := total%12 + 1
	if m <= 0 {
		m += 12
		y--
	}
	// 目标月的天数（取下个月第 0 天 = 本月最后一天）
	lastDay := time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(y, time.Month(m), day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// parseTimeOfDayVal 解析 `HH:MM[:SS[.fff]]`，返回纳秒与吃掉的字符数。
func parseTimeOfDayVal(s string) (int64, int, error) {
	j := parseTimeOfDay(s)
	if j <= 0 {
		return 0, 0, fmt.Errorf("invalid time of day in interval: %q", s)
	}
	tok := s[:j]
	parts := strings.Split(tok, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, fmt.Errorf("invalid time of day in interval: %q", tok)
	}
	h, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time of day in interval: %q", tok)
	}
	m, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time of day in interval: %q", tok)
	}
	sec := 0.0
	if len(parts) == 3 {
		if sec, err = strconv.ParseFloat(parts[2], 64); err != nil {
			return 0, 0, fmt.Errorf("invalid time of day in interval: %q", tok)
		}
	}
	ns := int64((h*3600 + m*60 + sec) * 1e9)
	return ns, j, nil
}

// parseTimeOfDay 判断 s 是否以 HH:MM[...] 开头，返回匹配长度（0 表示不是）。
func parseTimeOfDay(s string) int {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 || i > 4 {
		return 0
	}
	if i >= len(s) || s[i] != ':' {
		return 0
	}
	i++ // :
	j := i
	for j < len(s) && (s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j == i || j > i+2 {
		return 0
	}
	if j < len(s) && s[j] == ':' {
		j++
		k := j
		for k < len(s) && (s[k] >= '0' && s[k] <= '9') {
			k++
		}
		if k == j || k > j+2 {
			return 0
		}
		if k < len(s) && s[k] == '.' {
			k++
			m := k
			for m < len(s) && (s[m] >= '0' && s[m] <= '9') {
				m++
			}
			if m == k {
				return 0
			}
			k = m
		}
		return k
	}
	return j
}
