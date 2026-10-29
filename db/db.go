// Package db 把各层装配起来，对外暴露“执行一段 SQL”的能力。
package db

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"godb/catalog"
	"godb/exec"
	"godb/hrtime"
	"godb/lock"
	"godb/sql"
	"godb/storage"
	"godb/syscat"
	"godb/txn"
	"godb/types"
)

// Options 是数据库的并发控制配置。
type Options struct {
	Isolation txn.Isolation
	Lock      lock.Options
	// Name 是库名。空表示默认库（godb）。多库模式下由 Manager 逐个设。
	Name string
	// Progress 是启动阶段回调（「载入快照」「WAL 重放」…），可为 nil。
	// 启动页用它显示恢复进度；单库测试里留空即可。
	Progress func(msg string)
}

// execCheckpoint 处理 CHECKPOINT / FLUSH 语句：把 WAL 刷盘并写一份全量快照。
func (d *DB) execCheckpoint(sess *txn.Session) (*exec.Result, error) {
	if sess.InTx() {
		return nil, fmt.Errorf("CHECKPOINT cannot run inside a transaction block")
	}
	before := d.Store.Info()
	start := hrtime.Now()
	if err := d.Store.Flush(); err != nil {
		return nil, err
	}
	took := hrtime.Since(start)
	after := d.Store.Info()
	res := &exec.Result{
		Command:  "CHECKPOINT",
		Tag:      "CHECKPOINT",
		Columns:  []exec.Column{{Name: "item", Type: "text"}, {Name: "value", Type: "text"}},
		TookNs:   took,
		RowCount: 4,
	}
	res.Rows = [][]interface{}{
		{"snapshot_generation", fmt.Sprintf("%d", after.Generation)},
		{"next_wal_generation", fmt.Sprintf("%d", after.NextGeneration)},
		{"tables", fmt.Sprintf("%d", after.Tables)},
		{"tables_in_memory", fmt.Sprintf("%d", after.Loaded)},
		{"rows", fmt.Sprintf("%d", after.Rows)},
		{"data_bytes", fmt.Sprintf("%d", after.Bytes)},
		// 这一次实际写了多少（和 data_bytes 不是一回事：后者是表文件总大小）
		{"bytes_written", fmt.Sprintf("%d", after.Written)},
		{"bytes_written_total", fmt.Sprintf("%d", after.WrittenTotal)},
		{"wal_bytes_before", fmt.Sprintf("%d", before.WALBytes)},
		{"wal_fsyncs", fmt.Sprintf("%d", after.WALSyncs)},
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// ErrAborted 表示事务已中止。
var ErrAborted = errors.New("current transaction is aborted, commands ignored until end of transaction block")

// DB 是一个数据库实例。
type DB struct {
	dir    string
	Name   string // 库名，默认库是 "godb"
	Cat    *catalog.Catalog
	Store  *storage.Store
	Engine *exec.Engine
	Mgr    *txn.Manager
	Locks  *lock.Manager
	Iso    txn.Isolation

	// ClockGranNs 是本机时钟步进（纳秒），用于前端判断“耗时小于时钟精度”。
	ClockGranNs int64

	// SlowThreshold 大于 0 时，整批语句耗时超过它就调用 SlowLog。
	// 慢日志是诊断开关，只在命令行开，不进配置文件。
	SlowThreshold time.Duration
	// SlowLog 收到慢语句的原文与耗时；nil 表示不记录。
	SlowLog func(sqlText string, took time.Duration, stmts int)

	// execSem 是并发执行信号量：同时执行 SQL 的 goroutine 上限。
	// 超出的在 Exec 入口排队等（不占表锁、不扫表、不占内存）。
	// nil 表示不限制。
	execSem chan struct{}
	// execWaiters 是当前排队等待信号量的 goroutine 数（背压用）。
	execWaiters atomic.Int64
	// execQueueMax 是排队上限：超过就快速失败（背压），0 = 不限。
	execQueueMax int
	// execWaitTimeout 是排队最长等待时间：超过就快速失败。
	execWaitTimeout time.Duration

	// act 是监控面板的数据源（会话活动状态机 + 时长样本 + 慢 SQL 记录）。
	act *recorder
	// proc 是进程/存储资源的秒级采样器（第一档：全标准库，零 cgo）。
	proc *procSampler
	// timeouts 是面板底部的四个超时开关，热更新（0 = 不限）。
	timeouts atomic.Pointer[TimeoutStatus]

	lockOpts lock.Options

	mu       sync.Mutex
	sessions map[string]*txn.Session

	// stop 让会话巡检 goroutine 在 Close 时退出。多库模式下每库一个巡检，
	// 库被 DROP 掉还不退出的话 goroutine 会一直堆积。
	stop      chan struct{}
	closeOnce sync.Once

	// closed 在 Close 之后为 true。库被 DROP 掉时，还挂在旧对象上的连接
	// 后续请求会拿到明确的错误，而不是撞上一个已经关掉的 Store。
	closed atomic.Bool

	// mgr 指向管理这个库的 Manager（单库模式下为 nil）。
	// 只有 CREATE/DROP DATABASE 需要它 —— 那是跨库操作，得由 Manager 做。
	mgr *Manager
}

// ColumnInfo 是前端展示用的列信息。
type ColumnInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	NotNull    bool   `json:"notNull"`
	PrimaryKey bool   `json:"primaryKey"`
	Unique     bool   `json:"unique"`
	Serial     bool   `json:"serial"`
	Default    string `json:"default,omitempty"`
}

// TableInfo 是前端展示用的表信息。
type TableInfo struct {
	Name    string       `json:"name"`
	Rows    int          `json:"rows"`
	Columns []ColumnInfo `json:"columns"`
}

// Open 打开（或初始化）数据目录。
func Open(dir string, o Options) (*DB, error) {
	cat, err := catalog.Load(dir)
	if err != nil {
		return nil, err
	}
	st, err := storage.Open(dir, cat, o.Progress)
	if err != nil {
		return nil, err
	}
	// 只有「没设置」（0）才用默认 3 秒；-1 是调用方明确的「不限」。
	//
	// 注意：应用层（main.go）永远显式传值，配置文件里的 0 在这里被翻译成 -1，
	// 所以「出厂不限」不会被这条兜底改回 3 秒。这个 3 秒只留给内嵌/测试调用，
	// 免得它们什么都不传时永久挂住。
	if o.Lock.Timeout == 0 {
		o.Lock = lock.DefaultOptions()
	}
	name := strings.ToLower(strings.TrimSpace(o.Name))
	if name == "" {
		name = syscat.DefaultDatabaseName
	}
	d := &DB{
		dir:      dir,
		Name:     name,
		Cat:      cat,
		Store:    st,
		Mgr:      txn.NewManager(),
		Locks:    lock.New(),
		Iso:      o.Isolation,
		lockOpts: o.Lock,
		sessions: map[string]*txn.Session{},
		stop:     make(chan struct{}),
		act:      newRecorder(),
		proc:     newProcSampler(),
	}
	// Info.Name 是库名；All 由 Manager 在打开后注入（它才知道一共有哪些库）。
	d.Engine = &exec.Engine{Dir: dir, Cat: cat, Store: st,
		Info: &exec.DBInfo{Name: name}}
	d.ClockGranNs = int64(hrtime.Granularity())
	go d.sweepSessions()
	// 资源采样独立于面板：面板关着也要有曲线，否则打开时看到的永远是空历史。
	go d.proc.run(d, d.stop)
	return d, nil
}

// Close 落盘并关闭。
// Close 落盘并关闭。可以重复调用（Manager 退出和 DROP DATABASE 都会碰它）。
func (d *DB) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		if d.stop != nil {
			close(d.stop)
		}
		d.Store.SchemaMu.Lock()
		defer d.Store.SchemaMu.Unlock()
		// 先等 WAL 落盘，再写快照：任何已确认的提交都不会丢
		if e := d.Store.Flush(); e != nil {
			err = e
			return
		}
		err = d.Store.Close()
	})
	return err
}

// Dir 返回数据目录。
func (d *DB) Dir() string { return d.dir }

// SetMaxConcurrent 设置并发执行上限（0 = 不限）。
// 配置热更新时调用。已在执行的不受影响，新的按新值排。
func (d *DB) SetMaxConcurrent(n, queueMax int, waitTimeout time.Duration) {
	if n <= 0 {
		d.execSem = nil
		d.execQueueMax = 0
		return
	}
	d.execSem = make(chan struct{}, n)
	d.execQueueMax = queueMax
	d.execWaitTimeout = waitTimeout
}

// TooManyQueuedError 表示排队队列已满（背压快速失败）。
// SQLSTATE 53300 (too_many_connections)，客户端可识别为「暂时不可用」并重试。
type TooManyQueuedError struct {
	Queued int
}

func (e *TooManyQueuedError) Error() string {
	return fmt.Sprintf("too many queued requests (%d), try again later", e.Queued)
}

// QueueTimeoutError 表示排队等待超时（背压快速失败）。
// SQLSTATE 55P03 (lock_not_available)，客户端可识别为「锁不可用」并重试。
type QueueTimeoutError struct {
	Timeout time.Duration
}

func (e *QueueTimeoutError) Error() string {
	return fmt.Sprintf("timed out waiting for execution slot (%s)", e.Timeout)
}

// acquireExec 取一个执行令牌。带背压：
//   - 队列满 → 立即返回错误（快速失败，客户端能感知）
//   - 等待超时 → 立即返回错误
//   - 队列被取消（面板 Kill / 客户端断开）→ 立即返回
//   - 0 = 不限并发时直接放行
//
// 返回 release 函数。error 非 nil 时不需要调 release。
func (d *DB) acquireExec(sess *txn.Session) (release func(), err error) {
	sem := d.execSem
	if sem == nil {
		return func() {}, nil
	}
	// 背压：排队太长直接拒绝
	if d.execQueueMax > 0 && d.execWaiters.Load() >= int64(d.execQueueMax) {
		return nil, &TooManyQueuedError{Queued: d.execQueueMax}
	}
	d.execWaiters.Add(1)
	defer d.execWaiters.Add(-1)

	// 快速尝试
	select {
	case sem <- struct{}{}:
		d.act.set(sess.ID, ActRunning, nil, false)
		return func() { <-sem }, nil
	default:
	}

	// 排队等（带超时与取消）
	d.act.set(sess.ID, ActQueued, nil, false)
	start := hrtime.Now()
	timeout := d.execWaitTimeout
	var timer *time.Timer
	var expire <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		expire = timer.C
	}
	select {
	case sem <- struct{}{}:
		d.act.noteQueueWait(hrtime.Since(start))
		d.act.set(sess.ID, ActRunning, nil, false)
		return func() { <-sem }, nil
	case <-expire:
		return nil, &QueueTimeoutError{Timeout: timeout}
	case <-sess.CancelCh():
		return nil, ErrCanceled
	}
}

// ErrCanceled 表示操作被用户取消（面板 Kill）。
var ErrCanceled = errors.New("canceling statement due to user request")

// Session 取出（或创建）一个会话。
func (d *DB) Session(id string) *txn.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		s = txn.NewSession(id, d.Store, d.Mgr, d.Locks, d.Iso, d.lockOpts)
		// 注入信号量获取函数：session 在事务结束时调用 releaseAll 释放。
		s.SetExecToken(func() (func(), error) { return d.acquireExec(s) })
		// 注入观测钩子：txn 层只负责报告「等锁 / 执行 / 空闲」，
		// 剩下的（SQL 文本、耗时样本）由 db 层自己记。
		s.SetObserve(func(state string, tables []string) {
			// 事务结束要顺带把「还在事务里」的标记清掉，否则 Kill/回滚之后
			// 这条会话会一直挂在慢事务列表里（inTx 没人再改它）。
			if state == txn.ObsTxEnd {
				d.act.setTx(s.ID, false, time.Time{}, 0, time.Now())
				d.act.set(s.ID, ActIdle, nil, false)
				return
			}
			d.act.set(s.ID, state, tables, false)
		})
		d.sessions[id] = s
	}
	return s
}

// Exec 执行一段 SQL（可含多条语句），返回每条语句的结果。
// 出错时返回已成功语句的结果 + 错误。
//
// panic 一律在这里兜住并转成普通错误：执行过程中已经拿到的表锁、已经开好的
// 事务必须回滚掉，否则一个 bug 会让整个库的服务被一把锁吊死。上层（PG 协议的
// 消息循环）只需要把错误回给客户端，连接和进程都还能继续用。
func (d *DB) Exec(sess *txn.Session, text string) (out []*exec.Result, err error) {
	if d.closed.Load() {
		return nil, fmt.Errorf("database %q has been dropped", d.Name)
	}
	// 上一次 Kill 的取消标志如果还挂着，说明那条语句已经跑完并收过尾了。
	// 这里清掉再放行 —— 取消只针对「那一次动作」，不能让它把会话变成永久废人。
	if sess.Canceled() {
		sess.ClearCancel()
	}
	// 监控：SQL 文本先记下来，排队阶段面板也能看见「谁在等、等的是什么」。
	d.act.setUser(sess.ID, sess.User)
	d.act.setSQL(sess.ID, text, nil, false)

	// 并发上限：信号量按事务持有（不是按单条 SQL），避免锁×信号量交叉死锁。
	// 带背压：队列满/超时快速失败，客户端能感知并降级。
	if aerr := sess.AcquireExecToken(); aerr != nil {
		d.act.set(sess.ID, ActIdle, nil, false)
		return nil, aerr
	}
	// 兜底：如果 Exec 提前返回（解析错误等）且不在显式事务里，释放信号量。
	// 正常路径由 Stmt.End/releaseAll 释放，这里只兜异常出口。
	defer func() {
		if !sess.InTx() && sess.ExecTokenHeld() {
			sess.ForceRollback()
		}
	}()

	start := hrtime.Now()
	stmts, err := sql.Parse(text)
	if err != nil {
		d.act.set(sess.ID, ActIdle, nil, false)
		return nil, err
	}
	// 预扫描整批语句的表意图：显式事务一开就把锁全部拿到（保守 2PL）。
	// 这样既不会中途升级 S→X，也不会出现「事务之间交叉加锁」的死锁。
	pre := map[string]lock.Mode{}
	for _, st := range stmts {
		in, err := exec.TableIntents(st)
		if err != nil {
			d.act.set(sess.ID, ActIdle, nil, false)
			return nil, err
		}
		exec.MergeIntents(pre, in)
	}

	sess.Lock()
	defer sess.Unlock()
	sess.ResetLastWait()

	// 监控收尾：必须在持着 sess 锁时跑（要能安全回滚事务），
	// 又必须在 sess.Unlock 之前跑，所以注册在它之后（defer 是后进先出）。
	var totalTook int64
	defer func() {
		d.finishExec(sess, totalTook)
	}()

	defer func() {
		if r := recover(); r != nil {
			if os.Getenv("GODB_DEBUG_STACK") != "" {
				debug.PrintStack()
			}
			if sess.InTx() {
				sess.ForceRollback()
			}
			out = nil
			err = fmt.Errorf("internal error: %v", r)
		}
	}()

	for _, st := range stmts {
		var res *exec.Result
		var err error
		// 单条语句的表名/读写意图（比整批的 pre 精确，面板上看得准）。
		tables, write := stmtIntents(st)
		d.act.setSQL(sess.ID, text, tables, write)
		switch s := st.(type) {
		case *sql.CreateDatabase:
			res, err = d.createDatabase(sess, s)
		case *sql.DropDatabase:
			res, err = d.dropDatabase(sess, s)
		default:
			res, err = d.execOne(sess, st, pre)
		}
		if res != nil && res.TookNs > 0 {
			totalTook += res.TookNs
			d.act.noteExec(sess.ID, text, tables, write, res)
		}
		if err != nil {
			return out, err
		}
		// 事务控制语句不计入「已执行语句数」：面板上那个数字要回答
		// 「这个事务干了几件事」，BEGIN/COMMIT 本身不算事。
		if _, isTxCtl := st.(*sql.TxStmt); !isTxCtl {
			sess.NoteStmt()
		}
		out = append(out, res)
	}
	d.noteSlow(text, start, len(stmts))
	return out, nil
}

// finishExec 是一次 Exec 结束后的监控收尾：同步事务元信息、处理取消与慢 SQL 超时。
// 调用方必须持着会话锁。
func (d *DB) finishExec(sess *txn.Session, totalTook int64) {
	t := d.Timeouts()

	// 取消（面板 Kill）：执行中的语句没法打断，约定是「跑完就回滚」。
	if sess.Canceled() {
		if sess.InTx() {
			sess.ForceRollback()
		}
		sess.ClearCancel()
		d.act.countKill()
	}

	// 慢 SQL 超时：同样只能在语句结束后判定。整批耗时超阈值就回滚所在事务，
	// 让「一条 SQL 挂住整个事务」不至于一直持锁。
	if t.SlowSQLSec > 0 && sess.InTx() && totalTook > int64(t.SlowSQLSec)*int64(time.Second) {
		sess.ForceRollback()
		d.act.countSlowSQL()
	}

	inTx, txStart, stmts, _ := sess.TxInfo()
	d.act.setTx(sess.ID, inTx, txStart, stmts, time.Time{})
	d.act.set(sess.ID, ActIdle, nil, false)
}

// stmtIntents 取单条语句的表名与读写标记（供监控显示）。
func stmtIntents(st sql.Statement) ([]string, bool) {
	in, err := exec.TableIntents(st)
	if err != nil || len(in) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(in))
	write := false
	for name, mode := range in {
		names = append(names, name)
		if mode == lock.Exclusive {
			write = true
		}
	}
	sort.Strings(names)
	return names, write
}

// noteSlow 在整批语句耗时超过阈值时回调慢日志。
// 按「一次 Exec」而不是按单条语句计时：定位问题时看到的是一整条请求，
// 而且语句耗时各自记过（Result.TookNs），这里关心的是端到端。
func (d *DB) noteSlow(text string, start hrtime.Stamp, stmts int) {
	if d.SlowThreshold <= 0 || d.SlowLog == nil {
		return
	}
	took := time.Duration(hrtime.Since(start))
	if took < d.SlowThreshold {
		return
	}
	d.SlowLog(text, took, stmts)
}

func (d *DB) execOne(sess *txn.Session, st sql.Statement, pre map[string]lock.Mode) (*exec.Result, error) {
	start := hrtime.Now()
	res, err := d.execOneInner(sess, st, pre)
	if res != nil {
		res.TookNs = hrtime.Since(start)
	}
	return res, err
}

func (d *DB) execOneInner(sess *txn.Session, st sql.Statement, pre map[string]lock.Mode) (*exec.Result, error) {
	if ts, ok := st.(*sql.TxStmt); ok {
		return d.execTxControl(sess, ts, pre)
	}

	if _, ok := st.(*sql.Checkpoint); ok {
		return d.execCheckpoint(sess)
	}
	// ANALYZE：收集简单统计（行数、列基数），写进 syscat 供 pg_stats / pg_stat 使用。
	if an, ok := st.(*sql.Analyze); ok {
		return d.execAnalyze(sess, an)
	}

	// SHOW / SET 是会话参数，不碰表、不需要锁，也不受中毒事务影响
	// （真 PG 里 SET 在 aborted 事务里也能执行）。
	if sh, ok := st.(*sql.Show); ok {
		return exec.ShowResult(sh.Name), nil
	}
	if _, ok := st.(*sql.SetStmt); ok {
		return exec.SetResult(), nil
	}
	// DISCARD / RESET / DEALLOCATE：同样是会话级清理，不碰表、不需要锁。
	// 放在中毒事务检查之前，跟 SHOW / SET 一个待遇（客户端断开或还池时会发）。
	if sc, ok := st.(*sql.SessionCleanup); ok {
		return exec.CleanupResult(sc), nil
	}

	// SAVEPOINT 系列也要放在中毒检查之前：ROLLBACK TO 本来就是用来把事务
	// 从中毒状态里救回来的（ORM 的嵌套事务/TransactionScope 全靠这个）。
	if sp, ok := st.(*sql.SavepointStmt); ok {
		return d.execSavepoint(sess, sp)
	}

	if sess.InTx() && sess.Aborted() {
		return nil, ErrAborted
	}
	if sess.InTx() && isDDL(st) {
		return nil, fmt.Errorf("cannot run DDL inside a transaction block (this implementation limitation)")
	}

	intents, err := exec.TableIntents(st)
	if err != nil {
		return nil, err
	}
	// 按需加载：把这条语句要碰的表先读进内存。
	// 必须放在拿 SchemaMu 之前 —— 加载是磁盘 IO，不能持着锁做。
	if len(intents) > 0 {
		names := make([]string, 0, len(intents))
		for n := range intents {
			names = append(names, n)
		}
		d.Store.LoadTables(names)
	}

	// 1) 先拿表锁（可能长时间等待，此时不持 schema 锁，避免 DDL 被连带阻塞）
	stmt, err := sess.BeginStatement(intents)
	if err != nil {
		return nil, err
	}

	// 2) 再执行（DDL 独占 schema，普通语句持读锁）
	//
	// 解锁必须用 defer：引擎里任何一处 panic（曾被「无参函数取 vals[0]」踩到）
	// 都会跳过 Unlock，SchemaMu 被永久占住 —— 之后所有语句、甚至 Close 都会
	// 卡死，整个库看起来像挂了。panic 由 Exec 的 recover 兜住并转成错误，
	// 但锁不能再靠调用顺序保证。
	res, execErr := d.execLocked(stmt, st)
	// 扫描量：从事务计数器取走（取走即清零，所以显式事务里多条语句各算各的）。
	// 放在 End 之前 —— 自动提交会在 End 里把 tx 置空。
	if res != nil {
		if tx := stmt.Tx(); tx != nil {
			res.Scanned = tx.TakeScanRows()
		}
	}
	// lastval() 是会话级的：把本事务最后一次分配的序列值带回会话。
	if v, ok := stmt.Tx().LastSeqValue(); ok {
		sess.SetLastSeq(v)
	}

	// 3) 收尾：语句级回滚 + 自动提交
	endErr := stmt.End(execErr)
	// 事务计数（pg_stat_database 的 xact_commit / xact_rollback）
	if execErr != nil {
		syscat.NoteXact(false)
	} else if stmt.Auto() {
		syscat.NoteXact(endErr == nil)
	}
	if endErr != nil {
		return nil, endErr
	}
	return res, nil
}

func (d *DB) execTxControl(sess *txn.Session, ts *sql.TxStmt, pre map[string]lock.Mode) (*exec.Result, error) {
	switch ts.Kind {
	case "begin":
		if err := sess.Begin(pre); err != nil {
			return nil, err
		}
		// 客户端请求的事务模式只是记下来：godb 恒为 SERIALIZABLE（表级 S/X
		// 两阶段封锁），不降级。回显一下让客户端知道实际生效的级别。
		msg := ""
		if ts.Isolation != "" && ts.Isolation != "serializable" {
			msg = fmt.Sprintf("requested %s isolation, godb always runs SERIALIZABLE", ts.Isolation)
		}
		return &exec.Result{Command: "BEGIN", Tag: "BEGIN", Message: msg}, nil

	case "commit":
		if !sess.InTx() {
			return &exec.Result{Command: "COMMIT", Tag: "COMMIT", Message: "no transaction in progress"}, nil
		}
		// PG 语义：事务已中毒时 COMMIT 等价于 ROLLBACK
		if sess.Aborted() {
			_ = sess.Rollback()
			syscat.NoteXact(false)
			return &exec.Result{Command: "ROLLBACK", Tag: "ROLLBACK", Message: "事务已中止，已回滚"}, nil
		}
		d.Store.SchemaMu.RLock()
		err := sess.Commit()
		d.Store.SchemaMu.RUnlock()
		syscat.NoteXact(err == nil)
		if err != nil {
			return nil, err
		}
		return &exec.Result{Command: "COMMIT", Tag: "COMMIT"}, nil

	case "rollback":
		if !sess.InTx() {
			return &exec.Result{Command: "ROLLBACK", Tag: "ROLLBACK", Message: "no transaction in progress"}, nil
		}
		if err := sess.Rollback(); err != nil {
			return nil, err
		}
		syscat.NoteXact(false)
		return &exec.Result{Command: "ROLLBACK", Tag: "ROLLBACK"}, nil
	}
	return nil, fmt.Errorf("unsupported transaction command")
}

// execLocked 在 schema 锁保护下执行一条语句。DDL 独占，普通语句共享读锁；
// 解锁走 defer，保证 panic 时锁也一定会放开。
func (d *DB) execLocked(stmt *txn.Stmt, st sql.Statement) (*exec.Result, error) {
	if isDDL(st) {
		d.Store.SchemaMu.Lock()
		defer d.Store.SchemaMu.Unlock()
	} else {
		d.Store.SchemaMu.RLock()
		defer d.Store.SchemaMu.RUnlock()
	}
	return d.Engine.ExecStmt(stmt.Tx(), st)
}

// execSavepoint 实现 SAVEPOINT / RELEASE / ROLLBACK TO。
//
// 存储层本来就有语句级快照（Mark/RestoreTo），保存点就是在此基础上再压一层，
// 所以 ROLLBACK TO 能把事务从「中毒」状态里救回来继续用 —— ORM 的嵌套事务
// （Npgsql 的 TransactionScope）依赖这个语义。
func (d *DB) execSavepoint(sess *txn.Session, sp *sql.SavepointStmt) (*exec.Result, error) {
	switch sp.Kind {
	case "savepoint":
		if err := sess.Savepoint(sp.Name); err != nil {
			return nil, err
		}
		return &exec.Result{Command: "SAVEPOINT", Tag: "SAVEPOINT"}, nil
	case "release":
		if err := sess.ReleaseSavepoint(sp.Name); err != nil {
			return nil, err
		}
		return &exec.Result{Command: "RELEASE", Tag: "RELEASE"}, nil
	case "rollback_to":
		if err := sess.RollbackToSavepoint(sp.Name); err != nil {
			return nil, err
		}
		return &exec.Result{Command: "ROLLBACK", Tag: "ROLLBACK"}, nil
	}
	return nil, fmt.Errorf("unsupported savepoint command %q", sp.Kind)
}

func isDDL(st sql.Statement) bool {
	switch st.(type) {
	case *sql.CreateTable, *sql.DropTable, *sql.AlterTable, *sql.CommentOn:
		return true
	}
	return false
}

// execAnalyze 扫一遍用户表，收集行数与列基数（distinct 上限 1024 个值）。
func (d *DB) execAnalyze(sess *txn.Session, _ *sql.Analyze) (*exec.Result, error) {
	stmt, err := sess.BeginStatement(nil)
	if err != nil {
		return nil, err
	}
	defer stmt.End(nil)
	tx := stmt.Tx()
	nTables := 0
	for _, t := range d.Cat.Tables {
		if strings.HasPrefix(strings.ToLower(t.Name), "pg_") ||
			strings.HasPrefix(strings.ToLower(t.Name), "information_schema") {
			continue
		}
		cols := make(map[string]syscat.ColStats, len(t.Columns))
		seen := make([]map[string]bool, len(t.Columns))
		for i := range seen {
			seen[i] = map[string]bool{}
		}
		var rows int64
		err := tx.Scan(t.Name, func(_ storage.Ref, row types.Row) error {
			rows++
			for i := range t.Columns {
				if i >= len(row) || row[i].IsNull() {
					continue
				}
				c := t.Columns[i]
				cs := cols[strings.ToLower(c.Name)]
				cs.NotNull++
				if len(seen[i]) < 1024 {
					seen[i][types.Key([]types.Value{row[i]})] = true
					cs.Distinct = int64(len(seen[i]))
				}
				cols[strings.ToLower(c.Name)] = cs
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		syscat.RecordAnalyze(t.Name, rows, cols)
		nTables++
	}
	return &exec.Result{Command: "ANALYZE", Tag: "ANALYZE",
		Message: fmt.Sprintf("analyzed %d tables", nTables)}, nil
}

// TxState 是会话的事务状态（给前端显示用）。
type TxState struct {
	Active  bool     `json:"active"`
	Aborted bool     `json:"aborted"`
	Changes int      `json:"changes"`
	Held    []string `json:"held,omitempty"`
	WaitNs  int64    `json:"waitNs,omitempty"`
}

// TxState 读取会话当前事务状态。
func (d *DB) TxState(s *txn.Session) TxState {
	s.Lock()
	defer s.Unlock()
	st := TxState{Active: s.InTx(), Aborted: s.Aborted()}
	if tx := s.Current(); tx != nil {
		st.Changes = tx.Changes()
	}
	for name, mode := range s.HeldLocks() {
		st.Held = append(st.Held, name+":"+mode.String())
	}
	if w := s.LastWaitNs(); w > 0 {
		st.WaitNs = w
	}
	return st
}

// Tables 返回所有表结构与行数。
func (d *DB) Tables() []TableInfo {
	d.Store.SchemaMu.RLock()
	defer d.Store.SchemaMu.RUnlock()
	out := make([]TableInfo, 0, len(d.Cat.Tables))
	for _, t := range d.Cat.Tables {
		ti := TableInfo{Name: t.Name, Rows: d.Store.RowCount(t.Name)}
		for _, c := range t.Columns {
			ti.Columns = append(ti.Columns, ColumnInfo{
				Name:       c.Name,
				Type:       c.Type().String(),
				NotNull:    c.NotNull,
				PrimaryKey: c.PrimaryKey,
				Unique:     c.Unique,
				Serial:     c.Serial,
				Default:    c.Default,
			})
		}
		out = append(out, ti)
	}
	return out
}

// LockSnapshot 返回当前锁状态。
func (d *DB) LockSnapshot() lock.Snapshot {
	return d.Locks.Snapshot()
}

// DropAll 删除所有表（用于重置演示库 / 重建示例数据）。
//
// 删之前先把全部表 + 当时的 catalog 复制到 <data>/.trash/：这两个入口
// （POST /api/reset、POST /api/sample）都是一键操作，点错了没有任何补救机会。
// 备份失败就不删 —— 调用方拿到错误比拿到一个空库好。
func (d *DB) DropAll() error {
	d.Store.SchemaMu.Lock()
	defer d.Store.SchemaMu.Unlock()
	if _, err := d.Store.TrashTables(storage.TrashReasonDropAll, nil); err != nil {
		return fmt.Errorf("备份到回收站失败，已取消清空: %w", err)
	}
	names := d.Cat.Names()
	for _, n := range names {
		if err := d.Cat.Drop(n); err != nil {
			return err
		}
		d.Store.DropTable(n)
	}
	if err := d.Cat.Save(d.dir); err != nil {
		return err
	}
	return d.Store.Checkpoint()
}

// sweepSessions 定期回收长期空闲且持有事务的会话，防止锁被永久占用。
// 前端在事务打开期间会定时发心跳请求（刷新活动时间），因此只有真正被遗弃的事务会被回收。
//
// 两档阈值：
//   - 面板上的「慢事务超时」（默认不限）：事务空闲超过它就回滚 —— 这是给用户的保护开关；
//   - 3 分钟遗弃回收：与配置无关的安全网，PG 客户端异常断开时锁不会永久被占。
//
// 5 秒一轮而不是 30 秒：用户把慢事务阈值设成 10 秒时，30 秒的巡检精度等于没设。
func (d *DB) sweepSessions() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
		}
		d.mu.Lock()
		list := make([]*txn.Session, 0, len(d.sessions))
		for _, s := range d.sessions {
			list = append(list, s)
		}
		d.mu.Unlock()
		limit := time.Duration(d.Timeouts().SlowTxIdleSec) * time.Second
		for _, s := range list {
			// 先拿会话锁再读状态：InTx() 读的是裸指针 s.tx，而执行线程会在
			// 持这把锁的情况下改写它。TryLock 成功就说明没人正在执行，此时
			// 读到的状态是一致的；失败则说明这个会话正在跑语句，跳过即可
			// —— 它马上会自己收尾，硬抢锁会把执行打乱。
			if !s.TryLock() {
				continue
			}
			if !s.InTx() {
				s.Unlock()
				continue
			}
			idle := s.Idle()
			slow := limit > 0 && idle > limit
			abandoned := idle > 3*time.Minute
			if !slow && !abandoned {
				s.Unlock()
				continue
			}
			s.ForceRollback()
			s.Unlock()
			if slow {
				d.act.countSlowTx()
			}
		}
		// 空闲超过 10 分钟且没有事务的会话记录没必要留着（会话对象本身也一并在
		// 这里清掉：Web 每开一个浏览器就建一个，长期不清理会慢慢涨）。
		d.act.drop(10 * time.Minute)
	}
}
