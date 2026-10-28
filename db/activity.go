package db

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"godb/storage"
	"godb/txn"
)

// 本文件是监控面板的数据来源：会话活动状态机 + 时长样本 + 慢 SQL 记录。
//
// 为什么要单独一层：锁管理器只知道「谁持有表锁、从什么时候开始」，它不知道
// 持有者此刻是在跑 SQL 还是在干等客户端发下一条 —— 而「慢 SQL」和「慢事务」
// 的区别恰好就在这一条上。所以在 DB 层再记一份会话级活动：状态、进入时刻、
// 当前/最后一条 SQL、事务开始时刻、本事务已执行语句数。
//
// 采样开销刻意压到最低：每次状态切换是「一把小锁 + 一次 map 写」，SQL 文本
// 截断到 200 字节，样本用定长环形缓冲，不产生持续增长的垃圾。

// 会话活动状态。
const (
	ActIdle    = "idle"    // 空闲：等客户端发下一条
	ActQueued  = "queued"  // 排队：等并发执行槽位
	ActWaiting = "waiting" // 等锁：等表锁
	ActRunning = "running" // 执行：正在跑语句
)

const (
	// actSampleCap 是每类时长样本的容量（执行 / 等锁 / 排队各一份）。
	actSampleCap = 512
	// slowSQLCap 是慢 SQL 记录的容量：保留最近这么多条执行记录，前端按耗时取 Top N。
	slowSQLCap = 256
	// sqlPreviewLen 是 SQL 预览的最大字节数。
	sqlPreviewLen = 240
)

// actRec 是一条会话的活动记录。字段都由 recorder.mu 保护。
type actRec struct {
	id     string
	user   string
	state  string
	since  time.Time // 进入当前状态的时刻
	sql    string    // 当前 SQL（执行/等锁/排队时）或最后一条 SQL（空闲时）
	tables []string  // 当前语句涉及的表
	write  bool      // 当前语句是否写表

	lastAt  time.Time // 最后一条 SQL 结束的时刻（算空闲时长）
	inTx    bool
	txStart time.Time
	txStmts int
}

// Activity 是对外（JSON）的活动快照。
type Activity struct {
	Session   string   `json:"session"`
	User      string   `json:"user,omitempty"`
	State     string   `json:"state"`
	StateMs   int64    `json:"stateMs"` // 处于当前状态的毫秒数
	SQL       string   `json:"sql,omitempty"`
	Tables    []string `json:"tables,omitempty"`
	Write     bool     `json:"write,omitempty"`
	InTx      bool     `json:"inTx"`
	TxAgeMs   int64    `json:"txAgeMs,omitempty"`  // 事务已持续
	TxIdleMs  int64    `json:"txIdleMs,omitempty"` // 距最后一条 SQL
	TxStmts   int      `json:"txStmts,omitempty"`  // 本事务已执行语句数
	HeldLocks string   `json:"heldLocks,omitempty"`
}

// ring 是定长环形样本（纳秒）。
type ring struct {
	buf []int64
	pos int
	n   int
}

func newRing(cap int) *ring { return &ring{buf: make([]int64, cap)} }

func (r *ring) Add(v int64) {
	if v <= 0 {
		return
	}
	r.buf[r.pos] = v
	r.pos = (r.pos + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// Stats 返回 p50 / p95 / max（样本不足时按实际数量取，空样本返回 0）。
func (r *ring) Stats() (p50, p95, max int64) {
	if r.n == 0 {
		return 0, 0, 0
	}
	cp := make([]int64, r.n)
	copy(cp, r.buf[:r.n])
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	pick := func(q float64) int64 {
		idx := int(q * float64(len(cp)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return pick(0.50), pick(0.95), cp[len(cp)-1]
}

// execSample 是一条已完成的语句记录，供慢 SQL 列表取 Top N。
type execSample struct {
	session string
	sql     string
	tables  []string
	write   bool
	tookNs  int64
	at      time.Time
}

// recorder 是监控数据的收集器。
type recorder struct {
	mu   sync.RWMutex
	acts map[string]*actRec

	execRing  *ring
	waitRing  *ring
	queueRing *ring

	slow []execSample // 环形
	pos  int
	n    int

	kills         int64
	slowSQLAborts int64
	slowTxAborts  int64
	queueRejected int64
}

func newRecorder() *recorder {
	return &recorder{
		acts:      map[string]*actRec{},
		execRing:  newRing(actSampleCap),
		waitRing:  newRing(actSampleCap),
		queueRing: newRing(actSampleCap),
		slow:      make([]execSample, slowSQLCap),
	}
}

func (r *recorder) rec(id string) *actRec {
	a := r.acts[id]
	if a == nil {
		a = &actRec{id: id, state: ActIdle, since: time.Now(), lastAt: time.Now()}
		r.acts[id] = a
	}
	return a
}

// set 切换会话状态。tables 为 nil 时保留上一次的表名（执行结束时不想抹掉线索）。
// 同状态重复设置只刷新 since 的话会让「已持续」永远归零，所以状态没变就不动 since。
func (r *recorder) set(id, state string, tables []string, write bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.rec(id)
	if a.state != state {
		a.state = state
		a.since = time.Now()
	}
	if tables != nil {
		a.tables = tables
		a.write = write
	}
	if state == ActIdle {
		a.lastAt = time.Now()
	}
}

// setSQL 记录会话当前（或最后一条）SQL。
func (r *recorder) setSQL(id, text string, tables []string, write bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.rec(id)
	a.sql = previewSQL(text)
	if tables != nil {
		a.tables = tables
		a.write = write
	}
}

// setTx 同步事务元信息（由 db 层在语句结束时刷一次，避免每次状态切换都去问 Session）。
func (r *recorder) setTx(id string, inTx bool, start time.Time, stmts int, lastAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.rec(id)
	a.inTx = inTx
	a.txStart = start
	a.txStmts = stmts
	if !lastAt.IsZero() {
		a.lastAt = lastAt
	}
}

func (r *recorder) setUser(id, user string) {
	if user == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec(id).user = user
}

// noteExec 记一条完成的语句：进时长样本 + 慢 SQL 环形。
func (r *recorder) noteExec(id, text string, tables []string, write bool, tookNs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execRing.Add(tookNs)
	rec := execSample{session: id, sql: previewSQL(text), tables: tables, write: write, tookNs: tookNs, at: time.Now()}
	r.slow[r.pos] = rec
	r.pos = (r.pos + 1) % len(r.slow)
	if r.n < len(r.slow) {
		r.n++
	}
	if a := r.acts[id]; a != nil {
		a.lastAt = rec.at
	}
}

func (r *recorder) noteQueueWait(ns int64) {
	if ns <= 0 {
		return
	}
	r.mu.Lock()
	r.queueRing.Add(ns)
	r.mu.Unlock()
}

func (r *recorder) noteLockWait(ns int64) {
	if ns <= 0 {
		return
	}
	r.mu.Lock()
	r.waitRing.Add(ns)
	r.mu.Unlock()
}

// drop 把长时间没有活动的会话记录清掉（会话本身由 db.sweepSessions 管理）。
func (r *recorder) drop(olderThan time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, a := range r.acts {
		if a.state == ActIdle && !a.inTx && now.Sub(a.lastAt) > olderThan {
			delete(r.acts, id)
		}
	}
}

// snapshotActs 复制一份当前活动（调用方不再持锁后使用）。
func (r *recorder) snapshotActs() map[string]Activity {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now()
	out := make(map[string]Activity, len(r.acts))
	for id, a := range r.acts {
		out[id] = Activity{
			Session:  id,
			User:     a.user,
			State:    a.state,
			StateMs:  now.Sub(a.since).Milliseconds(),
			SQL:      a.sql,
			Tables:   a.tables,
			Write:    a.write,
			InTx:     a.inTx,
			TxAgeMs:  msSince(now, a.txStart),
			TxIdleMs: msSince(now, a.lastAt),
			TxStmts:  a.txStmts,
		}
	}
	return out
}

func (r *recorder) stats() (exec, wait, queue [3]int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p50, p95, mx := r.execRing.Stats()
	exec = [3]int64{p50, p95, mx}
	p50, p95, mx = r.waitRing.Stats()
	wait = [3]int64{p50, p95, mx}
	p50, p95, mx = r.queueRing.Stats()
	queue = [3]int64{p50, p95, mx}
	return
}

// topSlowSQL 返回耗时最高的 n 条已结束语句。
func (r *recorder) topSlowSQL(n int) []execSample {
	r.mu.RLock()
	list := make([]execSample, 0, r.n)
	for i := 0; i < r.n; i++ {
		idx := (r.pos - 1 - i + len(r.slow)*2) % len(r.slow)
		e := r.slow[idx]
		if e.tookNs > 0 {
			list = append(list, e)
		}
	}
	r.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].tookNs > list[j].tookNs })
	// 同一条 SQL（多语句批次里每次都会记一遍整批文本）只留最慢的那条，
	// 否则一个 CREATE TABLE a; CREATE TABLE b; ... 会把 Top 10 霸满。
	seen := map[string]bool{}
	out := make([]execSample, 0, n)
	for _, e := range list {
		key := e.session + "\x00" + e.sql
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
		if len(out) >= n {
			break
		}
	}
	return out
}

func previewSQL(s string) string {
	s = strings.TrimSpace(s)
	// 多行 SQL 压成一行，表格里只看得见首行，压平才有信息量。
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > sqlPreviewLen {
		s = s[:sqlPreviewLen] + "…"
	}
	return s
}

// msSince 算「从 t 到现在」的毫秒数。t 为零值时返回 0 ——
// 时间零值是 1970 年，直接相减会得到 9.2e12 毫秒这种鬼数字（真踩过）。
func msSince(now, t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return durMs(now.Sub(t))
}

func durMs(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

// ---- 对外快照 ----

// Water 是水位条数据。
type Water struct {
	Exec     int    `json:"exec"`     // 已占用的执行槽位（含空闲事务）
	ExecMax  int    `json:"execMax"`  // 并发上限，0 = 不限
	Wait     int    `json:"wait"`     // 正在等锁的事务数
	Queue    int    `json:"queue"`    // 正在排队等槽位的请求数
	QueueMax int    `json:"queueMax"` // 排队上限，0 = 不限
	DB       string `json:"db"`
	Level    string `json:"level"` // green / yellow / red
}

// Layer 是「执行 / 等锁 / 排队」中的一层。
type Layer struct {
	Count   int       `json:"count"`
	P50Ns   int64     `json:"p50Ns"`
	P95Ns   int64     `json:"p95Ns"`
	MaxNs   int64     `json:"maxNs"`
	Longest *Activity `json:"longest,omitempty"`
}

// Layers 是三层状态卡的数据。
type Layers struct {
	Running Layer `json:"running"`
	Waiting Layer `json:"waiting"`
	Queued  Layer `json:"queued"`
}

// LockWaiter 是一把锁上的等待者。
type LockWaiter struct {
	Session string `json:"session"`
	Mode    string `json:"mode"`
	WaitedM int64  `json:"waitedMs"`
}

// LockRow 是锁明细的一行（一个持有者一把锁）。
type LockRow struct {
	Table   string       `json:"table"`
	Mode    string       `json:"mode"`
	Session string       `json:"session"`
	HeldMs  int64        `json:"heldMs"`
	TxState string       `json:"txState"` // running / idle / none
	IdleMs  int64        `json:"idleMs"`  // 持有者距最后一条 SQL 的空闲时长
	LastSQL string       `json:"lastSql,omitempty"`
	Waiters []LockWaiter `json:"waiters,omitempty"`
}

// SlowSQLRow 是慢 SQL 列表的一行。
type SlowSQLRow struct {
	Session string   `json:"session"`
	SQL     string   `json:"sql"`
	Tables  []string `json:"tables,omitempty"`
	Write   bool     `json:"write"`
	TookNs  int64    `json:"tookNs"`
	Running bool     `json:"running"`
	AgeMs   int64    `json:"ageMs"`
}

// SlowTxRow 是慢事务列表的一行。
type SlowTxRow struct {
	Session  string   `json:"session"`
	TxAgeMs  int64    `json:"txAgeMs"`
	IdleMs   int64    `json:"idleMs"`
	HeldMs   int64    `json:"heldMs"`
	Stmts    int      `json:"stmts"`
	Locks    []string `json:"locks,omitempty"`
	LastSQL  string   `json:"lastSql,omitempty"`
	State    string   `json:"state"`
	HoldLock bool     `json:"holdLock"`
}

// IOProfile 是「存储与 IO」卡片的数据。
//
// 这里报的是「我这个进程的落盘等多久」，不是系统的磁盘 %util ——
// 后者在共用一块盘时无法归因，而 fsync 延迟直接就是 COMMIT 的尾巴。
type IOProfile struct {
	Fsync      storage.FsyncProfile      `json:"fsync"`
	Checkpoint storage.CheckpointProfile `json:"checkpoint"`
	Tables     []storage.TableSize       `json:"tables"`
	TotalBytes int64                     `json:"totalBytes"`
	Rates      IORates                   `json:"rates"`
}

// Monitor 是监控面板的完整快照。
type Monitor struct {
	NowMs    int64         `json:"nowMs"`
	Water    Water         `json:"water"`
	Layers   Layers        `json:"layers"`
	Locks    []LockRow     `json:"locks"`
	Slow     []SlowSQLRow  `json:"slowSql"`
	SlowTx   []SlowTxRow   `json:"slowTx"`
	Act      []Activity    `json:"activities"`
	Time     TimeoutStatus `json:"timeouts"`
	Counters Counters      `json:"counters"`
	Proc     ProcStats     `json:"proc"`
	IO       IOProfile     `json:"io"`
	// ClockGranNs 是本机时钟步进：前端把小于它的时长显示成「<0.20ms」，
	// 不然一堆 0.00ms 会让人以为测量坏了。
	ClockGranNs int64 `json:"clockGranNs"`
}

// TimeoutStatus 是底部超时开关的状态。
type TimeoutStatus struct {
	SlowSQLSec    int `json:"slowSqlSec"`
	SlowTxIdleSec int `json:"slowTxIdleSec"`
	LockWaitSec   int `json:"lockWaitSec"`
	QueueWaitSec  int `json:"queueWaitSec"`
}

// Counters 是面板上的累计计数（Kill 过几回、超时回滚过几回）。
type Counters struct {
	Kills      int64 `json:"kills"`
	SlowSQL    int64 `json:"slowSqlAborts"`
	SlowTx     int64 `json:"slowTxAborts"`
	QueueRejct int64 `json:"queueRejected"`
}

// Timeouts 返回当前超时配置（0 表示不限）。
func (d *DB) Timeouts() TimeoutStatus {
	if p := d.timeouts.Load(); p != nil {
		return *p
	}
	return TimeoutStatus{LockWaitSec: int(d.lockOpts.Timeout / time.Second), QueueWaitSec: int(d.execWaitTimeout / time.Second)}
}

// SetTimeouts 热更新四个超时开关：写进内存并立刻影响锁等待与排队等待。
// 配置文件的持久化由调用方负责（web 层写 config.json）。
func (d *DB) SetTimeouts(t TimeoutStatus) {
	d.timeouts.Store(&t)
	d.mu.Lock()
	// 锁等待：0 表示不限（lock 包认 <= 0），这里用 -1 明确表达「不限」，
	// 避免和「还没设置」的零值混淆。
	if t.LockWaitSec > 0 {
		d.lockOpts.Timeout = time.Duration(t.LockWaitSec) * time.Second
	} else {
		d.lockOpts.Timeout = -1
	}
	d.execWaitTimeout = time.Duration(t.QueueWaitSec) * time.Second
	sessions := make([]*txn.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		sessions = append(sessions, s)
	}
	d.mu.Unlock()
	// 已有会话各自持有 opts 的拷贝，要逐个刷新，否则老会话还按旧超时等。
	for _, s := range sessions {
		s.SetLockTimeout(d.lockOpts.Timeout)
	}
}

// KillSession 取消一个会话当前的活：排队中立刻退出、等锁中放弃并回滚、
// 空闲事务立刻回滚、执行中的语句等它跑完后自动回滚（保守版中断）。
func (d *DB) KillSession(id string) error {
	d.mu.Lock()
	sess := d.sessions[id]
	d.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", id)
	}
	sess.Cancel()
	// 唤醒所有等锁的 goroutine：只置标志不会让 cond.Wait 醒过来。
	d.Locks.WakeAll()

	// 没在跑语句（拿得到会话锁）就立刻回滚，锁马上释放。
	// 处理完立刻清取消标志：这次 Kill 已经生效，别让会话的下一条 SQL 也被判成取消。
	if sess.TryLock() {
		sess.ForceRollback()
		sess.Unlock()
		sess.ClearCancel()
		d.act.countKill()
	}
	d.act.set(id, ActIdle, nil, false)
	return nil
}

func (r *recorder) countKill()     { r.mu.Lock(); r.kills++; r.mu.Unlock() }
func (r *recorder) countSlowSQL()  { r.mu.Lock(); r.slowSQLAborts++; r.mu.Unlock() }
func (r *recorder) countSlowTx()   { r.mu.Lock(); r.slowTxAborts++; r.mu.Unlock() }
func (r *recorder) countQueueRej() { r.mu.Lock(); r.queueRejected++; r.mu.Unlock() }

func (r *recorder) counters() Counters {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Counters{Kills: r.kills, SlowSQL: r.slowSQLAborts, SlowTx: r.slowTxAborts, QueueRejct: r.queueRejected}
}

const (
	topSlowSQL = 10
	topSlowTx  = 10
)

// Monitor 汇总当前监控快照。topN 固定 10 条，够看不够刷屏。
//
// 锁明细按持有时长降序；等待者只挂在「该表第一行」上（同一个表有多个
// S 持有者时，等待者挂在最久的那一行，避免重复显示三遍）。
func (d *DB) Monitor(t TimeoutStatus) Monitor {
	now := time.Now()
	acts := d.act.snapshotActs()
	locks := d.Locks.Snapshot()

	e, w, q := d.act.stats()

	m := Monitor{
		NowMs:    now.UnixMilli(),
		Layers:   Layers{},
		Time:     t,
		Counters: d.act.counters(),
	}

	// ---- 水位条 ----
	// 「执行」= 已占用的执行槽位。不限并发（execSem 为 nil）时没有槽位可数，
	// 退化成「当前持有事务的会话数」——面板上照样能看出有几个事务在占资源。
	execN := len(d.execSem)
	if d.execSem == nil {
		for _, a := range acts {
			if a.InTx {
				execN++
			}
		}
	}
	m.Water = Water{
		Exec:     execN,
		ExecMax:  cap(d.execSem),
		Wait:     len(locks.Waiters),
		Queue:    int(d.execWaiters.Load()),
		QueueMax: d.execQueueMax,
		DB:       d.Name,
	}
	m.Water.Level = waterLevel(m.Water)

	// ---- 三层状态 ----
	m.Layers.Running.P50Ns, m.Layers.Running.P95Ns, m.Layers.Running.MaxNs = e[0], e[1], e[2]
	m.Layers.Waiting.P50Ns, m.Layers.Waiting.P95Ns, m.Layers.Waiting.MaxNs = w[0], w[1], w[2]
	m.Layers.Queued.P50Ns, m.Layers.Queued.P95Ns, m.Layers.Queued.MaxNs = q[0], q[1], q[2]

	for _, a := range acts {
		v := a
		switch a.State {
		case ActRunning:
			m.Layers.Running.Count++
			if m.Layers.Running.Longest == nil || v.StateMs > m.Layers.Running.Longest.StateMs {
				m.Layers.Running.Longest = &v
			}
		case ActWaiting:
			m.Layers.Waiting.Count++
			if m.Layers.Waiting.Longest == nil || v.StateMs > m.Layers.Waiting.Longest.StateMs {
				m.Layers.Waiting.Longest = &v
			}
		case ActQueued:
			m.Layers.Queued.Count++
			if m.Layers.Queued.Longest == nil || v.StateMs > m.Layers.Queued.Longest.StateMs {
				m.Layers.Queued.Longest = &v
			}
		}
	}

	// ---- 锁明细 ----
	m.Locks = []LockRow{}
	for _, tbl := range locks.Tables {
		waiters := make([]LockWaiter, 0, len(tbl.Waiters))
		for _, w := range tbl.Waiters {
			waiters = append(waiters, LockWaiter{
				Session: w.Session,
				Mode:    w.Mode.String(),
				WaitedM: durMs(now.Sub(w.Since)),
			})
		}
		for i, h := range tbl.Holders {
			row := LockRow{
				Table:   tbl.Name,
				Mode:    h.Mode.String(),
				Session: h.Session,
				HeldMs:  durMs(now.Sub(h.Since)),
			}
			if a, ok := acts[h.Session]; ok {
				row.TxState = a.State
				row.IdleMs = a.TxIdleMs
				row.LastSQL = a.SQL
			}
			if i == 0 {
				row.Waiters = waiters
			}
			m.Locks = append(m.Locks, row)
		}
	}
	sort.Slice(m.Locks, func(i, j int) bool {
		if m.Locks[i].HeldMs != m.Locks[j].HeldMs {
			return m.Locks[i].HeldMs > m.Locks[j].HeldMs
		}
		return m.Locks[i].Table < m.Locks[j].Table
	})

	// ---- 慢 SQL ----
	m.Slow = []SlowSQLRow{}
	for _, e := range d.act.topSlowSQL(topSlowSQL) {
		m.Slow = append(m.Slow, SlowSQLRow{
			Session: e.session,
			SQL:     e.sql,
			Tables:  e.tables,
			Write:   e.write,
			TookNs:  e.tookNs,
			AgeMs:   durMs(now.Sub(e.at)),
		})
	}
	// 正在跑且已经超过当前第 N 名的语句排在最前面（"还在跑"是这一栏的关键信息）。
	if len(m.Slow) > 0 {
		cut := m.Slow[len(m.Slow)-1].TookNs
		if m.Layers.Running.Longest != nil && m.Layers.Running.Longest.StateMs*int64(time.Millisecond) > cut {
			lg := m.Layers.Running.Longest
			m.Slow = append([]SlowSQLRow{{
				Session: lg.Session,
				SQL:     lg.SQL,
				Tables:  lg.Tables,
				Write:   lg.Write,
				TookNs:  lg.StateMs * int64(time.Millisecond),
				Running: true,
			}}, m.Slow...)
			if len(m.Slow) > topSlowSQL {
				m.Slow = m.Slow[:topSlowSQL]
			}
		}
	} else if m.Layers.Running.Longest != nil {
		lg := m.Layers.Running.Longest
		m.Slow = append(m.Slow, SlowSQLRow{
			Session: lg.Session,
			SQL:     lg.SQL,
			Tables:  lg.Tables,
			Write:   lg.Write,
			TookNs:  lg.StateMs * int64(time.Millisecond),
			Running: true,
		})
	}

	// ---- 慢事务 ----
	m.SlowTx = []SlowTxRow{}
	for _, a := range acts {
		if !a.InTx {
			continue
		}
		row := SlowTxRow{
			Session: a.Session,
			TxAgeMs: a.TxAgeMs,
			IdleMs:  a.TxIdleMs,
			Stmts:   a.TxStmts,
			LastSQL: a.SQL,
			State:   a.State,
		}
		// 持锁时长 = 该会话持有的最久那把锁的时长（事务开始时一次性拿锁，
		// 所以它约等于事务已持锁时长，且不受之后增量加锁影响）。
		for _, l := range m.Locks {
			if l.Session == a.Session {
				if l.HeldMs > row.HeldMs {
					row.HeldMs = l.HeldMs
				}
				row.Locks = append(row.Locks, l.Table+":"+l.Mode)
				row.HoldLock = true
			}
		}
		m.SlowTx = append(m.SlowTx, row)
	}
	sort.Slice(m.SlowTx, func(i, j int) bool {
		if m.SlowTx[i].HeldMs != m.SlowTx[j].HeldMs {
			return m.SlowTx[i].HeldMs > m.SlowTx[j].HeldMs
		}
		return m.SlowTx[i].IdleMs > m.SlowTx[j].IdleMs
	})
	if len(m.SlowTx) > topSlowTx {
		m.SlowTx = m.SlowTx[:topSlowTx]
	}

	// ---- 进程与存储资源（第一档：全标准库）----
	if d.proc != nil {
		proc, rates := d.proc.stats()
		m.Proc = proc
		m.IO.Rates = rates
	}
	if d.Store != nil {
		m.IO.Fsync = d.Store.FsyncProfile()
		m.IO.Checkpoint = d.Store.CheckpointProfile()
		m.IO.Tables = d.Store.TableSizes()
		var sum int64
		for _, t := range m.IO.Tables {
			sum += t.Bytes
		}
		m.IO.TotalBytes = sum
	}

	// ---- 全部会话（点开锁行/会话可以查详情）----
	m.Act = make([]Activity, 0, len(acts))
	for _, a := range acts {
		m.Act = append(m.Act, a)
	}
	sort.Slice(m.Act, func(i, j int) bool { return m.Act[i].StateMs > m.Act[j].StateMs })

	return m
}

// waterLevel 按文档的灯色规则判定健康度：
//
//	红：排队触顶，或等锁数超过执行数（说明锁成了瓶颈，不是算力）
//	黄：执行槽位占满，或已经有人在排队
//	绿：其余
func waterLevel(w Water) string {
	if w.QueueMax > 0 && w.Queue >= w.QueueMax {
		return "red"
	}
	if w.Exec > 0 && w.Wait > w.Exec {
		return "red"
	}
	if w.ExecMax > 0 && w.Exec >= w.ExecMax {
		return "yellow"
	}
	if w.Queue > 0 {
		return "yellow"
	}
	return "green"
}
