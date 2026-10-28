package db

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 同一目录第二次拿锁必须失败：这是「两个进程同时打开一个数据目录」的守门人。
func TestLockDataDirExclusive(t *testing.T) {
	dir := t.TempDir()

	unlock, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("第一次拿锁不该失败: %v", err)
	}
	// 用 Cleanup 而不是末尾手动释放：任何 t.Fatalf 提前返回的路径都要放锁，
	// 否则 Windows 上打开着的句柄会让 TempDir 删不掉（Release 是幂等的）。
	t.Cleanup(func() { unlock() })

	if _, err := LockDataDir(dir); !errors.Is(err, ErrDirLocked) {
		t.Fatalf("第二次应该报 ErrDirLocked，实际: %v", err)
	}

	// 锁文件里要能看出是哪个 pid，否则用户只会看到「被占用」三个字，无从下手。
	b, err := os.ReadFile(filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatalf("读锁文件: %v", err)
	}
	if !strings.Contains(string(b), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("锁文件里应该写着自己的 pid，实际内容:\n%s", b)
	}

	if err := unlock(); err != nil {
		t.Fatalf("释放锁: %v", err)
	}
	// 释放后要能重新拿到，否则一次失败就永久锁死了。
	unlock2, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("释放后应该能重新拿到锁: %v", err)
	}
	unlock2()
}

// 错误信息要带 pid 和目录，不然只看到「被占用」帮不上忙。
func TestLockDataDirMessage(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("拿锁: %v", err)
	}
	defer unlock()
	_, err = LockDataDir(dir)
	if err == nil {
		t.Fatal("应该失败")
	}
	msg := err.Error()
	for _, want := range []string{strconv.Itoa(os.Getpid()), dir, lockFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里缺少 %q:\n%s", want, msg)
		}
	}
}

// OpenManager 也要守门：它是启动路径上唯一的入口。
func TestOpenManagerTwice(t *testing.T) {
	dir := t.TempDir()
	o := Options{}
	m, _, err := OpenManager(dir, o)
	if err != nil {
		t.Fatalf("第一次打开: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if _, _, err := OpenManager(dir, o); !errors.Is(err, ErrDirLocked) {
		t.Fatalf("第二次打开应该被拒，实际: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}
	// 关掉之后必须能再打开——重启服务就是这条路径。
	m2, _, err := OpenManager(dir, o)
	if err != nil {
		t.Fatalf("关闭后重新打开应该成功: %v", err)
	}
	m2.Close()
}

// 默认库打开失败时不能把锁捂着不放（否则改完配置重试会被自己挡住）。
func TestOpenManagerReleasesLockOnFailure(t *testing.T) {
	dir := t.TempDir()
	// 用一份坏掉的 catalog 让默认库打开失败。
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte("{ 这不是 json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenManager(dir, Options{}); err == nil {
		t.Fatal("坏 catalog 应该让打开失败")
	}
	// 锁必须已经释放：再拿一次应该成功。
	unlock, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("失败后锁没释放: %v", err)
	}
	unlock()
}
