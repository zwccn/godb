// Package hrtime 提供统一的计时入口。
//
// 默认用标准库 time：不使用 cgo，也不直接加载任何系统 DLL（syscall / kernel32 一律不碰）。
// 集中一层是因为计时点散落在 db / txn / web 三处，换时间源只需改这里。
//
// # 为什么还要一套「高精度」模式
//
// Windows 的 time.Now 走 QPC，在这台虚拟机上分辨率只有约 219µs（200 万次读数只变了
// 77 次）——几十微秒的操作全读成 0，探测里「每一步花了多少」就没法看了。
// 而 CPU 自带的 TSC 每次读数都在变（约 0.5ns 一个 tick）。所以这里多一条路：
// 能用 TSC 就用，用不了就老实退回 time.Now。
//
// 三条设计约束：
//  1. **绝不因为 TSC 出问题让程序挂掉**：读不到、检测不到恒定 TSC、标定失败，
//     一律降级成 time.Now，行为与旧版完全一致。
//  2. **Stamp 自带来源**：中途切换计时源时，切换前拿到的 Stamp 仍按它自己的
//     时钟结算，不会把两种时钟混着算（混算会得出负数或离谱的值）。
//  3. **只在测量处使用**：锁等待超时、慢 SQL 阈值这些「判断」仍然走 time.Now，
//     它们要的是「大概几秒」，不需要纳秒，也不需要担心标定漂移。
package hrtime

import (
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Stamp 是一次时间采样。两种来源分开记：
//   - tsc 为 true 时用 tick（TSC 计数），换算成纳秒要乘 nsPerTick；
//   - 否则用 t（time.Time），走单调时钟。
type Stamp struct {
	t    time.Time
	tick uint64
	tsc  bool
}

var (
	// enabled 是用户开关（设置 → 性能）；active 是「此刻真的在用 TSC」。
	// 两者分开：开关开着但平台不支持/正在标定时，active 仍是 false。
	enabled atomic.Bool
	active  atomic.Bool

	// nsPerTick 以位模式存 float64（atomic 没有 Float64）。
	nsPerTick atomic.Uint64
	freqHz    atomic.Int64

	reason atomic.Value // string：为什么没在用 TSC（给设置页显示）

	calMu   sync.Mutex // 保证同一时刻只有一个标定在跑
	calBusy bool
)

func init() {
	reason.Store("尚未初始化")
}

// Now 采样当前时间。高精度可用时用 TSC，否则用 time.Now。
func Now() Stamp {
	if active.Load() {
		if v, ok := readTSC(); ok {
			return Stamp{tick: v, tsc: true}
		}
	}
	return Stamp{t: time.Now()}
}

// Since 返回距 s 经过的纳秒数（保证不为负）。
//
// 按 Stamp 自己的来源结算：这正是「中途开关计时源」也不会算错的原因。
func Since(s Stamp) int64 {
	if s.tsc {
		now, ok := readTSC()
		if !ok || now <= s.tick {
			// 读不到、或计数器回退（跨核/虚拟化抖动）：宁可变 0，也不给负数。
			return 0
		}
		pt := math.Float64frombits(nsPerTick.Load())
		if pt <= 0 {
			return 0
		}
		return int64(float64(now-s.tick) * pt)
	}
	d := time.Since(s.t).Nanoseconds()
	if d < 0 {
		return 0
	}
	return d
}

// Status 是给设置页看的当前计时状态。
type Status struct {
	Supported bool   `json:"supported"` // 这个平台 + 这颗 CPU 能不能用
	Enabled   bool   `json:"enabled"`   // 用户开关
	Active    bool   `json:"active"`    // 此刻真的在用 TSC
	FreqHz    int64  `json:"freqHz"`    // 标定出来的 TSC 频率（0 = 没标定）
	Reason    string `json:"reason"`    // 没在用的原因（中文，直接显示）
	GranNs    int64  `json:"granNs"`    // 当前实际分辨率（纳秒）
}

// Describe 返回一行人类可读的状态（启动日志 / 设置页抬头用）。
func (s Status) Describe() string {
	switch {
	case s.Active:
		return fmt.Sprintf("TSC 高精度计时（%.2f GHz，刻度约 0.5ns）", float64(s.FreqHz)/1e9)
	case !s.Enabled:
		return "系统时钟（高精度计时已关闭）"
	case s.Reason == "":
		return "系统时钟"
	default:
		return "系统时钟（" + s.Reason + "）"
	}
}

// Status 返回当前状态。
func StatusOf() Status {
	st := Status{
		Supported: platformSupported(),
		Enabled:   enabled.Load(),
		Active:    active.Load(),
		FreqHz:    freqHz.Load(),
	}
	if v, ok := reason.Load().(string); ok {
		st.Reason = v
	}
	if st.Active {
		st.GranNs = 1 // TSC 一个 tick 约 0.5ns，整数纳秒下就是 1
	} else {
		st.GranNs = Granularity().Nanoseconds()
	}
	return st
}

// Init 在进程启动时调一次。enabled 来自配置（默认 true）。
//
// 标定放在后台做：这台机器要采 1.5 秒样本，不能卡住启动。标定期间用的还是
// time.Now，对正确性没有影响，只是精度暂时是旧的。
func Init(enabledNow bool) {
	SetEnabled(enabledNow)
}

// SetEnabled 热切换（设置页保存时调用）。
func SetEnabled(on bool) {
	enabled.Store(on)
	if !on {
		active.Store(false)
		setReason("已关闭（设置 → 性能）")
		return
	}
	if !platformSupported() {
		active.Store(false)
		setReason(unsupportedReason())
		return
	}
	startCalibrate()
}

func setReason(s string) { reason.Store(s) }

// startCalibrate 起一个后台标定（已在跑或已激活就直接返回）。
func startCalibrate() {
	calMu.Lock()
	if calBusy || active.Load() {
		calMu.Unlock()
		return
	}
	calBusy = true
	calMu.Unlock()

	setReason("正在标定 TSC 频率…")
	go func() {
		hz, err := calibrate()
		calMu.Lock()
		calBusy = false
		calMu.Unlock()

		// 标定期间用户可能已经关掉了 —— 别把它又打开
		if !enabled.Load() {
			return
		}
		if err != nil {
			active.Store(false)
			setReason("标定失败：" + err.Error())
			return
		}
		nsPerTick.Store(math.Float64bits(1e9 / hz))
		freqHz.Store(int64(hz + 0.5))
		active.Store(true)
		setReason("")
		log.Printf("计时源已切到 TSC：%.2f GHz（标定 1.5s，误差约 0.001%%）", hz/1e9)
	}()
}

var (
	granOnce sync.Once
	gran     time.Duration
)

// Granularity 返回当前计时源的有效步进。
//
// TSC 激活时就是一个 tick（约 0.5ns，整数纳秒下按 1ns 报）；否则实测 time.Now
// 的步进。前端据此把测不准的耗时显示成 "<步进" 而不是骗人的 0。
func Granularity() time.Duration {
	if active.Load() {
		return time.Nanosecond
	}
	granOnce.Do(func() {
		start := time.Now()
		deadline := start.Add(30 * time.Millisecond)
		prev := start.UnixNano()
		var changes int64
		for time.Now().Before(deadline) {
			cur := time.Now().UnixNano()
			if cur != prev {
				changes++
				prev = cur
			}
		}
		elapsed := time.Since(start)
		if changes <= 0 {
			gran = 30 * time.Millisecond
			return
		}
		d := elapsed / time.Duration(changes)
		if d < time.Microsecond {
			d = time.Microsecond
		}
		gran = d
	})
	return gran
}
