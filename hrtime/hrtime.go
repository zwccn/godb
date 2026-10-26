// Package hrtime 提供统一的计时入口。
//
// 只用标准库 time：不使用 cgo，也不直接加载任何系统 DLL（syscall / kernel32 一律不碰）。
// 集中一层是因为计时点散落在 db / txn / web 三处，将来换时间源只需改这里。
//
// 关于精度：Go 的 time.Now() 精度取决于操作系统时间源。正常机器上是亚微秒级，
// 但在虚拟机/沙箱里可能只有零点几毫秒的步进 —— 此时几十微秒的操作会读成 0。
// Granularity 会在启动时实测本机时钟的有效步进，前端据此把测不准的耗时显示成
// "<0.5 ms" 而不是骗人的 0。
package hrtime

import (
	"sync"
	"time"
)

// Stamp 是一次时间采样。用结构体而不是裸 int64，是为了保留 time.Time 里的
// 单调时钟读数（time.Since 会优先用它，不受系统时间被调整影响）。
type Stamp struct {
	t time.Time
}

// Now 采样当前时间。
func Now() Stamp { return Stamp{t: time.Now()} }

// Since 返回距 s 经过的纳秒数（单调时钟，保证不为负）。
func Since(s Stamp) int64 {
	d := time.Since(s.t).Nanoseconds()
	if d < 0 {
		return 0
	}
	return d
}

var (
	granOnce sync.Once
	gran     time.Duration
)

// Granularity 返回本机时钟的有效步进（只算一次，结果缓存）。
//
// 做法：在 30ms 内不停读时钟，统计读数发生变化的次数；平均值就是有效步进。
// 比“取最小非零增量”稳，因为后者会受到读操作本身开销的干扰。
func Granularity() time.Duration {
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
