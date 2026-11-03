//go:build linux || darwin

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// vecMapping 是一个只读的文件映射视图（POSIX 版）。
type vecMapping struct {
	view []byte
}

// mapFileTo 把文件的前 size 字节只读映射进地址空间。
func mapFileTo(f *os.File, size int64) (*vecMapping, error) {
	if size <= 0 {
		return &vecMapping{}, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("storage: mmap 失败: %w", err)
	}
	return &vecMapping{view: b}, nil
}

// Close 解除映射。
func (m *vecMapping) Close() error {
	if m == nil || m.view == nil {
		return nil
	}
	err := syscall.Munmap(m.view)
	m.view = nil
	return err
}
