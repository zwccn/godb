package db

import (
	"fmt"
	"strings"
	"testing"
)

func TestFastCountMinMax(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE fa (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, s, `CREATE INDEX fa_v ON fa (v)`)
	for i := 1; i <= 5; i++ {
		run(t, d, s, `INSERT INTO fa VALUES (`+itoa(i)+`, `+itoa(i*10)+`)`)
	}

	assertRows(t, run(t, d, s, `SELECT count(*) FROM fa`), []string{"5"})
	assertRows(t, run(t, d, s, `SELECT min(id), max(id) FROM fa`), []string{"1|5"})
	assertRows(t, run(t, d, s, `SELECT min(v), max(v) FROM fa`), []string{"10|50"})

	// 事务内 overlay 也要算进去
	run(t, d, s, `BEGIN`)
	run(t, d, s, `INSERT INTO fa VALUES (10, 99), (0, 1)`)
	run(t, d, s, `DELETE FROM fa WHERE id = 3`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM fa`), []string{"6"})
	assertRows(t, run(t, d, s, `SELECT min(id), max(id) FROM fa`), []string{"0|10"})
	assertRows(t, run(t, d, s, `SELECT min(v), max(v) FROM fa`), []string{"1|99"})
	run(t, d, s, `ROLLBACK`)

	assertRows(t, run(t, d, s, `SELECT count(*) FROM fa`), []string{"5"})

	// 空表
	run(t, d, s, `CREATE TABLE fa_empty (id INTEGER PRIMARY KEY)`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM fa_empty`), []string{"0"})
	assertRows(t, run(t, d, s, `SELECT min(id) FROM fa_empty`), []string{"<nil>"})

	// 计划应走快聚合
	plan := explainPlan(t, d, s, `SELECT count(*) FROM fa`)
	if !strings.Contains(plan, "Fast Aggregate") {
		t.Fatalf("count(*) 应走快聚合，实际: %s", plan)
	}
	plan = explainPlan(t, d, s, `SELECT min(v), max(v) FROM fa`)
	if !strings.Contains(plan, "Fast Aggregate") {
		t.Fatalf("min/max 应走快聚合，实际: %s", plan)
	}
	// 带 WHERE 的仍走通用聚合
	plan = explainPlan(t, d, s, `SELECT count(*) FROM fa WHERE id > 1`)
	if strings.Contains(plan, "Fast Aggregate") {
		t.Fatalf("带 WHERE 不该走快聚合，实际: %s", plan)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestDistinctOrderByLimitTopN(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dt (a INTEGER, b INTEGER)`)
	run(t, d, s, `INSERT INTO dt VALUES (1,10),(2,20),(1,10),(3,30),(2,20),(4,40)`)

	// DISTINCT + ORDER BY + LIMIT：结果要与全排序一致
	full := rowsOf(t, run(t, d, s, `SELECT DISTINCT a FROM dt ORDER BY a DESC`))
	top := rowsOf(t, run(t, d, s, `SELECT DISTINCT a FROM dt ORDER BY a DESC LIMIT 2`))
	if len(top) != 2 {
		t.Fatalf("LIMIT 2 应 2 行，实际 %d", len(top))
	}
	for i := range top {
		if fmt.Sprint(top[i][0]) != fmt.Sprint(full[i][0]) {
			t.Fatalf("DISTINCT Top-N 第 %d 行 %v，期望 %v", i, top[i], full[i])
		}
	}
	// 序号排序也支持
	top2 := rowsOf(t, run(t, d, s, `SELECT DISTINCT a FROM dt ORDER BY 1 ASC LIMIT 2`))
	if fmt.Sprint(top2[0][0]) != "1" || fmt.Sprint(top2[1][0]) != "2" {
		t.Fatalf("ORDER BY 1 LIMIT 2 = %v", top2)
	}
	// 计划应含 Top-N
	plan := explainPlan(t, d, s, `SELECT DISTINCT a FROM dt ORDER BY a DESC LIMIT 2`)
	if !strings.Contains(plan, "Top-N") && !strings.Contains(plan, "Sort") {
		t.Fatalf("应有排序/Top-N，实际: %s", plan)
	}
}

func TestCoveringIndex(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ci (id INTEGER PRIMARY KEY, a INTEGER, b TEXT)`)
	run(t, d, s, `CREATE INDEX ci_a_b ON ci (a, b)`)
	run(t, d, s, `INSERT INTO ci VALUES (1, 10, 'x'), (2, 20, 'y'), (3, 30, 'z')`)

	// a,b 都在索引 (a,b) 里 → 覆盖扫描
	rows := rowsOf(t, run(t, d, s, `SELECT a, b FROM ci WHERE a >= 20`))
	if len(rows) != 2 {
		t.Fatalf("覆盖扫描应 2 行，实际 %d: %v", len(rows), rows)
	}
	plan := explainPlan(t, d, s, `SELECT a, b FROM ci WHERE a >= 20`)
	if !strings.Contains(plan, "Index Only Scan") {
		t.Fatalf("应是 Index Only Scan，实际: %s", plan)
	}

	// 用到 id（不在 (a,b) 索引里）→ 普通索引扫
	plan = explainPlan(t, d, s, `SELECT id, a FROM ci WHERE a >= 20`)
	if strings.Contains(plan, "Index Only Scan") {
		t.Fatalf("含 id 不该走覆盖，实际: %s", plan)
	}

	// SELECT * 也回表
	plan = explainPlan(t, d, s, `SELECT * FROM ci WHERE a >= 20`)
	if strings.Contains(plan, "Index Only Scan") {
		t.Fatalf("SELECT * 不该走覆盖，实际: %s", plan)
	}

	// 结果正确性：文本列
	assertRows(t, run(t, d, s, `SELECT a, b FROM ci WHERE a = 20`), []string{"20|y"})
}
