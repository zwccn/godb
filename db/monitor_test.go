package db

import (
	"strings"
	"testing"
	"time"
)

// 一条语句跑完之后，活动记录必须落到「空闲」并留下 SQL 文本与耗时样本 ——
// 面板的慢 SQL 列表、「最后一条 SQL」列都靠它。
func TestMonitorTracksActivity(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "m1", "CREATE TABLE mt (id int primary key, name text)")
	run(t, d, "m1", "INSERT INTO mt VALUES (1, 'a')")
	run(t, d, "m1", "SELECT * FROM mt")

	m := d.Monitor(d.Timeouts())
	if len(m.Act) == 0 {
		t.Fatal("活动列表为空")
	}
	var got *Activity
	for i := range m.Act {
		if m.Act[i].Session == "m1" {
			got = &m.Act[i]
		}
	}
	if got == nil {
		t.Fatal("找不到会话 m1 的活动记录")
	}
	if got.State != ActIdle {
		t.Errorf("语句结束后状态 = %q，期望 idle", got.State)
	}
	if !strings.Contains(got.SQL, "SELECT * FROM mt") {
		t.Errorf("最后一条 SQL = %q，期望包含 SELECT", got.SQL)
	}
	// 三条语句各记一条样本
	if m.Layers.Running.P95Ns <= 0 {
		t.Errorf("执行时长样本为空，p95=%d", m.Layers.Running.P95Ns)
	}
	if len(m.Slow) == 0 {
		t.Error("慢 SQL 列表为空（应该至少有刚才那几条记录）")
	}
	if m.Water.Exec != 0 {
		t.Errorf("没有事务时执行槽位 = %d，期望 0", m.Water.Exec)
	}
}

// 事务里持着 S 锁但不提交：锁明细要能显示持有时长，并且事务状态是「空闲」——
// 这正是「慢 SQL 都很快但事务不提交」的那类问题。
func TestMonitorLockDetail(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "m2", "CREATE TABLE lt (id int primary key)")
	run(t, d, "m2", "INSERT INTO lt VALUES (1)")
	run(t, d, "m2", "BEGIN")
	run(t, d, "m2", "SELECT * FROM lt")

	m := d.Monitor(d.Timeouts())
	if m.Water.Exec == 0 {
		t.Error("事务持有执行槽位，水位条应该 > 0")
	}
	if len(m.Locks) == 0 {
		t.Fatal("锁明细为空，但事务里读过 lt，应该有 S 锁")
	}
	l := m.Locks[0]
	if l.Table != "lt" || l.Mode != "S" {
		t.Errorf("锁明细首行 = %s/%s，期望 lt/S", l.Table, l.Mode)
	}
	if l.TxState != ActIdle {
		t.Errorf("事务状态 = %q，期望 idle（语句已跑完，在等客户端）", l.TxState)
	}
	if !strings.Contains(l.LastSQL, "SELECT") {
		t.Errorf("最后一条 SQL = %q，期望包含 SELECT", l.LastSQL)
	}
	// 慢事务统计只记「已经结束的事务」：这时还没提交，不该有条目
	if len(m.SlowTx) != 0 {
		t.Errorf("事务还没结束就进了统计：%+v", m.SlowTx)
	}

	run(t, d, "m2", "ROLLBACK")

	// 事务结束后按「表集合 + 读写」归档一条，表集合里要有 lt
	m = d.Monitor(d.Timeouts())
	if len(m.SlowTx) == 0 {
		t.Fatalf("事务结束后应该归档一条统计（SlowTx=%+v, StmtLive=%d, TxLive=%d）", m.SlowTx, m.StmtLive, m.TxLive)
	}
	if !strings.Contains(strings.Join(m.SlowTx[0].Tables, ","), "lt") {
		t.Errorf("事务结构 = %v，期望含 lt", m.SlowTx[0].Tables)
	}
	if m.SlowTx[0].N != 1 {
		t.Errorf("事务次数 = %d，期望 1", m.SlowTx[0].N)
	}
}

// Kill 一个空闲事务：立刻回滚、锁立刻释放。
func TestKillIdleTransaction(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "k1", "CREATE TABLE kt (id int primary key)")
	run(t, d, "k1", "BEGIN")
	run(t, d, "k1", "INSERT INTO kt VALUES (1)")

	if len(d.Locks.Snapshot().Tables) == 0 {
		t.Fatal("事务应该持有 kt 的 X 锁")
	}
	if err := d.KillSession("k1"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if tabs := d.Locks.Snapshot().Tables; len(tabs) != 0 {
		t.Errorf("Kill 之后锁没释放：%+v", tabs)
	}
	// 事务没了，插入也回滚了
	if n := len(rowsOf(t, run(t, d, "k1", "SELECT * FROM kt"))); n != 0 {
		t.Errorf("Kill 之后行数 = %d，期望 0（插入应已回滚）", n)
	}
	if d.Monitor(d.Timeouts()).Counters.Kills == 0 {
		t.Error("Kill 计数没有增加")
	}
}

// 慢事务超时：事务空闲超过阈值就自动回滚。巡检 5 秒一轮，这里给足余量。
func TestSlowTxIdleTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("依赖巡检 tick，短模式跳过")
	}
	d := newTestDB(t)
	run(t, d, "st", "CREATE TABLE stx (id int primary key)")
	d.SetTimeouts(TimeoutStatus{SlowTxIdleSec: 1, LockWaitSec: 3, QueueWaitSec: 3})

	run(t, d, "st", "BEGIN")
	run(t, d, "st", "INSERT INTO stx VALUES (1)")

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if len(d.Locks.Snapshot().Tables) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if tabs := d.Locks.Snapshot().Tables; len(tabs) != 0 {
		t.Fatalf("慢事务超时没有回滚，锁还在：%+v", tabs)
	}
	if d.Monitor(d.Timeouts()).Counters.SlowTx == 0 {
		t.Error("慢事务超时计数没有增加")
	}
}

// 队列上限被触顶时要计一次「排队被拒」，面板上要能看见背压发生过。
func TestMonitorQueueRejectCounter(t *testing.T) {
	d := newTestDB(t)
	// 排队超时 2 秒：不设的话（0 = 不限）排队的那个会一直等到 q1 释放，
	// 这个用例就变成「测试卡死」而不是「验证背压」了。
	d.SetMaxConcurrent(1, 1, 2*time.Second)
	defer d.SetMaxConcurrent(0, 0, 0)

	// 占住唯一的执行槽位
	run(t, d, "q1", "BEGIN")
	run(t, d, "q1", "SELECT 1")

	// q2 在 goroutine 里排队（会阻塞在这里），
	// 只有它真的卡住之后，q3 才会撞上「队列已满」。
	queued := make(chan error, 1)
	go func() {
		_, err := d.Exec(d.Session("q2"), "SELECT 1")
		queued <- err
	}()
	time.Sleep(300 * time.Millisecond)

	_, err := d.Exec(d.Session("q3"), "SELECT 1")
	if err == nil {
		t.Fatal("队列已满时第三个请求应该被快速拒绝")
	}
	if _, ok := err.(*TooManyQueuedError); !ok {
		t.Fatalf("错误类型 = %T，期望 *TooManyQueuedError", err)
	}

	m := d.Monitor(d.Timeouts())
	if m.Water.QueueMax != 1 {
		t.Errorf("QueueMax = %d，期望 1", m.Water.QueueMax)
	}
	if m.Water.Queue == 0 {
		t.Error("水位条应该显示有 1 个请求在排队")
	}
	if m.Layers.Queued.Count == 0 {
		t.Error("排队层计数应该为 1")
	}

	// 放开槽位，排队的 q2 应该拿到并跑完
	d.Session("q1").ForceRollback()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("q2 拿到槽位后应该成功，实际: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q2 一直没拿到槽位")
	}
}

// 灯色规则：执行槽位占满 → 黄；等锁数多于执行数 → 红。
func TestWaterLevel(t *testing.T) {
	cases := []struct {
		w    Water
		want string
	}{
		{Water{Exec: 0, ExecMax: 32, QueueMax: 256}, "green"},
		{Water{Exec: 32, ExecMax: 32, QueueMax: 256}, "yellow"},
		{Water{Exec: 3, ExecMax: 32, Queue: 2, QueueMax: 256}, "yellow"},
		{Water{Exec: 2, ExecMax: 32, Wait: 3, QueueMax: 256}, "red"},
		{Water{Exec: 2, ExecMax: 32, Queue: 256, QueueMax: 256}, "red"},
		// 不限并发（ExecMax=0）时不应该因为「满了」被判黄
		{Water{Exec: 5, ExecMax: 0}, "green"},
	}
	for _, c := range cases {
		if got := waterLevel(c.w); got != c.want {
			t.Errorf("%+v → %s，期望 %s", c.w, got, c.want)
		}
	}
}

// 环形样本的百分位：偶数/奇数样本都要稳，空样本不能 panic。
func TestRingStats(t *testing.T) {
	r := newRing(8)
	if p50, p95, mx := r.Stats(); p50 != 0 || p95 != 0 || mx != 0 {
		t.Errorf("空样本 = %d/%d/%d，期望全 0", p50, p95, mx)
	}
	big := newRing(200)
	for i := 1; i <= 100; i++ {
		big.Add(int64(i))
	}
	p50, p95, mx := big.Stats()
	if mx != 100 {
		t.Errorf("max = %d，期望 100", mx)
	}
	if p50 < 49 || p50 > 51 {
		t.Errorf("p50 = %d，期望约 50", p50)
	}
	if p95 < 94 || p95 > 96 {
		t.Errorf("p95 = %d，期望约 95", p95)
	}
	// 环形写满之后只保留最近 8 条（93..100）
	for i := 1; i <= 100; i++ {
		r.Add(int64(i))
	}
	p50, p95, mx = r.Stats()
	if mx != 100 {
		t.Errorf("写满后 max = %d，期望 100", mx)
	}
	if p50 < 95 || p50 > 97 {
		t.Errorf("写满后 p50 = %d，期望约 96（只剩 93..100）", p50)
	}
	_ = p95
}
