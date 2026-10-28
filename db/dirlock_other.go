//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package db

import "os"

// 既没有 flock 也没有独占 CreateFile 的平台（plan9、aix、solaris…）：
// 退化成「能编译、不互斥」。这些平台本来也不跑生产，宁可少一层保护，
// 也不要让 CGO_ENABLED=0 的交叉编译挂掉 —— 跨平台可编译是硬约束。
func openExclusiveLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
}

func releaseLockFile(f *os.File) error {
	return f.Close()
}
