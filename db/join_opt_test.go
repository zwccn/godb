package db

import (
	"fmt"
	"strings"
	"testing"
)

// setupJoinTable 造两张用于连接优化的表：
//
//	dept(4 行) / emp(40 行，dept_id 指向 dept.id)，emp.dept_id 上建索引。
func setupJoinTable(t *testing.T) *DB {
	t.Helper()
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE dept (id int4 PRIMARY KEY, name text)`)
	run(t, d, s, `CREATE TABLE emp (id int4 PRIMARY KEY, dept_id int4, name text, salary int4)`)
	run(t, d, s, `INSERT INTO dept VALUES (1,'研发'),(2,'销售'),(3,'财务'),(4,'空部门')`)
	var b strings.Builder
	b.WriteString(`INSERT INTO emp VALUES `)
	for i := 1; i <= 40; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		// dept_id 只落在 1..3，dept 4 故意没有任何员工
		fmt.Fprintf(&b, "(%d,%d,'e%d',%d)", i, i%3+1, i, 1000+i*10)
	}
	run(t, d, s, b.String())
	run(t, d, s, `CREATE INDEX emp_dept ON emp (dept_id)`)
	return d
}

// 谓词下推：条件在表上先过滤，结果必须和「只在最后过滤」完全一致。
func TestPushdownEquivalence(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	cases := []struct {
		sql  string
		want string // count(*) 的期望值
	}{
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 1`, "13"},
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE emp.dept_id = 2`, "14"},
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 4`, "0"},
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE emp.salary > 1300`, "10"},
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.name = '研发'`, "13"},
		// 跨表条件不下推，但结果要对
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE emp.salary > dept.id * 500`, "27"},
		// OR 里的条件不属于单表 → 整条不下推
		{`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 1 OR emp.dept_id = 2`, "27"},
	}
	for _, c := range cases {
		got := rowsOf(t, run(t, d, s, c.sql))
		if fmt.Sprint(got[0][0]) != c.want {
			t.Errorf("%s\n  期望 %s 行，实际 %v", c.sql, c.want, got[0][0])
		}
	}
}

// 下推确实发生了：计划里能看到 pushed 的 Filter，且事实表走了索引。
func TestPushdownShowsInPlan(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	plan := explainPlan(t, d, s, `SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id WHERE emp.dept_id = 2`)
	if !strings.Contains(plan, "pushed: (emp.dept_id = 2)") {
		t.Errorf("计划里应有下推标记，实际: %s", plan)
	}
	if !strings.Contains(plan, "Index Scan using emp_dept") {
		t.Errorf("下推的条件应能走索引，实际: %s", plan)
	}

	// 条件不在索引首列上时，要说明为什么没走索引
	plan = explainPlan(t, d, s, `SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id WHERE emp.name = 'e1'`)
	if !strings.Contains(plan, "下推条件没走索引") {
		t.Errorf("应给出没走索引的原因，实际: %s", plan)
	}
}

// LEFT JOIN 的可空侧**不能**下推：否则 `WHERE b.x IS NULL` 会拿到错的行。
func TestLeftJoinNullableSideNotPushed(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	// dept 4 没有任何员工；LEFT JOIN 后 dept.id IS NULL 不该匹配到任何行
	sql := `SELECT count(*) FROM dept LEFT JOIN emp ON dept.id = emp.dept_id WHERE emp.dept_id IS NULL`
	assertRows(t, run(t, d, s, sql), []string{"1"}) // 只有 dept 4 那一行

	// 如果是 INNER JOIN，同样的条件结果就是 0 行
	assertRows(t, run(t, d, s,
		`SELECT count(*) FROM emp JOIN dept ON dept.id = emp.dept_id WHERE emp.dept_id IS NULL`), []string{"0"})

	// LEFT JOIN 的**左**侧（不可空侧）可以下推
	plan := explainPlan(t, d, s, `SELECT * FROM dept LEFT JOIN emp ON dept.id = emp.dept_id WHERE dept.id = 1`)
	if !strings.Contains(plan, "pushed: (dept.id = 1)") {
		t.Errorf("LEFT JOIN 左表应能下推，实际: %s", plan)
	}
	// 右表上的条件不该出现 pushed 标记
	if strings.Contains(plan, "pushed: (emp.") {
		t.Errorf("LEFT JOIN 右表不该下推，实际: %s", plan)
	}
}

// 索引连接的结果必须和哈希连接一致：拿「强制走哈希」的写法做对照。
func TestIndexJoinMatchesHashJoin(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	cases := []string{
		`SELECT emp.id, dept.name FROM emp JOIN dept ON emp.dept_id = dept.id ORDER BY emp.id`,
		`SELECT emp.id, dept.name FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 2 ORDER BY emp.id`,
		`SELECT dept.id, emp.id FROM dept LEFT JOIN emp ON dept.id = emp.dept_id ORDER BY dept.id, emp.id`,
		`SELECT count(*) FROM emp JOIN dept ON emp.dept_id = dept.id AND emp.salary > 1200`,
	}
	for _, sql := range cases {
		got := rowsOf(t, run(t, d, s, sql))
		// 同样的语义，但用子查询掐断索引连接的下推路径，逼它走哈希
		alt := strings.Replace(sql,
			"JOIN dept ON emp.dept_id = dept.id",
			"JOIN (SELECT id, name FROM dept) dept ON emp.dept_id = dept.id", 1)
		alt = strings.Replace(alt,
			"LEFT JOIN emp ON dept.id = emp.dept_id",
			"LEFT JOIN (SELECT id, dept_id, name, salary FROM emp) emp ON dept.id = emp.dept_id", 1)
		want := rowsOf(t, run(t, d, s, alt))
		if len(got) != len(want) {
			t.Fatalf("%s\n  行数 %d != 子查询版本 %d", sql, len(got), len(want))
		}
		for i := range got {
			for c := range got[i] {
				if fmt.Sprint(got[i][c]) != fmt.Sprint(want[i][c]) {
					t.Fatalf("%s 第 %d 行不一致: %v vs %v", sql, i, got[i], want[i])
				}
			}
		}
	}
}

// 多列连接键、类型不一致、NULL 值都不能出错。
func TestIndexJoinEdgeCases(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE j2a (id int4, seq int4, tag text, PRIMARY KEY (id, seq))`)
	run(t, d, s, `CREATE TABLE j2b (id int8, seq int8, note text)`)
	run(t, d, s, `INSERT INTO j2a VALUES (1,1,'x'),(1,2,'y'),(2,1,'z')`)
	// 右表类型是 int8，连接时要把键归一
	run(t, d, s, `INSERT INTO j2b VALUES (1,1,'a'),(1,2,'b'),(3,3,'c')`)
	run(t, d, s, `INSERT INTO j2b VALUES (NULL,1,'nullrow')`)

	assertRows(t, run(t, d, s,
		`SELECT count(*) FROM j2b JOIN j2a ON j2b.id = j2a.id AND j2b.seq = j2a.seq`), []string{"2"})
	assertRows(t, run(t, d, s,
		`SELECT count(*) FROM j2b LEFT JOIN j2a ON j2b.id = j2a.id AND j2b.seq = j2a.seq`), []string{"4"})
}

// EXPLAIN ANALYZE 要给出每个节点的实际行数与耗时。
func TestExplainAnalyze(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	res := run(t, d, s, `EXPLAIN ANALYZE SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 1`)
	if res[0].Tag != "EXPLAIN ANALYZE" {
		t.Fatalf("tag = %q", res[0].Tag)
	}
	plan := fmt.Sprint(rowsOf(t, run(t, d, s,
		`EXPLAIN ANALYZE SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id WHERE dept.id = 1`)))
	if !strings.Contains(plan, "rows=") || !strings.Contains(plan, "time=") {
		t.Fatalf("EXPLAIN ANALYZE 应带行数与耗时，实际: %s", plan)
	}
	// 普通 EXPLAIN 不该带运行统计（它不执行）
	plain := explainPlan(t, d, s, `SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id`)
	if strings.Contains(plain, "rows=") {
		t.Fatalf("普通 EXPLAIN 不该有运行统计，实际: %s", plain)
	}
}

// EXPLAIN ANALYZE 会真的执行，所以 DML 的结果不能被应用两次或半次。
func TestExplainAnalyzeDoesNotChangeData(t *testing.T) {
	d := setupJoinTable(t)
	s := "s"
	before := rowsOf(t, run(t, d, s, `SELECT count(*) FROM emp`))
	run(t, d, s, `EXPLAIN ANALYZE SELECT * FROM emp JOIN dept ON emp.dept_id = dept.id`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM emp`), []string{fmt.Sprint(before[0][0])})
}
