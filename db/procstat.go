package db

import (
	"math"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// 本文件是「第一档」资源观测：只依赖标准库，零 cgo，三平台一致。
//
// 两类数据源，职责不同：
//   - 进程自身的 CPU / 内存 / goroutine / GC：runtime/metrics + syscall 取 CPU 累计秒
//   - 存储层速率（fsync 次数、WAL 写入字节）：由 storage 的累计值两次采样求差
//
// CPU 为什么不用 /cpu/classes/total:cpu-seconds：那个指标只在 GC 边界刷新，
// 不是实时值（实测 400ms 忙等期间恒为 0，触发 GC 后才跳到 12.9s，且包含
// 32 核并行 GC worker 的时间，跟业务代码占用对不上）。实时值只能从 OS 取：
// Getrusage（linux/darwin）与 GetProcessTimes（windows）都是标准库导出。

const (
	// procInterval 是采样间隔。CPU 与磁盘速率都是「差值÷间隔」得到的，
	// 间隔太短数值跳动大，一秒是经验值（也是 iostat 的默认粒度）。
	procInterval = time.Second
	// procSeriesCap 是 sparkline 保留的点数：300 点 = 5 分钟。
	// 默认 checkpoint 周期 60 秒，五分钟刚好能看出一个完整周期。
	procSeriesCap = 300
	// rateWindow 是速率平均的窗口长度（秒）。
	rateWindow = 5
	// gcPauseCap 是 GC 暂停样本数。只保留最近若干次，避免长跑之后 p95 被
	// 建库早期的抖动永久稀释。
	gcPauseCap = 256
)

// 用到的标准库指标名。都在 Go 1.20 里存在。
const (
	mHeapObjects = "/memory/classes/heap/objects:bytes"
	mTotal       = "/memory/classes/total:bytes"
	mReleased    = "/memory/classes/heap/released:bytes"
	mGoal        = "/gc/heap/goal:bytes"
	mGoroutines  = "/sched/goroutines:goroutines"
	mGCCycles    = "/gc/cycles/total:gc-cycles"
)

// GC 暂停直方图：Go 1.21 起改名为 /sched/pauses/total/gc:seconds，
// 1.20 及更早叫 /gc/pauses:seconds。两个都探测，优先用新名。
var pauseMetricCandidates = []string{
	"/sched/pauses/total/gc:seconds",
	"/gc/pauses:seconds",
}

// ProcPoint 是一个秒级采样点，只留 sparkline 需要的三个量。
type ProcPoint struct {
	AtMs      int64   `json:"atMs"`
	CPUCore   float64 `json:"cpuCore"`
	HeapBytes int64   `json:"heapBytes"`
	GCPauseNs int64   `json:"gcPauseP95Ns"`
}

// ProcStats 是进程资源卡片的完整数据。
type ProcStats struct {
	NumCPU int `json:"numCpu"`

	// CPU 占用。Core 是「相当于几个核」，Percent 是占全部核的比例。
	// 分开报用户态/内核态：用户态高说明在算 SQL，内核态高说明在等 IO/系统调用。
	CPUCore       float64 `json:"cpuCore"`
	CPUUserCore   float64 `json:"cpuUserCore"`
	CPUKernelCore float64 `json:"cpuKernelCore"`
	CPUPercent    float64 `json:"cpuPercent"`

	// 内存三个口径 + 下次 GC 目标。只看一个数字容易误判：
	// 「活对象 118 MB、进程占用 156 MB」说明有 38 MB 是预留或已归还前的残留。
	HeapObjects int64 `json:"heapObjects"`
	TotalBytes  int64 `json:"totalBytes"`
	Released    int64 `json:"released"`
	GCGoal      int64 `json:"gcGoal"`

	Goroutines int64 `json:"goroutines"`
	GCCycles   int64 `json:"gcCycles"`

	GCPauseP50Ns int64 `json:"gcPauseP50Ns"`
	GCPauseP95Ns int64 `json:"gcPauseP95Ns"`
	GCPauseMaxNs int64 `json:"gcPauseMaxNs"`

	// Sampled 为 false 表示还没采到第二个点，CPU 那几项暂时无意义。
	Sampled bool `json:"sampled"`
	// Series 是最近五分钟的秒级曲线（按时间正序）。
	Series []ProcPoint `json:"series,omitempty"`
}

// IORates 是存储层每秒速率。
type IORates struct {
	FsyncPerSec    int64 `json:"fsyncPerSec"`
	CommitsPerSec  int64 `json:"commitsPerSec"`
	WALBytesPerSec int64 `json:"walBytesPerSec"`
}

// procSampler 每秒采一次进程与存储的资源数据。
//
// 独立于面板轮询：面板关着也要有数据，否则用户打开面板看到的 p95 永远是 0，
// 也没法判断「刚才那阵子发生了什么」。
type procSampler struct {
	mu sync.Mutex

	// metrics 采样槽。名字在初始化时筛过，只保留本机运行时真的提供的。
	samples []metrics.Sample
	index   map[string]int

	// GC 暂停：上次读到的直方图累计计数，用来求这一秒内的新增样本。
	// pauseMetric 是实际可用的指标名（空串 = 本机运行时没有该指标）。
	pauseMetric     string
	prevPauseCounts []uint64
	pauseRing       *ring

	// CPU 累计秒的基线。
	lastUser   time.Duration
	lastKernel time.Duration
	lastAt     time.Time
	hasLast    bool

	// 存储累计值的基线（算每秒速率用）。
	lastSyncRuns int64
	lastCommits  int64
	lastWALBytes int64
	hasRateBase  bool
	// 速率做 5 秒滑动平均：单秒计数在面板上会 0 和峰值之间乱跳，
	// 看的人没法判断「现在到底多忙」。
	rateWin [rateWindow]IORates
	ratePos int
	rateN   int

	cur ProcStats
	// series 是定长环形缓冲，pos 指向下一个写入位置。
	series []ProcPoint
	pos    int
	filled bool

	// ---- 吞吐（见 rates.go）----
	// 语句/事务累计数的基线，用差值得到每秒速率。
	lastStmts int64
	lastXact  int64
	curConns  int
	// thru 是 throughput 的缩写，避开上面 IO 速率用的 ratePos。
	thruSeries []RatePoint
	thruPos    int
	thruFilled bool
}

func newProcSampler() *procSampler {
	p := &procSampler{
		pauseRing:  newRing(gcPauseCap),
		series:     make([]ProcPoint, procSeriesCap),
		thruSeries: make([]RatePoint, rateSeriesCap),
		index:      map[string]int{},
	}
	p.initMetrics()
	p.cur.NumCPU = runtime.NumCPU()
	p.readLive()
	// 先取一次 CPU 基线：这样第一个 tick 就能算出真实速率，
	// 而不是把「进程启动至今」的总时长当成一秒。
	p.lastUser, p.lastKernel = procCPU()
	p.lastAt = time.Now()
	p.hasLast = true
	return p
}

// initMetrics 筛出本机运行时支持的指标名。
//
// 直接按不存在的名字 Read 会 panic，所以先查 metrics.All()。
// 这一步只在启动时做一次。
func (p *procSampler) initMetrics() {
	want := []string{mHeapObjects, mTotal, mReleased, mGoal, mGoroutines, mGCCycles}
	have := map[string]bool{}
	for _, d := range metrics.All() {
		have[d.Name] = true
	}
	for _, n := range want {
		if !have[n] {
			continue
		}
		p.index[n] = len(p.samples)
		p.samples = append(p.samples, metrics.Sample{Name: n})
	}
	for _, n := range pauseMetricCandidates {
		if have[n] {
			p.index[n] = len(p.samples)
			p.samples = append(p.samples, metrics.Sample{Name: n})
			p.pauseMetric = n
			break
		}
	}
}

func (p *procSampler) getUint(name string) int64 {
	i, ok := p.index[name]
	if !ok {
		return 0
	}
	return int64(p.samples[i].Value.Uint64())
}

// readLive 读一次内存/goroutine/GC（不含 CPU，CPU 要靠两次差值）。
func (p *procSampler) readLive() {
	if len(p.samples) == 0 {
		return
	}
	metrics.Read(p.samples)
	p.cur.HeapObjects = p.getUint(mHeapObjects)
	p.cur.TotalBytes = p.getUint(mTotal)
	p.cur.Released = p.getUint(mReleased)
	p.cur.GCGoal = p.getUint(mGoal)
	p.cur.Goroutines = p.getUint(mGoroutines)
	p.cur.GCCycles = p.getUint(mGCCycles)
}

// tick 是一秒一次的主循环体。
func (p *procSampler) tick(d *DB) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.readLive()
	p.readPauses()

	// ---- CPU ----
	u, k := procCPU()
	now := time.Now()
	if p.hasLast {
		dt := now.Sub(p.lastAt).Seconds()
		if dt > 0 {
			du := float64(u-p.lastUser) / float64(time.Second)
			dk := float64(k-p.lastKernel) / float64(time.Second)
			if du < 0 {
				du = 0
			}
			if dk < 0 {
				dk = 0
			}
			p.cur.CPUUserCore = du / dt
			p.cur.CPUKernelCore = dk / dt
			p.cur.CPUCore = (du + dk) / dt
			if p.cur.NumCPU > 0 {
				p.cur.CPUPercent = p.cur.CPUCore / float64(p.cur.NumCPU) * 100
			}
			p.cur.Sampled = true
		}
	}
	p.lastUser, p.lastKernel, p.lastAt, p.hasLast = u, k, now, true

	p50, p95, mx := p.pauseRing.Stats()
	p.cur.GCPauseP50Ns, p.cur.GCPauseP95Ns, p.cur.GCPauseMaxNs = p50, p95, mx

	// ---- 存储速率 ----
	//
	// 第一次 tick 时 lastSyncRuns 还是 0，差值会把「进程启动至今」当成一秒，
	// 所以用一个 hasRateBase 标记跳过首轮。
	if d != nil && d.Store != nil {
		fp := d.Store.FsyncProfile()
		commits := d.Store.TotalCommits()
		if p.hasRateBase {
			one := IORates{
				FsyncPerSec:    fp.Runs - p.lastSyncRuns,
				CommitsPerSec:  commits - p.lastCommits,
				WALBytesPerSec: fp.Bytes - p.lastWALBytes,
			}
			if one.FsyncPerSec < 0 {
				one.FsyncPerSec = 0
			}
			if one.CommitsPerSec < 0 {
				one.CommitsPerSec = 0
			}
			if one.WALBytesPerSec < 0 {
				one.WALBytesPerSec = 0
			}
			p.rateWin[p.ratePos] = one
			p.ratePos = (p.ratePos + 1) % rateWindow
			if p.rateN < rateWindow {
				p.rateN++
			}
		}
		p.hasRateBase = true
		p.lastSyncRuns = fp.Runs
		p.lastCommits = commits
		p.lastWALBytes = fp.Bytes
	}

	// ---- 秒级曲线 ----
	p.series[p.pos] = ProcPoint{
		AtMs:      now.UnixMilli(),
		CPUCore:   p.cur.CPUCore,
		HeapBytes: p.cur.HeapObjects,
		GCPauseNs: p.cur.GCPauseP95Ns,
	}
	p.pos = (p.pos + 1) % len(p.series)
	if p.pos == 0 {
		p.filled = true
	}

	// ---- 吞吐（QPS / TPS / 连接数，见 rates.go）----
	// 和上面同一个节拍：两条曲线横轴一致，对着看才有意义。
	p.tickRates(d, now)
}

// readPauses 把 GC 暂停直方图的增量转成样本。
//
// 直方图给的是累计桶计数，两次采样相减才能得到「这一秒新发生的暂停」。
// 桶的代表值取上界（保守估计），单桶一次最多灌 64 个样本 —— 否则一次
// GC 风暴会把环形缓冲冲空，p95 就变成噪声了。
func (p *procSampler) readPauses() {
	name := p.pauseMetric
	if name == "" {
		return
	}
	i, ok := p.index[name]
	if !ok {
		return
	}
	h := p.samples[i].Value.Float64Histogram()
	if h == nil || len(h.Counts) == 0 {
		return
	}
	if len(p.prevPauseCounts) != len(h.Counts) {
		// 第一次（或桶结构变了）：只记基线。不记的话会把「进程启动至今的
		// 所有暂停」当成这一秒发生的，p95 直接失真。
		p.prevPauseCounts = append([]uint64(nil), h.Counts...)
		return
	}
	for idx := range h.Counts {
		d := h.Counts[idx] - p.prevPauseCounts[idx]
		if d == 0 {
			continue
		}
		// 桶上界是这一桶里暂停耗时的上限；最后一桶是 +Inf，用前一桶的 2 倍兜底。
		var ns int64
		if idx+1 < len(h.Buckets) && !math.IsInf(h.Buckets[idx+1], 1) {
			ns = int64(h.Buckets[idx+1] * 1e9)
		} else if idx < len(h.Buckets) && !math.IsInf(h.Buckets[idx], 1) {
			ns = int64(h.Buckets[idx] * 2 * 1e9)
		}
		if ns <= 0 {
			continue
		}
		if d > 64 {
			d = 64
		}
		for k := uint64(0); k < d; k++ {
			p.pauseRing.Add(ns)
		}
	}
	copy(p.prevPauseCounts, h.Counts)
}

// stats 返回一份快照（含按时间正序展开的曲线）。
func (p *procSampler) stats() (ProcStats, IORates, RateStats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.cur
	out.Series = nil
	n := p.pos
	if p.filled {
		n = len(p.series)
	}
	if n > 0 {
		out.Series = make([]ProcPoint, 0, n)
		start := 0
		if p.filled {
			start = p.pos
		}
		for i := 0; i < n; i++ {
			out.Series = append(out.Series, p.series[(start+i)%len(p.series)])
		}
	}
	return out, p.avgRates(), p.rateStatsLocked()
}

// avgRates 返回最近若干秒的速率平均值。
func (p *procSampler) avgRates() IORates {
	if p.rateN == 0 {
		return IORates{}
	}
	var sum IORates
	for i := 0; i < p.rateN; i++ {
		sum.FsyncPerSec += p.rateWin[i].FsyncPerSec
		sum.CommitsPerSec += p.rateWin[i].CommitsPerSec
		sum.WALBytesPerSec += p.rateWin[i].WALBytesPerSec
	}
	n := int64(p.rateN)
	return IORates{
		FsyncPerSec:    sum.FsyncPerSec / n,
		CommitsPerSec:  sum.CommitsPerSec / n,
		WALBytesPerSec: sum.WALBytesPerSec / n,
	}
}

// run 是后台采样循环。stop 由 DB.Close 关闭。
func (p *procSampler) run(d *DB, stop <-chan struct{}) {
	t := time.NewTicker(procInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.tick(d)
		}
	}
}
