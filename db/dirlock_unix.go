//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package db

import (
	"errors"
	"os"
	"syscall"
)

// openExclusiveLock 用 flock(LOCK_EX|LOCK_NB) 拿独占锁。
//
// 选 flock 而不是「O_EXCL 创建文件」：flock 跟着文件描述符走，进程被
// kill -9 时内核自动释放，不会留下需要手工清理的「僵死锁文件」。
// LOCK_NB 是为了不阻塞启动 —— 拿不到就明确报错退出。
func openExclusiveLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return f, nil
}

func releaseLockFile(f *os.File) error {
	// 先显式解锁再关：某些文件系统上 close 不保证立刻释放，显式一点更稳。
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
