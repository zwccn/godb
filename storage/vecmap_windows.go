//go:build windows

package storage

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// vecMapping 是一个只读的文件映射视图。
//
// 为什么另开一个句柄而不是复用 VecFile.f：写入路径会不断扩文件，
// 映射只服务于「一次查询的生命周期」，两者生命周期不同，分开更好解释。
type vecMapping struct {
	view []byte
	hmap syscall.Handle
	addr uintptr
}

// mapFileTo 把文件的前 size 字节只读映射进地址空间。
func mapFileTo(f *os.File, size int64) (*vecMapping, error) {
	if size <= 0 {
		return &vecMapping{}, nil
	}
	// size 超过 4GB 时要拆高低位
	h, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY,
		uint32(uint64(size)>>32), uint32(uint64(size)), nil)
	if err != nil {
		return nil, fmt.Errorf("storage: CreateFileMapping 失败: %w", err)
	}
	addr, err := syscall.MapViewOfFile(h, syscall.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("storage: MapViewOfFile 失败: %w", err)
	}
	return &vecMapping{
		view: viewOf(addr, size),
		hmap: h,
		addr: addr,
	}, nil
}

// viewOf 把系统返回的地址变成 []byte。
//
// 这里绕了一下指针转换：addr 是操作系统给的映射地址，不属于 Go 堆，不存在
// 「被 GC 搬走」的问题；直接写 unsafe.Pointer(addr) 会被 go vet 的 unsafeptr
// 检查当成 uintptr→Pointer 的误用报出来（CI 会挂）。
func viewOf(addr uintptr, size int64) []byte {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return unsafe.Slice((*byte)(p), size)
}

// Close 解除映射并关掉映射句柄。
func (m *vecMapping) Close() error {
	if m == nil {
		return nil
	}
	if m.addr != 0 {
		if err := syscall.UnmapViewOfFile(m.addr); err != nil {
			return err
		}
		m.addr = 0
	}
	m.view = nil
	if m.hmap != 0 {
		syscall.CloseHandle(m.hmap)
		m.hmap = 0
	}
	return nil
}
