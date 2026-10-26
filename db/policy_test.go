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

func TestCheckpointByTime(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等 2 秒")
	}
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Seconds: 1})
	run(t, d, "s", `CREATE TABLE pt (id SERIAL PRIMARY KEY, v INTEGER)`)
	run(t, d, "s", `INSERT INTO pt (v) VALUES (1)`)

	base := d.Store.CheckpointStatsNow().Runs
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if d.Store.CheckpointStatsNow().Runs > base {
			return // 成功
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等了 6 秒也没按时间落盘（runs 还是 %d）", base)
}

// 没有改动时不应该空转写快照。
func TestTimeCheckpointSkipsWhenIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("需要等 2 秒")
	}
	d := newTestDB(t)
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Seconds: 1})
	run(t, d, "s", `CREATE TABLE pi (id SERIAL PRIMARY KEY, v INTEGER)`)
	run(t, d, "s", `INSERT INTO pi (v) VALUES (1)`)
	run(t, d, "s", `CHECKPOINT`) // 把计数清零

	base := d.Store.CheckpointStatsNow().Runs
	time.Sleep(2200 * time.Millisecond)
	if got := d.Store.CheckpointStatsNow().Runs; got != base {
		t.Fatalf("没有改动也应该不动（runs %d → %d）", base, got)
	}
	// 计数也不该被别人无端清零
	if since := d.Store.CheckpointStatsNow().CommitsSince; since != 0 {
		t.Fatalf("空闲时提交计数应为 0，实际 %d", since)
	}
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
