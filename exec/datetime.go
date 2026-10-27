package exec

import (
	"fmt"
	"math"
	"strings"
	"time"

	"godb/types"
)

// 这个文件放日期时间的取字段 / 截断逻辑，供 EXTRACT(... FROM ...) 和
// date_trunc() 共用。两者都按字段名分派，大小写不敏感。

// extractField 取出时间值的某个字段。返回值用浮点表示（second 带小数位），
// 和 PostgreSQL 的 extract 一致。
func extractField(t time.Time, field string) (float64, error) {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "year":
		return float64(t.Year()), nil
	case "month":
		return float64(int(t.Month())), nil
	case "day":
		return float64(t.Day()), nil
	case "hour":
		return float64(t.Hour()), nil
	case "minute":
		return float64(t.Minute()), nil
	case "second":
		return float64(t.Second()) + float64(t.Nanosecond())/1e9, nil
	case "milliseconds":
		return float64(t.Second())*1000 + float64(t.Nanosecond())/1e6, nil
	case "microseconds":
		return float64(t.Second())*1e6 + float64(t.Nanosecond())/1e3, nil
	case "quarter":
		return float64((int(t.Month())-1)/3 + 1), nil
	case "dow":
		// 0 = 周日，和 PG 一致
		return float64(int(t.Weekday())), nil
	case "isodow":
		d := int(t.Weekday())
		if d == 0 {
			d = 7
		}
		return float64(d), nil
	case "doy":
		return float64(t.YearDay()), nil
	case "week":
		_, w := t.ISOWeek()
		return float64(w), nil
	case "isoyear":
		y, _ := t.ISOWeek()
		return float64(y), nil
	case "epoch":
		return float64(t.UnixNano()) / 1e9, nil
	case "century":
		// PG 的世纪从 1 开始：2000 年属于 20 世纪
		return float64((t.Year()-1)/100 + 1), nil
	case "decade":
		return float64(t.Year() / 10), nil
	case "millennium":
		return float64((t.Year()-1)/1000 + 1), nil
	case "timezone":
		_, off := t.Zone()
		return float64(off), nil
	case "timezone_hour":
		_, off := t.Zone()
		return float64(off / 3600), nil
	case "timezone_minute":
		_, off := t.Zone()
		return float64(off%3600) / 60, nil
	}
	return 0, fmt.Errorf("unit %q not recognized for type timestamp", strings.ToLower(field))
}

// dateTrunc 把时间截断到指定精度。截断按值自身的时区做，结果保持该时区，
// 这样 date_trunc('day', timestamptz) 不会把日期挪一天。
func dateTrunc(t time.Time, field string) (time.Time, error) {
	y, mo, d := t.Date()
	h, mi, s := t.Clock()
	loc := t.Location()
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "microseconds":
		return time.Date(y, mo, d, h, mi, s, t.Nanosecond()/1000*1000, loc), nil
	case "milliseconds":
		return time.Date(y, mo, d, h, mi, s, t.Nanosecond()/1e6*1e6, loc), nil
	case "second":
		return time.Date(y, mo, d, h, mi, s, 0, loc), nil
	case "minute":
		return time.Date(y, mo, d, h, mi, 0, 0, loc), nil
	case "hour":
		return time.Date(y, mo, d, h, 0, 0, 0, loc), nil
	case "day":
		return time.Date(y, mo, d, 0, 0, 0, 0, loc), nil
	case "week":
		// ISO 周：截到本周一
		back := (int(t.Weekday()) + 6) % 7
		return time.Date(y, mo, d-back, 0, 0, 0, 0, loc), nil
	case "month":
		return time.Date(y, mo, 1, 0, 0, 0, 0, loc), nil
	case "quarter":
		q := time.Month((int(mo)-1)/3*3 + 1)
		return time.Date(y, q, 1, 0, 0, 0, 0, loc), nil
	case "year":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc), nil
	case "decade":
		return time.Date(y/10*10, 1, 1, 0, 0, 0, 0, loc), nil
	case "century":
		c := (y-1)/100 + 1
		return time.Date((c-1)*100+1, 1, 1, 0, 0, 0, 0, loc), nil
	case "millennium":
		m := (y-1)/1000 + 1
		return time.Date((m-1)*1000+1, 1, 1, 0, 0, 0, 0, loc), nil
	}
	return time.Time{}, fmt.Errorf("unit %q not recognized for type timestamp", strings.ToLower(field))
}

// timeLayouts 是文本转时间时尝试的格式，按常见程度排序。
// EXTRACT / date_trunc 的参数来自客户端时（'2026-01-01'::text）需要它兜底。
var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"15:04:05.999999999",
	"15:04:05",
}

// parseTimeText 尽力把文本解析成时间；失败返回 false。
func parseTimeText(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// valueAsTime 把 SQL 值转成时间。时间类型的值直接取，文本尽力解析，
// 纯数字按 Unix 秒理解（客户端偶尔会这么传）。
func valueAsTime(v types.Value) (time.Time, bool) {
	switch v.K {
	case types.KTime:
		return v.Time(), true
	case types.KText:
		return parseTimeText(v.TextForm())
	case types.KInt, types.KFloat:
		sec := v.Float()
		ip, frac := math.Modf(sec)
		return time.Unix(int64(ip), int64(frac*1e9)).UTC(), true
	}
	return time.Time{}, false
}
