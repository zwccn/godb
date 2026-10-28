//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package db

import "time"

// procCPU 在这些平台上没有纯标准库的实现，返回 0。
//
// 面板上 CPU 那几项会显示「—」，其余指标照常——内存、goroutine、GC 都来自
// runtime/metrics，本来就跟平台无关。这里留一个空实现而不是让编译失败，
// 是为了「不影响跨平台」这条底线。
func procCPU() (user, kernel time.Duration) { return 0, 0 }
