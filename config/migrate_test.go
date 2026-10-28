package config

import "testing"

// v1 配置文件（早期版本生成）升级到最新版时：并发上限要补齐成 32/256，
// 但等锁 / 排队超时不能再被固化回 3 秒 —— v3 起出厂默认是「不限」。
func TestMigrateV1ToLatest(t *testing.T) {
	// 这是真实生产文件的形状：有 version=1、没有 performance、没有 timeouts。
	c := Config{
		Version: 1,
		Storage: Storage{CheckpointCommits: 128, CheckpointSeconds: 60, CheckpointWALMB: 64},
		Network: Network{HTTPPort: 8080, PGEnabled: true, PGPort: 6432},
		Auth:    Auth{User: "godb"},
	}
	c.Normalize()

	if c.Version != Version {
		t.Errorf("版本 = %d，期望 %d", c.Version, Version)
	}
	if c.Timeouts.LockWaitSec != 0 {
		t.Errorf("等锁超时 = %d，期望 0（v3 起默认不限）", c.Timeouts.LockWaitSec)
	}
	if c.Timeouts.QueueWaitSec != 0 {
		t.Errorf("排队超时 = %d，期望 0（v3 起默认不限）", c.Timeouts.QueueWaitSec)
	}
	if c.Performance.MaxConcurrent != 32 || c.Performance.MaxQueued != 256 {
		t.Errorf("并发上限 = %d/%d，期望 32/256",
			c.Performance.MaxConcurrent, c.Performance.MaxQueued)
	}
	// 慢 SQL / 慢事务是新增的保护，默认不限
	if c.Timeouts.SlowSQLSec != 0 || c.Timeouts.SlowTxIdleSec != 0 {
		t.Errorf("慢SQL/慢事务超时应默认为 0，实际 %d/%d",
			c.Timeouts.SlowSQLSec, c.Timeouts.SlowTxIdleSec)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("迁移后的配置不合法: %v", err)
	}
}

// 已经写过 performance 的 v1 文件（waitTimeoutMs=5000）要按毫秒换算成秒。
func TestMigrateV1WaitTimeout(t *testing.T) {
	c := Config{
		Version:     1,
		Performance: Performance{MaxConcurrent: 8, MaxQueued: 64, WaitTimeoutMs: 5000},
	}
	c.Normalize()
	if c.Timeouts.QueueWaitSec != 5 {
		t.Errorf("排队超时 = %d，期望 5", c.Timeouts.QueueWaitSec)
	}
	if c.Performance.MaxConcurrent != 8 || c.Performance.MaxQueued != 64 {
		t.Errorf("不该覆盖用户已设的并发上限：%+v", c.Performance)
	}
	if c.Performance.WaitTimeoutMs != 0 {
		t.Errorf("废弃字段应清零，实际 %d", c.Performance.WaitTimeoutMs)
	}
}

// v2 → v3：v2 的出厂默认 3 秒会原样写进文件，和「用户手设 3 秒」分不开。
// 约定是：恰好等于 3 的视为没动过，迁成「不限」；其它值保留。
func TestMigrateV2Timeouts(t *testing.T) {
	c := Config{
		Version:     2,
		Performance: Performance{MaxConcurrent: 32, MaxQueued: 256},
		Timeouts:    Timeouts{LockWaitSec: 3, QueueWaitSec: 3},
	}
	c.Normalize()
	if c.Version != Version {
		t.Errorf("版本 = %d，期望 %d", c.Version, Version)
	}
	if c.Timeouts.LockWaitSec != 0 || c.Timeouts.QueueWaitSec != 0 {
		t.Errorf("v2 的出厂 3 秒应迁成不限，实际 %d/%d",
			c.Timeouts.LockWaitSec, c.Timeouts.QueueWaitSec)
	}

	// 用户自己设的值不能被这次迁移抹掉。
	c = Config{
		Version:     2,
		Performance: Performance{MaxConcurrent: 32, MaxQueued: 256},
		Timeouts:    Timeouts{LockWaitSec: 30, QueueWaitSec: 5},
	}
	c.Normalize()
	if c.Timeouts.LockWaitSec != 30 || c.Timeouts.QueueWaitSec != 5 {
		t.Errorf("用户显式设置的值被改写了：%+v", c.Timeouts)
	}
}

// 已经是最新版的配置不能再被迁移逻辑改写：用户把超时显式设成 0（不限）
// 是一个合法选择，重启后必须还是 0。
func TestLatestConfigKeepsExplicitZero(t *testing.T) {
	c := Config{
		Version:     Version,
		Performance: Performance{MaxConcurrent: 16, MaxQueued: 32},
		Timeouts:    Timeouts{SlowSQLSec: 0, SlowTxIdleSec: 0, LockWaitSec: 0, QueueWaitSec: 0},
	}
	c.Normalize()
	if c.Timeouts.LockWaitSec != 0 || c.Timeouts.QueueWaitSec != 0 {
		t.Errorf("最新版的「不限」被改写了：%+v", c.Timeouts)
	}
	if c.Performance.MaxConcurrent != 16 {
		t.Errorf("最新版的并发上限被改写了: %d", c.Performance.MaxConcurrent)
	}
}

// 四个超时的取值域：0 = 不限，超过 24 小时必须报错。
func TestTimeoutsValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置不合法: %v", err)
	}
	c.Timeouts.SlowTxIdleSec = -1
	if err := c.Validate(); err == nil {
		t.Error("负值应该报错")
	}
	c = Default()
	c.Timeouts.QueueWaitSec = 86401
	if err := c.Validate(); err == nil {
		t.Error("超过 86400 秒应该报错")
	}
}
