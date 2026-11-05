package db

import (
	"time"

	"godb/netio"
	"godb/syscat"
)

// 本文件是「吞吐走势」：QPS / TPS / 连接数。
//
// 和 procstat.go 的分工：那边回答「这台机器还够不够」（CPU、内存、GC），
// 这边回答「库有多忙」（每秒跑了多少语句、多少事务、几个连接）。
// 两者共用同一个一秒节拍 —— 多开一个 ticker 只会让两条曲线的横轴错位，
// 对着看反而更难。
//
// 为什么要有走势而不只是当前值：面板原来的数字全是瞬时快照，
// 「刚才那阵子到底发生过什么」完全看不出来。QPS 掉到 0 的那十秒，
// 是没人来，还是全卡在锁上，只有曲线和等锁数放在一起才答得出。

// rateSeriesCap 与 CPU 曲线同长度：300 点 = 5 分钟。
// 同一个节拍、同一个窗口，两条曲线才能并排看。
const rateSeriesCap = procSeriesCap

// RatePoint 是每秒的吞吐采样点。
type RatePoint struct {
	AtMs int64 `json:"atMs"`
	QPS  int   `json:"qps"`
	TPS  int   `json:"tps"`
	// Conns 是采样这一刻的会话数（瞬时值，不是每秒增量）。
	Conns int `json:"conns"`

	// NetIn / NetOut 是这一秒 PG 端口的收发字节数（每秒增量）。
	// 分收发两条而不是加总：查大结果集时 发 远大于 收，收写入时反过来，
	// 合成一个数就看不出「是在拖数据还是在灌数据」了。
	NetIn  int64 `json:"netIn"`
	NetOut int64 `json:"netOut"`
}

// RateStats 是「吞吐与连接」卡片的完整数据。
type RateStats struct {
	// 当前值取最近 5 秒平均：1 秒粒度的 QPS 会在 0 和峰值之间来回跳，
	// 盯着看的人根本判断不出「现在到底多忙」。曲线仍然按秒画，原始抖动不丢。
	QPS   int `json:"qps"`
	TPS   int `json:"tps"`
	Conns int `json:"conns"` // 当前连接数（瞬时）

	// NetIn / NetOut 是当前带宽（字节/秒，同样取最近 5 秒平均）。
	// 单秒值抖动太大 —— 一个 8KB 的查询响应进来就跳一下，盯着看没意义。
	NetIn  int64 `json:"netIn"`
	NetOut int64 `json:"netOut"`

	// 峰值取整个窗口（5 分钟），用来回答「忙到什么程度过」。
	PeakQPS    int   `json:"peakQps"`
	PeakTPS    int   `json:"peakTps"`
	PeakConns  int   `json:"peakConns"`
	PeakNetIn  int64 `json:"peakNetIn"`
	PeakNetOut int64 `json:"peakNetOut"`

	// Sampled 为 false 表示还没采到第二个点（刚启动），速率暂时无意义。
	Sampled bool `json:"sampled"`
	// Series 是最近五分钟的秒级曲线（按时间正序）。
	Series []RatePoint `json:"series,omitempty"`
}

// sessionCount 返回当前会话数。加锁读：d.sessions 由执行线程增删。
func (d *DB) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

// tickRates 采一个吞吐点，由 procSampler 的秒级 tick 调用。
//
// 三个数都是「累计值求差」：语句数用 d.stmtTotal，事务数用 syscat 的
// xact_commit + xact_rollback。不另开一份计数 —— 同一个事实存两处，
// 迟早会有一处忘记加。
func (p *procSampler) tickRates(d *DB, now time.Time) {
	qps, tps, conns := 0, 0, 0
	var netIn, netOut int64
	if d != nil {
		stmts := d.stmtTotal.Load()
		ds := syscat.GetDBStats()
		xact := ds.XactCommit + ds.XactRollback
		conns = d.sessionCount()
		// 只读一次：读两次的话基线会比取样点晚一点，晚到的字节就被算进下一秒，
		// 数值没错但两个数的时刻对不上，排查时容易看糊涂。
		cin, cout := netio.PG.Totals()
		// p.thruPos>0 || p.thruFilled 表示「已经写过至少一个点」，也就是有这个
		// 差值基线了。首轮绝不能算：会把「进程启动至今」的所有语句当成这一秒。
		if p.thruPos > 0 || p.thruFilled {
			if dv := stmts - p.lastStmts; dv > 0 {
				qps = int(dv)
			}
			if dv := xact - p.lastXact; dv > 0 {
				tps = int(dv)
			}
			// 网络字节走同一个基线约定。计数器是进程级、只增不减，
			// 所以热切换端口也不用重置 —— 换了监听接着累加，差值仍然对。
			if dv := cin - p.lastNetIn; dv > 0 {
				netIn = dv
			}
			if dv := cout - p.lastNetOut; dv > 0 {
				netOut = dv
			}
		}
		p.lastStmts, p.lastXact = stmts, xact
		p.lastNetIn, p.lastNetOut = cin, cout
		p.curConns = conns
	}

	p.thruSeries[p.thruPos] = RatePoint{
		AtMs: now.UnixMilli(), QPS: qps, TPS: tps, Conns: conns,
		NetIn: netIn, NetOut: netOut,
	}
	p.thruPos = (p.thruPos + 1) % len(p.thruSeries)
	if p.thruPos == 0 {
		p.thruFilled = true
	}
}

// rateStatsLocked 把环形缓冲展开成曲线并算当前值/峰值。调用方须持 p.mu。
func (p *procSampler) rateStatsLocked() RateStats {
	out := RateStats{Conns: p.curConns}
	n := p.thruPos
	if p.thruFilled {
		n = len(p.thruSeries)
	}
	if n == 0 {
		return out
	}
	out.Sampled = n > 1
	out.Series = make([]RatePoint, 0, n)
	start := 0
	if p.thruFilled {
		start = p.thruPos
	}
	var sumQ, sumT int
	var sumIn, sumOut int64
	tail := n
	if tail > rateWindow {
		tail = rateWindow
	}
	for i := 0; i < n; i++ {
		pt := p.thruSeries[(start+i)%len(p.thruSeries)]
		out.Series = append(out.Series, pt)
		if pt.QPS > out.PeakQPS {
			out.PeakQPS = pt.QPS
		}
		if pt.TPS > out.PeakTPS {
			out.PeakTPS = pt.TPS
		}
		if pt.Conns > out.PeakConns {
			out.PeakConns = pt.Conns
		}
		if pt.NetIn > out.PeakNetIn {
			out.PeakNetIn = pt.NetIn
		}
		if pt.NetOut > out.PeakNetOut {
			out.PeakNetOut = pt.NetOut
		}
		if i >= n-tail {
			sumQ += pt.QPS
			sumT += pt.TPS
			sumIn += pt.NetIn
			sumOut += pt.NetOut
		}
	}
	out.QPS = sumQ / tail
	out.TPS = sumT / tail
	out.NetIn = sumIn / int64(tail)
	out.NetOut = sumOut / int64(tail)
	return out
}
