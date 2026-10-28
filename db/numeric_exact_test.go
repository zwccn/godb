package db

import "testing"

func TestNumericExact(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	// 0.1 + 0.2 = 0.3（float 会得 0.30000000000000004）
	assertRows(t, run(t, d, s, `SELECT 0.1::numeric + 0.2::numeric`), []string{"0.3"})
	assertRows(t, run(t, d, s, `SELECT 0.1::numeric + 0.2::numeric`), []string{"0.3"})
	// 大数精确
	assertRows(t, run(t, d, s, `SELECT 1234567890123.45::numeric`), []string{"1234567890123.45"})
	assertRows(t, run(t, d, s, `SELECT 1234567890123.45::numeric * 2`), []string{"2469135780246.9"})
	// 聚合精确
	run(t, d, s, `CREATE TABLE nx (v NUMERIC(12,2))`)
	run(t, d, s, `INSERT INTO nx VALUES (0.1), (0.2), (0.3)`)
	assertRows(t, run(t, d, s, `SELECT sum(v) FROM nx`), []string{"0.6"})
	// 四舍五入到标度
	assertRows(t, run(t, d, s, `SELECT 90.567::numeric(6,2)`), []string{"90.57"})
	// 排序与比较
	run(t, d, s, `CREATE TABLE ns (v NUMERIC(10,4))`)
	run(t, d, s, `INSERT INTO ns VALUES (0.3), (0.1), (0.2)`)
	assertRows(t, run(t, d, s, `SELECT v FROM ns WHERE v = 0.3`), []string{"0.3"})
}
