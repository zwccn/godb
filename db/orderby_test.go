package db

import (
	"fmt"
	"strings"
	"testing"
)

// 建一张用来验证「索引序 + OR 合并」的表。
//
// a = i%10、b = i/10，(a,b) 组合唯一；code 用来做等值/前缀；note 无索引对照。
func setupOrderTable(t *testing.T) *DB {
	t.Helper()
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE ob (id int4 PRIMARY KEY, a int4, b int4, code text, note text)`)
	for start := 0; start < 200; start += 50 {
		var b strings.Builder
		b.WriteString(`INSERT INTO ob VALUES `)
		for i := start; i < start+50; i++ {
			if i > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,%d,%d,'c%d','n%d')", i+1, i%10, i/10, i%3, i)
		}
		run(t, d, s, b.String())
	}
	run(t, d, s, `CREATE INDEX ob_a_b ON ob (a, b)`)
	run(t, d, s, `CREATE INDEX ob_code ON ob (code)`)
	return d
}

func explainPlan(t *testing.T, d *DB, s, sql string) string {
	t.Helper()
	return fmt.Sprint(rowsOf(t, run(t, d, s, "EXPLAIN "+sql)))
}

// ORDER BY 正好命中索引列（或列前缀）时不该再有 Sort 节点。
func TestOrderBySkipsSort(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	cases := []struct {
		sql  string
		want string // 必须出现的计划片段
	}{
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY a`, "Index Range Scan using ob_a_b"},
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY a, b`, "Index Range Scan using ob_a_b"},
		{`SELECT * FROM ob WHERE a = 3 ORDER BY a, b`, "Index Range Scan using ob_a_b"},
		{`SELECT * FROM ob WHERE id BETWEEN 10 AND 20 ORDER BY id`, "Index Range Scan using ob_pkey"},
		{`SELECT * FROM ob WHERE id = 7 ORDER BY id`, "Index Scan using ob_pkey"},
		{`SELECT * FROM ob WHERE code = 'c1' ORDER BY code`, "Index Scan using ob_code"},
	}
	for _, c := range cases {
		plan := explainPlan(t, d, s, c.sql)
		if !strings.Contains(plan, c.want) {
			t.Errorf("%s\n  期望计划含 %q，实际: %s", c.sql, c.want, plan)
		}
		if strings.Contains(plan, "Sort") {
			t.Errorf("%s\n  不该再排序，实际: %s", c.sql, plan)
		}
	}
}

// 用不上索引序的情况必须老实排序（否则结果顺序就是错的）。
func TestOrderByStillSorts(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	cases := []struct {
		sql, why string
	}{
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY b`, "只排第二列，索引帮不上"},
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY a, b DESC`, "混合方向"},
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY a + 0`, "表达式排序"},
		{`SELECT * FROM ob WHERE note > 'n1' ORDER BY note`, "note 没索引"},
		{`SELECT * FROM ob WHERE a BETWEEN 0 AND 9 ORDER BY id`, "排序列不在索引里"},
		{`SELECT * FROM ob ORDER BY id`, "没有 WHERE，不走索引"},
	}
	for _, c := range cases {
		plan := explainPlan(t, d, s, c.sql)
		if !strings.Contains(plan, "Sort") {
			t.Errorf("%s（%s）\n  必须排序，实际: %s", c.sql, c.why, plan)
		}
	}
}

// 免排序的前提是「结果真的有序」——不只计划里没有 Sort，数据也要对。
func TestOrderByResultOrder(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	asc := rowsOf(t, run(t, d, s, `SELECT a, b FROM ob WHERE a BETWEEN 2 AND 4 ORDER BY a, b`))
	if len(asc) != 60 {
		t.Fatalf("应 60 行，实际 %d", len(asc))
	}
	prevA, prevB := -1, -1
	for _, r := range asc {
		a, b := intOf(t, r[0]), intOf(t, r[1])
		if a < prevA || (a == prevA && b < prevB) {
			t.Fatalf("升序被打乱：(%d,%d) 出现在 (%d,%d) 之后", a, b, prevA, prevB)
		}
		prevA, prevB = a, b
	}

	desc := rowsOf(t, run(t, d, s, `SELECT a, b FROM ob WHERE a BETWEEN 2 AND 4 ORDER BY a DESC, b DESC`))
	if len(desc) != 60 {
		t.Fatalf("降序应 60 行，实际 %d", len(desc))
	}
	prevA, prevB = 99, 99
	for _, r := range desc {
		a, b := intOf(t, r[0]), intOf(t, r[1])
		if a > prevA || (a == prevA && b > prevB) {
			t.Fatalf("降序被打乱：(%d,%d) 出现在 (%d,%d) 之后", a, b, prevA, prevB)
		}
		prevA, prevB = a, b
	}

	// 只排首列、倒序：同 a 内部顺序不保证，但 a 必须单调不增
	byA := rowsOf(t, run(t, d, s, `SELECT a FROM ob WHERE a >= 5 ORDER BY a DESC`))
	prev := 99
	for _, r := range byA {
		a := intOf(t, r[0])
		if a > prev {
			t.Fatalf("ORDER BY a DESC 出现 %d 在 %d 之后", a, prev)
		}
		prev = a
	}
}

// 无 WHERE 的 ORDER BY 仍然走全表扫 + 排序：索引里不含 NULL 键，
// 直接拿索引当「全表顺序」会漏掉那些行为 NULL 的行。
func TestOrderByWithoutWhereKeepsNulls(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE obn (id int4 PRIMARY KEY, v int4)`)
	run(t, d, s, `CREATE INDEX obn_v ON obn (v)`)
	run(t, d, s, `INSERT INTO obn VALUES (1, 5), (2, NULL), (3, 1)`)
	rows := rowsOf(t, run(t, d, s, `SELECT id FROM obn ORDER BY v`))
	if len(rows) != 3 {
		t.Fatalf("NULL 行不能丢，实际 %d 行", len(rows))
	}
}

// OR 合并：两个分支落在同一个索引上，一次多区间扫描。
func TestOrUsesIndexUnion(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	cases := []struct {
		sql  string
		want string
		rows int
	}{
		{`SELECT id FROM ob WHERE id = 3 OR id = 7`, "Index Union Scan", 2},
		{`SELECT id FROM ob WHERE a = 1 OR a = 2`, "Index Union Scan", 40},
		{`SELECT id FROM ob WHERE code = 'c0' OR code = 'c1'`, "Index Union Scan", 134}, // 67+67
		{`SELECT id FROM ob WHERE a = 1 OR a = 2 OR a = 3`, "Index Union Scan", 60},
		{`SELECT id FROM ob WHERE a > 8 OR a < 1`, "Index Union Scan", 40},
	}
	for _, c := range cases {
		plan := explainPlan(t, d, s, c.sql)
		if !strings.Contains(plan, c.want) {
			t.Errorf("%s\n  期望 %q，实际: %s", c.sql, c.want, plan)
		}
		got := rowsOf(t, run(t, d, s, c.sql))
		if len(got) != c.rows {
			t.Errorf("%s 应 %d 行，实际 %d", c.sql, c.rows, len(got))
		}
	}
}

// OR 里只要有一个分支用不上索引，整条就退回全表扫 —— 部分命中等于漏行。
// 结果必须和全表扫一致。
func TestOrFallsBackWhenBranchNotIndexed(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sql := `SELECT id FROM ob WHERE a = 1 OR note = 'n5'`
	plan := explainPlan(t, d, s, sql)
	if strings.Contains(plan, "Index Union") {
		t.Fatalf("不该走索引合并，实际: %s", plan)
	}
	// a=1（20 行）+ note='n5'（1 行，且它的 a=5 不在前面）
	got := rowsOf(t, run(t, d, s, sql))
	if len(got) != 21 {
		t.Fatalf("应 21 行，实际 %d", len(got))
	}

	// 两个分支落在不同索引上：同样放弃
	sql2 := `SELECT id FROM ob WHERE a = 1 OR code = 'c0'`
	if p := explainPlan(t, d, s, sql2); strings.Contains(p, "Index Union") {
		t.Fatalf("跨索引用不了合并，实际: %s", p)
	}
	// a=1 有 20 行、code='c0' 有 67 行，交集 6 行
	if n := len(rowsOf(t, run(t, d, s, sql2))); n != 81 {
		t.Fatalf("应 81 行，实际 %d", n)
	}
}

// OR 合并出来的多个区间可能重叠、也可能刚好相邻，行不能重复也不能漏。
func TestOrUnionNoDuplicates(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	// 区间重叠：a>3 与 a BETWEEN 2 AND 6 交叠在 4,5,6
	got := rowsOf(t, run(t, d, s, `SELECT id FROM ob WHERE a > 3 OR a BETWEEN 2 AND 6`))
	if len(got) != 20*8 { // 2..9 共 8 个值
		t.Fatalf("重叠区间应去重成 160 行，实际 %d", len(got))
	}
	seen := map[int]bool{}
	for _, r := range got {
		id := intOf(t, r[0])
		if seen[id] {
			t.Fatalf("id=%d 出现两次", id)
		}
		seen[id] = true
	}
}

// OR + ORDER BY：多区间扫描的输出仍然是键序，可以继续免排序。
func TestOrWithOrderBySkipsSort(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sql := `SELECT a, b FROM ob WHERE a = 1 OR a = 2 ORDER BY a, b`
	plan := explainPlan(t, d, s, sql)
	if strings.Contains(plan, "Sort") {
		t.Fatalf("OR 合并后仍有序，不该排序，实际: %s", plan)
	}
	rows := rowsOf(t, run(t, d, s, sql))
	if len(rows) != 40 {
		t.Fatalf("应 40 行，实际 %d", len(rows))
	}
	prevA, prevB := -1, -1
	for _, r := range rows {
		a, b := intOf(t, r[0]), intOf(t, r[1])
		if a < prevA || (a == prevA && b < prevB) {
			t.Fatalf("多区间输出乱序：(%d,%d) 在 (%d,%d) 之后", a, b, prevA, prevB)
		}
		prevA, prevB = a, b
	}
}

// 事务里改过键的行必须归并到正确位置，不能被甩到末尾。
func TestOrAndOrderInsideTransaction(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	run(t, d, s, `BEGIN`)
	// 把 a=1 的行改成 a=3（区间外），把 a=5 的行改成 a=2（区间内）
	run(t, d, s, `UPDATE ob SET a = 3 WHERE id = 2`) // 原 a=1,b=0
	run(t, d, s, `UPDATE ob SET a = 2 WHERE id = 6`) // 原 a=5,b=0
	run(t, d, s, `INSERT INTO ob VALUES (9999, 2, 77, 'c9', 'new')`)
	rows := rowsOf(t, run(t, d, s, `SELECT a, b, id FROM ob WHERE a = 1 OR a = 2 ORDER BY a, b`))
	run(t, d, s, `COMMIT`)

	prevA, prevB := -1, -1
	found := map[int]bool{}
	for _, r := range rows {
		a, b, id := intOf(t, r[0]), intOf(t, r[1]), intOf(t, r[2])
		if a < prevA || (a == prevA && b < prevB) {
			t.Fatalf("事务内乱序：(%d,%d) 在 (%d,%d) 之后", a, b, prevA, prevB)
		}
		prevA, prevB = a, b
		found[id] = true
	}
	if found[2] {
		t.Fatal("id=2 已被改出区间，不该出现")
	}
	if !found[6] || !found[9999] {
		t.Fatalf("新进区间的行应出现：id=6 %v，id=9999 %v", found[6], found[9999])
	}
}

// UPDATE / DELETE 的 WHERE 也吃 OR。
func TestOrOnUpdateAndDelete(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	if res := run(t, d, s, `UPDATE ob SET note = 'hit' WHERE id = 5 OR id = 9`); res[0].Affected != 2 {
		t.Fatalf("UPDATE 应影响 2 行，实际 %d", res[0].Affected)
	}
	if res := run(t, d, s, `DELETE FROM ob WHERE a = 7 OR a = 8`); res[0].Affected != 40 {
		t.Fatalf("DELETE 应影响 40 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob`), []string{"160"})
}

// 区间里夹着 NULL 值时，OR 分支的 NULL 等值要当空集，而不是匹配一切。
func TestOrWithNullConstant(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE obz (id int4 PRIMARY KEY, v int4)`)
	run(t, d, s, `CREATE INDEX obz_v ON obz (v)`)
	run(t, d, s, `INSERT INTO obz VALUES (1, 1), (2, 2), (3, NULL)`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM obz WHERE v = 1 OR v = NULL`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM obz WHERE v = NULL OR v = NULL`), []string{"0"})
}

func intOf(t *testing.T, v interface{}) int {
	t.Helper()
	switch x := v.(type) {
	case int64:
		return int(x)
	case int:
		return x
	case int32:
		return int(x)
	case string:
		n := 0
		if _, err := fmt.Sscanf(x, "%d", &n); err != nil {
			t.Fatalf("不是整数: %q", x)
		}
		return n
	}
	t.Fatalf("不是整数: %v (%T)", v, v)
	return 0
}

// OR 各分支约束的列不一样：一个只卡首列、一个卡满复合索引。
// 校验「行是否在区间内」必须用**索引完整列**的键，否则只卡首列那个分支
// 会把另一个分支的行判成不在区间，静默漏行。
func TestOrBranchesWithDifferentColumns(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	// a=1 → 20 行（b=0）；a=2 AND b=3 → i=32 那一行
	got := rowsOf(t, run(t, d, s, `SELECT id FROM ob WHERE a = 1 OR (a = 2 AND b = 3)`))
	if len(got) != 21 {
		t.Fatalf("应 21 行（20 + 1），实际 %d —— 少的都是被漏掉的", len(got))
	}
	// 反向：先完整等值、再只卡首列
	got2 := rowsOf(t, run(t, d, s, `SELECT id FROM ob WHERE (a = 2 AND b = 3) OR a = 1`))
	if len(got2) != 21 {
		t.Fatalf("换顺序应同为 21 行，实际 %d", len(got2))
	}
}

// 对拍：任意一组 a 值写成 OR，结果必须和「全表扫 + 逐行判断」完全一致。
// 这比手写期望值更能抓住区间合并、去重、排序上的错。
func TestOrMatchesFullScan(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sets := [][]int{
		{1}, {1, 2}, {0, 9}, {1, 2, 3, 4, 5}, {3, 3, 3}, {0, 2, 4, 6, 8},
		{1, 3, 5, 7, 9}, {0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	}
	for _, set := range sets {
		var conds []string
		in := map[int]bool{}
		for _, v := range set {
			conds = append(conds, fmt.Sprintf("a = %d", v))
			in[v] = true
		}
		sql := `SELECT id FROM ob WHERE ` + strings.Join(conds, " OR ") + ` ORDER BY id`

		// 期望：全表扫一遍，挑出 a 在集合里的行
		want := make([]string, 0, 200)
		for i := 0; i < 200; i++ {
			if in[i%10] {
				want = append(want, fmt.Sprint(i+1))
			}
		}
		got := rowsOf(t, run(t, d, s, sql))
		if len(got) != len(want) {
			t.Fatalf("%v：应 %d 行，实际 %d", set, len(want), len(got))
		}
		for i := range want {
			if fmt.Sprint(got[i][0]) != want[i] {
				t.Fatalf("%v：第 %d 行应为 %s，实际 %v", set, i, want[i], got[i][0])
			}
		}
	}
}

// 同一批数据，OR 的区间合并结果要和逐个单点查询的并集一致（含去重）。
func TestOrUnionEqualsSeparateQueries(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	union := map[int]bool{}
	for _, v := range []int{2, 5, 7} {
		for _, r := range rowsOf(t, run(t, d, s, fmt.Sprintf(`SELECT id FROM ob WHERE a = %d`, v))) {
			union[intOf(t, r[0])] = true
		}
	}
	got := rowsOf(t, run(t, d, s, `SELECT id FROM ob WHERE a = 2 OR a = 5 OR a = 7`))
	if len(got) != len(union) {
		t.Fatalf("应 %d 行，实际 %d", len(union), len(got))
	}
	for _, r := range got {
		if !union[intOf(t, r[0])] {
			t.Fatalf("多出了 id=%v", r[0])
		}
	}
}
