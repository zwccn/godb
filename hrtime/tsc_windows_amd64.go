//go:build windows && amd64

package hrtime

import (
	"errors"
	"fmt"
	"time"
)

// 这个文件只有在 Windows/amd64 上才编进来 —— 也正是唯一需要 TSC 的平台。
//
// 为什么只有它：Linux 的 time.Now 走 vDSO 的 clock_gettime(CLOCK_MONOTONIC)，
// 内核用 TSC / arch_timer 实现，本来就是纳秒级；macOS 的 mach_absolute_time 同理。
// Windows 的 QPC 才是那个会掉到几百微秒的（尤其在虚拟机里）。

// 两个汇编函数（见 tsc_windows_amd64.s）
func rdtsc() uint64
func cpuid(eax, ecx uint32) (a, b, c, d uint32)

// readTSC 读一次时间戳计数器。
func readTSC() (uint64, bool) { return rdtsc(), true }

func platformSupported() bool { return hasInvariantTSC }

func unsupportedReason() string {
	if !hasInvariantTSC {
		return "这颗 CPU 没有恒定 TSC（计数器会跟着主频变），不能用来计时"
	}
	return "平台不支持"
}

// hasInvariantTSC 在包初始化时探一次：CPUID 0x80000007 的 EDX bit8
// 表示「恒定 TSC + 不随 C-state 停走」。没有这个保证就不能用 ——
// 降频时 TSC 走得慢，测出来的耗时会把「省电降频」记成「变快了」。
var hasInvariantTSC = detectInvariantTSC()

func detectInvariantTSC() bool {
	maxExt, _, _, _ := cpuid(0x80000000, 0)
	if maxExt < 0x80000007 {
		return false
	}
	_, _, _, d := cpuid(0x80000007, 0)
	return d&(1<<8) != 0
}

// calibrate 标定 TSC 频率（Hz）。
//
// 为什么不能只量一次：这台机器的 time.Now 分辨率约 219µs，单次量 200ms 的相对
// 误差是 0.05%，10 秒的查询就会差 5ms。这里改成「30 个样本做最小二乘」——
// 量化噪声互相抵消，1.5 秒标定能把误差压到 0.001% 量级。
//
// 用 time.Sleep 而不是忙等：标定是在后台跑的，不占 CPU 更好。
// x 轴记录实际经过的时间（time.Since），所以 sleep 不准也没关系。
func calibrate() (float64, error) {
	const samples = 30
	const gap = 50 * time.Millisecond

	var (
		ts = make([]float64, 0, samples)
		cs = make([]float64, 0, samples)
	)
	c0, ok := readTSC()
	if !ok {
		return 0, errors.New("读不到 TSC")
	}
	t0 := time.Now()
	for i := 0; i < samples; i++ {
		time.Sleep(gap)
		c, ok := readTSC()
		if !ok {
			return 0, errors.New("读不到 TSC")
		}
		ts = append(ts, float64(time.Since(t0).Nanoseconds()))
		cs = append(cs, float64(c-c0))
	}

	n := float64(len(ts))
	var sx, sy, sxx, sxy float64
	for i := range ts {
		sx += ts[i]
		sy += cs[i]
		sxx += ts[i] * ts[i]
		sxy += ts[i] * cs[i]
	}
	den := n*sxx - sx*sx
	if den <= 0 {
		return 0, errors.New("样本异常，无法拟合")
	}
	slope := (n*sxy - sx*sy) / den // 每纳秒多少 tick
	if slope <= 0 {
		return 0, errors.New("拟合出的斜率为负")
	}
	hz := slope * 1e9
	// 1 GHz 到 100 GHz 之外的值说明哪里读错了，宁可不启用。
	if hz < 1e9 || hz > 1e11 {
		return 0, fmt.Errorf("标定结果不合理（%.2f GHz）", hz/1e9)
	}
	return hz, nil
}
