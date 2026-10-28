//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package db

import (
	"syscall"
	"time"
)

// procCPU 返回本进程累计的用户态/内核态 CPU 时间。
//
// 为什么不用 runtime/metrics 的 /cpu/classes/total:cpu-seconds：
// 那个指标只在 GC 边界刷新（实测忙等 400ms 期间恒为 0，触发 GC 后才跳变），
// 而且包含并行 GC worker 烧掉的 CPU，跟业务代码的占用对不上。
// getrusage 是实时的，且直接对应「我这个进程占了多少 CPU」。
func procCPU() (user, kernel time.Duration) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	tv := func(t syscall.Timeval) time.Duration {
		return time.Duration(t.Sec)*time.Second + time.Duration(t.Usec)*time.Microsecond
	}
	return tv(ru.Utime), tv(ru.Stime)
}
