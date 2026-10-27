package db

import (
	"fmt"
	"strings"
	"testing"
)

// setupRange 建一张 100 行的表：qty = id*10，code = 'c' + (id%10)。
func setupRange(t *testing.T) (*DB, string) {
	t.Helper()
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE rng (id int4 PRIMARY KEY, code text, qty int4)`)
	for i := 1; i <= 100; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO rng VALUES (%d,'c%d',%d)`, i, i%10, i*10))
	}
	run(t, d, s, `CREATE INDEX rng_qty ON rng (qty)`)
	run(t, d, s, `CREATE INDEX rng_code ON rng (code)`)
	return d, s
}

func planOf(t *testing.T, d *DB, s, sqlText string) string {
	t.Helper()
	return fmt.Sprint(rowsOf(t, run(t, d, s, "EXPLAIN "+sqlText)))
}

// 各种范围写法都要走区间扫描，而不是全表扫。
func TestRangeScanPlans(t *testing.T) {
	d, s := setupRange(t)
	cases := []string{
		`SELECT * FROM rng WHERE qty > 500`,
		`SELECT * FROM rng WHERE qty >= 500`,
		`SELECT * FROM rng WHERE qty < 100`,
		`SELECT * FROM rng WHERE qty <= 100`,
		`SELECT * FROM rng WHERE qty BETWEEN 200 AND 300`,
		`SELECT * FROM rng WHERE 300 > qty`, // 常量在左边
		`SELECT * FROM rng WHERE code LIKE 'c3%'`,
		`SELECT * FROM rng WHERE id BETWEEN 10 AND 20`, // 主键范围
	}
	for _, q := range cases {
		plan := planOf(t, d, s, q)
		if !strings.Contains(plan, "Index Range Scan") {
			t.Fatalf("%s 应该走区间扫描，实际计划: %s", q, plan)
		}
	}
	// 没有索引的列仍然全表扫
	plan := planOf(t, d, s, `SELECT * FROM rng WHERE id + 0 > 5`)
	if !strings.Contains(plan, "Seq Scan") {
		t.Fatalf("表达式条件不该用索引，实际计划: %s", plan)
	}
}

// 区间边界：开闭必须分清，漏一行多一行都算错。
func TestRangeScanBounds(t *testing.T) {
	d, s := setupRange(t)
	// qty 是 10,20,...,1000
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 500`), []string{"50"})  // 510..1000
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty >= 500`), []string{"51"}) // 500..1000
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty < 500`), []string{"49"})  // 10..490
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty <= 500`), []string{"50"}) // 10..500
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty BETWEEN 100 AND 200`), []string{"11"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 100 AND qty < 200`), []string{"9"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty >= 100 AND qty <= 200`), []string{"11"})
	// 范围末尾
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty >= 1000`), []string{"1"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 1000`), []string{"0"})
	// 完全落空的区间不能误返回
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 5000`), []string{"0"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty < 0`), []string{"0"})
	// 边界值取到正确的行
	assertRows(t, run(t, d, s, `SELECT id, qty FROM rng WHERE qty <= 20 ORDER BY qty`),
		[]string{"1|10", "2|20"})
	// 常量在左边也要算对
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE 500 < qty`), []string{"50"})
}

// LIKE 'x%' 走前缀区间：要包含前缀本身，排除更短/相邻的串。
func TestPrefixScan(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE px (id int4 PRIMARY KEY, name text)`)
	for i, v := range []string{"abc", "abcd", "abc-zzz", "ab", "abd", "abz", "b", "abc中文"} {
		run(t, d, s, fmt.Sprintf(`INSERT INTO px VALUES (%d,'%s')`, i+1, v))
	}
	run(t, d, s, `CREATE INDEX px_name ON px (name)`)

	plan := planOf(t, d, s, `SELECT * FROM px WHERE name LIKE 'abc%'`)
	if !strings.Contains(plan, "Index Range Scan using px_name") {
		t.Fatalf("前缀匹配应走区间扫描，实际计划: %s", plan)
	}
	got := rowsOf(t, run(t, d, s, `SELECT name FROM px WHERE name LIKE 'abc%' ORDER BY name`))
	if len(got) != 4 {
		t.Fatalf("应命中 4 行，实际 %v", got)
	}
	for _, r := range got {
		if !strings.HasPrefix(fmt.Sprint(r[0]), "abc") {
			t.Fatalf("命中了不该命中的行: %v", r)
		}
	}
	// 中间的 % / _ 不能当前缀处理（会漏行），必须退回全表扫
	for _, q := range []string{
		`SELECT count(*) FROM px WHERE name LIKE '%bc'`,
		`SELECT count(*) FROM px WHERE name LIKE 'a%c'`,
		`SELECT count(*) FROM px WHERE name LIKE 'a_c'`,
	} {
		if strings.Contains(planOf(t, d, s, q), "Index Range Scan using px_name") {
			t.Fatalf("%s 不该用前缀索引", q)
		}
		run(t, d, s, q) // 结果正确性由全表扫保证
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM px WHERE name LIKE '%bc'`), []string{"1"})
}

// 复合索引 (a,b) 上只用首列的范围：键是 enc(a)+enc(b)，所以 a 的区间必须
// 精确落在前缀上（含 a 的所有 b，且不越到下一个 a）。
func TestRangeOnCompositeFirstColumn(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE comp (a int4, b int4, v text)`)
	for _, p := range [][2]int{{1, 1}, {1, 2}, {1, 3}, {2, 1}, {2, 2}, {3, 1}, {3, 5}} {
		run(t, d, s, fmt.Sprintf(`INSERT INTO comp VALUES (%d,%d,'x')`, p[0], p[1]))
	}
	run(t, d, s, `CREATE INDEX comp_ab ON comp (a, b)`)

	plan := planOf(t, d, s, `SELECT * FROM comp WHERE a >= 2`)
	if !strings.Contains(plan, "Index Range Scan using comp_ab") {
		t.Fatalf("复合索引首列范围应走区间扫描，实际计划: %s", plan)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a >= 2`), []string{"4"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a > 1`), []string{"4"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a = 1`), []string{"3"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a < 2`), []string{"3"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a >= 2 AND a <= 3`), []string{"4"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a > 3`), []string{"0"})
	// 首列范围 + 第二列条件：索引只管 a，b 由 filter 判
	assertRows(t, run(t, d, s, `SELECT count(*) FROM comp WHERE a >= 1 AND b = 1`), []string{"3"})
}

// 事务里未提交的增删改必须对区间扫描可见。
func TestRangeScanSeesOwnChanges(t *testing.T) {
	d, s := setupRange(t)
	run(t, d, s, `BEGIN`)
	// 新插入的行落在区间内
	run(t, d, s, `INSERT INTO rng VALUES (200, 'c9', 2000)`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 1500`), []string{"1"})
	// 未提交的删除
	run(t, d, s, `DELETE FROM rng WHERE id = 100`)                                           // qty = 1000
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty >= 1000`), []string{"1"}) // 只剩新插的 2000
	// 改键：行从区间里移出去（qty 10 → 5000）
	run(t, d, s, `UPDATE rng SET qty = 5000 WHERE id = 1`)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty < 100`), []string{"8"})
	// 改键：行移进区间
	run(t, d, s, `UPDATE rng SET qty = 1600 WHERE id = 2`)
	// 2000（新插）+ 5000（改出去的）+ 1600（改进来的）
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 1500`), []string{"3"})
	run(t, d, s, `ROLLBACK`)

	// 回滚后恢复原样
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 1500`), []string{"0"})
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty < 100`), []string{"9"}) // 10..90
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng`), []string{"100"})
}

// 区间扫描 + 残余条件：索引只缩小候选，其余条件由 filter 兜住，结果不能多也不能少。
func TestRangeScanWithResidualFilter(t *testing.T) {
	d, s := setupRange(t)
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 500 AND code = 'c1'`), []string{"5"})
	assertRows(t, run(t, d, s, `SELECT id FROM rng WHERE qty BETWEEN 90 AND 110 AND code = 'c0' ORDER BY id`),
		[]string{"10"}) // 90/110 分别是 c9/c1，只有 100 是 c0
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 900 OR id = 1`), []string{"11"})
}

// 重启后（索引重建）范围扫描仍然正确。
func TestRangeScanAfterRestart(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	s := "s"
	run(t, d, s, `CREATE TABLE rr (id int4 PRIMARY KEY, qty int4)`)
	for i := 1; i <= 50; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO rr VALUES (%d,%d)`, i, i*10))
	}
	run(t, d, s, `CREATE INDEX rr_qty ON rr (qty)`)
	run(t, d, s, `DELETE FROM rr WHERE id % 2 = 0`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT count(*) FROM rr WHERE qty > 200`), []string{"15"}) // 21..49 奇数
	assertRows(t, run(t, d2, s, `SELECT count(*) FROM rr WHERE qty BETWEEN 100 AND 200`), []string{"5"})
	plan := fmt.Sprint(rowsOf(t, run(t, d2, s, `EXPLAIN SELECT * FROM rr WHERE qty > 200`)))
	if !strings.Contains(plan, "Index Range Scan using rr_qty") {
		t.Fatalf("重启后仍应能走区间扫描，计划: %s", plan)
	}
}

// UPDATE / DELETE 的 WHERE 是范围条件时也走区间扫描。
func TestRangeScanForDML(t *testing.T) {
	d, s := setupRange(t)
	res := run(t, d, s, `UPDATE rng SET code = 'updated' WHERE qty BETWEEN 100 AND 200`)
	if res[0].Affected != 11 {
		t.Fatalf("按范围更新应影响 11 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE code = 'updated'`), []string{"11"})

	res = run(t, d, s, `DELETE FROM rng WHERE qty > 900`)
	if res[0].Affected != 10 {
		t.Fatalf("按范围删除应影响 10 行，实际 %d", res[0].Affected)
	}
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng`), []string{"90"})
	// 删完之后区间扫描不能捞到已删的行
	assertRows(t, run(t, d, s, `SELECT count(*) FROM rng WHERE qty > 900`), []string{"0"})
}
