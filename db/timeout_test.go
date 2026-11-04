package db

import (
	"errors"
	"strings"
	"testing"
	"time"

	"godb/txn"
)

// seedWide 造一张 rows 行的表，用来制造「跑得够久」的查询。
//
// 为什么需要它：超时和 Kill 都要在语句**执行中**生效，而执行中唯一的检查点是
// 存储层的行游标（每 1024 行一次）。数据太少的话语句在第一个检查点之前就跑完了，
// 测出来的是「没有超时」而不是「超时生效」。
func seedWide(t *testing.T, d *DB, sess, table string, rows int) {
	t.Helper()
	run(t, d, sess, `CREATE TABLE `+table+` (id int PRIMARY KEY, v int)`)
	const batch = 1000
	for start := 1; start <= rows; start += batch {
		end := start + batch - 1
		if end > rows {
			end = rows
		}
		var b strings.Builder
		b.WriteString(`INSERT INTO ` + table + ` VALUES `)
		for i := start; i <= end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(")
			b.WriteString(itoa(i))
			b.WriteString(",")
			b.WriteString(itoa(i % 97))
			b.WriteString(")")
		}
		run(t, d, sess, b.String())
	}
}

// SET statement_timeout 的值解析：单位、裸数字（秒）、0=不限、非法值。
func TestParseStatementTimeout(t *testing.T) {
	ok := map[string]time.Duration{
		"0":     0,
		"'0'":   0,
		"30":    30 * time.Second, // 裸数字 = 秒（和 PG 的毫秒不同，见函数注释）
		"30s":   30 * time.Second,
		"'30s'": 30 * time.Second,
		"1.5s":  1500 * time.Millisecond,
		"500ms": 500 * time.Millisecond,
		"1min":  time.Minute,
		"1m":    time.Minute,
		"2h":    2 * time.Hour,
		"1d":    24 * time.Hour,
		" 30s ": 30 * time.Second,
	}
	for in, want := range ok {
		got, err := parseStatementTimeout(in)
		if err != nil {
			t.Errorf("%q 解析失败: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %v，期望 %v", in, got, want)
		}
	}
	for _, in := range []string{"", "abc", "s30", "-5s", "30x", "1e999h"} {
		if _, err := parseStatementTimeout(in); err == nil {
			t.Errorf("%q 应该报错", in)
		}
	}
}

// 端到端：设置超时后，跑到一半被中断，报的是「超时」而不是「跑完再说」。
func TestStatementTimeoutInterrupts(t *testing.T) {
	d := newTestDB(t)
	s := "timeout"
	seedWide(t, d, s, "t_to", 50000)

	// 1ms：5 万行扫一遍要好几毫秒，必然在扫描中途撞上截止时刻。
	// 不能用更长的值 —— 测试库的数据全在内存里，比落盘后的正式环境快得多，
	// 5ms 有时候整条查询已经跑完了（测出来是「没超时」，但其实没测到东西）。
	run(t, d, s, `SET statement_timeout = 1ms`)

	start := time.Now()
	_, err := d.Exec(d.Session(s), `SELECT sum(id) FROM t_to WHERE v >= 0`)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("超过 statement_timeout 应该报错（整条查询只跑了 %v）", elapsed)
	}
	if !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("错误应是超时，实际: %v", err)
	}
	var ste *txn.StatementTimeoutError
	if !errors.As(err, &ste) {
		t.Fatalf("错误类型应是 *txn.StatementTimeoutError，实际 %T", err)
	}
	// 关键：是「到点就停」而不是「跑完再说」。5 万行的完整扫描约十几毫秒，
	// 这里给 1 秒的宽松上限 —— 只排除「等整条跑完」的行为。
	if elapsed > time.Second {
		t.Fatalf("超时应该立刻中断，实际等了 %v", elapsed)
	}

	// 清掉之后同一条查询要能正常跑完：超时是「这次动作」的属性，不能把会话搞坏。
	run(t, d, s, `SET statement_timeout = 0`)
	res := run(t, d, s, `SELECT count(*) FROM t_to WHERE v >= 0`)
	if got := fmtRow(rowsOf(t, res)[0]); got != "50000" {
		t.Fatalf("清掉超时后 count = %s，期望 50000", got)
	}

	// RESET 也要能清（psql \unset / 客户端还池会发）
	run(t, d, s, `SET statement_timeout = 1h`)
	run(t, d, s, `RESET statement_timeout`)
	if got := d.Session(s).StatementTimeout(); got != 0 {
		t.Fatalf("RESET 之后超时应为 0，实际 %v", got)
	}

	// 非法值要报错，不能悄悄当 0（否则用户以为设了限制，其实没设）
	err = runErr(t, d, s, `SET statement_timeout = 'abc'`)
	if !strings.Contains(err.Error(), "statement_timeout") {
		t.Fatalf("非法值的报错应指出参数名: %v", err)
	}
}

// Kill 以前是「语句跑完才生效」，现在应该在执行中就被打断。
func TestKillInterruptsRunningStatement(t *testing.T) {
	d := newTestDB(t)
	s := "victim"
	seedWide(t, d, s, "t_kill", 50000)

	sess := d.Session(s)
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec(sess, `SELECT count(*) FROM t_kill WHERE v >= 0`)
		done <- err
	}()

	// 3ms 后发 Kill：此时扫描还在进行（5 万行要十几毫秒）。
	time.Sleep(3 * time.Millisecond)
	if err := d.KillSession(s); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("被 Kill 的语句应该返回错误")
		}
		if !errors.Is(err, txn.ErrCanceled) && !errors.Is(err, ErrCanceled) {
			t.Fatalf("错误应是取消，实际: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill 之后 2 秒还没返回：说明中断点没生效（又回到「跑完才停」）")
	}

	// 会话还能继续用（取消只针对那一次动作）
	run(t, d, s, `SELECT count(*) FROM t_kill WHERE v = 1`)
}

// 显式事务里超时：语句失败 → 事务进 aborted → ROLLBACK 后才能继续。
// 这是 PG 的约定（语句级错误不留半个事务），也是「按现有约定回滚」的落点。
func TestStatementTimeoutAbortsTransaction(t *testing.T) {
	d := newTestDB(t)
	s := "txto"
	seedWide(t, d, s, "t_to2", 50000)

	run(t, d, s, `BEGIN`)
	run(t, d, s, `SET statement_timeout = 1ms`)
	_, err := d.Exec(d.Session(s), `SELECT sum(id) FROM t_to2 WHERE v >= 0`)
	if err == nil {
		t.Fatal("事务里的语句超时也该报错")
	}
	var ste *txn.StatementTimeoutError
	if !errors.As(err, &ste) {
		t.Fatalf("错误类型应是超时，实际 %T: %v", err, err)
	}
	// 事务已中毒：后续普通语句被拒（SET 例外，PG 里 aborted 事务也能 SET）
	if err := runErr(t, d, s, `SELECT 1`); !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("超时后事务应处于 aborted，实际报：%v", err)
	}
	// ROLLBACK 之后恢复可用
	run(t, d, s, `ROLLBACK`)
	run(t, d, s, `SET statement_timeout = 0`)
	if got := fmtRow(rowsOf(t, run(t, d, s, `SELECT count(*) FROM t_to2`))[0]); got != "50000" {
		t.Fatalf("回滚后 count = %s，期望 50000", got)
	}
}
