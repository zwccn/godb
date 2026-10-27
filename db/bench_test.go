package db

import (
	"fmt"
	"testing"
	"time"

	"godb/lock"
	"godb/txn"
)

func benchOptions(iso txn.Isolation) Options {
	return Options{
		Isolation: iso,
		Lock: lock.Options{
			Timeout:        10 * time.Second,
			Retry:          5 * time.Millisecond,
			DeadlockDetect: true,
			DeadlockAfter:  2 * time.Second,
		},
	}
}

// setupBench 建一张 rows 行的表，返回数据库。
func setupBench(b *testing.B, iso txn.Isolation, rows int) *DB {
	b.Helper()
	d, err := Open(b.TempDir(), benchOptions(iso))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { d.Close() })
	sess := d.Session("setup")
	mustExec(b, d, sess, `CREATE TABLE bench (id SERIAL PRIMARY KEY, v INTEGER, t TEXT)`)
	var buf []byte
	buf = append(buf, "INSERT INTO bench (v, t) VALUES "...)
	for i := 0; i < rows; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, fmt.Sprintf("(%d,'row-%d')", i, i)...)
	}
	buf = append(buf, ';')
	mustExec(b, d, sess, string(buf))
	return d
}

func mustExec(b *testing.B, d *DB, sess *txn.Session, q string) {
	b.Helper()
	if _, err := d.Exec(sess, q); err != nil {
		b.Fatalf("exec %q: %v", q, err)
	}
}

const benchQuery = `SELECT count(*), sum(v) FROM bench WHERE v > 10`

// 单会话：对比两种并发控制模式的固定开销。
func BenchmarkSelectSerial(b *testing.B) {
	d := setupBench(b, txn.SerialLock, 2000)
	sess := d.Session("s")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustExec(b, d, sess, benchQuery)
	}
}

func BenchmarkSelectTableLock(b *testing.B) {
	d := setupBench(b, txn.TableLock, 2000)
	sess := d.Session("s")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustExec(b, d, sess, benchQuery)
	}
}

// 并发纯读：表级锁下读读相容，吞吐应随并发上升。
func BenchmarkSelectParallel(b *testing.B) {
	d := setupBench(b, txn.TableLock, 2000)
	pool := make(chan *txn.Session, 8)
	for i := 0; i < 8; i++ {
		pool <- d.Session(fmt.Sprintf("p%d", i))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := <-pool
		defer func() { pool <- s }()
		for pb.Next() {
			mustExec(b, d, s, benchQuery)
		}
	})
}

// 并发写同一张表：表级 X 锁互斥，吞吐会被串行化。
func BenchmarkUpdateSameTable(b *testing.B) {
	d := setupBench(b, txn.TableLock, 200)
	pool := make(chan *txn.Session, 4)
	for i := 0; i < 4; i++ {
		pool <- d.Session(fmt.Sprintf("w%d", i))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := <-pool
		defer func() { pool <- s }()
		for pb.Next() {
			mustExec(b, d, s, `UPDATE bench SET v = v + 1 WHERE id = 1`)
		}
	})
}

// 并发写不同表：彼此不冲突，应能真正并行。
func BenchmarkUpdateDifferentTables(b *testing.B) {
	d := setupBench(b, txn.TableLock, 10)
	setup := d.Session("setup2")
	for i := 0; i < 4; i++ {
		mustExec(b, d, setup, fmt.Sprintf(`CREATE TABLE w%d (id INTEGER PRIMARY KEY, v INTEGER)`, i))
		mustExec(b, d, setup, fmt.Sprintf(`INSERT INTO w%d VALUES (1, 0)`, i))
	}
	pool := make(chan *txn.Session, 4)
	for i := 0; i < 4; i++ {
		pool <- d.Session(fmt.Sprintf("d%d", i))
	}
	var n int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := <-pool
		defer func() { pool <- s }()
		var idx int64
		for pb.Next() {
			q := fmt.Sprintf(`UPDATE w%d SET v = v + 1 WHERE id = 1`, idx%4)
			idx++
			mustExec(b, d, s, q)
		}
	})
	_ = n
}

// 单行 UPDATE 的提交成本。
//
// 改造前它是 O(表大小)：每次提交都要重组整个行切片并重建所有索引，
// 20 万行的表一次约 40 ms（带两个二级索引是 158 ms）。现在只碰写集里的那一行，
// 应该和表大小无关。
func BenchmarkUpdateOneRow200k(b *testing.B) {
	d := setupBench(b, txn.TableLock, 200000)
	sess := d.Session("s")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustExec(b, d, sess, fmt.Sprintf("UPDATE bench SET v = %d WHERE id = %d", i, i%2000+1))
	}
}

// 同一条语句插一行（分配槽位 + 增量索引）。
func BenchmarkInsertOneRow200k(b *testing.B) {
	d := setupBench(b, txn.TableLock, 200000)
	sess := d.Session("s")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mustExec(b, d, sess, fmt.Sprintf("INSERT INTO bench (v, t) VALUES (%d,'x')", i))
	}
}
