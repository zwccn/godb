package db

import (
	"testing"
)

// GROUP BY 要认「输出别名」和「序号」，这是 PG 的两条基本写法。
//
// 以前两种都不认：`GROUP BY 1` 被当成常量 1（整表一组），接着在投影里报一句
// 「column id must appear in the GROUP BY clause」；`GROUP BY k` 直接报
// 「column "k" does not exist」。
func TestGroupByAliasAndOrdinal(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE g (id int PRIMARY KEY, name text)`)
	run(t, d, s, `INSERT INTO g VALUES (1,'a'),(2,'b'),(3,'a'),(4,'c'),(5,'b'),(6,'a')`)

	// 基准：直接按输入列分组
	want := rowsOf(t, run(t, d, s, `SELECT name, count(*) FROM g GROUP BY name ORDER BY name`))
	if len(want) != 3 {
		t.Fatalf("基准分组数 = %d，期望 3：%v", len(want), want)
	}

	// 1) 输出别名
	assertSameRows(t, want, rowsOf(t, run(t, d, s,
		`SELECT name AS c, count(*) FROM g GROUP BY c ORDER BY c`)), "GROUP BY 别名")

	// 2) 序号
	assertSameRows(t, want, rowsOf(t, run(t, d, s,
		`SELECT name AS c, count(*) FROM g GROUP BY 1 ORDER BY 1`)), "GROUP BY 序号")

	// 3) 别名指向的是表达式（以前这条直接报错）
	got := rowsOf(t, run(t, d, s,
		`SELECT id % 2 AS k, count(*) FROM g GROUP BY k ORDER BY k`))
	if len(got) != 2 {
		t.Fatalf("按表达式别名分组应得 2 组，实际 %d：%v", len(got), got)
	}
	// id 1..6 的奇偶各 3 行
	for _, r := range got {
		if fmtRow(r) != "1|3" && fmtRow(r) != "0|3" {
			t.Fatalf("分组结果不对：%v", got)
		}
	}

	// 4) 序号越界要明确报错，而不是当成常量
	if err := runErr(t, d, s, `SELECT name, count(*) FROM g GROUP BY 5`); err == nil {
		t.Fatal("GROUP BY 5 应该报错")
	}
}

// 裸名字既是输入列又是输出列时，GROUP BY 按**输入列**解释（PG 文档原话），
// ORDER BY 则相反、按输出列。这条容易写反，所以单独钉一次。
func TestGroupByPrefersInputColumn(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE g2 (id int PRIMARY KEY, name text)`)
	// 'a' 和 'A' 是两行：按输入列 name 分组 = 2 组；若按输出别名 upper(name) 分组 = 1 组
	run(t, d, s, `INSERT INTO g2 VALUES (1,'a'),(2,'A')`)

	got := rowsOf(t, run(t, d, s,
		`SELECT upper(name) AS name, count(*) FROM g2 GROUP BY name ORDER BY 2 DESC`))
	if len(got) != 2 {
		t.Fatalf("应按输入列分成 2 组，实际 %d 组：%v", len(got), got)
	}
}

// assertSameRows 比较两份结果的行内容（顺序由调用方的 ORDER BY 决定）。
func assertSameRows(t *testing.T, want, got [][]interface{}, what string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s：行数 %d，期望 %d\n实际 %v\n期望 %v", what, len(got), len(want), got, want)
	}
	for i := range want {
		if fmtRow(got[i]) != fmtRow(want[i]) {
			t.Fatalf("%s：第 %d 行 %q，期望 %q", what, i, fmtRow(got[i]), fmtRow(want[i]))
		}
	}
}
