package web

import (
	"strings"
	"testing"
	"time"

	"godb/db"
	"godb/lock"
)

// 基准核心逻辑的轻量单测：把规模压到 2000 行，跑通全部 8 个场景，
// 只断言「不报错 + 吞吐为正 + 历史能读写」。真跑 100 万行的耗时不是单测该承担的。
func benchTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir(), db.Options{
		Lock: lock.Options{
			Timeout:        5 * time.Second,
			Retry:          5 * time.Millisecond,
			DeadlockDetect: true,
			DeadlockAfter:  500 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestBenchScenariosRun(t *testing.T) {
	benchScaleOverride = 2000 // 小规模跑通全链路
	defer func() { benchScaleOverride = 0 }()

	d := benchTestDB(t)
	s := New(d)

	st, ok := s.bench.start()
	if !ok {
		t.Fatal("start 应能启动（首次）")
	}
	// 同步跑（不 go），直接等结果
	s.runBench(st)

	if st.err != "" {
		t.Fatalf("runBench 报错：%s", st.err)
	}
	if st.result == nil {
		t.Fatal("result 为空")
	}
	if len(st.result.Scenarios) != 9 {
		t.Fatalf("应跑 9 个场景，实际 %d", len(st.result.Scenarios))
	}
	// 向量场景必须报出「几行 × 几维」：没有这个口径，数字和别的场景没法比。
	sawVec := false
	for _, sc := range st.result.Scenarios {
		if sc.Name != "向量 Top-K" {
			continue
		}
		sawVec = true
		if !strings.Contains(sc.Commit, "维") || !strings.Contains(sc.Commit, "行") {
			t.Fatalf("向量场景的 commit 应写明行数与维度，实际 %q", sc.Commit)
		}
	}
	if !sawVec {
		t.Fatal("缺少「向量 Top-K」场景")
	}
	for _, sc := range st.result.Scenarios {
		if sc.Error != "" {
			t.Fatalf("场景 %q 报错：%s", sc.Name, sc.Error)
		}
		if sc.PerSec <= 0 {
			t.Fatalf("场景 %q 吞吐应 > 0，实际 %v", sc.Name, sc.PerSec)
		}
	}
	// 批量插入应扫出 4 个档位
	if len(st.result.Batches) != len(benchBatchSizes) {
		t.Fatalf("批量插入档位应 %d 个，实际 %d", len(benchBatchSizes), len(st.result.Batches))
	}
	// 逐条提交场景必须真的报出 fsync 次数：这一项的意义就是「落盘 QPS」，
	// 文案退化成静态字符串时，数字就悄悄失去依据了。
	for _, sc := range st.result.Scenarios {
		if sc.Name == "逐条提交插入" && !strings.Contains(sc.Commit, "fsync") {
			t.Fatalf("逐条提交插入应报实测 fsync 次数，实际 commit=%q", sc.Commit)
		}
	}
	// 测完即删
	for _, tb := range d.Tables() {
		if tb.Name == benchTable || tb.Name == benchCommitTable {
			t.Fatalf("基准表 %s 应已删除，但仍存在", tb.Name)
		}
	}

	// 落盘的那份基线必须标着「跑完了」。前端拿 scale 和各场景行数判断可比性，
	// finished 则是「这份记录完整」的凭据 —— 原来它在写盘之后才被 defer 置 true，
	// 于是磁盘上永远是 false：数据是对的，标记是错的。
	hist := s.loadBenchHistory()
	if hist == nil {
		t.Fatal("跑完后应落盘一份历史")
	}
	if !hist.Finished {
		t.Fatal("历史应标记 finished=true（写盘之前就要置上，不能等 defer）")
	}
	if hist.Scale != scale() || len(hist.Scenarios) != len(st.result.Scenarios) {
		t.Fatalf("历史与本次结果对不上：scale=%d 场景=%d", hist.Scale, len(hist.Scenarios))
	}
}

func TestBenchHistoryRoundTrip(t *testing.T) {
	benchScaleOverride = 500
	defer func() { benchScaleOverride = 0 }()

	d := benchTestDB(t)
	s := New(d)

	// 直接塞一份结果再读回，验证序列化往返（不真跑基准）
	res := &benchResult{Scale: 500, Version: "0.4.0", StartedAt: "2026-11-03 12:00:00"}
	res.Scenarios = []benchScene{{Name: "单条插入", PerSec: 123.4}}
	s.saveBenchHistory(res)

	got := s.loadBenchHistory()
	if got == nil {
		t.Fatal("历史应能读回")
	}
	if got.Scale != 500 || got.Version != "0.4.0" || len(got.Scenarios) != 1 || got.Scenarios[0].PerSec != 123.4 {
		t.Fatalf("历史往返不一致：%+v", got)
	}
}

func TestBenchSingleAtATime(t *testing.T) {
	d := benchTestDB(t)
	s := New(d)

	if _, ok := s.bench.start(); !ok {
		t.Fatal("首次 start 应成功")
	}
	// 第二次应被拒（已有任务在跑）
	if _, ok := s.bench.start(); ok {
		t.Fatal("第二次 start 应返回 false（同一时刻只跑一个）")
	}
}

// 取消路径也必须清掉基准表：cancel 是协作式的，可能发生在「灌数据」这一步之后、
// 场景循环之前 —— 不清理就会留一张完整的 __bench_items 在库里。
func TestBenchCancelCleansUp(t *testing.T) {
	benchScaleOverride = 500
	defer func() { benchScaleOverride = 0 }()

	d := benchTestDB(t)
	s := New(d)

	st, ok := s.bench.start()
	if !ok {
		t.Fatal("start 应成功")
	}
	st.mu.Lock()
	st.cancel = true // 先标记取消：runBench 灌完数据就会在循环前停下
	st.mu.Unlock()

	s.runBench(st)

	// 表必须已删（defer 清理）
	for _, tb := range d.Tables() {
		if tb.Name == benchTable {
			t.Fatalf("取消后基准表应已清理，但仍存在")
		}
	}
}
