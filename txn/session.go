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
		s.aborted = false
		return nil
	}

	tx := s.st.Begin()
	id, age := s.locks.NewTx(s.ID)
	start := hrtime.Now()
	if err := s.locks.AcquireAll(id, s.ID, age, intents, s.opts); err != nil {
		tx.Rollback()
		s.lastWaitNs = hrtime.Since(start)
		return err
	}
	s.noteWait(start)
	s.tx = tx
	s.lockTx = id
	s.age = age
	s.aborted = false
	return nil
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
	s.tx = nil
	s.lockTx = 0
	s.age = 0
	s.aborted = false
}
