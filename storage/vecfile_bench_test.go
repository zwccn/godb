package storage

import (
	"path/filepath"
	"testing"
)

// 逐条读取的三条路径谁快、差多少。
//
//	go test ./storage/ -run XXX -bench VecRead -benchtime 2x
//
// 存在的理由：exec 层每行都要取一条向量，走哪条路由这里的数字决定。
// 两万行 × 128 维 = 10.2 MB（压小规模是为了跑得快，比值不受影响），
// 全部落在页缓存里，量的是纯 CPU 与系统调用开销，与磁盘无关。三条路径分别是：
//   - Slot  ：Read，单槽 + 64KB 预读块（按需随机取）；
//   - Range ：ReadRange，一次 ReadAt 取一批（顺序扫）；
//   - View  ：mmap 视图的 Slice（顺序扫的快速路径）。
func benchVecFile(b *testing.B, n, dim int) *VecFile {
	b.Helper()
	vf, err := OpenVecFile(filepath.Join(b.TempDir(), "t1.c1.vec"), dim)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { vf.Close() })
	batch := make([]float32, 1024*dim)
	for i := 0; i < dim*1024; i++ {
		batch[i] = float32(i%97) / 97
	}
	for start := int64(0); start < int64(n); start += 1024 {
		m := 1024
		if int(start)+m > n {
			m = n - int(start)
		}
		if err := vf.WriteRange(start, batch[:m*dim]); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	if err := vf.Sync(); err != nil {
		b.Fatalf("sync: %v", err)
	}
	return vf
}

func BenchmarkVecReadSlot(b *testing.B) {
	vf := benchVecFile(b, vecBenchN, vecBenchDim)
	buf := make([]float32, vecBenchDim)
	b.SetBytes(int64(vecBenchN * vecBenchDim * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for id := int64(0); id < int64(vecBenchN); id++ {
			if _, err := vf.Read(id, buf); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkVecReadRange(b *testing.B) {
	vf := benchVecFile(b, vecBenchN, vecBenchDim)
	var buf []float32
	b.SetBytes(int64(vecBenchN * vecBenchDim * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for from := int64(0); from < int64(vecBenchN); from += 4096 {
			n := 4096
			if int(from)+n > vecBenchN {
				n = vecBenchN - int(from)
			}
			var err error
			if buf, err = vf.ReadRange(from, n, buf); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkVecViewSlice(b *testing.B) {
	vf := benchVecFile(b, vecBenchN, vecBenchDim)
	vw, err := vf.NewView()
	if err != nil {
		b.Skipf("no view on this platform: %v", err)
	}
	defer vw.Close()
	var sum float32
	b.SetBytes(int64(vecBenchN * vecBenchDim * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for id := int64(0); id < int64(vecBenchN); id++ {
			v := vw.At(id)
			sum += v[0]
		}
	}
	vecBenchSink = sum
}

var vecBenchSink float32

const (
	vecBenchN   = 20000
	vecBenchDim = 128
)
