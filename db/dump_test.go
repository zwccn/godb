package db

import (
	"strings"
	"testing"
)

// 备份导出的验收标准只有一条：**导出去、灌回来，数据得一样**。
// 所以这个测试不看 SQL 文本长什么样（那太脆），而是把它真的执行到一个空库里再比对。
func TestDumpRestore(t *testing.T) {
	src := newTestDB(t)
	run(t, src, "s", `CREATE TABLE t (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		amount NUMERIC(10, 2),
		ok BOOLEAN,
		at TIMESTAMP
	)`)
	run(t, src, "s", `INSERT INTO t VALUES
		(1, 'plain', 12.50, true, '2024-01-02 03:04:05'),
		(2, 'O''Reilly', -3.25, false, NULL),
		(3, E'line1\nline2', NULL, NULL, '2024-12-31 23:59:59.123456')`)
	run(t, src, "s", `CREATE INDEX ix_t_name ON t (name)`)

	// 自增列单独一张表：恢复后必须还能继续插（序列要跟着对齐）
	run(t, src, "s", `CREATE TABLE s (id SERIAL PRIMARY KEY, v TEXT)`)
	run(t, src, "s", `INSERT INTO s (v) VALUES ('a'), ('b'), ('c')`)

	dump, err := src.DumpSQL("testdb")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	for _, want := range []string{
		`CREATE TABLE "t"`, `CREATE TABLE "s"`, `INSERT INTO "t"`,
		`CREATE INDEX "ix_t_name"`, `setval('s_id_seq', 3, true)`,
		// 引号要按 SQL 规则翻倍，否则恢复回来就成了截断的字符串
		`'O''Reilly'`,
	} {
		if !strings.Contains(dump, want) {
			t.Fatalf("导出里缺少 %q：\n%s", want, dump)
		}
	}

	// 灌到一个干净库
	dst := newTestDB(t)
	if _, err := dst.Exec(dst.Session("r"), dump); err != nil {
		t.Fatalf("恢复失败：%v\n---\n%s", err, dump)
	}

	// 逐行比对（按 id 排序，值用文本形式比，避免 float/decimal 的表示差异）
	srcRows := rowsOf(t, run(t, src, "s", `SELECT id, name, amount, ok, at FROM t ORDER BY id`))
	dstRows := rowsOf(t, run(t, dst, "r", `SELECT id, name, amount, ok, at FROM t ORDER BY id`))
	if len(srcRows) != len(dstRows) {
		t.Fatalf("行数不一致：%d vs %d", len(srcRows), len(dstRows))
	}
	for i := range srcRows {
		if len(srcRows[i]) != len(dstRows[i]) {
			t.Fatalf("第 %d 行列数不一致", i)
		}
		for c := range srcRows[i] {
			a, b := sqlLiteral(srcRows[i][c]), sqlLiteral(dstRows[i][c])
			if a != b {
				t.Fatalf("第 %d 行第 %d 列不一致：%s vs %s", i, c, a, b)
			}
		}
	}

	// 恢复后自增要能接着用（序列没对齐的话这里会撞主键）
	run(t, dst, "r", `INSERT INTO s (v) VALUES ('d')`)
	got := rowsOf(t, run(t, dst, "r", `SELECT id FROM s ORDER BY id`))
	if len(got) != 4 {
		t.Fatalf("恢复后插入失败，行数 = %d", len(got))
	}
	last := got[3][0]
	if n, ok := last.(int64); !ok || n != 4 {
		t.Fatalf("自增从 %v 开始，期望 4", last)
	}

	// 索引也要在（不然恢复出来的库慢得莫名其妙）
	var hasIx bool
	for _, ix := range dst.Cat.Tables[0].Indexes {
		if ix.Name == "ix_t_name" {
			hasIx = true
		}
	}
	if !hasIx {
		t.Fatalf("索引没恢复出来")
	}
}

// 空库导出不该报错，也不该产出半截语句：只留文件头。
func TestDumpEmpty(t *testing.T) {
	d := newTestDB(t)
	dump, err := d.DumpSQL("empty")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if strings.Contains(dump, "CREATE TABLE") || strings.Contains(dump, "INSERT INTO") {
		t.Fatalf("空库不该有建表/插入语句：\n%s", dump)
	}
	if !strings.Contains(dump, "godb 备份") {
		t.Fatalf("缺少文件头：\n%s", dump)
	}
}
