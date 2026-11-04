package db

import (
	"fmt"
	"strings"
	"testing"
)

// 向量 Top-K 的并行求值必须和串行版给出**完全相同**的结果。
//
// 并行只改变「什么时候算距离」，不改变入堆顺序 —— 所以并列值在边界上的取舍
// 也应该一致。这条用「不带 LIMIT 的全量排序」（走串行）+ 取前 N 行当参照。
func TestVectorTopKParallelSameAsSerial(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE pv (id int PRIMARY KEY, v vector(8))`)

	// 4000 行 > 并行批大小的一倍，保证真的走了并行分支（见 sortParBatch）。
	const rows = 4000
	var b strings.Builder
	b.WriteString(`INSERT INTO pv VALUES `)
	for i := 1; i <= rows; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		// 第 0 维带 i*0.001，保证距离互不相等（避免并列导致顺序不可比）。
		fmt.Fprintf(&b, `(%d,'[%f,%d,%d,0,0,0,0,0]')`, i, float64(i)*0.001, i%7, (i*3)%11)
	}
	run(t, d, s, b.String())

	q := `'[0.5,3,5,0,0,0,0,0]'`
	const k = 5

	// 参照：全量排序（topN=0 → 串行），取前 k 个 id。
	ref := rowsOf(t, run(t, d, s, `SELECT id FROM pv ORDER BY v <=> `+q))
	want := make([]string, 0, k)
	for i := 0; i < k; i++ {
		want = append(want, fmt.Sprint(ref[i][0]))
	}

	// 并行：Top-K 走 sortOp 的并行分支。
	got := rowsOf(t, run(t, d, s, fmt.Sprintf(`SELECT id FROM pv ORDER BY v <=> %s LIMIT %d`, q, k)))
	if len(got) != k {
		t.Fatalf("Top-%d 返回 %d 行", k, len(got))
	}
	for i := 0; i < k; i++ {
		if g := fmt.Sprint(got[i][0]); g != want[i] {
			t.Fatalf("第 %d 名不一致：并行 %s，串行 %s（全量 %v / TopK %v）", i+1, g, want[i], want, got)
		}
	}

	// 计划里要能看出并行，否则读者会以为这几百毫秒是魔法。
	plan := explainPlan(t, d, s, fmt.Sprintf(`SELECT id FROM pv ORDER BY v <=> %s LIMIT %d`, q, k))
	if !strings.Contains(plan, "并行求值") {
		t.Fatalf("计划里没有并行求值说明：%s", plan)
	}

	// 其它距离算子也照旧（只有 <-> 不是特例）。
	for _, op := range []string{"<->", "<=>", "<#>"} {
		rows := rowsOf(t, run(t, d, s, fmt.Sprintf(`SELECT id FROM pv ORDER BY v %s %s LIMIT %d`, op, q, k)))
		if len(rows) != k {
			t.Fatalf("%s 的 Top-%d 返回 %d 行", op, k, len(rows))
		}
	}
}
