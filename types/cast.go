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
