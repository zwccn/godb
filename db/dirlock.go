package db

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lockFileName 是数据目录根部的独占锁文件。
//
// 它同时干两件事：
//  1. 互斥 —— 由平台层保证（Unix 用 flock，Windows 用不共享的 CreateFile）。
//     进程退出（包括被 taskkill / kill -9）时由内核释放，所以不会有僵死锁。
//  2. 留痕 —— 文件里写着占用者的 pid / 启动时间 / 程序路径，第二次启动
//     能直接报出是哪个进程占着，不用去猜。
//
// 故意**不删**这个文件：先释放再删会和「别人刚拿到锁」抢同一个路径，
// 出现「A 删掉 B 正持有的文件 → C 新建同名文件拿到锁」的双持有窗口。
// 文件留着无害，下次打开直接复用、内容被覆盖成新的。
const lockFileName = ".lock"

// ErrDirLocked 表示数据目录已经被另一个活着的进程持有。
//
// 定成变量是为了调用方能 errors.Is 判断，也方便测试。
var ErrDirLocked = errors.New("数据目录已被另一个进程占用")

// errLockBusy 是平台层「锁被别的进程拿着」的内部信号，
// 由 lockDataDir 翻译成带 pid 的 ErrDirLocked。
var errLockBusy = errors.New("lock busy")

type dirLock struct {
	path string
	f    *os.File
}

// lockDataDir 拿数据目录的独占锁。
//
// 必须在读 catalog 之前调用：两个进程同时打开同一个目录会各写各的快照，
// 事后根本分不清是谁覆盖了谁（2026-10-28 那次丢表的现场里就同时起过两个实例，
// 只靠端口抢占挡住，数据目录没有任何保护）。
func lockDataDir(dir string) (*dirLock, error) {
	// 首次启动时目录可能还不存在（配置加载会建，但离线恢复走的是另一条路）。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	path := filepath.Join(dir, lockFileName)
	f, err := openExclusiveLock(path)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, lockedError(dir, path)
		}
		return nil, fmt.Errorf("无法建立数据目录锁 %s: %w", path, err)
	}
	lk := &dirLock{path: path, f: f}
	// 写不进去不影响互斥（锁已经在手上了），只是下次报错时看不到 pid。
	_ = lk.stamp(dir)
	return lk, nil
}

// LockDataDir 抢占数据目录的独占锁，返回释放函数。
//
// 给离线操作用（回收站恢复、外部工具直接改表文件）：拿不到锁就说明还有
// 一个 godb 在跑，这时候改磁盘上的表文件会被它内存里那份反向覆盖。
// 调用方拿到错误后直接退出即可，错误信息里已经带了 pid 和处理建议。
func LockDataDir(dir string) (func() error, error) {
	lk, err := lockDataDir(dir)
	if err != nil {
		return nil, err
	}
	return lk.Release, nil
}

// Release 释放锁。可以重复调用（Close 会被调用多次）。
func (l *dirLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return releaseLockFile(f)
}

// stamp 把占用者信息写进锁文件，纯粹为了「下次启动时能说清是谁」。
func (l *dirLock) stamp(dir string) error {
	txt := fmt.Sprintf("pid %d\nstarted %s\ndir %s\nexe %s\n",
		os.Getpid(),
		time.Now().Format(time.RFC3339),
		dir,
		exePath(),
	)
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := l.f.WriteString(txt); err != nil {
		return err
	}
	return l.f.Sync()
}

// lockedError 把「锁被占用」翻译成一句能直接照做的中文。
//
// 必须用 %w 把 ErrDirLocked 包进链条里：调用方（和测试）靠 errors.Is 判断
// 「是不是被占用」，而不是去匹配一段翻译过的中文。所以这里先用 %w 起头
// 再拼细节，不能另起一个 errors.New。
func lockedError(dir, path string) error {
	pid, since, exe := readLockStamp(path)
	var b strings.Builder
	if pid > 0 {
		fmt.Fprintf(&b, "（pid %d", pid)
		if since != "" {
			b.WriteString("，" + since + " 启动")
		}
		if exe != "" {
			b.WriteString("，" + exe)
		}
		b.WriteString("）")
	} else {
		b.WriteString("（锁文件里没有 pid，可能来自另一个用户或一份被改坏的文件）")
	}
	b.WriteString("\n数据目录: " + dir)
	b.WriteString("\n同一个数据目录不能同时被两个 godb 打开 —— 两边会各写各的快照，互相覆盖表结构和数据。")
	if pid > 0 {
		fmt.Fprintf(&b, "\n请先停掉那个进程（任务管理器，或 tasklist /FI \"PID eq %d\" 确认后 taskkill /PID %d /F）再启动。", pid, pid)
	}
	b.WriteString("\n确认没有任何 godb 在跑时，删掉 " + path + " 再试。")
	return fmt.Errorf("%w%s", ErrDirLocked, b.String())
}

// readLockStamp 解析锁文件内容。拿不到就当空（不影响主流程）。
func readLockStamp(path string) (pid int, since, exe string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			pid, _ = strconv.Atoi(v)
		case "started":
			since = v
		case "exe":
			exe = v
		}
	}
	return pid, since, exe
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}
