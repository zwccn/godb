//go:build !windows && !linux && !darwin

package storage

import (
	"fmt"
	"os"
)

// vecMapping 在没有 mmap 的平台上是个空壳：NewView 会返回错误，
// 调用方退回到 VecFile.ReadRange（能用，只是慢）。
type vecMapping struct{}

func mapFileTo(f *os.File, size int64) (*vecMapping, error) {
	return nil, fmt.Errorf("storage: 当前平台不支持文件映射，请改用 ReadRange")
}

func (m *vecMapping) Close() error { return nil }
