package db

import (
	"fmt"
	"strings"
	"testing"
)

// IN 列表和 OR 合并走同一条路：每个值一个等值（前缀）区间，一次多区间扫描。
func TestInUsesIndexUnion(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	cases := []struct {
		sql  string
		want string // 计划里必须出现的片段
		rows int
	}{
		{`SELECT id FROM ob WHERE id IN (1, 2, 3)`, "Index Union Scan using ob_pkey (3 ranges)", 3},
		{`SELECT id FROM ob WHERE a IN (1, 2)`, "Index Union Scan using ob_a_b (2 ranges)", 40},
		{`SELECT id FROM ob WHERE code IN ('c0', 'c1')`, "Index Union Scan using ob_code", 134},
		// 单值等于等值查询，只剩一段区间
		{`SELECT id FROM ob WHERE a IN (1)`, "Index Range Scan using ob_a_b", 20},
		{`SELECT id FROM ob WHERE id IN (7)`, "Index Range Scan using ob_pkey", 1},
	}
	for _, c := range cases {
		plan := explainPlan(t, d, s, c.sql)
		if !strings.Contains(plan, c.want) {
			t.Errorf("%s\n  期望计划含 %q，实际: %s", c.sql, c.want, plan)
		}
		got := rowsOf(t, run(t, d, s, c.sql))
		if len(got) != c.rows {
			t.Errorf("%s 应 %d 行，实际 %d", c.sql, c.rows, len(got))
		}
	}
}

// IN 与「等价的 OR 写法」结果必须完全一致；再和全表扫对拍一次。
func TestInMatchesOrAndFullScan(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sets := [][]int{
		{1}, {1, 2}, {0, 9}, {2, 4, 6, 8}, {0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, {7, 7, 7},
	}
	for _, set := range sets {
		var inParts, orParts []string
		hit := map[int]bool{}
		for _, v := range set {
			inParts = append(inParts, fmt.Sprint(v))
			orParts = append(orParts, fmt.Sprintf("a = %d", v))
			hit[v] = true
		}
		inSQL := `SELECT id FROM ob WHERE a IN (` + strings.Join(inParts, ",") + `) ORDER BY id`
		orSQL := `SELECT id FROM ob WHERE ` + strings.Join(orParts, " OR ") + ` ORDER BY id`

		in := rowsOf(t, run(t, d, s, inSQL))
		or := rowsOf(t, run(t, d, s, orSQL))
		if len(in) != len(or) {
			t.Fatalf("%v：IN %d 行、OR %d 行", set, len(in), len(or))
		}
		// 期望：全表扫一遍挑出 a 在集合里的行
		want := make([]string, 0, 200)
		for i := 0; i < 200; i++ {
			if hit[i%10] {
				want = append(want, fmt.Sprint(i+1))
			}
		}
		if len(in) != len(want) {
			t.Fatalf("%v：应 %d 行，实际 %d", set, len(want), len(in))
		}
		for i := range want {
			if fmt.Sprint(in[i][0]) != want[i] {
				t.Fatalf("%v：第 %d 行应为 %s，实际 %v", set, i, want[i], in[i][0])
			}
			if fmt.Sprint(or[i][0]) != want[i] {
				t.Fatalf("%v（OR 写法）：第 %d 行应为 %s，实际 %v", set, i, want[i], or[i][0])
			}
		}
	}
}

// NOT IN 语义是「不在集合里」，索引帮不上忙，必须退回全表扫（结果仍要对）。
func TestNotInFallsBack(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sql := `SELECT id FROM ob WHERE a NOT IN (1, 2)`
	if plan := explainPlan(t, d, s, sql); strings.Contains(plan, "Index") {
		t.Fatalf("NOT IN 不该走索引，实际: %s", plan)
	}
	if n := len(rowsOf(t, run(t, d, s, sql))); n != 160 {
		t.Fatalf("应 160 行，实际 %d", n)
	}
}

// IN 列表里的 NULL 恒不成立：跳过它，但其余值照常命中。
func TestInWithNullMember(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (1, NULL)`), []string{"20"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (NULL, NULL)`), []string{"0"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (1, NULL) OR a = 2`), []string{"40"})
}

// IN 和 OR 混用：一起合并成多段区间，不能因为分支形态不同就放弃。
func TestInMixedWithOr(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sql := `SELECT id FROM ob WHERE a IN (1, 2) OR a = 5`
	if plan := explainPlan(t, d, s, sql); !strings.Contains(plan, "Index Union Scan") {
		t.Fatalf("应合并成多区间扫描，实际: %s", plan)
	}
	if n := len(rowsOf(t, run(t, d, s, sql))); n != 60 {
		t.Fatalf("应 60 行，实际 %d", n)
	}
}

// IN + ORDER BY 命中索引列时照样免排序（多区间输出本来就是有序的）。
func TestInWithOrderBySkipsSort(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	sql := `SELECT a, b FROM ob WHERE a IN (3, 1, 2) ORDER BY a, b`
	plan := explainPlan(t, d, s, sql)
	if strings.Contains(plan, "Sort") {
		t.Fatalf("多区间仍有序，不该排序，实际: %s", plan)
	}
	rows := rowsOf(t, run(t, d, s, sql))
	if len(rows) != 60 {
		t.Fatalf("应 60 行，实际 %d", len(rows))
	}
	prevA, prevB := -1, -1
	for _, r := range rows {
		a, b := intOf(t, r[0]), intOf(t, r[1])
		if a < prevA || (a == prevA && b < prevB) {
			t.Fatalf("输出乱序：(%d,%d) 在 (%d,%d) 之后", a, b, prevA, prevB)
		}
		prevA, prevB = a, b
	}
}

// IN 落在复合索引的首列：每个值要覆盖整组，不能只匹配「键恰好等于该值」。
func TestInOnCompositeIndexPrefix(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	// a=1 有 20 行（b 从 0 到 19），若按精确键查只会命中 0 行
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (1)`), []string{"20"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (1, 4)`), []string{"40"})
	// 第二列不是索引首列，用不上：退回全表扫但结果要对
	sql := `SELECT count(*) FROM ob WHERE b IN (3, 4)`
	if plan := explainPlan(t, d, s, `SELECT id FROM ob WHERE b IN (3, 4)`); strings.Contains(plan, "Index") {
		t.Fatalf("b 不是索引首列，不该走索引，实际: %s", plan)
	}
	assertRows(t, run(t, d, s, sql), []string{"20"})
}

// 类型不同的常量要按列类型归一后再编码（'1' 与 1 对 int 列是同一个键）。
// 反过来（整数常量打 text 列）也成立：键必须和索引里存的一致，否则查不到行。
func TestInCastsConstants(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN ('1', '2')`), []string{"40"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE code IN ('c0', 'c1')`), []string{"134"})
	// 真的转不了类型时 SQL 本身就该报错（和有没有索引无关），这里只确认不会因为
	// 「先拿错类型的键去查索引」而静默返回 0 行
	if _, err := d.Exec(d.Session(s), `SELECT count(*) FROM ob WHERE a IN ('x')`); err == nil {
		t.Fatal("整数列和 'x' 比较应该报类型错误")
	}
}

// UPDATE / DELETE 的 WHERE 也吃 IN。
func TestInOnUpdateAndDelete(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	if res := run(t, d, s, `UPDATE ob SET note = 'in' WHERE id IN (2, 4, 6)`); res[0].Affected != 3 {
		t.Fatalf("UPDATE 应影响 3 行，实际 %d", res[0].Affected)
	}
	if res := run(t, d, s, `DELETE FROM ob WHERE a IN (7, 8)`); res[0].Affected != 40 {
		t.Fatalf("DELETE 应影响 40 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob`), []string{"160"})
}

// 事务内（有写集）IN 也要看到本事务的改动。
func TestInInsideTransaction(t *testing.T) {
	d := setupOrderTable(t)
	s := "s"
	run(t, d, s, `BEGIN`)
	run(t, d, s, `UPDATE ob SET a = 9 WHERE id = 2`) // 原 a=1
	run(t, d, s, `INSERT INTO ob VALUES (9997, 2, 0, 'c9', 'n')`)
	run(t, d, s, `DELETE FROM ob WHERE id = 4`) // 原 a=3

	// a=1 少了 id=2；a=3 少了 id=4；a=9 多了 id=2；a=2 多了 9997
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (1, 3)`), []string{"38"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (2)`), []string{"21"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE a IN (9)`), []string{"21"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM ob WHERE id IN (2, 4)`), []string{"1"})
	run(t, d, s, `COMMIT`)
}
