package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Top-N 的差分测试：`ORDER BY … LIMIT n` 与「全排序后取前 n 行」必须是同一批值。
//
// 为什么需要它：`sortOp` 的 Top-N 用的是自己手写的大顶堆（替换了 container/heap），
// 堆的不变量一旦写错，结果不是报错、而是「悄悄少了一行 / 多留了一行」——
// 和之前覆盖索引、MIN/MAX 那两个静默错误是同一类。这里用两条不同算法对同一份数据
// 做对拍：全排序（topN=0）走 sort.Slice，Top-N 走堆，两侧比较器相同、选择算法不同。
//
// 比较的是「值的多重集」而不是行序：重复值很多时，两条路径从同一组并列里挑哪几行
// 是允许不同的（SQL 不保证并列行的顺序），但挑出来的值必须一样。
func TestTopNMatchesFullSort(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE tk (id int PRIMARY KEY, a int, tag text)`)

	// a 只有 7 个取值：故意制造大量并列，逼堆走「等于堆顶」的分支。
	// 再掺 9 行 NULL，让 NULLS FIRST/LAST 这条也进差分。
	const rows = 3000
	for start := 1; start <= rows; start += 500 {
		var vals []string
		for i := start; i < start+500 && i <= rows; i++ {
			if i%997 == 0 {
				vals = append(vals, fmt.Sprintf("(%d, NULL, 'n%d')", i, i))
				continue
			}
			vals = append(vals, fmt.Sprintf("(%d, %d, 't%d')", i, i%7, i))
		}
		run(t, d, s, `INSERT INTO tk VALUES `+strings.Join(vals, ","))
	}

	// 参照实现：不限制行数 → 走全排序。
	refOf := func(order string) []string {
		rows := rowsOf(t, run(t, d, s, `SELECT a FROM tk ORDER BY a `+order))
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = fmt.Sprint(r[0])
		}
		return out
	}

	for _, order := range []string{"", "DESC"} {
		ref := refOf(order)
		if len(ref) != rows {
			t.Fatalf("参照结果应是 %d 行，实际 %d", rows, len(ref))
		}
		for _, limit := range []int{1, 2, 3, 7, 8, 50, 499, rows - 1, rows} {
			for _, offset := range []int{0, 1, 3, 2500} {
				if limit+offset > rows {
					continue
				}
				sqlText := fmt.Sprintf(`SELECT a FROM tk ORDER BY a %s LIMIT %d OFFSET %d`, order, limit, offset)
				got := rowsOf(t, run(t, d, s, sqlText))
				want := ref[offset : offset+limit]
				gotVals := make([]string, len(got))
				for i, r := range got {
					gotVals[i] = fmt.Sprint(r[0])
				}
				if len(gotVals) != len(want) {
					t.Fatalf("%s：期望 %d 行，实际 %d 行", sqlText, len(want), len(gotVals))
				}
				sort.Strings(gotVals)
				sorted := append([]string(nil), want...)
				sort.Strings(sorted)
				for i := range sorted {
					if gotVals[i] != sorted[i] {
						t.Fatalf("%s：第 %d 个值 %s，期望 %s\n实际 %v\n期望 %v",
							sqlText, i, gotVals[i], sorted[i], gotVals, sorted)
					}
				}
			}
		}
	}

	// 全部并列：任何 k 行都是合法答案，但必须正好 k 行、且都是同一个值。
	// 这条盯的是「堆满了以后还用不用继续比」——写错会少返回行。
	run(t, d, s, `CREATE TABLE tk2 (id int PRIMARY KEY, a int)`)
	var same []string
	for i := 1; i <= 200; i++ {
		same = append(same, fmt.Sprintf("(%d, 5)", i))
	}
	run(t, d, s, `INSERT INTO tk2 VALUES `+strings.Join(same, ","))
	for _, limit := range []int{1, 2, 3, 199, 200} {
		got := rowsOf(t, run(t, d, s, fmt.Sprintf(`SELECT a FROM tk2 ORDER BY a LIMIT %d`, limit)))
		if len(got) != limit {
			t.Fatalf("全并列 LIMIT %d：期望 %d 行，实际 %d 行", limit, limit, len(got))
		}
		for _, r := range got {
			if fmt.Sprint(r[0]) != "5" {
				t.Fatalf("全并列 LIMIT %d：出现了非 5 的值 %v", limit, r[0])
			}
		}
	}

	// 空结果与「k 大于行数」：不能 panic、不能多吐行。
	if got := rowsOf(t, run(t, d, s, `SELECT a FROM tk2 WHERE id > 99999 ORDER BY a LIMIT 5`)); len(got) != 0 {
		t.Fatalf("空表结果应为 0 行，实际 %d", len(got))
	}
	if got := rowsOf(t, run(t, d, s, `SELECT a FROM tk2 ORDER BY a LIMIT 1000`)); len(got) != 200 {
		t.Fatalf("LIMIT 超过行数时应返回 200 行，实际 %d", len(got))
	}
}
