package syscat

import (
	"sync"
	"time"
)

// Activity 是一条 PostgreSQL 后端连接在上面的可观测状态。
//
// 放在 syscat 而不是 pgwire：pgwire → exec → syscat 是单向依赖，
// 反过来会让 syscat 依赖协议层。协议层只负责登记/更新，这里只负责存。
type Activity struct {
	PID       int32
	User      string
	DB        string
	AppName   string
	Addr      string
	Port      int32
	State     string // active / idle / idle in transaction
	Query     string
	Started   time.Time // 连接建立时间
	QueryTime time.Time // 当前语句开始时间
}

var (
	actMu  sync.RWMutex
	acts         = map[int32]*Activity{}
	nextPi int32 = 1000
)

// RegisterActivity 登记一条连接，返回注销函数（连接结束时调用）。
func RegisterActivity(a Activity) (pid int32, unregister func()) {
	actMu.Lock()
	nextPi++
	pid = nextPi
	a.PID = pid
	if a.Started.IsZero() {
		a.Started = time.Now()
	}
	acts[pid] = &a
	actMu.Unlock()
	return pid, func() {
		actMu.Lock()
		delete(acts, pid)
		actMu.Unlock()
	}
}

// SetActivityState 更新连接状态与正在执行的语句。
func SetActivityState(pid int32, state, query string) {
	actMu.Lock()
	defer actMu.Unlock()
	if a, ok := acts[pid]; ok {
		a.State = state
		if query != "" {
			a.Query = query
			a.QueryTime = time.Now()
		}
	}
}

// ActivitySnapshot 返回当前所有连接的快照（按 pid 排序）。
func ActivitySnapshot() []Activity {
	actMu.RLock()
	out := make([]Activity, 0, len(acts))
	for _, a := range acts {
		out = append(out, *a)
	}
	actMu.RUnlock()
	// 按 pid 排一下，客户端看起来稳定
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].PID < out[j-1].PID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ActivityCount 返回当前连接数（网页控制台 / pg_stat_database 用）。
func ActivityCount() int {
	actMu.RLock()
	defer actMu.RUnlock()
	return len(acts)
}
