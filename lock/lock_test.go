package lock

import (
	"testing"
	"time"
)

func opts(timeout time.Duration) Options {
	return Options{
		Timeout:        timeout,
		Retry:          5 * time.Millisecond,
		DeadlockDetect: true,
		DeadlockAfter:  50 * time.Millisecond,
	}
}

func mustAcquire(t *testing.T, m *Manager, tx int64, session string, want map[string]Mode, o Options) {
	t.Helper()
	if err := m.AcquireAll(tx, session, tx, want, o); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
}

func TestSharedCompatible(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t": Shared}, opts(time.Second))
	mustAcquire(t, m, tx2, "s2", map[string]Mode{"t": Shared}, opts(time.Second))

	if len(m.Held(tx1)) != 1 || m.Held(tx1)["t"] != Shared {
		t.Fatalf("tx1 should hold S")
	}
}

func TestExclusiveBlocksShared(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t": Exclusive}, opts(time.Second))

	err := m.AcquireAll(tx2, "s2", tx2, map[string]Mode{"t": Shared}, opts(80*time.Millisecond))
	if err == nil {
		t.Fatalf("expected timeout, got nil")
	}
	if _, ok := err.(*TimeoutError); !ok {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
}

func TestSharedBlocksExclusive(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t": Shared}, opts(time.Second))

	err := m.AcquireAll(tx2, "s2", tx2, map[string]Mode{"t": Exclusive}, opts(80*time.Millisecond))
	te, ok := err.(*TimeoutError)
	if !ok {
		t.Fatalf("expected TimeoutError, got %T", err)
	}
	if len(te.Blocked) != 1 || te.Blocked[0].Table != "t" {
		t.Fatalf("blocked info wrong: %+v", te.Blocked)
	}
	if te.Blocked[0].Holders[0].Session != "s1" {
		t.Fatalf("blocker session wrong: %+v", te.Blocked[0].Holders)
	}
}

func TestBatchAtomic(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"b": Exclusive}, opts(time.Second))

	// 申请 {a, b}：b 被占，则 a 也不能被授予
	err := m.AcquireAll(tx2, "s2", tx2, map[string]Mode{"a": Exclusive, "b": Shared}, opts(50*time.Millisecond))
	if err == nil {
		t.Fatalf("expected failure")
	}
	if _, held := m.Held(tx2)["a"]; held {
		t.Fatalf("partial lock leaked: a should not be held")
	}
}

func TestReleaseUnblocks(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t": Exclusive}, opts(time.Second))

	done := make(chan error, 1)
	go func() {
		done <- m.AcquireAll(tx2, "s2", tx2, map[string]Mode{"t": Shared}, opts(2*time.Second))
	}()
	time.Sleep(60 * time.Millisecond)
	m.Release(tx1)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquire after release failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("waiter not woken up")
	}
}

func TestUpgrade(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	// 两个事务都持 S
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t": Shared}, opts(time.Second))
	mustAcquire(t, m, tx2, "s2", map[string]Mode{"t": Shared}, opts(time.Second))

	// 自升级被其他 S 持有者挡住
	err := m.AcquireAll(tx1, "s1", tx1, map[string]Mode{"t": Exclusive}, opts(50*time.Millisecond))
	if err == nil {
		t.Fatalf("upgrade should be blocked by another S holder")
	}

	// 唯一持有者时可以升级
	m.Release(tx2)
	if err := m.AcquireAll(tx1, "s1", tx1, map[string]Mode{"t": Exclusive}, opts(time.Second)); err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	if m.Held(tx1)["t"] != Exclusive {
		t.Fatalf("mode should be X after upgrade")
	}
}

func TestDeadlockDetection(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("old")
	tx2, _ := m.NewTx("young")
	mustAcquire(t, m, tx1, "old", map[string]Mode{"a": Exclusive}, opts(time.Second))
	mustAcquire(t, m, tx2, "young", map[string]Mode{"b": Exclusive}, opts(time.Second))

	type res struct {
		name string
		err  error
	}
	ch := make(chan res, 2)
	go func() {
		ch <- res{"old", m.AcquireAll(tx1, "old", tx1, map[string]Mode{"b": Exclusive}, opts(3*time.Second))}
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		ch <- res{"young", m.AcquireAll(tx2, "young", tx2, map[string]Mode{"a": Exclusive}, opts(3*time.Second))}
	}()

	// 年轻的那个应该报死锁并放弃，老的随后拿到锁
	var deadlockSeen bool
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			if r.err != nil {
				if _, ok := r.err.(*DeadlockError); !ok {
					t.Fatalf("%s got unexpected error %T: %v", r.name, r.err, r.err)
				}
				if r.name != "young" {
					t.Fatalf("deadlock should abort the youngest, but %s aborted", r.name)
				}
				deadlockSeen = true
				m.Release(tx2)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("timeout waiting for results")
		}
	}
	if !deadlockSeen {
		t.Fatalf("no deadlock detected")
	}
}

func TestSnapshot(t *testing.T) {
	m := New()
	tx1, _ := m.NewTx("s1")
	tx2, _ := m.NewTx("s2")
	mustAcquire(t, m, tx1, "s1", map[string]Mode{"t1": Exclusive, "t2": Shared}, opts(time.Second))
	mustAcquire(t, m, tx2, "s2", map[string]Mode{"t2": Shared}, opts(time.Second))

	snap := m.Snapshot()
	if len(snap.Tables) != 2 {
		t.Fatalf("tables = %d, want 2", len(snap.Tables))
	}
	for _, tb := range snap.Tables {
		if tb.Name == "t2" && len(tb.Holders) != 2 {
			t.Fatalf("t2 should have 2 holders, got %d", len(tb.Holders))
		}
	}
}
