// Package lock 实现表级共享/排它锁（S/X），支持批量原子申请、等待重试、
// 超时放弃与等待图死锁检测。
//
// 设计要点：
//   - 锁粒度是「表」，模式只有 S（共享/读）和 X（排它/写）；
//   - 一个事务对一张表最多持一把锁，只能把 S 升级成 X，不能降级；
//   - 一次可以申请多张表，全部可授予才一次性授予（要么全给，要么一个不给）；
//   - 拿不到就等：每 Retry 间隔重试一次，超过 Timeout 放弃；
//   - 等待超过 DeadlockAfter 后检查等待图，成环时环里最年轻的事务自己放弃。
package lock

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mode 是锁模式。
type Mode int

const (
	None Mode = iota
	Shared
	Exclusive
)

// MarshalJSON 让模式以 "S"/"X" 字符串出现在 JSON 里，前端好读。
func (m Mode) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

func (m Mode) String() string {
	switch m {
	case Shared:
		return "S"
	case Exclusive:
		return "X"
	}
	return "-"
}

// Options 控制等待行为。
type Options struct {
	Timeout        time.Duration // 单个申请的最长等待
	Retry          time.Duration // 轮询间隔（会加 ±50% 抖动）
	DeadlockDetect bool          // 是否做等待图检测
	DeadlockAfter  time.Duration // 等待多久后开始检测
}

// DefaultOptions 是默认等待策略。
func DefaultOptions() Options {
	return Options{
		Timeout:        3 * time.Second,
		Retry:          20 * time.Millisecond,
		DeadlockDetect: true,
		DeadlockAfter:  500 * time.Millisecond,
	}
}

// Holder 是一个锁持有者。
type Holder struct {
	Tx      int64     `json:"tx"`
	Session string    `json:"session"`
	Age     int64     `json:"age"`
	Mode    Mode      `json:"mode"`
	Since   time.Time `json:"since"`
}

// Table 是某张表在某一时刻的持锁与等待情况。
type Table struct {
	Name    string   `json:"name"`
	Holders []Holder `json:"holders"`
	Waiters []Holder `json:"waiters"`
}

// Waiter 是一个正在等待的事务。
type Waiter struct {
	Tx      int64     `json:"tx"`
	Session string    `json:"session"`
	Tables  []string  `json:"tables"`
	Since   time.Time `json:"since"`
}

// Snapshot 是锁状态的快照，供面板展示。
type Snapshot struct {
	Tables    []Table  `json:"tables"`
	Waiters   []Waiter `json:"waiters"`
	Conflicts int64    `json:"conflicts"`
	LastBlock string   `json:"lastBlock"`
}

// Blocked 描述一个申请被哪些持有者挡住。
type Blocked struct {
	Table   string
	Holders []Holder
}

// TimeoutError 表示等待锁超时。
type TimeoutError struct {
	Waited  time.Duration
	Blocked []Blocked
}

func (e *TimeoutError) Error() string {
	parts := make([]string, 0, len(e.Blocked))
	for _, b := range e.Blocked {
		names := make([]string, 0, len(b.Holders))
		for _, h := range b.Holders {
			names = append(names, fmt.Sprintf("%s(%s)", shortSession(h.Session), h.Mode))
		}
		parts = append(parts, fmt.Sprintf("表 %s 被 %s 持有", b.Table, strings.Join(names, "、")))
	}
	return fmt.Sprintf("等待锁超时（已等 %s）：%s", e.Waited.Truncate(time.Millisecond), strings.Join(parts, "；"))
}

// DeadlockError 表示多个事务互相等待（等待环），本事务被判定为环里最年轻的而放弃。
// 解决策略是「后进让先进」：最年轻的事务让位，避免长时间互等。
type DeadlockError struct {
	Cycle []int64
	Peers []string
	Waited time.Duration // 本次已等待时长
}

func (e *DeadlockError) Error() string {
	return fmt.Sprintf("等待锁冲突（已等 %s）：%d 个事务互相等待（%s），后进让先进，本事务被回滚，请改用原子写法或重试",
		e.Waited.Truncate(time.Millisecond), len(e.Peers), strings.Join(e.Peers, " → "))
}

func shortSession(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	if s == "" {
		return "?"
	}
	return s
}

type waitInfo struct {
	tx      int64
	session string
	age     int64
	want    map[string]Mode
	since   time.Time
}

// Manager 是表级锁管理器。
type Manager struct {
	mu     sync.Mutex
	tables map[string]map[int64]*Holder
	waits  map[int64]*waitInfo
	seq    int64

	conflicts int64
	lastBlock string
}

// New 创建锁管理器。
func New() *Manager {
	return &Manager{
		tables: map[string]map[int64]*Holder{},
		waits:  map[int64]*waitInfo{},
	}
}

// NewTx 分配一个事务标识（同时作为优先级年龄，越大越年轻）。
func (m *Manager) NewTx(session string) (txID int64, age int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return m.seq, m.seq
}

// AcquireAll 申请一批表锁，全部可授予才一起授予；否则等待。
func (m *Manager) AcquireAll(txID int64, session string, age int64, want map[string]Mode, opts Options) error {
	if len(want) == 0 {
		return nil
	}
	if opts.Timeout <= 0 {
		opts = DefaultOptions()
	}
	if opts.Retry <= 0 {
		opts.Retry = 20 * time.Millisecond
	}
	if opts.DeadlockAfter <= 0 {
		opts.DeadlockAfter = 500 * time.Millisecond
	}

	start := time.Now()
	deadline := start.Add(opts.Timeout)

	for {
		blocked, ok := m.tryAcquire(txID, session, age, want)
		if ok {
			m.clearWait(txID)
			return nil
		}
		m.setWait(txID, session, age, want, start)

		if opts.DeadlockDetect && time.Since(start) >= opts.DeadlockAfter {
			if cycle, peers := m.findCycle(txID); cycle != nil && youngestIs(cycle, txID) {
				m.clearWait(txID)
				return &DeadlockError{Cycle: cycle, Peers: peers, Waited: time.Since(start)}
			}
		}
		if !time.Now().Before(deadline) {
			m.clearWait(txID)
			return &TimeoutError{Waited: time.Since(start), Blocked: blocked}
		}
		time.Sleep(jitter(opts.Retry))
	}
}

// tryAcquire 检查整批锁是否可授予，可以则一次性授予。
// 返回被挡住的明细；ok 为 true 表示已授予。
func (m *Manager) tryAcquire(txID int64, session string, age int64, want map[string]Mode) ([]Blocked, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := sortedKeys(want)
	var blocked []Blocked

	for _, name := range names {
		mode := want[name]
		holders := m.tables[name]
		if holders == nil {
			continue
		}
		var hits []Holder
		for id, h := range holders {
			if id == txID {
				continue
			}
			if mode == Shared && h.Mode == Shared {
				continue
			}
			hits = append(hits, *h)
		}
		if len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool { return hits[i].Age < hits[j].Age })
			blocked = append(blocked, Blocked{Table: name, Holders: hits})
		}
	}

	if len(blocked) > 0 {
		m.lastBlock = describe(blocked)
		return blocked, false
	}

	now := time.Now()
	for _, name := range names {
		mode := want[name]
		holders := m.tables[name]
		if holders == nil {
			holders = map[int64]*Holder{}
			m.tables[name] = holders
		}
		if h, ok := holders[txID]; ok {
			// 升级：只记录更高的模式
			if mode > h.Mode {
				h.Mode = mode
			}
			continue
		}
		holders[txID] = &Holder{Tx: txID, Session: session, Age: age, Mode: mode, Since: now}
	}
	return nil, true
}

func describe(blocked []Blocked) string {
	parts := make([]string, 0, len(blocked))
	for _, b := range blocked {
		names := make([]string, 0, len(b.Holders))
		for _, h := range b.Holders {
			names = append(names, shortSession(h.Session))
		}
		parts = append(parts, b.Table+":"+strings.Join(names, ","))
	}
	return strings.Join(parts, " ")
}

// Release 释放某个事务持有的全部锁（提交、回滚、超时放弃都走这里）。
func (m *Manager) Release(txID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, holders := range m.tables {
		delete(holders, txID)
		if len(holders) == 0 {
			delete(m.tables, name)
		}
	}
	delete(m.waits, txID)
}

// Held 返回某事务当前持有的表锁集合。
func (m *Manager) Held(txID int64) map[string]Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Mode{}
	for name, holders := range m.tables {
		if h, ok := holders[txID]; ok {
			out[name] = h.Mode
		}
	}
	return out
}

// Snapshot 返回当前锁状态快照。
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := Snapshot{Tables: []Table{}, Waiters: []Waiter{}, Conflicts: m.conflicts, LastBlock: m.lastBlock}
	names := make([]string, 0, len(m.tables))
	for name := range m.tables {
		names = append(names, name)
	}
	sort.Strings(names)

	waitersByTable := map[string][]Holder{}
	for _, w := range m.waits {
		for name := range w.want {
			waitersByTable[name] = append(waitersByTable[name], Holder{
				Tx: w.tx, Session: w.session, Age: w.age, Mode: w.want[name], Since: w.since,
			})
		}
	}

	for _, name := range names {
		t := Table{Name: name}
		for _, h := range m.tables[name] {
			t.Holders = append(t.Holders, *h)
		}
		sort.Slice(t.Holders, func(i, j int) bool { return t.Holders[i].Age < t.Holders[j].Age })
		t.Waiters = waitersByTable[name]
		sort.Slice(t.Waiters, func(i, j int) bool { return t.Waiters[i].Age < t.Waiters[j].Age })
		snap.Tables = append(snap.Tables, t)
	}

	for _, w := range m.waits {
		tables := make([]string, 0, len(w.want))
		for name := range w.want {
			tables = append(tables, name)
		}
		sort.Strings(tables)
		snap.Waiters = append(snap.Waiters, Waiter{Tx: w.tx, Session: w.session, Tables: tables, Since: w.since})
	}
	sort.Slice(snap.Waiters, func(i, j int) bool { return snap.Waiters[i].Since.Before(snap.Waiters[j].Since) })
	return snap
}

func (m *Manager) setWait(txID int64, session string, age int64, want map[string]Mode, since time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.waits[txID]; ok {
		w.want = want
		w.since = since
		return
	}
	m.conflicts++ // 只统计“开始等待”的次数，不统计每次重试
	m.waits[txID] = &waitInfo{tx: txID, session: session, age: age, want: want, since: since}
}

func (m *Manager) clearWait(txID int64) {
	m.mu.Lock()
	delete(m.waits, txID)
	m.mu.Unlock()
}

// findCycle 从 startTx 出发在等待图里找环。
// 节点只包含「正在等待」的事务；边为 等待者 → 挡住它的持有者（且该持有者也在等待）。
func (m *Manager) findCycle(startTx int64) ([]int64, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	edges := map[int64][]int64{}
	for tx, w := range m.waits {
		for name := range w.want {
			for id := range m.tables[name] {
				if id == tx {
					continue
				}
				if _, waiting := m.waits[id]; !waiting {
					continue
				}
				edges[tx] = append(edges[tx], id)
			}
		}
	}

	var path []int64
	onPath := map[int64]bool{}
	var dfs func(cur int64) bool
	dfs = func(cur int64) bool {
		path = append(path, cur)
		onPath[cur] = true
		for _, next := range edges[cur] {
			if next == startTx {
				return true
			}
			if onPath[next] {
				continue
			}
			if dfs(next) {
				return true
			}
		}
		path = path[:len(path)-1]
		onPath[cur] = false
		return false
	}
	if !dfs(startTx) {
		return nil, nil
	}

	peers := make([]string, 0, len(path))
	for _, tx := range path {
		if w, ok := m.waits[tx]; ok {
			peers = append(peers, shortSession(w.session))
		}
	}
	return path, peers
}

// youngestIs 判断 txID 是否是环里最年轻（年龄最大）的那个。
func youngestIs(cycle []int64, txID int64) bool {
	max := cycle[0]
	for _, id := range cycle[1:] {
		if id > max {
			max = id
		}
	}
	return max == txID
}

func sortedKeys(mp map[string]Mode) []string {
	out := make([]string, 0, len(mp))
	for k := range mp {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jitter(d time.Duration) time.Duration {
	f := 0.5 + rand.Float64()
	return time.Duration(float64(d) * f)
}
