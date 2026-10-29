package db

import (
	"testing"
	"time"
)

// QPS 的数据源：每执行成功一条语句累加一次，事务控制语句不算。
//
// 口径必须和 sess.NoteStmt 一致 —— 分叉的话，面板上的 QPS 会和会话详情里
// 「这个事务干了几件事」对不上，看的人只会怀疑监控坏了。
func TestStmtTotalForQPS(t *testing.T) {
	d, err := Open(t.TempDir(), testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	run(t, d, "s", `CREATE TABLE q (id int4)`)
	run(t, d, "s", `INSERT INTO q VALUES (1)`)
	run(t, d, "s", `SELECT count(*) FROM q`)
	if got := d.stmtTotal.Load(); got != 3 {
		t.Fatalf("三条 DDL/DML/DQL 之后 stmtTotal = %d，期望 3", got)
	}

	// BEGIN / COMMIT 是事务控制，本身不算「干了一件事」
	run(t, d, "s", `BEGIN`)
	run(t, d, "s", `INSERT INTO q VALUES (2)`)
	run(t, d, "s", `COMMIT`)
	if got := d.stmtTotal.Load(); got != 4 {
		t.Fatalf("显式事务里的那条 INSERT 只该 +1，stmtTotal = %d，期望 4", got)
	}
}

// 连接数：采样器读到的是当前会话数，会话建了就算。
func TestSessionCountForThroughput(t *testing.T) {
	d, err := Open(t.TempDir(), testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	if got := d.sessionCount(); got != 0 {
		t.Fatalf("刚打开时会话数 = %d，期望 0", got)
	}
	d.Session("a")
	d.Session("b")
	if got := d.sessionCount(); got != 2 {
		t.Fatalf("建两个会话后 sessionCount = %d，期望 2", got)
	}
}

// 采样环：第二个点之后才有速率，且曲线按时间正序展开。
//
// 这里用一个独立的 sampler 直接驱动 tickRates，不走 d.proc 的后台 ticker：
// 测试跑得快，也不会和真实的一秒节拍抢着写同一个环。
func TestThroughputSampler(t *testing.T) {
	d, err := Open(t.TempDir(), testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	p := &procSampler{thruSeries: make([]RatePoint, rateSeriesCap)}
	now := time.Now()

	// 第一个点只立基线：不能把「进程启动至今」的语句数当成这一秒的 QPS
	p.tickRates(d, now)
	if got := p.thruSeries[0].QPS; got != 0 {
		t.Fatalf("第一个采样点的 QPS = %d，期望 0（只立基线）", got)
	}

	run(t, d, "s", `CREATE TABLE r (id int4)`)
	run(t, d, "s", `INSERT INTO r VALUES (1)`)
	p.tickRates(d, now.Add(time.Second))
	if got := p.thruSeries[1].QPS; got != 2 {
		t.Fatalf("第二个采样点的 QPS = %d，期望 2", got)
	}

	rs := p.rateStatsLocked()
	if !rs.Sampled || len(rs.Series) != 2 {
		t.Fatalf("采样点 = %+v，期望两个点且 Sampled", rs.Series)
	}
	if rs.PeakQPS != 2 || rs.QPS != 1 {
		t.Fatalf("峰值 %d / 平均 %d，期望 2 / 1", rs.PeakQPS, rs.QPS)
	}
	if rs.Conns != 1 {
		t.Fatalf("连接数 = %d，期望 1（run 建了一个会话）", rs.Conns)
	}
}
