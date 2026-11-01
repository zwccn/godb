//go:build !windows || !amd64

package hrtime

import "errors"

// 其它平台不需要 TSC，这里只保证编译通过、平台检测如实返回「不支持」。
//
// Linux 的 time.Now 走 vDSO 的 clock_gettime(CLOCK_MONOTONIC)，内核用 TSC /
// arch_timer 实现，本来就是纳秒级；macOS 的 mach_absolute_time 同理。
// 真正会掉到几百微秒的只有 Windows 的 QPC，所以只有 windows/amd64 有实现。
//
// 将来要支持 Windows/arm64 时，在那边补一个 readTSC（CNTVCT_EL0）即可，
// 这份文件不用动。

func readTSC() (uint64, bool) { return 0, false }

func platformSupported() bool { return false }

func unsupportedReason() string { return "这个平台的系统时钟已经足够细，不需要 TSC" }

func calibrate() (float64, error) { return 0, errors.New("当前平台没有 TSC 实现") }
