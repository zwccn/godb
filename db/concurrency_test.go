package db

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 两个会话并发执行，返回各自的结果。
func concurrent(t *testing.T, fns ...func() error) []error {
	t.Helper()
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}(i, fn)
	}
	close(start)
	wg.Wait()
	return errs
}

func execErr(d *DB, sess, q string) error {
	_, err := d.Exec(d.Session(sess), q)
	return err
}

// 读读并发：两个事务读同一张表，都不应该被挡。
func TestReadReadConcurrent(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, "s1", `INSERT INTO t VALUES (1, 'a'), (2, 'b')`)

	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `SELECT * FROM t`)
	run(t, d, "s2", `BEGIN`)
	run(t, d, "s2", `SELECT * FROM t`)

	// 两个读事务同时持有 S，互不阻塞
	assertRows(t, run(t, d, "s2", `SELECT count(*) FROM t`), []string{"2"})
	run(t, d, "s2", `ROLLBACK`)
	run(t, d, "s1", `ROLLBACK`)
}

// 不同表并行：s1 在事务里改 a 表，s2 改 b 表不应等待。
func TestWriteDifferentTablesConcurrent(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `CREATE TABLE b (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO a VALUES (1, 0)`)
	run(t, d, "s1", `INSERT INTO b VALUES (1, 0)`)

	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `UPDATE a SET v = 1 WHERE id = 1`) // a 上加 X

	start := time.Now()
	run(t, d, "s2", `UPDATE b SET v = 1 WHERE id = 1`) // b 上加 X，应立即成功
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("写不同表不该等待，实际耗时 %s", elapsed)
	}
	run(t, d, "s1", `COMMIT`)

	assertRows(t, run(t, d, "s2", `SELECT v FROM a WHERE id = 1`), []string{"1"})
	assertRows(t, run(t, d, "s2", `SELECT v FROM b WHERE id = 1`), []string{"1"})
}

// 读挡写：s1 读过 a（持 S），s2 写 a（要 X）必须等到超时。
func TestReadBlocksWrite(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO a VALUES (1, 0)`)

	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `SELECT * FROM a`)

	err := execErr(d, "s2", `UPDATE a SET v = 9 WHERE id = 1`)
	if err == nil {
		t.Fatalf("写被读锁挡住的表应该失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "等待锁超时") || !strings.Contains(msg, "s1") {
		t.Fatalf("错误信息应说明等待超时与阻塞者，实际: %s", msg)
	}
	// 超时即放弃：s2 的事务/语句已回滚
	if d.Session("s2").InTx() {
		t.Fatalf("超时后不该还留着事务")
	}
	run(t, d, "s1", `ROLLBACK`)
	// 释放后可以写
	run(t, d, "s2", `UPDATE a SET v = 9 WHERE id = 1`)
	assertRows(t, run(t, d, "s2", `SELECT v FROM a`), []string{"9"})
}

// 写挡读：s1 改了 a（持 X），s2 连读都读不了。
func TestWriteBlocksRead(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO a VALUES (1, 0)`)

	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `UPDATE a SET v = 5 WHERE id = 1`)

	err := execErr(d, "s2", `SELECT * FROM a`)
	if err == nil {
		t.Fatalf("读被别人写锁住的表应该失败")
	}
	if !strings.Contains(err.Error(), "等待锁超时") {
		t.Fatalf("错误信息不对: %s", err)
	}
	run(t, d, "s1", `ROLLBACK`)
}

// 保守预扫描：整批提交的「先读后写同一张表」不会卡在升级上。
func TestConservativePrelockAvoidsUpgrade(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO t VALUES (1, 0)`)

	batch := `BEGIN;
		SELECT * FROM t WHERE id = 1;
		UPDATE t SET v = v + 1 WHERE id = 1;
		COMMIT;`

	errs := concurrent(t,
		func() error { return execErr(d, "s1", batch) },
		func() error { return execErr(d, "s2", batch) },
	)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("会话 %d 的整批事务失败（说明预扫描没生效）: %v", i+1, err)
		}
	}
	// 两次 +1，且没有丢失更新
	assertRows(t, run(t, d, "s1", `SELECT v FROM t WHERE id = 1`), []string{"2"})
}

// 逐条提交时交叉加锁会成环：年轻的那个应该被死锁检测干掉，老的继续。
func TestDeadlockDetection(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `CREATE TABLE b (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO a VALUES (1, 0)`)
	run(t, d, "s1", `INSERT INTO b VALUES (1, 0)`)

	// s1 先拿 a，s2 先拿 b
	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `UPDATE a SET v = 1 WHERE id = 1`)
	run(t, d, "s2", `BEGIN`)
	run(t, d, "s2", `UPDATE b SET v = 1 WHERE id = 1`)

	// 交叉申请：s1 要 b，s2 要 a
	errs := concurrent(t,
		func() error { return execErr(d, "s1", `UPDATE b SET v = 2 WHERE id = 1`) },
		func() error { return execErr(d, "s2", `UPDATE a SET v = 2 WHERE id = 1`) },
	)

	var deadlocks int
	for i, err := range errs {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "死锁") {
			deadlocks++
			if i != 1 {
				t.Fatalf("应该由后进来的会话（s2）放弃，实际是会话 %d", i+1)
			}
			continue
		}
		t.Fatalf("意外错误: %v", err)
	}
	if deadlocks != 1 {
		t.Fatalf("应该恰好有一个事务因死锁回滚，实际 %d", deadlocks)
	}

	// s2 已被回滚并释放 b，s1 应该能拿到
	if d.Session("s2").InTx() {
		t.Fatalf("s2 被死锁回滚后不该还有事务")
	}
	run(t, d, "s1", `COMMIT`)
	assertRows(t, run(t, d, "s1", `SELECT v FROM b WHERE id = 1`), []string{"2"})
}

// 隔离性：未提交的修改别的会话看不到。
func TestUncommittedInvisible(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`)
	run(t, d, "s1", `INSERT INTO t VALUES (1, 100)`)

	run(t, d, "s1", `BEGIN`)
	run(t, d, "s1", `UPDATE t SET v = 999 WHERE id = 1`)
	assertRows(t, run(t, d, "s1", `SELECT v FROM t`), []string{"999"})

	// s2 在 s1 持 X 期间读不到（会被锁挡住），这里验证锁确实生效
	if err := execErr(d, "s2", `SELECT v FROM t`); err == nil {
		t.Fatalf("s1 持写锁时 s2 不该能读")
	}
	run(t, d, "s1", `COMMIT`)
	assertRows(t, run(t, d, "s2", `SELECT v FROM t`), []string{"999"})
}

// 提交顺序决定可见顺序：后提交的覆盖先提交的（写写互斥保证不丢更新）。
func TestSerializableCounter(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "s1", `CREATE TABLE c (id INTEGER PRIMARY KEY, n INTEGER)`)
	run(t, d, "s1", `INSERT INTO c VALUES (1, 0)`)

	const workers = 4
	const rounds = 5
	var wg sync.WaitGroup
	errCh := make(chan error, workers*rounds)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(sess string) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				_, err := d.Exec(d.Session(sess), `BEGIN;
					UPDATE c SET n = n + 1 WHERE id = 1;
					COMMIT;`)
				if err != nil {
					errCh <- err
					return
				}
			}
		}("w" + string(rune('0'+i)))
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发自增失败: %v", err)
	}
	assertRows(t, run(t, d, "s1", `SELECT n FROM c`), []string{"20"})
}
