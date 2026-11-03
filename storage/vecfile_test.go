package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"godb/types"
)

func newTestVec(t *testing.T, dim int) (*VecFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t000001_c01.vec")
	vf, err := OpenVecFile(path, dim)
	if err != nil {
		t.Fatalf("OpenVecFile: %v", err)
	}
	t.Cleanup(func() { vf.Close() })
	return vf, path
}

func TestVecFileWriteRead(t *testing.T) {
	const dim = 4
	vf, path := newTestVec(t, dim)

	want := [][]float32{
		{1, 2, 3, 4},
		{-0.5, 0, 0.5, 1e3},
		{0, 0, 0, 0},
	}
	for _, v := range want {
		if _, err := vf.Append(v); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := vf.Count(); got != int64(len(want)) {
		t.Fatalf("Count = %d，期望 %d", got, len(want))
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if wantSize := int64(vecHdrSize + len(want)*dim*4); st.Size() != wantSize {
		t.Fatalf("文件长度 %d，期望 %d", st.Size(), wantSize)
	}

	var buf []float32
	for i, w := range want {
		got, err := vf.Read(int64(i), buf)
		if err != nil {
			t.Fatalf("Read(%d): %v", i, err)
		}
		buf = got // 复用同一块
		for j := range w {
			if got[j] != w[j] {
				t.Fatalf("槽位 %d 第 %d 维 = %v，期望 %v", i, j, got[j], w[j])
			}
		}
	}

	// 越界读必须是 ErrVecMissing（调用方据此把行投影成 NULL）
	if _, err := vf.Read(int64(len(want)), buf); !errors.Is(err, ErrVecMissing) {
		t.Fatalf("越界读错误 = %v，期望 ErrVecMissing", err)
	}
	if _, err := vf.Read(-1, buf); !errors.Is(err, ErrVecMissing) {
		t.Fatalf("负槽位读错误 = %v，期望 ErrVecMissing", err)
	}
	// 维度不对的写入必须拒绝，不能截断/补零
	if err := vf.Write(0, []float32{1, 2}); err == nil {
		t.Fatal("维度不匹配的 Write 应该报错")
	}
}

func TestVecFileReopen(t *testing.T) {
	const dim = 3
	vf, path := newTestVec(t, dim)
	vals := [][]float32{{1, 2, 3}, {4, 5, 6}}
	for _, v := range vals {
		if _, err := vf.Append(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := vf.Close(); err != nil {
		t.Fatal(err)
	}

	vf2, err := OpenVecFile(path, dim)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer vf2.Close()
	if got := vf2.Count(); got != int64(len(vals)) {
		t.Fatalf("重开后 Count = %d，期望 %d", got, len(vals))
	}
	for i, w := range vals {
		got, err := vf2.Read(int64(i), nil)
		if err != nil {
			t.Fatalf("重开后 Read(%d): %v", i, err)
		}
		for j := range w {
			if got[j] != w[j] {
				t.Fatalf("重开后槽位 %d 第 %d 维 = %v，期望 %v", i, j, got[j], w[j])
			}
		}
	}
}

// 「头里的 count 还没落盘」是最常见的崩溃形态：数据写进去了，头还是旧值。
// 重新打开时必须按文件长度把槽位找回来，否则已提交的行会指向空槽。
func TestVecFileRecoverFromStaleHeader(t *testing.T) {
	const dim = 2
	vf, path := newTestVec(t, dim)
	// 头只在每 vecHdrFlush 次扩展后落盘，所以写到 3 个槽位时头里还是 0
	for i := 0; i < 3; i++ {
		if err := vf.Write(int64(i), []float32{float32(i), float32(i * 2)}); err != nil {
			t.Fatal(err)
		}
	}
	// 故意不 Sync、不 Close，模拟「数据已进页缓存但头没刷」。
	// 先复制一份文件再打开，避免 Close 把真头写进去。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vf.Close()

	copyPath := filepath.Join(t.TempDir(), "crash.vec")
	if err := os.WriteFile(copyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	vf2, err := OpenVecFile(copyPath, dim)
	if err != nil {
		t.Fatalf("打开崩溃副本失败: %v", err)
	}
	defer vf2.Close()
	if got := vf2.Count(); got != 3 {
		t.Fatalf("按文件长度恢复的 Count = %d，期望 3", got)
	}
	got, err := vf2.Read(2, nil)
	if err != nil {
		t.Fatalf("读第 3 个槽位失败: %v", err)
	}
	if got[0] != 2 || got[1] != 4 {
		t.Fatalf("槽位 2 = %v，期望 [2 4]", got)
	}
}

// 文件被截断（拷了一半 / 掉电丢了尾部）时，count 要按实际长度收回，
// 而且不能报错 —— 丢了向量是「那一行投影成 NULL」，不是「库打不开」。
func TestVecFileTruncatedFile(t *testing.T) {
	const dim = 4
	vf, path := newTestVec(t, dim)
	for i := 0; i < 4; i++ {
		if _, err := vf.Append([]float32{1, 1, 1, float32(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := vf.Sync(); err != nil {
		t.Fatal(err)
	}
	vf.Close()

	// 砍掉一个半槽位
	st, _ := os.Stat(path)
	if err := os.Truncate(path, st.Size()-int64(dim*4*3/2)); err != nil {
		t.Fatal(err)
	}
	vf2, err := OpenVecFile(path, dim)
	if err != nil {
		t.Fatalf("截断后打开失败: %v", err)
	}
	defer vf2.Close()
	if got := vf2.Count(); got != 2 {
		t.Fatalf("截断后 Count = %d，期望 2（按完整槽位向下取整）", got)
	}
	if _, err := vf2.Read(2, nil); !errors.Is(err, ErrVecMissing) {
		t.Fatalf("被截掉的槽位应返回 ErrVecMissing，得到 %v", err)
	}

	// 修好之后再追加，必须从 count 继续，不能覆盖已有数据
	id, err := vf2.Append([]float32{9, 9, 9, 9})
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("截断后新槽位 = %d，期望 2", id)
	}
}

func TestVecFileFreeReuse(t *testing.T) {
	const dim = 2
	vf, _ := newTestVec(t, dim)
	for i := 0; i < 4; i++ {
		if _, err := vf.Append([]float32{float32(i), 0}); err != nil {
			t.Fatal(err)
		}
	}
	vf.Free(1)
	vf.Free(3)
	if got := vf.FreeSlots(); got != 2 {
		t.Fatalf("FreeSlots = %d，期望 2", got)
	}
	// 重复归还不能把计数抬高
	vf.Free(1)
	if got := vf.FreeSlots(); got != 2 {
		t.Fatalf("重复 Free 后 FreeSlots = %d，期望 2", got)
	}
	// 越界归还直接忽略
	vf.Free(99)
	vf.Free(-1)

	id, err := vf.Append([]float32{7, 7})
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 && id != 3 {
		t.Fatalf("复用的槽位 = %d，期望 1 或 3", id)
	}
	got, err := vf.Read(id, nil)
	if err != nil || got[0] != 7 {
		t.Fatalf("复用槽位内容 = %v, %v", got, err)
	}
	if vf.Count() != 4 {
		t.Fatalf("复用不该增加 Count，得到 %d", vf.Count())
	}

	// 全部归还后，分配必须优先走空闲
	vf.Free(0)
	vf.Free(2)
	vf.Free(1)
	vf.Free(3)
	vf2, _ := newTestVec(t, dim)
	_ = vf2
	for i := 0; i < 4; i++ {
		id, err := vf.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		if id < 0 || id > 3 {
			t.Fatalf("全部空闲时分配到了 %d，期望落在 0..3", id)
		}
	}
	if vf.Count() != 4 {
		t.Fatalf("复用全部空闲槽位后 Count = %d，期望 4", vf.Count())
	}
}

func TestVecFileTruncateTo(t *testing.T) {
	const dim = 2
	vf, _ := newTestVec(t, dim)
	for i := 0; i < 10; i++ {
		if _, err := vf.Append([]float32{float32(i), 0}); err != nil {
			t.Fatal(err)
		}
	}
	vf.Free(8)
	vf.Free(2)
	if err := vf.TruncateTo(4); err != nil {
		t.Fatalf("TruncateTo: %v", err)
	}
	if vf.Count() != 4 {
		t.Fatalf("截断后 Count = %d", vf.Count())
	}
	if vf.FreeSlots() != 1 { // 只剩槽位 2 还在范围内
		t.Fatalf("截断后 FreeSlots = %d，期望 1", vf.FreeSlots())
	}
	// 被截掉的槽位 8 不能再被分配出来：第一个分到的是还在范围内的 2
	id, err := vf.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("截断后首个分配 = %d，期望 2", id)
	}
	// 空闲用完之后继续从 count 往后长，这是正常的；关键是文件长度跟着回来
	if err := vf.Write(4, []float32{1, 2}); err != nil {
		t.Fatalf("截断后扩展写失败: %v", err)
	}
	if vf.Count() != 5 {
		t.Fatalf("扩展后 Count = %d，期望 5", vf.Count())
	}
	st, err := os.Stat(vf.Path())
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(vecHdrSize + 5*dim*4); st.Size() != want {
		t.Fatalf("扩展后文件长度 = %d，期望 %d", st.Size(), want)
	}
}

func TestVecFileBatch(t *testing.T) {
	const dim = 8
	const rows = 500
	vf, _ := newTestVec(t, dim)

	batch := make([]float32, rows*dim)
	for i := 0; i < rows; i++ {
		for j := 0; j < dim; j++ {
			batch[i*dim+j] = float32(i*100 + j)
		}
	}
	if err := vf.WriteRange(0, batch); err != nil {
		t.Fatalf("WriteRange: %v", err)
	}
	if vf.Count() != rows {
		t.Fatalf("Count = %d，期望 %d", vf.Count(), rows)
	}
	if err := vf.WriteRange(0, make([]float32, dim+1)); err == nil {
		t.Fatal("长度不是维度整数倍时应报错")
	}

	// 顺序扫描：一次拿 64 行
	var buf []float32
	total := 0
	for from := int64(0); from < rows; from += 64 {
		got, err := vf.ReadRange(from, 64, buf)
		if err != nil {
			t.Fatalf("ReadRange(%d): %v", from, err)
		}
		buf = got
		n := len(got) / dim
		if n == 0 {
			t.Fatalf("ReadRange(%d) 空", from)
		}
		for k := 0; k < n; k++ {
			row := from + int64(k)
			for j := 0; j < dim; j++ {
				if got[k*dim+j] != batch[int(row)*dim+j] {
					t.Fatalf("行 %d 第 %d 维不一致", row, j)
				}
			}
		}
		total += n
	}
	if total != rows {
		t.Fatalf("扫描到 %d 行，期望 %d", total, rows)
	}
	// 越界范围读要返回空而不是报错
	if got, err := vf.ReadRange(rows+10, 8, buf); err != nil || len(got) != 0 {
		t.Fatalf("越界 ReadRange = %d, %v", len(got), err)
	}
}

func TestVecFileDimMismatch(t *testing.T) {
	const dim = 4
	vf, path := newTestVec(t, dim)
	if _, err := vf.Append([]float32{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	vf.Close()

	if _, err := OpenVecFile(path, 8); !errors.Is(err, ErrVecDimMismatch) {
		t.Fatalf("维度不符应返回 ErrVecDimMismatch，得到 %v", err)
	}
}

func TestVecFileCorruptHeader(t *testing.T) {
	vf, path := newTestVec(t, 2)
	if _, err := vf.Append([]float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	vf.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'X'
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVecFile(path, 2); !errors.Is(err, ErrVecCorrupt) {
		t.Fatalf("magic 被改应返回 ErrVecCorrupt，得到 %v", err)
	}

	raw[0] = 'G'
	raw[28] ^= 0xFF // crc
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVecFile(path, 2); !errors.Is(err, ErrVecCorrupt) {
		t.Fatalf("crc 被改应返回 ErrVecCorrupt，得到 %v", err)
	}
}

// 并发写不能串数据：每条向量第一个元素放 goroutine 号、第二个放序号，
// 写完全部读回，按 (g, i) 核对唯一性。
func TestVecFileConcurrentWrites(t *testing.T) {
	const dim = 4
	const workers = 8
	const perWorker = 100
	vf, _ := newTestVec(t, dim)

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := vf.Append([]float32{float32(g), float32(i), float32(g * i), 1}); err != nil {
					t.Errorf("goroutine %d 写第 %d 条: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if got := vf.Count(); got != workers*perWorker {
		t.Fatalf("Count = %d，期望 %d", got, workers*perWorker)
	}
	seen := make(map[[2]int]bool, workers*perWorker)
	for id := int64(0); id < vf.Count(); id++ {
		v, err := vf.Read(id, nil)
		if err != nil {
			t.Fatalf("Read(%d): %v", id, err)
		}
		key := [2]int{int(v[0]), int(v[1])}
		if seen[key] {
			t.Fatalf("槽位 %d 的 (g,i) = %v 重复了", id, key)
		}
		seen[key] = true
		if v[2] != float32(key[0]*key[1]) {
			t.Fatalf("槽位 %d 内容串了: %v", id, v)
		}
	}
}

// 行编码必须能存取向量：ref 只占 11 字节，内联字面量按文本兜底。
// 早先 KVector 落到 appendValue 的 default 分支会被写成 NULL —— 静默丢数据。
func TestVectorRowEncoding(t *testing.T) {
	row := types.Row{
		types.Int(1),
		types.VectorRef(12345, 768),
		types.VectorLiteral("[1,2,3]", 3),
		types.Null,
		types.Text("x"),
	}
	buf := appendRow(nil, row)
	got, off, err := readRowValues(buf, 0)
	if err != nil {
		t.Fatalf("readRowValues: %v", err)
	}
	if off != len(buf) {
		t.Fatalf("读了 %d 字节，编码是 %d 字节", off, len(buf))
	}
	if len(got) != len(row) {
		t.Fatalf("列数 %d，期望 %d", len(got), len(row))
	}
	if id, ok := got[1].VecID(); !ok || id != 12345 || got[1].VecDim() != 768 {
		t.Fatalf("ref 还原不对: %+v", got[1])
	}
	if got[2].K != types.KVector || got[2].TextForm() != "[1,2,3]" || got[2].VecDim() != 3 {
		t.Fatalf("内联字面量还原不对: %+v", got[2])
	}
	if !got[3].IsNull() {
		t.Fatalf("NULL 坏了: %+v", got[3])
	}
	if got[4].S != "x" {
		t.Fatalf("后面的列串了: %+v", got[4])
	}

	// ref 必须只占 1 + 8 + 2 字节，超过就说明把 float32 写进日志了
	refOnly := appendValue(nil, types.VectorRef(1, 768))
	if len(refOnly) != 11 {
		t.Fatalf("向量的 WAL 编码是 %d 字节，期望 11", len(refOnly))
	}
}

// 顺序扫描吞吐：这是向量检索的唯一热路径，退出条件写在设计文档里。
// 走 t.Short 跳过，`go test -short` 不跑。
func TestVecFileScanThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("吞吐测试，-short 跳过")
	}
	const dim = 128
	const rows = 1000000 // 512 MB
	vf, _ := newTestVec(t, dim)

	batch := make([]float32, 10000*dim)
	for i := range batch {
		batch[i] = float32(i%7) - 3
	}
	t0 := time.Now()
	for r := 0; r < rows/10000; r++ {
		if err := vf.WriteRange(int64(r*10000), batch); err != nil {
			t.Fatalf("WriteRange: %v", err)
		}
	}
	writeDur := time.Since(t0)
	if err := vf.Sync(); err != nil {
		t.Fatal(err)
	}
	gb := float64(rows*dim*4) / (1 << 30)
	t.Logf("写入 %d 行 × %d 维 = %.2f GB，%.2fs (%.2f GB/s)",
		rows, dim, gb, writeDur.Seconds(), gb/writeDur.Seconds())

	// 每 4 个 float 碰一次：够触发整行预取，又不至于把测量变成 ALU 基准
	touch := func(v []float32) float64 {
		var s float64
		for i := 0; i < len(v); i += 4 {
			s += float64(v[i])
		}
		return s
	}

	// (1) ReadAt 单线程：Windows 上同一句柄的 I/O 被串行化，这条路径就是慢
	var buf []float32
	var sum float64
	t0 = time.Now()
	for from := int64(0); from < rows; from += 1024 {
		got, err := vf.ReadRange(from, 1024, buf)
		if err != nil {
			t.Fatalf("ReadRange(%d): %v", from, err)
		}
		buf = got
		sum += touch(got)
	}
	readAtGBs := gb / time.Since(t0).Seconds()

	// (2) 映射视图单线程
	view, err := vf.NewView()
	if err != nil {
		t.Fatalf("NewView: %v", err)
	}
	defer view.Close()
	if view.Count() != rows {
		t.Fatalf("视图 Count = %d，期望 %d", view.Count(), rows)
	}
	// 映射刚建立时第一次触碰每页要陷缺页（本机约 2 GB/s），那是「把页准备好」
	// 的一次性成本，不是扫描速度。预热一遍再计。
	for from := int64(0); from < rows; from += 1024 {
		sum += touch(view.Slice(from, 1024))
	}
	t0 = time.Now()
	for from := int64(0); from < rows; from += 1024 {
		sum += touch(view.Slice(from, 1024))
	}
	view1GBs := gb / time.Since(t0).Seconds()

	// (3) 映射视图 4 线程（查询层实际会这么用）
	const workers = 4
	per := int64(rows) / workers
	sums := make([]float64, workers)
	t0 = time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var s float64
			for from := int64(i) * per; from < int64(i+1)*per; from += 1024 {
				s += touch(view.Slice(from, 1024))
			}
			sums[i] = s // 必须回写，否则整个循环会被编译器消掉
		}(i)
	}
	wg.Wait()
	viewNBGBs := gb / time.Since(t0).Seconds()
	for _, s := range sums {
		sum += s
	}

	t.Logf("顺序扫描 %.2f GB：ReadAt 单线程 %.2f GB/s；映射视图 单线程 %.2f GB/s、%d 线程 %.2f GB/s (sum=%.1f)",
		gb, readAtGBs, view1GBs, workers, viewNBGBs, sum)

	if view1GBs < 6 {
		t.Fatalf("映射视图单线程只有 %.2f GB/s，低于验收线 6 GB/s", view1GBs)
	}
	if viewNBGBs < 12 {
		t.Fatalf("%d 线程只有 %.2f GB/s，低于验收线 12 GB/s", workers, viewNBGBs)
	}
}

// 映射视图必须和写入的数据逐位一致，并且 Slice 的边界要老实。
func TestVecViewSlices(t *testing.T) {
	const dim = 3
	const rows = 10
	vf, _ := newTestVec(t, dim)
	want := make([]float32, rows*dim)
	for i := 0; i < rows; i++ {
		for j := 0; j < dim; j++ {
			want[i*dim+j] = float32(i) + float32(j)/10
		}
	}
	if err := vf.WriteRange(0, want); err != nil {
		t.Fatal(err)
	}
	if err := vf.Sync(); err != nil {
		t.Fatal(err)
	}

	view, err := vf.NewView()
	if err != nil {
		t.Fatalf("NewView: %v", err)
	}
	defer view.Close()

	all := view.Slice(0, rows)
	if len(all) != rows*dim {
		t.Fatalf("全长切片 %d，期望 %d", len(all), rows*dim)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("第 %d 个元素 = %v，期望 %v", i, all[i], want[i])
		}
	}
	// 越界要收窄
	if got := view.Slice(8, 10); len(got) != 2*dim {
		t.Fatalf("越界切片长度 %d，期望 %d", len(got), 2*dim)
	}
	if got := view.Slice(rows, 5); got != nil {
		t.Fatalf("完全越界应返回 nil，得到 %d", len(got))
	}
	if got := view.Slice(-1, 5); got != nil {
		t.Fatal("负起点应返回 nil")
	}
	if got := view.At(3); len(got) != dim || got[0] != 3 {
		t.Fatalf("At(3) = %v", got)
	}
	// 写入后重新开视图必须能看到新数据（视图不是快照）
	if err := vf.Write(0, []float32{7, 7, 7}); err != nil {
		t.Fatal(err)
	}
	view2, err := vf.NewView()
	if err != nil {
		t.Fatal(err)
	}
	defer view2.Close()
	if got := view2.At(0); got[0] != 7 {
		t.Fatalf("新视图没看到写入: %v", got)
	}
}

// View 是「整文件映射」的零拷贝读取，exec 层的距离查询全靠它。
// 这条用例盯四件事，每一件出错都不会报错、只会悄悄给出错数据：
//  1. 映射建立之后再写入，映射里必须看得见（页缓存是同一份）；
//  2. 分配了但还没写的槽位必须返回 false，不能被当成「全零向量」；
//  3. 文件长大之后，新槽位要么走重映射读到，要么返回 false 让上层退回 Read；
//  4. 截断之后不能再有任何映射指到文件末尾之外（那会直接崩进程）。
func TestVecFileViewCoherence(t *testing.T) {
	const dim = 4
	vf, _ := newTestVec(t, dim)

	if err := vf.Write(0, []float32{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	// 先读一次，把映射建起来
	got, ok := vf.View(0)
	if !ok {
		t.Fatal("View(0) 应当可用")
	}
	if got[0] != 1 || got[3] != 4 {
		t.Fatalf("View(0) = %v", got)
	}

	// 1. 映射已存在时写入，映射必须立刻看到新值
	if err := vf.Write(1, []float32{5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if got, ok := vf.View(1); !ok || got[0] != 5 || got[3] != 8 {
		t.Fatalf("写入后映射没看到新数据: %v ok=%v", got, ok)
	}

	// 2. 分配了但还没写的槽位 = 没有数据
	id, err := vf.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vf.View(id); ok {
		t.Fatalf("槽位 %d 还没写，View 不该说「有数据」", id)
	}
	if _, err := vf.Read(id, make([]float32, dim)); !errors.Is(err, ErrVecMissing) {
		t.Fatalf("未写槽位 Read 应返回 ErrVecMissing，得到 %v", err)
	}

	// 3. 文件长大：把映射撑破后新槽位仍然读得到
	big := make([]float32, 200*dim)
	for i := range big {
		big[i] = float32(i + 1)
	}
	if err := vf.WriteRange(vf.Count(), big); err != nil {
		t.Fatal(err)
	}
	last := vf.Count() - 1
	if got, ok := vf.View(last); !ok || got[0] != big[(200-1)*dim] {
		t.Fatalf("长大后的末槽位读不到: %v ok=%v 期望 %v", got, ok, big[(200-1)*dim])
	}
	// 重映射之后老槽位也得还在
	if got, ok := vf.View(0); !ok || got[0] != 1 {
		t.Fatalf("重映射后老槽位丢失: %v ok=%v", got, ok)
	}

	// 4. 截断：映射必须先解除，否则读取会踩到文件末尾之外的内存
	if err := vf.TruncateTo(2); err != nil {
		t.Fatal(err)
	}
	if _, ok := vf.View(last); ok {
		t.Fatal("截断之后越界槽位不该可读")
	}
	if got, ok := vf.View(1); !ok || got[0] != 5 {
		t.Fatalf("截断后保留的槽位读不到: %v ok=%v", got, ok)
	}
}
