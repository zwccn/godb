package db

import (
	"fmt"
	"strings"
	"testing"
)

// 规划开销的回归基准。用 `EXPLAIN`（只规划、不执行）把「每条语句都要付的固定成本」
// 单独拎出来量：
//
//	go test ./db/ -run XXX -bench Plan -benchtime 3000x -cpuprofile plan.prof
//
// 为什么值得留一条基准：2026-11 之前这里每条语句要 115 µs，pprof 一看，86% 花在
// `tableSchema → syscat.RelationOID → buildMeta` —— 为了拿一个表 OID，把所有表、
// 所有列重新算了一遍（类型名转小写、查 OID、算 typlen）。修掉之后是 15 µs。
// 以后谁再往这条路径上加东西，这两个数字会立刻报警。
func benchDB(b *testing.B) *DB {
	b.Helper()
	d, err := Open(b.TempDir(), testOptions())
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { d.Close() })
	sess := d.Session("s")
	must := func(q string) {
		if _, err := d.Exec(sess, q); err != nil {
			b.Fatalf("exec %q: %v", q, err)
		}
	}
	must(`CREATE TABLE big (id int4 PRIMARY KEY, g int4, v int4)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO big VALUES `)
	for i := 1; i <= 2000; i++ {
		if i > 1 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "(%d,%d,%d)", i, i%7, i*3)
	}
	must(sb.String())
	return d
}

func BenchmarkPlanSimple(b *testing.B) {
	d := benchDB(b)
	sess := d.Session("s")
	q := `EXPLAIN SELECT * FROM big WHERE v > 59985 LIMIT 100`
	if _, err := d.Exec(sess, q); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(sess, q); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}

func BenchmarkPlanJoin(b *testing.B) {
	d := benchDB(b)
	sess := d.Session("s")
	if _, err := d.Exec(sess, `CREATE TABLE dept (id int4 PRIMARY KEY, name text)`); err != nil {
		b.Fatalf("create dept: %v", err)
	}
	if _, err := d.Exec(sess, `INSERT INTO dept VALUES (1,'a'),(2,'b'),(3,'c')`); err != nil {
		b.Fatalf("insert dept: %v", err)
	}
	q := `EXPLAIN SELECT g, count(*) AS n FROM big JOIN dept ON big.g = dept.id GROUP BY g ORDER BY n DESC LIMIT 5`
	if _, err := d.Exec(sess, q); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(sess, q); err != nil {
			b.Fatalf("exec: %v", err)
		}
	}
}
