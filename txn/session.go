package txn

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"godb/hrtime"
	"godb/lock"
	"godb/storage"
)

// ErrNoTx 表示当前没有活动事务。
var ErrNoTx = errors.New("there is no transaction in progress")

// ErrNothingToCommit 表示提交时没有事务。
var ErrNothingToCommit = errors.New("there is no transaction in progress")

// Isolation 是并发控制模式。
type Isolation int

const (
	// TableLock 表级 S/X 锁（默认）：读表加 S、写表加 X，锁持到事务结束。
	TableLock Isolation = iota
	// SerialLock 全局串行（旧行为）：同一时刻只允许一个事务。
	SerialLock
)

func (i Isolation) String() string {
	if i == SerialLock {
		return "serial"
	}
	return "table"
}

// ParseIsolation 解析隔离模式参数。
func ParseIsolation(s string) (Isolation, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "table", "tablelock", "rowlock":
		return TableLock, nil
	case "serial", "global":
		return SerialLock, nil
	}
	return TableLock, fmt.Errorf("未知的隔离模式 %q（可选 table / serial）", s)
}

// Session 是一个客户端会话，持有（可选的）显式事务与其表锁。
type Session struct {
	ID string
	// User 是登录用户名（current_user 回显）。
	User string

	// execToken 是并发执行信号量的令牌（由 db 层注入）。
	// 信号量按事务持有：BEGIN 时获取，COMMIT/ROLLBACK 时释放。
	// 这样保证「持有表锁 ⊆ 持有信号量」，不会出现锁×信号量交叉死锁。
	execToken   bool
	releaseExec func()
	_execAcquire func() (func(), error) // db 层注入的信号量获取函数

	mu    sync.Mutex // 保证同一会话的请求串行执行
	st    *storage.Store
	iso   Isolation
	mgr   *Manager
	locks *lock.Manager
	opts  lock.Options

	tx         *storage.Tx
	lockTx     int64
	age        int64
	global     bool // serial 模式下是否持全局锁
	aborted    bool
	last       time.Time
	lastWaitNs int64
	// savepoints 是 SAVEPOINT 栈（由内到外）。提交/回滚时整栈丢弃。
	savepoints []savepoint
	// lastSeq 是会话级的最后一次序列值（lastval() 跨语句有效）。
	lastSeq    int64
	hasLastSeq bool
}

// savepoint 是一个保存点：名字 + 当时的事务快照。
type savepoint struct {
	name string
	mark *storage.TxSnapshot
}

// Savepoint 建立保存点。同名保存点允许并存：查找时最近的那个生效，
// 和 PostgreSQL 的行为一致（旧的只是被挡住，RELEASE 到底名时才被丢弃）。
func (s *Session) Savepoint(name string) error {
	if s.tx == nil {
		return errors.New("SAVEPOINT can only be used in transaction blocks")
	}
	s.savepoints = append(s.savepoints, savepoint{name: normalizeSavepointName(name), mark: s.tx.Mark()})
	s.Touch()
	return nil
}

// RollbackToSavepoint 回滚到保存点：保存点本身保留（可以再回滚一次），
// 之后建立的保存点全部丢弃，事务从中毒状态恢复可用。
func (s *Session) RollbackToSavepoint(name string) error {
	if s.tx == nil {
		return errors.New("ROLLBACK TO SAVEPOINT can only be used in transaction blocks")
	}
	idx := s.findSavepoint(name)
	if idx < 0 {
		return fmt.Errorf("savepoint %q does not exist", name)
	}
	s.tx.RestoreTo(s.savepoints[idx].mark.Clone())
	s.savepoints = s.savepoints[:idx+1]
	s.aborted = false
	s.Touch()
	return nil
}

// ReleaseSavepoint 释放保存点及其之后的所有层。
func (s *Session) ReleaseSavepoint(name string) error {
	if s.tx == nil {
		return errors.New("RELEASE SAVEPOINT can only be used in transaction blocks")
	}
	idx := s.findSavepoint(name)
	if idx < 0 {
		return fmt.Errorf("savepoint %q does not exist", name)
	}
	s.savepoints = s.savepoints[:idx]
	s.Touch()
	return nil
}

// SetLastSeq 记录会话最后一次分配的自增值。
func (s *Session) SetLastSeq(v int64) {
	s.lastSeq, s.hasLastSeq = v, true
}

// SavepointDepth 返回当前保存点层数（测试与前端用）。
func (s *Session) SavepointDepth() int { return len(s.savepoints) }

func (s *Session) findSavepoint(name string) int {
	want := normalizeSavepointName(name)
	for i := len(s.savepoints) - 1; i >= 0; i-- {
		if s.savepoints[i].name == want {
			return i
		}
	}
	return -1
}

func normalizeSavepointName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// NewSession 创建会话。
func NewSession(id string, st *storage.Store, mgr *Manager, locks *lock.Manager, iso Isolation, opts lock.Options) *Session {
	return &Session{
		ID:    id,
		st:    st,
		iso:   iso,
		mgr:   mgr,
		locks: locks,
		opts:  opts,
		last:  time.Now(),
	}
}

// Lock 由执行层加锁，保证同一会话内语句不并发执行；同时刷新活动时间。
func (s *Session) Lock() {
	s.mu.Lock()
	s.Touch()
}

// Unlock 释放会话锁。
func (s *Session) Unlock() { s.mu.Unlock() }

// InTx 是否处于显式事务中。
func (s *Session) InTx() bool { return s.tx != nil }

// Aborted 事务是否已因错误进入“中毒”状态（PG 行为）。
func (s *Session) Aborted() bool { return s.aborted }

// Current 返回当前事务（可能为 nil）。
func (s *Session) Current() *storage.Tx { return s.tx }

// Isolation 返回隔离模式。
func (s *Session) Isolation() Isolation { return s.iso }

// Touch 刷新活动时间。
func (s *Session) Touch() { s.last = time.Now() }

// Idle 返回距上次活动的时长。
func (s *Session) Idle() time.Duration { return time.Since(s.last) }

// LastWait 返回最近一次加锁等待的时长（0 表示没等过）。
func (s *Session) LastWait() time.Duration { return time.Duration(s.lastWaitNs) }

// LastWaitNs 返回最近一次加锁等待的纳秒数。
func (s *Session) LastWaitNs() int64 { return s.lastWaitNs }

// ResetLastWait 清掉上次等待时长。
func (s *Session) ResetLastWait() { s.lastWaitNs = 0 }

// startTx 建立一个新事务并一次性拿到它需要的锁（保守 2PL）。
func (s *Session) startTx(intents map[string]lock.Mode) error {
	if s.iso == SerialLock {
		if err := s.mgr.Acquire(s.ID, s.opts.Timeout); err != nil {
			return err
		}
		s.global = true
		s.tx = s.st.Begin()
		s.tx.User = s.User
		s.inheritLastSeq()
		s.aborted = false
		return nil
	}

	tx := s.st.Begin()
	tx.User = s.User
	id, age := s.locks.NewTx(s.ID)
	start := hrtime.Now()
	if err := s.locks.AcquireAll(id, s.ID, age, intents, s.opts); err != nil {
		tx.Rollback()
		s.lastWaitNs = hrtime.Since(start)
		return err
	}
	s.noteWait(start)
	s.tx = tx
	s.inheritLastSeq()
	s.lockTx = id
	s.age = age
	s.aborted = false
	return nil
}

// inheritLastSeq 把会话级的 lastval 值带进新事务（lastval() 跨语句有效）。
func (s *Session) inheritLastSeq() {
	if s.tx != nil && s.hasLastSeq {
		s.tx.SetLastSeq(s.lastSeq)
	}
}

func (s *Session) noteWait(start hrtime.Stamp) {
	if d := hrtime.Since(start); d > 2_000_000 { // 超过 2ms 才记录
		s.lastWaitNs = d
	}
}

// EnsureLocks 为事务补齐新语句需要的表锁（增量加锁）。
func (s *Session) EnsureLocks(intents map[string]lock.Mode) error {
	if s.iso == SerialLock || len(intents) == 0 || s.lockTx == 0 {
		return nil
	}
	held := s.locks.Held(s.lockTx)
	need := map[string]lock.Mode{}
	for name, mode := range intents {
		if cur, ok := held[name]; ok && cur >= mode {
			continue
		}
		need[name] = mode
	}
	if len(need) == 0 {
		return nil
	}
	start := hrtime.Now()
	if err := s.locks.AcquireAll(s.lockTx, s.ID, s.age, need, s.opts); err != nil {
		s.lastWaitNs = hrtime.Since(start)
		return err
	}
	s.noteWait(start)
	return nil
}

// Stmt 是一条语句的执行上下文。
type Stmt struct {
	s    *Session
	auto bool
	mark *storage.TxSnapshot
}

// Tx 返回语句所在事务。
func (st *Stmt) Tx() *storage.Tx { return st.s.tx }

// Auto 返回这条语句是否是自动提交（不在显式事务里）。
func (st *Stmt) Auto() bool { return st.auto }

// BeginStatement 开始一条语句：确保有事务且锁已就位（可能阻塞等待）。
// 调用方在拿到 *Stmt 后再去执行引擎执行，最后必须调用 End。
//
// 约定：锁拿不到（超时/死锁）即整事务放弃回滚，而不是把事务留在中毒状态 ——
// 否则它会一直持着之前拿到的锁把别人堵住。
func (s *Session) BeginStatement(intents map[string]lock.Mode) (*Stmt, error) {
	auto := s.tx == nil
	if auto {
		if err := s.startTx(intents); err != nil {
			return nil, err
		}
	} else if err := s.EnsureLocks(intents); err != nil {
		s.tx.Rollback()
		s.releaseAll()
		return nil, err
	}
	s.Touch()
	return &Stmt{s: s, auto: auto, mark: s.tx.Mark()}, nil
}

// End 收尾一条语句：语句级回滚 +（自动提交时）提交。
func (st *Stmt) End(err error) error {
	s := st.s
	if err != nil {
		s.tx.RestoreTo(st.mark)
		if st.auto {
			s.tx.Rollback()
			s.releaseAll()
		} else {
			s.aborted = true
		}
		return err
	}
	if st.auto {
		cerr := s.tx.Commit()
		s.releaseAll()
		return cerr
	}
	return nil
}

// Begin 开启显式事务。pre 是整批语句预扫描出来的表意图（可为空）。
func (s *Session) Begin(pre map[string]lock.Mode) error {
	if s.tx != nil {
		return errors.New("there is already a transaction in progress")
	}
	if err := s.startTx(pre); err != nil {
		return err
	}
	s.Touch()
	return nil
}

// Commit 提交显式事务。
func (s *Session) Commit() error {
	if s.tx == nil {
		return ErrNothingToCommit
	}
	tx := s.tx
	err := tx.Commit()
	s.releaseAll()
	s.Touch()
	return err
}

// Rollback 回滚显式事务。
func (s *Session) Rollback() error {
	if s.tx == nil {
		return ErrNoTx
	}
	s.tx.Rollback()
	s.releaseAll()
	s.Touch()
	return nil
}

// MarkAborted 把事务置为中毒状态（后续语句一律失败，直到 ROLLBACK）。
func (s *Session) MarkAborted() { s.aborted = true }

// ForceRollback 在会话超时/断连时强制清理事务并释放锁。
func (s *Session) ForceRollback() {
	if s.tx == nil {
		s.releaseAll()
		return
	}
	s.tx.Rollback()
	s.releaseAll()
	s.Touch()
}

// HeldLocks 返回本会话当前持有的表锁。
func (s *Session) HeldLocks() map[string]lock.Mode {
	if s.iso == SerialLock || s.lockTx == 0 {
		return nil
	}
	return s.locks.Held(s.lockTx)
}

func (s *Session) releaseAll() {
	if s.iso == SerialLock {
		if s.global {
			s.mgr.Release(s.ID)
			s.global = false
		}
	} else if s.lockTx != 0 {
		s.locks.Release(s.lockTx)
	}
	// 释放并发执行信号量（按事务持有，事务结束才放）
	if s.execToken {
		s.execToken = false
		if s.releaseExec != nil {
			s.releaseExec()
			s.releaseExec = nil
		}
	}
	s.tx = nil
	s.lockTx = 0
	s.age = 0
	s.aborted = false
	s.savepoints = nil
}

// SetExecToken 注入并发执行信号量的获取/释放函数（db 层调用）。
func (s *Session) SetExecToken(acquire func() (func(), error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s._execAcquire = acquire
}

// AcquireExecToken 获取并发执行信号量（按事务持有）。
// 已持有则跳过。返回 error 表示背压拒绝（队列满/超时）。
func (s *Session) AcquireExecToken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execToken {
		return nil
	}
	if s._execAcquire == nil {
		return nil
	}
	release, err := s._execAcquire()
	if err != nil {
		return err
	}
	s.releaseExec = release
	s.execToken = true
	return nil
}

// ExecTokenHeld 是否已持有信号量。
func (s *Session) ExecTokenHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execToken
}
