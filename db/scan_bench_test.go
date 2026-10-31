package db

import (
	"fmt"
	"strings"
	"testing"
)

// 扫描路径的基准：20 万行，缓冲池故意压到 64 页（512KB），逼出「换页 + 重新解码」。
//
// 为什么用基准而不是测试：这条路径的差别只有几毫秒到几十毫秒，靠断言测不出来，
// 但改一行解码代码就可能翻倍。基准不参与 `go test`，只在想看数字时跑：
//
//	go test ./db/ -bench BenchmarkScanEvicted -benchtime 5x
//
// 三个子基准分别对应「全驻内存 / 换出后扫描 / 换出后只取一列」，方便判断优化
// 到底落在哪：
//   - resident ：池子能装下整表，行都在 rows[] 里，纯粹是执行器开销；
//   - evicted  ：池子只有 64 页，扫描要反复「取页 → 解码 → 再被淘汰」；
//   - narrow   ：同样换出，但只取一列，用来区分「行解码成本」和「表达式求值成本」。
func BenchmarkScanEvicted(b *testing.B) {
	dir := b.TempDir()

	open := func(poolPages int) *DB {
		d, err := Open(dir, Options{BufferPoolPages: poolPages, Lock: testOptions().Lock})
		if err != nil {
			b.Fatal(err)
		}
		return d
	}
	exec := func(d *DB, sql string) {
		if _, err := d.Exec(d.Session("bench"), sql); err != nil {
			b.Fatalf("exec %q: %v", sql, err)
		}
	}

	// 建数据（只做一次）
	b.StopTimer()
	{
		d := open(1024)
		exec(d, `CREATE TABLE big (id INTEGER PRIMARY KEY, name TEXT, v TEXT)`)
		pad := strings.Repeat("v", 16)
		const n = 200000
		for start := 1; start <= n; start += 5000 {
			var sb strings.Builder
			sb.WriteString("INSERT INTO big VALUES ")
			for i := start; i < start+5000 && i <= n; i++ {
				if i > start {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, "(%d,'名字%06d','%s-%06d')", i, i, pad, i)
			}
			exec(d, sb.String())
		}
		exec(d, `CHECKPOINT`)
		if err := d.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StartTimer()

	const wide = `SELECT count(*), sum(id) FROM big`
	const narrow = `SELECT count(*) FROM big`

	bench := func(name string, poolPages int, sql string) {
		b.Run(name, func(b *testing.B) {
			d := open(poolPages)
			defer d.Close()
			exec(d, sql) // 预热：加载行、打开索引
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				exec(d, sql)
			}
		})
	}

	bench("resident", 4096, wide) // 装得下整表
	bench("evicted", 64, wide)    // 512KB，装不下
	bench("narrow", 64, narrow)   // 只看一列
}
