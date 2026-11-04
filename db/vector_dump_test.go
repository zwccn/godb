package db

import (
	"strings"
	"testing"
)

// 备份导出 → 恢复：向量列必须原样回来。
//
// dump 走的是 SELECT *（投影层会把槽位号换成 `[1,2,3]` 文本），所以这条路本来
// 就是对的；但没有测试钉住，谁把导出改成直接读行就会静默退化成 `[vector:0]`。
// 恢复端则是把整份 SQL 喂回 Exec —— 文本字面量靠隐式转换进 vector 列。
func TestDumpRestoreVector(t *testing.T) {
	src := newTestDB(t)
	s := "s"
	run(t, src, s, `CREATE TABLE dv (id int PRIMARY KEY, name text, v vector(4))`)
	run(t, src, s, `INSERT INTO dv VALUES (1,'a','[1,2,3,4]'), (2,'b','[0.5,0.25,0,0]')`)

	dump, err := src.DumpSQL("test")
	if err != nil {
		t.Fatalf("DumpSQL: %v", err)
	}
	for _, want := range []string{`"v" vector(4)`, `'[1,2,3,4]'`, `'[0.5,0.25,0,0]'`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("导出内容里缺少 %q", want)
		}
	}
	if strings.Contains(dump, "vector:") {
		t.Fatal("导出内容里出现了内部引用 [vector:N]")
	}

	dst := newTestDB(t)
	r := "r"
	if _, err := dst.Exec(dst.Session(r), dump); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	rows := rowsOf(t, run(t, dst, r, `SELECT id, name, v FROM dv ORDER BY id`))
	if len(rows) != 2 {
		t.Fatalf("恢复出 %d 行，期望 2", len(rows))
	}
	if got := rows[0][2]; got != "[1,2,3,4]" {
		t.Fatalf("第 1 行向量 = %v，期望 [1,2,3,4]", got)
	}
	if got := rows[1][2]; got != "[0.5,0.25,0,0]" {
		t.Fatalf("第 2 行向量 = %v，期望 [0.5,0.25,0,0]", got)
	}
	// 恢复后还能继续按距离检索（说明列文件也建对了，不只是文本对）。
	res := run(t, dst, r, `SELECT id FROM dv ORDER BY v <=> '[1,2,3,4]' LIMIT 1`)
	if got := rowsOf(t, res)[0][0]; got != int64(1) {
		t.Fatalf("最近的一条 id = %v，期望 1", got)
	}
}
