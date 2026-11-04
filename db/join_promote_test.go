package db

import (
	"fmt"
	"strings"
	"testing"
)

/* 逗号连接写在 WHERE 里的等值条件，要能变成真正的连接条件。

   以前 `FROM a, b WHERE a.id = b.id` 只会做成「笛卡尔积 + 事后过滤」：
   3000 行 × 3000 行 = 900 万行，先撞上 200 万行的保护才报错。这也是那次
   压测堆冲到 20 GB 的来源 —— 保护没错，是计划在做不该做的事。

   改写逻辑在 exec/promote.go，这里从 SQL 层验行为：计划形状 + 结果一致。
*/

func TestCommaJoinGetsJoinKeys(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ca (id int PRIMARY KEY, v text)`)
	run(t, d, s, `CREATE TABLE cb (id int PRIMARY KEY, name text)`)

	// 3000 行 × 3000 行：不提升就是 900 万行，会直接撞上连接上限
	const n = 3000
	for start := 1; start <= n; start += 500 {
		var a, b []string
		for i := start; i < start+500 && i <= n; i++ {
			a = append(a, fmt.Sprintf("(%d,'v%d')", i, i))
			b = append(b, fmt.Sprintf("(%d,'n%d')", i, i))
		}
		run(t, d, s, `INSERT INTO ca VALUES `+strings.Join(a, ","))
		run(t, d, s, `INSERT INTO cb VALUES `+strings.Join(b, ","))
	}

	// 1) 没有额外过滤：输出必须正好 3000 行（不是 900 万，也不报错）
	got := rowsOf(t, run(t, d, s, `SELECT count(*) FROM ca, cb WHERE ca.id = cb.id`))
	if fmt.Sprint(got[0][0]) != "3000" {
		t.Fatalf("count(*) = %v，期望 3000", got[0][0])
	}
	plan := explainPlan(t, d, s, `SELECT count(*) FROM ca, cb WHERE ca.id = cb.id`)
	if strings.Contains(plan, "Cross Join") {
		t.Fatalf("逗号连接仍退化成笛卡尔积：%s", plan)
	}
	if !strings.Contains(plan, "Inner Join") {
		t.Fatalf("计划里没有内连接：%s", plan)
	}

	// 2) 和 JOIN … ON 写法结果逐行一致
	comma := rowsOf(t, run(t, d, s,
		`SELECT ca.id, cb.name FROM ca, cb WHERE ca.id = cb.id AND ca.id = 7`))
	on := rowsOf(t, run(t, d, s,
		`SELECT ca.id, cb.name FROM ca JOIN cb ON ca.id = cb.id WHERE ca.id = 7`))
	if len(comma) != 1 || len(on) != 1 || fmtRow(comma[0]) != fmtRow(on[0]) {
		t.Fatalf("两种写法结果不一致：%v vs %v", comma, on)
	}
	if fmtRow(comma[0]) != "7|n7" {
		t.Fatalf("结果 = %q，期望 7|n7", fmtRow(comma[0]))
	}
}

// 三张表：条件分别挂在各自的连接节点上。FROM 是左深链 (a × b) × c，
// a=b 挂内层、b=c 挂外层，两个都得提升，否则中间那步还是笛卡尔积。
func TestCommaJoinThreeTables(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE j1 (id int PRIMARY KEY, x int)`)
	run(t, d, s, `CREATE TABLE j2 (id int PRIMARY KEY, y int)`)
	run(t, d, s, `CREATE TABLE j3 (id int PRIMARY KEY, z int)`)
	for i := 1; i <= 400; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO j1 VALUES (%d,%d)`, i, i*10))
		run(t, d, s, fmt.Sprintf(`INSERT INTO j2 VALUES (%d,%d)`, i, i*100))
		run(t, d, s, fmt.Sprintf(`INSERT INTO j3 VALUES (%d,%d)`, i, i*1000))
	}

	plan := explainPlan(t, d, s,
		`SELECT count(*) FROM j1, j2, j3 WHERE j1.id = j2.id AND j2.id = j3.id`)
	if strings.Contains(plan, "Cross Join") {
		t.Fatalf("三表逗号连接仍有一层笛卡尔积：%s", plan)
	}
	got := rowsOf(t, run(t, d, s,
		`SELECT count(*) FROM j1, j2, j3 WHERE j1.id = j2.id AND j2.id = j3.id`))
	if fmt.Sprint(got[0][0]) != "400" {
		t.Fatalf("count(*) = %v，期望 400", got[0][0])
	}
	row := rowsOf(t, run(t, d, s,
		`SELECT j1.x, j2.y, j3.z FROM j1, j2, j3 WHERE j1.id = j2.id AND j2.id = j3.id AND j1.id = 5`))
	if len(row) != 1 || fmtRow(row[0]) != "50|500|5000" {
		t.Fatalf("结果 = %v，期望 50|500|5000", row)
	}
}

// 外连接的可空侧不能吸收条件：`a LEFT JOIN b … , c WHERE b.id = c.id` 里这条
// 条件必须在外连接**之后**生效（它会把 b 为 NULL 的那些行滤掉）。
// 提升到内层连接里就变成了「先删一部分 b 再补 NULL」，结果不一样。
func TestCommaJoinAfterLeftJoin(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE la (id int PRIMARY KEY, x int)`)
	run(t, d, s, `CREATE TABLE lb (id int PRIMARY KEY, y int)`)
	run(t, d, s, `CREATE TABLE lc (id int PRIMARY KEY, z int)`)
	run(t, d, s, `INSERT INTO la VALUES (1,10),(2,20),(3,30)`)
	run(t, d, s, `INSERT INTO lb VALUES (1,100),(3,300)`)
	run(t, d, s, `INSERT INTO lc VALUES (1,1000),(3,3000)`)

	// la 的 2 号在 lb 里没有，LEFT JOIN 后 b 侧是 NULL，再被 b.id = c.id 滤掉
	assertRows(t, run(t, d, s,
		`SELECT la.id, lb.y, lc.z FROM la LEFT JOIN lb ON la.id = lb.id, lc
		 WHERE lb.id = lc.id ORDER BY la.id`),
		[]string{"1|100|1000", "3|300|3000"})
}

// 不该提升的写法保持原样（宁可慢，也不能改语义）：
//   · 不是「列 = 列」的等值（这里是 OR）
//   · 两侧同属一张表
//   · 列没写限定符（归属判不准）
func TestCommaJoinNotPromoted(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE na (id int PRIMARY KEY, v int)`)
	run(t, d, s, `CREATE TABLE nb (id int PRIMARY KEY, w int)`)
	run(t, d, s, `INSERT INTO na VALUES (1,7),(2,8)`)
	run(t, d, s, `INSERT INTO nb VALUES (1,7),(2,9)`)

	// OR 不算连接条件：仍然走交叉连接 + 过滤，但结果必须对
	got := rowsOf(t, run(t, d, s,
		`SELECT na.id FROM na, nb WHERE na.id = nb.id OR na.v = nb.w ORDER BY na.id`))
	if len(got) != 2 {
		t.Fatalf("OR 条件结果 %d 行，期望 2 行：%v", len(got), got)
	}
	// 同表两列相等：留在 WHERE 里当过滤条件
	got = rowsOf(t, run(t, d, s, `SELECT na.id FROM na, nb WHERE na.id = na.v AND nb.id = 1`))
	if len(got) != 0 {
		t.Fatalf("同表条件不该被当成连接键，结果 %v", got)
	}
}
