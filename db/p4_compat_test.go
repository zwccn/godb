package db

import (
	"strings"
	"testing"
)

// 压测报告 P4 小批：random / lpad / rpad / ANALYZE / CREATE INDEX IF NOT EXISTS / unnest
func TestP4FunctionsAndGrammar(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	// random() ∈ [0,1)
	r := rowsOf(t, run(t, d, s, `SELECT random()`))
	if len(r) != 1 {
		t.Fatalf("random() 行数 = %d", len(r))
	}

	// lpad / rpad
	assertRows(t, run(t, d, s, `SELECT lpad('1', 12, '0')`), []string{"000000000001"})
	assertRows(t, run(t, d, s, `SELECT rpad('1', 5, 'ab')`), []string{"1abab"})
	assertRows(t, run(t, d, s, `SELECT lpad('hello', 3, 'x')`), []string{"hel"})

	// ANALYZE 不报错
	run(t, d, s, `ANALYZE`)
	run(t, d, s, `ANALYZE VERBOSE`)

	// CREATE INDEX IF NOT EXISTS：第二次不再报错
	run(t, d, s, `CREATE TABLE p4t (id int PRIMARY KEY, v int)`)
	run(t, d, s, `CREATE INDEX IF NOT EXISTS ix_p4t_v ON p4t (v)`)
	run(t, d, s, `CREATE INDEX IF NOT EXISTS ix_p4t_v ON p4t (v)`) // 幂等
	err := runErr(t, d, s, `CREATE INDEX ix_p4t_v ON p4t (v)`)     // 不带 IF 仍报错
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("不带 IF NOT EXISTS 重名应报错，实际 %v", err)
	}

	// unnest：FROM 形式（已有）与 SELECT 列表形式（展开成多行）
	assertRows(t, run(t, d, s, `SELECT unnest(array[1,2,3])`), []string{"1", "2", "3"})
	assertRows(t, run(t, d, s, `SELECT * FROM unnest(array['a','b'])`), []string{"a", "b"})
}
