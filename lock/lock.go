// Package lock 实现表级共享/排它锁（S/X），支持批量原子申请、事件通知等待、
// 超时放弃与等待图死锁检测。
//
// 设计要点：
//   - 锁粒度是「表」，模式只有 S（共享/读）和 X（排它/写）；
//   - 一个事务对一张表最多持一把锁，只能把 S 升级成 X，不能降级；
//   - 一次可以申请多张表，全部可授予才一次性授予（要么全给，要么一个不给）；
//   - 拿不到就挂起（sync.Cond 事件通知），锁释放时被唤醒——不再轮询，避免惊群；
//   - 超时兜底：time.AfterFunc 在 Timeout 时广播唤醒，确保没人永久挂起；
//   - 等待超过 DeadlockAfter 后检查等待图，成环时环里最年轻的事务自己放弃。
package lock

import (
	"fmt"
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
	Timeout        time.Duration // 单个申请的最长等待；<= 0 表示不限（只靠取消与死锁检测兜底）
	Retry          time.Duration // 轮询间隔（会加 ±50% 抖动）
	DeadlockDetect bool          // 是否做等待图检测
	DeadlockAfter  time.Duration // 等待多久后开始检测

	// Canceled 在等待期间被反复调用；返回 true 表示调用方要求放弃（监控面板 Kill）。
	// 配上 Manager.WakeAll 使用：外部先置取消标志，再唤醒，等待者醒来即退出。
	Canceled func() bool
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
	Cycle  []int64
	Peers  []string
	Waited time.Duration // 本次已等待时长
}

func (e *DeadlockError) Error() string {
	return fmt.Sprintf("等待锁冲突（已等 %s）：%d 个事务互相等待（%s），后进让先进，本事务被回滚，请改用原子写法或重试",
		e.Waited.Truncate(time.Millisecond), len(e.Peers), strings.Join(e.Peers, " → "))
}

// CanceledError 表示申请被外部取消（面板 Kill）。这不是超时也不是死锁，
// 调用方应当把整个事务回滚并告知用户「已取消」。
type CanceledError struct {
	Waited time.Duration
}

func (e *CanceledError) Error() string {
	return fmt.Sprintf("canceling statement due to user request (waited %s)",
		e.Waited.Truncate(time.Millisecond))
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
	cond   *sync.Cond // 锁释放时 Broadcast，唤醒等待者
	tables map[string]map[int64]*Holder
	waits  map[int64]*waitInfo
	seq    int64

	conflicts int64
	lastBlock string
}

// New 创建锁管理器。
func New() *Manager {
	m := &Manager{
		tables: map[string]map[int64]*Holder{},
		waits:  map[int64]*waitInfo{},
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// NewTx 分配一个事务标识（同时作为优先级年龄，越大越年轻）。
func (m *Manager) NewTx(session string) (txID int64, age int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return m.seq, m.seq
}

// AcquireAll 申请一批表锁，全部可授予才一起授予；否则挂起等待。
//
// 等待机制：sync.Cond 事件通知（锁释放时 Broadcast），不再轮询。
// 超时兜底：time.AfterFunc 在 Timeout 时广播唤醒，确保没人永久挂起。
// 死锁检测：每次被唤醒时若已等超过 DeadlockAfter，扫等待图找环。
func (m *Manager) AcquireAll(txID int64, session string, age int64, want map[string]Mode, opts Options) error {
	if len(want) == 0 {
		return nil
	}
	if opts.DeadlockAfter <= 0 {
		opts.DeadlockAfter = 500 * time.Millisecond
	}

	// Timeout <= 0 表示「不限」：不起超时定时器，只在被取消或检测到死锁时退出。
	// 这里不回落到 DefaultOptions —— 监控面板里的「不限」是用户的显式选择，
	// 悄悄塞一个 3 秒进去会让面板显示和实际行为对不上。
	unlimited := opts.Timeout <= 0

	start := time.Now()
	var deadline time.Time
	if !unlimited {
		deadline = start.Add(opts.Timeout)
	}

	// 超时兜底：到点广播唤醒所有等待者，让他们检查 deadline。
	// 没有这一步，cond.Wait() 可能永远醒不过来（对方也在等锁、没人 Release）。
	if !unlimited {
		timer := time.AfterFunc(opts.Timeout, func() {
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
		})
		defer timer.Stop()
	}

	// 死锁检测定时器：等待超过 DeadlockAfter 后广播唤醒，触发等待图检测。
	// 互相等待时没人 Release，只能靠这个定时器唤醒。
	if opts.DeadlockDetect && opts.DeadlockAfter > 0 {
		deadlockTimer := time.AfterFunc(opts.DeadlockAfter, func() {
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
		})
		defer deadlockTimer.Stop()
	}

	m.mu.Lock()
	for {
		// 尝试获取（mu 已持有，用 locked 版本）
		blocked, ok := m.tryAcquireLocked(txID, session, age, want)
		if ok {
			delete(m.waits, txID)
			m.mu.Unlock()
			return nil
		}

		// 取消检查（面板 Kill / 客户端断开）。放在登记等待之前，
		// 这样被取消的事务不会在快照里留下一个假等待者。
		if opts.Canceled != nil && opts.Canceled() {
			delete(m.waits, txID)
			m.mu.Unlock()
			return &CanceledError{Waited: time.Since(start)}
		}

		// 登记等待信息（供死锁检测与快照用）
		if w, exists := m.waits[txID]; exists {
			w.want = want
		} else {
			m.conflicts++
			m.waits[txID] = &waitInfo{tx: txID, session: session, age: age, want: want, since: start}
		}

		// 死锁检测：等待超过 DeadlockAfter 后扫等待图
		if opts.DeadlockDetect && time.Since(start) >= opts.DeadlockAfter {
			if cycle, peers := m.findCycleLocked(txID); cycle != nil && youngestIs(cycle, txID) {
				delete(m.waits, txID)
				m.mu.Unlock()
				return &DeadlockError{Cycle: cycle, Peers: peers, Waited: time.Since(start)}
			}
		}

		// 超时检查（放在 Wait 之前，避免错过 timer 广播）
		if !unlimited && !time.Now().Before(deadline) {
			delete(m.waits, txID)
			m.mu.Unlock()
			return &TimeoutError{Waited: time.Since(start), Blocked: blocked}
		}

		// 挂起等待：Release 时 Broadcast 唤醒，或 timer 到点唤醒。
		// Wait 会释放 mu，唤醒后重新持有。
		m.cond.Wait()
	}
}

// WakeAll 广播唤醒所有等待者，让他们各自重新检查取消/超时条件。
// 面板 Kill 一个正在等锁的事务时用：只置取消标志不够，cond.Wait 醒不过来。
func (m *Manager) WakeAll() {
	m.mu.Lock()
	m.cond.Broadcast()
	m.mu.Unlock()
}

// tryAcquireLocked 检查整批锁是否可授予，可以则一次性授予。
// 调用方必须持有 m.mu。返回被挡住的明细；ok 为 true 表示已授予。
func (m *Manager) tryAcquireLocked(txID int64, session string, age int64, want map[string]Mode) ([]Blocked, bool) {
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
	// 唤醒所有等待者：锁释放了，让他们重新尝试获取
	m.cond.Broadcast()
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

// findCycleLocked 从 startTx 出发在等待图里找环。
// 调用方必须持有 m.mu。
// 节点只包含「正在等待」的事务；边为 等待者 → 挡住它的持有者（且该持有者也在等待）。
func (m *Manager) findCycleLocked(startTx int64) ([]int64, []string) {
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
