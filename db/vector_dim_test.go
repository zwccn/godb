package db

import (
	"fmt"
	"strings"
	"testing"
)

// 向量列的维度是物理布局，改它有两种截然不同的结局：
//
//   - 表里还有行：拒绝。每行存的是 dim 个 float32，没法就地「扩容」；
//     以前报的是 Cast 里的 `column is 4, value has 3`，看着像数据坏了。
//   - 表是空的：允许，但必须**换一个 .vec 文件**。文件头写的是旧维度，
//     OpenVecFile 会拒绝打开维度不一致的文件 —— 不换的话运行期插入还按旧维度
//     校验，重启后这一列干脆打不开（按 NULL 处理，像数据全丢了）。
func TestAlterVectorDimension(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := "s"
	run(t, d, s, `CREATE TABLE vdim (id int PRIMARY KEY, v vector(3))`)
	run(t, d, s, `INSERT INTO vdim VALUES (1,'[1,2,3]')`)

	e := runErr(t, d, s, `ALTER TABLE vdim ALTER COLUMN v TYPE vector(4)`)
	if !strings.Contains(e.Error(), "cannot change vector dimension") {
		t.Fatalf("有数据时改维度应给出专门报错，实际: %v", e)
	}
	// 拒绝之后数据要原样还在。
	if got := fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT v FROM vdim`))[0][0]); got != "[1,2,3]" {
		t.Fatalf("拒绝之后向量被动了: %q", got)
	}

	// 空表：改维度要真的生效（换文件 + 改目录），插入新维度、拒绝旧维度。
	run(t, d, s, `CREATE TABLE vempty (id int PRIMARY KEY, v vector(3))`)
	run(t, d, s, `ALTER TABLE vempty ALTER COLUMN v TYPE vector(4)`)
	run(t, d, s, `INSERT INTO vempty VALUES (1,'[1,2,3,4]')`)
	if err := runErr(t, d, s, `INSERT INTO vempty VALUES (2,'[1,2,3]')`); !strings.Contains(err.Error(), "column is 4") {
		t.Fatalf("旧维度应被拒绝且报列是 4 维，实际: %v", err)
	}
	if got := fmt.Sprint(rowsOf(t, run(t, d, s, `SELECT v FROM vempty`))[0][0]); got != "[1,2,3,4]" {
		t.Fatalf("新维度写入后读回 %q", got)
	}

	// 重启：新维度要继续可读可写（文件头必须是 4，否则这一列会打不开）。
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { d2.Close() })
	if got := fmt.Sprint(rowsOf(t, run(t, d2, s, `SELECT v FROM vempty`))[0][0]); got != "[1,2,3,4]" {
		t.Fatalf("重启后向量读回 %q", got)
	}
	run(t, d2, s, `INSERT INTO vempty VALUES (3,'[9,9,9,9]')`)
	if got := len(rowsOf(t, run(t, d2, s, `SELECT id FROM vempty`))); got != 2 {
		t.Fatalf("重启后应能继续写入，行数 %d", got)
	}
}
