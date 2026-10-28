package types

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Cast 把值显式/隐式转换为目标类型。NULL 始终保持 NULL。
func Cast(v Value, t Type) (Value, error) {
	if v.K == KNull {
		return Null, nil
	}
	// interval 以纳秒存进 KInt，文本要按「1 day 02:30:00」这类写法解析
	if strings.EqualFold(t.Name, "interval") {
		switch v.K {
		case KInt:
			return v, nil
		case KFloat:
			return Int(int64(math.Round(v.Float()))), nil
		case KText:
			ns, err := ParseInterval(v.S)
			if err != nil {
				return Null, err
			}
			return Int(ns), nil
		}
	}
	switch t.Kind {
	case KInt:
		switch v.K {
		case KInt:
			return v, nil
		case KFloat:
			return Int(int64(math.Round(v.Float()))), nil
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
		if t.Len > 0 && len([]rune(s)) > t.Len {
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

// ParseInterval 解析 PG 风格的 interval 文本，返回纳秒数。
//
// 支持：`1 day` / `2 days` / `1 d` / `01:30:00` / `1 day 02:03:04` /
// `3 months` / `1 year` / `2 weeks` / `1.5 seconds` / 组合。
// month 按 30 天、year 按 365 天近似（教学库够用，精确月长要配 timestamp 逐次加）。
func ParseInterval(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
	}
	var total int64
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
				return 0, err
			}
			total += ns
			i += n
			continue
		}
		// 数字 + 单位
		j := i
		for j < len(s) && (s[j] == '+' || s[j] == '-' || s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
		}
		numStr := s[i:j]
		// 科学计数法（PG 不支持，但 ParseFloat 能吃）；这里只接受普通小数
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid input syntax for type interval: %q", s)
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
			return 0, fmt.Errorf("missing unit in interval: %q", s)
		}
		var mult int64
		switch unit {
		case "usec", "us", "microsecond", "microseconds":
			mult = 1_000
		case "msec", "ms", "millisecond", "milliseconds":
			mult = 1_000_000
		case "s", "sec", "secs", "second", "seconds":
			mult = 1_000_000_000
		case "m", "min", "mins", "minute", "minutes":
			mult = 60 * 1_000_000_000
		case "h", "hr", "hrs", "hour", "hours":
			mult = 3600 * 1_000_000_000
		case "d", "day", "days":
			mult = 24 * 3600 * 1_000_000_000
		case "w", "week", "weeks":
			mult = 7 * 24 * 3600 * 1_000_000_000
		case "mon", "mons", "month", "months":
			mult = 30 * 24 * 3600 * 1_000_000_000
		case "y", "year", "years":
			mult = 365 * 24 * 3600 * 1_000_000_000
		case "decade", "decades":
			mult = 10 * 365 * 24 * 3600 * 1_000_000_000
		case "century", "centuries":
			mult = 100 * 365 * 24 * 3600 * 1_000_000_000
		default:
			return 0, fmt.Errorf("invalid interval unit %q", unit)
		}
		total += int64(f * float64(mult))
		i = k
	}
	return total, nil
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
