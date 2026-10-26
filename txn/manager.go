// Package txn 提供“全局串行化”的写锁与会话状态。
package txn

import (
	"fmt"
	"sync"
	"time"
)

// Manager 用容量为 1 的信号量实现“同一时刻只允许一个写事务”。
// 持有锁的事务从 BEGIN 到 COMMIT/ROLLBACK 期间独占，因此天然可串行化。
type Manager struct {
	sem chan struct{}

	mu     sync.Mutex
	holder string
	since  time.Time
}

// NewManager 创建锁管理器。
func NewManager() *Manager {
	m := &Manager{sem: make(chan struct{}, 1)}
	m.sem <- struct{}{}
	return m
}

// Acquire 获取写锁，超时返回错误。
func (m *Manager) Acquire(owner string, timeout time.Duration) error {
	select {
	case <-m.sem:
	default:
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		holder, since := m.Holder()
		select {
		case <-m.sem:
		case <-time.After(timeout):
			wait := ""
			if !since.IsZero() {
				wait = fmt.Sprintf("（已持有 %s）", time.Since(since).Truncate(time.Second))
			}
			return fmt.Errorf("等待写锁超时：会话 %s 正在执行事务%s，请先在那边 COMMIT 或 ROLLBACK", shortID(holder), wait)
		}
	}
	m.mu.Lock()
	m.holder = owner
	m.since = time.Now()
	m.mu.Unlock()
	return nil
}

// Release 释放写锁（仅持有者可释放）。
func (m *Manager) Release(owner string) {
	m.mu.Lock()
	if m.holder == owner {
		m.holder = ""
		m.since = time.Time{}
	}
	m.mu.Unlock()
	select {
	case m.sem <- struct{}{}:
	default:
	}
}

// Holder 返回当前持有者与开始时间。
func (m *Manager) Holder() (string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holder, m.since
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "?"
	}
	return id
}
