package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 数据文件被删掉之后，这张表必须「打不开」，而不是变成 0 行的空表。
//
// 这条守的是最危险的一种状态：看着完全正常的空表。一旦有人往里写，下一次落盘
// 就会用新的空页把原来的位置占掉 ——「这里曾经有数据」从此没有任何证据。
func TestMissingDataFileIsNotAnEmptyTable(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d, "s", `CREATE TABLE keep (id int4 PRIMARY KEY, v text)`)
	run(t, d, "s", `INSERT INTO keep VALUES (1, 'hello'), (2, 'world')`)
	tbl, ok := d.Cat.Get("keep")
	if !ok {
		t.Fatal("建表之后 catalog 里应该有 keep")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 把页文件挪走，模拟「文件丢了」
	dat := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.dat", tbl.ID))
	if err := os.Remove(dat); err != nil {
		t.Fatalf("remove %s: %v", dat, err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { d2.Close() })

	// 查：报错，不能返回 0 行
	e := runErr(t, d2, "s", `SELECT count(*) FROM keep`)
	if !strings.Contains(e.Error(), "数据文件丢失") {
		t.Fatalf("期望「数据文件丢失」错误，实际 %v", e)
	}
	// 写：一样拦住
	runErr(t, d2, "s", `INSERT INTO keep VALUES (3, 'x')`)
	// 收拾残局的动作要放行，否则这张表永远删不掉
	run(t, d2, "s", `DROP TABLE keep`)
}
