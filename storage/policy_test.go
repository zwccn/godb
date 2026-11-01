package storage

// 「按时间落盘」这条策略的测试。
//
// 为什么在这个包而不是 db 包：判定完全由 tickCheckpoint() 里的三样东西决定 ——
// cpSeconds（阈值）、cpLastNs（上次落盘时刻）、cpSince（未落盘的提交数）。
// 直接从包内把它们摆好再调一次 tickCheckpoint()，判定逻辑就被完整覆盖了，
// 而且**一秒都不用等**。
//
// 原来的写法（db/policy_test.go）只能靠真实时间：后台 ticker 每秒一次、
// 阈值最小 1 秒，于是「验证到点会落盘」至少等 1 秒，「验证空闲不落盘」固定
// 睡 2.2 秒。等时间不只是慢 —— 机器一忙就可能等超时，红得莫名其妙。

import (
	"testing"
	"time"

	"godb/catalog"
	"godb/types"
)

// newPolicyStore 起一个只有一张表的库，写一行并提交（= 有未落盘的改动）。
func newPolicyStore(t *testing.T, name string) *Store {
	t.Helper()
	dir := t.TempDir()
	cat := catalog.New()
	tbl := &catalog.Table{Name: name, Columns: []*catalog.Column{
		catalog.NewColumn("id", types.Type{Kind: types.KInt, Name: "integer"}),
	}}
	if err := cat.Add(tbl); err != nil {
		t.Fatalf("add table: %v", err)
	}
	if err := cat.Save(dir); err != nil {
		t.Fatalf("save catalog: %v", err)
	}
	s, err := Open(dir, cat, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tx := s.Begin()
	if _, err := tx.Insert(name, types.Row{types.Int(1)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return s
}

// backdateCheckpoint 把「上次落盘时刻」拨到 d 之前。
//
// 这是本文件存在的关键：不改生产代码、不加测试专用 API，只把时钟往前推，
// 就能让「距上次落盘已过 N 秒」这个条件立刻成立。
func backdateCheckpoint(s *Store, d time.Duration) {
	s.cpLastNs.Store(time.Now().Add(-d).UnixNano())
}

// 到点了 + 有改动 → 落盘，并且把提交计数清零。
func TestTickCheckpointBySeconds(t *testing.T) {
	s := newPolicyStore(t, "pt")
	s.SetCheckpointPolicy(CheckpointPolicy{Seconds: 1})

	if since := s.CheckpointStatsNow().CommitsSince; since == 0 {
		t.Fatal("插入提交后应有未落盘的提交计数，否则这条用例什么也没验")
	}
	base := s.CheckpointStatsNow().Runs
	backdateCheckpoint(s, 2*time.Second)
	s.tickCheckpoint()

	after := s.CheckpointStatsNow()
	if after.Runs <= base {
		t.Fatalf("阈值 1 秒、上次落盘在 2 秒前、又有改动，应该落盘（runs %d → %d）",
			base, after.Runs)
	}
	if after.CommitsSince != 0 {
		t.Fatalf("落盘后提交计数应归零，实际 %d", after.CommitsSince)
	}
}

// 到点了但**没有改动** → 不落盘（不空转重写），并把「上次落盘时刻」刷新到现在。
//
// 后半句是这条路径的要点：如果不刷新时钟，等下次真有改动时，会因为「距上次
// 落盘已经很久」而立刻又写一次快照 —— 空闲越久，下一次改动越容易触发全量重写。
func TestTickCheckpointSkipsWhenIdle(t *testing.T) {
	s := newPolicyStore(t, "pi")
	s.SetCheckpointPolicy(CheckpointPolicy{Seconds: 1})

	// 显式落一次盘，把提交计数清零 —— 之后就没有任何未落盘的改动了。
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if since := s.CheckpointStatsNow().CommitsSince; since != 0 {
		t.Fatalf("显式落盘后计数应为 0，实际 %d", since)
	}
	base := s.CheckpointStatsNow().Runs

	backdateCheckpoint(s, 2*time.Second) // 就算「早该落了」也不许空转
	before := time.Now()
	s.tickCheckpoint()

	st := s.CheckpointStatsNow()
	if st.Runs != base {
		t.Fatalf("没有改动就不该落盘（runs %d → %d）", base, st.Runs)
	}
	if st.LastAt.Before(before) {
		t.Fatalf("空闲 tick 应把「上次落盘」刷新到现在（否则下次一有改动就立刻全量重写），实际还停在 %v",
			st.LastAt.Format("15:04:05.000"))
	}
}

// 阈值配置为 0 时，后台 ticker 什么都不做 —— 哪怕时钟已经很旧、也有改动。
func TestTickCheckpointSecondsZeroDisabled(t *testing.T) {
	s := newPolicyStore(t, "pd")
	s.SetCheckpointPolicy(CheckpointPolicy{}) // Seconds = 0
	base := s.CheckpointStatsNow().Runs
	backdateCheckpoint(s, time.Hour)
	s.tickCheckpoint()
	if got := s.CheckpointStatsNow().Runs; got != base {
		t.Fatalf("按时间落盘关掉后不该落盘（runs %d → %d）", base, got)
	}
}
