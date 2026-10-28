package db

import (
	"fmt"
	"testing"
	"time"

	"godb/storage"
)

// 进程采样器：内存 / goroutine / GC 立刻有值，CPU 要等第二个采样点（差值才有意义）。
func TestProcSamplerProducesData(t *testing.T) {
	d := newTestDB(t)
	if d.proc == nil {
		t.Fatal("采样器没有初始化")
	}
	p, _ := d.proc.stats()
	if p.NumCPU <= 0 {
		t.Errorf("NumCPU = %d，应该是本机核数", p.NumCPU)
	}
	if p.Goroutines <= 0 {
		t.Errorf("goroutine 数 = %d，应该大于 0", p.Goroutines)
	}
	if p.HeapObjects <= 0 {
		t.Errorf("活对象字节 = %d，测试进程不可能为 0", p.HeapObjects)
	}

	// CPU 靠两次采样的差值，等后台 ticker 跑两轮。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, _ = d.proc.stats()
		if p.Sampled && len(p.Series) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !p.Sampled {
		t.Fatal("等不到 CPU 采样（Sampled 仍为 false）")
	}
	if len(p.Series) < 2 {
		t.Fatalf("曲线点 = %d，至少应该有 2 个", len(p.Series))
	}
	// 曲线必须按时间正序，否则前端画出来是乱的
	for i := 1; i < len(p.Series); i++ {
		if p.Series[i].AtMs < p.Series[i-1].AtMs {
			t.Fatalf("曲线点 %d 的时间早于前一个点", i)
		}
	}
	if p.CPUCore < 0 || p.CPUUserCore < 0 || p.CPUKernelCore < 0 {
		t.Errorf("CPU 值不可能为负：%+v", p)
	}
}

// checkpoint 画像要能回答「上一次为什么写盘、写了多少、花了多久」。
func TestCheckpointProfileRecordsReason(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "c1", "CREATE TABLE cp_probe (id int primary key, v text)")
	for i := 1; i <= 5; i++ {
		run(t, d, "c1", fmt.Sprintf("INSERT INTO cp_probe VALUES (%d, 'row')", i))
	}
	// CHECKPOINT 语句先 Flush（刷 WAL）再写快照，所以原因记成 flush
	run(t, d, "c1", "CHECKPOINT")

	cp := d.Store.CheckpointProfile()
	if cp.LastReason != storage.CPReasonFlush {
		t.Errorf("LastReason = %q，期望 %q", cp.LastReason, storage.CPReasonFlush)
	}
	if cp.Runs < 1 {
		t.Errorf("Runs = %d，至少应该落过一次盘", cp.Runs)
	}
	if cp.LastBytes <= 0 {
		t.Errorf("LastBytes = %d，有数据的表不可能写 0 字节", cp.LastBytes)
	}
	if cp.P50Ns <= 0 {
		t.Errorf("P50Ns = %d，耗时样本应该被记录", cp.P50Ns)
	}
	if cp.Reasons[storage.CPReasonFlush] < 1 {
		t.Errorf("原因分布里应该有 flush：%v", cp.Reasons)
	}
	if cp.LastAtMs <= 0 {
		t.Error("LastAtMs 应该记录上一次落盘的时刻")
	}
}

// 空库启动时的 checkpoint 一张表都没写，不能污染耗时样本，也不能算一次落盘。
func TestCheckpointProfileSkipsEmptyRun(t *testing.T) {
	d := newTestDB(t)
	// 新建的库在 Open 阶段跑过一次 checkpoint，但那时没有任何表可写。
	cp := d.Store.CheckpointProfile()
	if cp.LastReason != "" {
		t.Errorf("空库不该记录触发原因，实际 %q", cp.LastReason)
	}
	if cp.P50Ns != 0 {
		t.Errorf("空库不该有耗时样本，实际 p50 = %d ns", cp.P50Ns)
	}
}

// 表大小要能按文件真实大小和行数报出来。
func TestTableSizes(t *testing.T) {
	d := newTestDB(t)
	run(t, d, "c1", "CREATE TABLE sz_probe (id int primary key, v text)")
	for i := 1; i <= 40; i++ {
		run(t, d, "c1", fmt.Sprintf("INSERT INTO sz_probe VALUES (%d, 'payload')", i))
	}
	run(t, d, "c1", "CHECKPOINT")

	var found *storage.TableSize
	for _, ts := range d.Store.TableSizes() {
		if ts.Name == "sz_probe" {
			v := ts
			found = &v
			break
		}
	}
	if found == nil {
		t.Fatal("TableSizes 里没有 sz_probe")
	}
	if found.Rows != 40 {
		t.Errorf("行数 = %d，期望 40", found.Rows)
	}
	if found.Bytes <= 0 {
		t.Errorf("落盘后字节数应该大于 0，实际 %d", found.Bytes)
	}

	// Monitor 里也要带上这些数据，否则前端拿不到
	m := d.Monitor(d.Timeouts())
	if m.IO.TotalBytes <= 0 {
		t.Errorf("Monitor 的 IO.TotalBytes = %d，应该大于 0", m.IO.TotalBytes)
	}
	if len(m.IO.Tables) == 0 {
		t.Error("Monitor 的 IO.Tables 为空")
	}
	if m.IO.Fsync.Runs == 0 {
		t.Error("Monitor 里应该有 fsync 次数")
	}
	if m.Proc.NumCPU == 0 {
		t.Error("Monitor 里没有进程数据")
	}
}
