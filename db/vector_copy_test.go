package db

import (
	"fmt"
	"strings"
	"testing"
)

// COPY 是「把表倒出来再灌回去」的通道，向量列必须导成 `[1,2,3]` 而不是内部引用。
//
// 实测过：CopyOutText 走的是原始的 row[ci].TextForm()，行里存的是槽位号，
// 于是导出成 `1	[vector:0]` —— 再 COPY FROM 回来会报
// `invalid input syntax for type vector: "[vector:0]"`（好在是报错，不是静默写坏）。
func TestCopyVector(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE cpv (id int4 PRIMARY KEY, v vector(3))`)
	run(t, d, s, `INSERT INTO cpv VALUES (1,'[1,2,3]'), (2,'[4,5,6]')`)

	lines, n, err := d.CopyOut(d.Session(s), sqlCopyFor("cpv", true))
	if err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	if n != 2 || len(lines) != 2 {
		t.Fatalf("导出 %d 行 / %d 条，期望 2", n, len(lines))
	}
	for _, l := range lines {
		if strings.Contains(l, "vector:") {
			t.Fatalf("导出了内部引用而不是向量内容: %q", l)
		}
	}
	if !strings.Contains(lines[0], "[1,2,3]") {
		t.Fatalf("第一行应是 [1,2,3]，实际 %q", lines[0])
	}

	// 导出 → 清空 → 导回，内容要一模一样
	run(t, d, s, `DELETE FROM cpv`)
	cnt, err := d.CopyIn(d.Session(s), sqlCopyFor("cpv", false), []byte(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("导回 %d 行，期望 2", cnt)
	}
	res := run(t, d, s, `SELECT id, v FROM cpv ORDER BY id`)
	got := fmt.Sprint(res[0].Rows)
	if !strings.Contains(got, "[1,2,3]") || !strings.Contains(got, "[4,5,6]") {
		t.Fatalf("往返后的内容不对: %v", got)
	}
}
