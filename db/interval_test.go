package db

import (
	"fmt"
	"strings"
	"testing"

	"godb/types"
)

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1 day", 24 * 3600 * 1e9},
		{"2 days", 48 * 3600 * 1e9},
		{"01:30:00", 5400 * 1e9},
		{"1 day 02:03:04", 24*3600*1e9 + 2*3600*1e9 + 3*60*1e9 + 4*1e9},
		{"1 hour", 3600 * 1e9},
		{"30 min", 1800 * 1e9},
		{"1.5 seconds", 1500 * 1e6},
		{"1 week", 7 * 24 * 3600 * 1e9},
	}
	for _, c := range cases {
		got, err := types.ParseInterval(c.in)
		if err != nil {
			t.Fatalf("ParseInterval(%q) err: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseInterval(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIntervalArithmetic(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	// INTERVAL 字面量 + 时间运算
	r := rowsOf(t, run(t, d, s, `SELECT DATE '2024-01-02' - INTERVAL '1 day'`))
	got := strings.TrimSpace(fmt.Sprint(r[0][0]))
	if !strings.HasPrefix(got, "2024-01-01") {
		t.Fatalf("DATE - INTERVAL = %q, want 2024-01-01", got)
	}

	r = rowsOf(t, run(t, d, s, `SELECT TIMESTAMP '2024-01-01 10:00:00' + INTERVAL '02:30:00'`))
	got = strings.TrimSpace(fmt.Sprint(r[0][0]))
	if !strings.Contains(got, "12:30:00") {
		t.Fatalf("TIMESTAMP + INTERVAL = %q, want ...12:30:00", got)
	}

	// now() - interval 只验证不报错且是时间
	r = rowsOf(t, run(t, d, s, `SELECT now() - INTERVAL '1 day'`))
	if len(r) != 1 {
		t.Fatalf("now()-interval 行数 = %d", len(r))
	}

	// interval + interval：日/时相加，文本形式
	got = strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT INTERVAL '1 day' + INTERVAL '2 hours'`)[0].Rows[0][0]))
	if !strings.Contains(got, "1 day") || !strings.Contains(got, "02:00:00") {
		t.Fatalf("interval+interval = %q", got)
	}

	// timestamp - timestamp → interval（纯时间差）
	got = strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT TIMESTAMP '2024-01-02 00:00:00' - TIMESTAMP '2024-01-01 00:00:00'`)[0].Rows[0][0]))
	if !strings.Contains(got, "1 day") {
		t.Fatalf("ts-ts = %q", got)
	}

	// ::interval 与 INTERVAL '…' 等价
	got = strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT '3 days'::interval`)[0].Rows[0][0]))
	if !strings.Contains(got, "3 days") {
		t.Fatalf("'3 days'::interval = %q", got)
	}
}

// 月/年按日历加，不是 30/365 天近似。
func TestIntervalCalendarMonths(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	// 2024-01-31 + 1 month = 2024-02-29（闰年二月）
	got := strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT DATE '2024-01-31' + INTERVAL '1 month'`)[0].Rows[0][0]))
	if !strings.HasPrefix(got, "2024-02-29") {
		t.Fatalf("1/31 + 1 mon = %q, want 2024-02-29", got)
	}
	// 2024-03-31 + 1 month = 2024-04-30（4 月没有 31）
	got = strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT DATE '2024-03-31' + INTERVAL '1 month'`)[0].Rows[0][0]))
	if !strings.HasPrefix(got, "2024-04-30") {
		t.Fatalf("3/31 + 1 mon = %q, want 2024-04-30", got)
	}
	// 2024-02-29 + 1 year = 2025-02-28（非闰年）
	got = strings.TrimSpace(fmt.Sprint(run(t, d, s, `SELECT DATE '2024-02-29' + INTERVAL '1 year'`)[0].Rows[0][0]))
	if !strings.HasPrefix(got, "2025-02-28") {
		t.Fatalf("2/29 + 1 year = %q, want 2025-02-28", got)
	}
}
