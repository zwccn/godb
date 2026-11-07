package db

import (
	"testing"
	"time"

	"godb/netio"
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

// 带宽：收 / 发各自求差，首点只立基线，曲线里带的是每秒增量。
//
// 这里的字节由测试自己往 netio.PG 里加 —— 单测没有 PG 监听，
// 真实链路由 netio 自己的用例和端到端手册覆盖。
func TestNetBytesRate(t *testing.T) {
	d, err := Open(t.TempDir(), testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	p := &procSampler{thruSeries: make([]RatePoint, rateSeriesCap)}
	now := time.Now()

	// 第一个点：计数器里可能已有别的用例留下的字节，但绝不能算进这一秒
	p.tickRates(d, now)
	if pt := p.thruSeries[0]; pt.NetIn != 0 || pt.NetOut != 0 {
		t.Fatalf("第一个采样点 netIn=%d netOut=%d，期望都是 0（只立基线）", pt.NetIn, pt.NetOut)
	}

	netio.PG.AddIn(1024)
	netio.PG.AddOut(8192)
	p.tickRates(d, now.Add(time.Second))
	pt := p.thruSeries[1]
	if pt.NetIn != 1024 || pt.NetOut != 8192 {
		t.Fatalf("第二个点 netIn=%d netOut=%d，期望 1024 / 8192", pt.NetIn, pt.NetOut)
	}

	// 没有新流量的一秒必须是 0，不能把上一秒的值再报一遍
	p.tickRates(d, now.Add(2*time.Second))
	if pt := p.thruSeries[2]; pt.NetIn != 0 || pt.NetOut != 0 {
		t.Fatalf("空闲的一秒 netIn=%d netOut=%d，期望 0 / 0", pt.NetIn, pt.NetOut)
	}

	rs := p.rateStatsLocked()
	if rs.PeakNetIn != 1024 || rs.PeakNetOut != 8192 {
		t.Fatalf("峰值 in=%d out=%d，期望 1024 / 8192", rs.PeakNetIn, rs.PeakNetOut)
	}
	// 三个点的平均：(0 + 1024 + 0)/3 = 341，(0 + 8192 + 0)/3 = 2730
	if rs.NetIn != 341 || rs.NetOut != 2730 {
		t.Fatalf("平均 in=%d out=%d，期望 341 / 2730", rs.NetIn, rs.NetOut)
	}
}

// 磁盘：读 = 页文件 pread 的字节，写 = WAL 追加的字节。首点同样只立基线。
//
// 构造上是「写一次、冷读一次」：第一次 Open 建表插数据后关掉，第二次 Open
// 的缓冲池是空的，接下来的 SELECT 必然要 pread 页文件。
func TestDiskBytesRate(t *testing.T) {
	dir := t.TempDir()
	d1, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d1, "s", `CREATE TABLE dsk (id int4, s text)`)
	// 数据要够多：只有一两行时，数据页还在 WAL 里、Open 时从 WAL 回放进内存，
	// 一次 SELECT 下来一个字节的 pread 都不会发生（踩过，断言直接红）。
	// 翻倍到几千行，页已经 checkpoint 到页文件，冷读必然 pread。
	for i := 0; i < 7; i++ {
		run(t, d1, "s", `INSERT INTO dsk VALUES (1, 'abcdefghijklmnopqrstuvwxyz')`)
		if i > 0 {
			run(t, d1, "s", `INSERT INTO dsk SELECT * FROM dsk`)
		}
	}
	for i := 0; i < 6; i++ {
		run(t, d1, "s", `INSERT INTO dsk SELECT * FROM dsk`)
	}
	d1.Close()

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()

	p := &procSampler{thruSeries: make([]RatePoint, rateSeriesCap)}
	now := time.Now()

	// 第一个点只立基线：不能把「进程启动至今」的字节全算成这一秒
	p.tickRates(d2, now)
	if pt := p.thruSeries[0]; pt.DiskRead != 0 || pt.DiskWrite != 0 {
		t.Fatalf("第一个采样点 diskRead=%d diskWrite=%d，期望都是 0（只立基线）", pt.DiskRead, pt.DiskWrite)
	}

	run(t, d2, "s", `INSERT INTO dsk VALUES (1, 'x')`) // 写：WAL 追加
	run(t, d2, "s", `SELECT * FROM dsk`)               // 读：冷读页文件
	p.tickRates(d2, now.Add(time.Second))
	pt := p.thruSeries[1]
	if pt.DiskWrite <= 0 {
		t.Fatalf("插入后 diskWrite = %d，期望 > 0（WAL 追加）", pt.DiskWrite)
	}
	if pt.DiskRead <= 0 {
		t.Fatalf("冷读整表后 diskRead = %d，期望 > 0（pread 页文件）", pt.DiskRead)
	}

	// 没有新 I/O 的一秒必须是 0 —— 不能把上一秒的值再报一遍
	p.tickRates(d2, now.Add(2*time.Second))
	if pt := p.thruSeries[2]; pt.DiskRead != 0 || pt.DiskWrite != 0 {
		t.Fatalf("空闲的一秒 diskRead=%d diskWrite=%d，期望 0 / 0", pt.DiskRead, pt.DiskWrite)
	}

	rs := p.rateStatsLocked()
	if rs.PeakDiskWrite <= 0 || rs.PeakDiskRead <= 0 {
		t.Fatalf("峰值 read=%d write=%d，期望都 > 0", rs.PeakDiskRead, rs.PeakDiskWrite)
	}
	if rs.DiskWrite != pt.DiskWrite/3 || rs.DiskRead != pt.DiskRead/3 {
		t.Fatalf("三点平均 read=%d write=%d，期望 %d / %d",
			rs.DiskRead, rs.DiskWrite, pt.DiskRead/3, pt.DiskWrite/3)
	}
}
