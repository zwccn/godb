//go:build windows

package db

import (
	"syscall"
	"time"
)

// procCPU 用 GetProcessTimes 取本进程累计 CPU 时间。
//
// 注意这里**不是**自己绑定 DLL：syscall.GetProcessTimes 是标准库导出并封装好的
// 函数（zsyscall_windows.go 里由 syscall 包生成），调用它和调用 os.Stat 是同一性质
// —— os.Stat 内部也是 GetFileAttributesExW。所以「不用 cgo、不自己写平台绑定」
// 这两条都成立，CGO_ENABLED=0 交叉编译无影响。
func procCPU() (user, kernel time.Duration) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0
	}
	var create, exit, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &create, &exit, &k, &u); err != nil {
		return 0, 0
	}
	// Filetime 是 100ns 单位，Nanoseconds() 已经换算好。
	return time.Duration(u.Nanoseconds()), time.Duration(k.Nanoseconds())
}
