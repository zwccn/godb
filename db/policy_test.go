package db

import (
	"fmt"
	"testing"
	"time"

	"godb/storage"
)

// 三条落盘条件各自都能触发，这是「设置页改完真的有用」的回归。
func TestCheckpointByCommitCount(t *testing.T) {
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Commits: 3})
	run(t, d, "s", `CREATE TABLE pc (id SERIAL PRIMARY KEY, v INTEGER)`)

	base := d.Store.CheckpointStatsNow().Runs
	for i := 0; i < 3; i++ {
		run(t, d, "s", fmt.Sprintf(`INSERT INTO pc (v) VALUES (%d)`, i))
	}
	after := d.Store.CheckpointStatsNow()
	if after.Runs <= base {
		t.Fatalf("3 次提交后应该落盘一次（runs %d → %d）", base, after.Runs)
	}
	if after.CommitsSince != 0 {
		t.Fatalf("落盘后计数应归零，实际 %d", after.CommitsSince)
	}
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM pc`), []string{"3"})
}

func TestCheckpointDisabled(t *testing.T) {
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{})
	run(t, d, "s", `CREATE TABLE pd (id SERIAL PRIMARY KEY, v INTEGER)`)

	base := d.Store.CheckpointStatsNow().Runs
	for i := 0; i < 5; i++ {
		run(t, d, "s", fmt.Sprintf(`INSERT INTO pd (v) VALUES (%d)`, i))
	}
	after := d.Store.CheckpointStatsNow()
	if after.Runs != base {
		t.Fatalf("三条都关闭时不应该自动落盘（runs %d → %d）", base, after.Runs)
	}
	if after.CommitsSince != 5 {
		t.Fatalf("提交计数应累计到 5，实际 %d", after.CommitsSince)
	}
}

func TestCheckpointByWALSize(t *testing.T) {
	d := newTestDB(t)
	// 阈值设成 1 字节：任何一次提交都会让 WAL 超过它
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{WALBytes: 1})
	run(t, d, "s", `CREATE TABLE pw (id SERIAL PRIMARY KEY, v INTEGER)`)

	base := d.Store.CheckpointStatsNow().Runs
	run(t, d, "s", `INSERT INTO pw (v) VALUES (1)`)
	after := d.Store.CheckpointStatsNow()
	if after.Runs <= base {
		t.Fatalf("WAL 超过阈值后应该落盘（runs %d → %d）", base, after.Runs)
	}
	// 轮转后 WAL 只剩一个文件头
	if after.WALBytes > 64 {
		t.Fatalf("落盘后 WAL 应该被轮转，实际还有 %d 字节", after.WALBytes)
	}
}

// 后台定时器确实接着「按秒落盘」这条。
//
// 这是本文件里唯一必须等真实时间的用例（约 1 秒）：它验的是「ticker 被启动了」
// 这件接线事实，绕不开。判定逻辑本身 —— 到点才写、空闲不写、阈值 0 时不动 ——
// 在 storage/policy_test.go 里直接拨 cpLastNs 覆盖，一秒都不用等。
//
// 之所以这么拆：原来的两条用例靠「等 ticker」来验判定，于是空闲那条固定睡
// 2.2 秒、到点那条最多等 6 秒；机器一忙还可能等超时，红得莫名其妙。
func TestAutoCheckpointTickerWired(t *testing.T) {
	if testing.Short() {
		t.Skip("要等一次 ticker（约 1 秒）")
	}
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Seconds: 1})
	run(t, d, "s", `CREATE TABLE pt (id SERIAL PRIMARY KEY, v INTEGER)`)
	run(t, d, "s", `INSERT INTO pt (v) VALUES (1)`)

	base := d.Store.CheckpointStatsNow().Runs
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.Store.CheckpointStatsNow().Runs > base {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("3 秒内后台 ticker 一次都没落盘（runs 还是 %d）——检查 Open 之后有没有调 StartAutoCheckpoint", base)
}

func TestCheckpointPolicyHotUpdate(t *testing.T) {
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Commits: 10, Seconds: 5, WALBytes: 1 << 20})
	got := d.Store.CheckpointPolicyNow()
	if got.Commits != 10 || got.Seconds != 5 || got.WALBytes != 1<<20 {
		t.Fatalf("策略没生效: %+v", got)
	}
	// 负数应当被夹成 0
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Commits: -5, Seconds: -1, WALBytes: -9})
	got = d.Store.CheckpointPolicyNow()
	if got.Commits != 0 || got.Seconds != 0 || got.WALBytes != 0 {
		t.Fatalf("负数应被夹成 0: %+v", got)
	}
}
