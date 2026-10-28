//go:build windows

package db

import (
	"os"
	"syscall"
)

// ERROR_SHARING_VIOLATION = 32 没有从 syscall 包导出（它在 internal/syscall/windows 里），
// 只能自己写常量：这个值和 Windows SDK 的 ERROR_SHARING_VIOLATION 一致。
const errorSharingViolation syscall.Errno = 32

// openExclusiveLock 用「不共享写」的 CreateFile 拿独占锁。
//
// 参数含义（和 Unix 的 flock 等价，只是换成 Windows 的共享语义）：
//   - access 要 READ|WRITE：只要读权限的话，两个进程都能拿到，起不到互斥作用
//   - share 只给 FILE_SHARE_READ：别人还能读文件内容（这样第二次启动能读出
//     占用者的 pid 来提示用户），但不能再申请写访问，所以第二个实例必定失败
//
// 句柄随进程退出由系统关闭，锁自然释放，不需要清理残留。
func openExclusiveLock(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		// 只把「共享冲突」当成被占用。ACCESS_DENIED 是权限问题（只读文件、
		// 另一个用户的目录），属于另一类错，要让它照原样露出来。
		if err == errorSharingViolation {
			return nil, errLockBusy
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func releaseLockFile(f *os.File) error {
	return f.Close()
}
