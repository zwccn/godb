package storage

import (
	"log"
	"time"
)

// CheckpointPolicy 描述「什么时候落盘（写快照 + 轮转 WAL）」。
//
// 三条条件**任一**满足就触发，各自填 0 表示关闭该条：
//
//	Commits  每多少次提交
//	Seconds  每隔多少秒（有改动才写）
//	WALBytes WAL 增长到多少字节
//
// 它不影响提交时的 WAL fsync —— 那是持久性本身。
type CheckpointPolicy struct {
	Commits  int
	Seconds  int
	WALBytes int64
}

// DefaultPolicy 是出厂默认：128 次提交 / 60 秒 / 64 MB。
func DefaultPolicy() CheckpointPolicy {
	return CheckpointPolicy{Commits: 128, Seconds: 60, WALBytes: 64 << 20}
}

// CheckpointStats 是给设置页显示的落盘状态。
type CheckpointStats struct {
	Policy       CheckpointPolicy
	LastAt       time.Time
	CommitsSince int64
	Runs         int64
	WALBytes     int64
}

// SetCheckpointPolicy 热更新落盘策略。
//
// 只改「下次什么时候落盘」，不影响已经落盘的数据，所以可以在服务运行中调用。
func (s *Store) SetCheckpointPolicy(p CheckpointPolicy) {
	if p.Commits < 0 {
		p.Commits = 0
	}
	if p.Seconds < 0 {
		p.Seconds = 0
	}
	if p.WALBytes < 0 {
		p.WALBytes = 0
	}
	s.cpCommits.Store(int64(p.Commits))
	s.cpSeconds.Store(int64(p.Seconds))
	s.cpWALBytes.Store(p.WALBytes)
}

// CheckpointPolicyNow 返回当前生效的策略。
func (s *Store) CheckpointPolicyNow() CheckpointPolicy {
	return CheckpointPolicy{
		Commits:  int(s.cpCommits.Load()),
		Seconds:  int(s.cpSeconds.Load()),
		WALBytes: s.cpWALBytes.Load(),
	}
}

// CheckpointStatsNow 返回落盘状态快照。
func (s *Store) CheckpointStatsNow() CheckpointStats {
	st := CheckpointStats{
		Policy:       s.CheckpointPolicyNow(),
		CommitsSince: s.cpSince.Load(),
		Runs:         s.cpRuns.Load(),
	}
	if ns := s.cpLastNs.Load(); ns > 0 {
		st.LastAt = time.Unix(0, ns)
	}
	if s.wal != nil {
		st.WALBytes = s.wal.LiveBytes()
	}
	return st
}

// noteCheckpointLocked 在 checkpointBodyLocked 成功后调用（已持 CommitMu）。
//
// reason 回答「这一次为什么写盘」，tookNs 是整轮耗时（拍快照 + 写盘 + 收尾），
// bytes 是这一次实际写进磁盘的字节数。这三样是排查「提交为什么突然变慢」的
// 第一手材料：fsync 慢的那几秒，往往正好有一次大快照在写。
//
// wrote 是这次真正写了多少张表。为空时（启动时空库、或所有表都没加载）
// 只清零计数，不记耗时样本 —— 否则一次「什么也没写」的 0ns 会混进 p50，
// 让面板上的「快照 p50」永远是假的零点几毫秒。
func (s *Store) noteCheckpointLocked(reason string, tookNs, bytes int64, wrote int) {
	s.cpLastNs.Store(time.Now().UnixNano())
	s.cpSince.Store(0)
	if wrote <= 0 {
		// 一张表都没写：只清零计数（不然后台定时器会立刻再跑一次），
		// 不算「落过一次盘」。
		return
	}
	s.cpRuns.Add(1)
	if s.cpRing != nil {
		s.cpRing.add(tookNs)
	}
	s.cpDurNs.Store(tookNs)
	s.cpBytes.Store(bytes)
	s.cpStatMu.Lock()
	s.cpLastReason = reason
	if s.cpReasons == nil {
		s.cpReasons = map[string]int64{}
	}
	s.cpReasons[reason]++
	s.cpStatMu.Unlock()
}

// needCheckpointLocked 判断「现在该不该落盘」，并给出触发原因。
//
// 两条硬约束：提交次数攒够了 / WAL 涨到阈值了。后者无条件执行 ——
// 不落盘就没法轮转 WAL，日志会无限长。
//
// 提交次数这条还要再过一道「值不值得」：落盘是**整表重写**，只改一行就
// 重写 20 MB 的话写放大接近 2 万倍（实测）。所以大表要攒够比例、小表
// 随便写；攒着的这段时间数据在 WAL 里，崩溃恢复照样不丢，只是重启时要
// 多重放几条记录。
func (s *Store) needCheckpointLocked() (bool, string) {
	if b := s.cpWALBytes.Load(); b > 0 && s.wal != nil && s.wal.LiveBytes() >= b {
		return true, CPReasonWALMB
	}
	if n := s.cpCommits.Load(); n > 0 && s.cpSince.Load() >= n {
		if s.worthWritingLocked() {
			return true, CPReasonCommits
		}
	}
	return false, ""
}

const (
	// minCheckpointBytes 以下的小库不值得省这次写：写几十 KB 和写几 MB 不是一回事。
	minCheckpointBytes = 1 << 20
	// 大表要求「自上次落盘以来改过的行 >= 总行数的 1/10」才整表重写。
	dirtyRatioNum = 1
	dirtyRatioDen = 10
	// bytesPerRow 估算行在磁盘上的大小（实测 4 列约 51 字节），只用来判断值不值得写。
	bytesPerRow = 64
)

// worthWritingLocked 判断攒的改动是否值得为它重写整张表。
func (s *Store) worthWritingLocked() bool {
	var dirtyRows int64
	for key, d := range s.dirty {
		if d {
			dirtyRows += s.dirtyRows[key]
		}
	}
	if dirtyRows == 0 {
		return false // 一点改动都没有，没必要写
	}
	var totalRows int64
	for key, td := range s.Data {
		if td != nil && s.loaded[key] {
			totalRows += int64(td.SlotCount())
		}
	}
	if totalRows*bytesPerRow <= minCheckpointBytes {
		return true // 小库：直接写，省这点没意义
	}
	return dirtyRows*dirtyRatioDen >= totalRows*dirtyRatioNum
}

// resetCheckpointClock 把「上次落盘时间」设为现在（启动时用）。
func (s *Store) resetCheckpointClock() { s.cpLastNs.Store(time.Now().UnixNano()) }

// StartAutoCheckpoint 启动后台定时器，实现「每隔 N 秒落盘」这条。
//
// 幂等：重复调用只生效一次。Close 会停掉它。
func (s *Store) StartAutoCheckpoint() {
	s.cpOnce.Do(func() {
		s.cpStop = make(chan struct{})
		s.cpWG.Add(1)
		go func() {
			defer s.cpWG.Done()
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-s.cpStop:
					return
				case <-t.C:
					s.tickCheckpoint()
					// 顺手看一眼哪张表删出来的洞该收了（自己按 60 秒节流）
					s.tickAutoVacuum()
				}
			}
		}()
	})
}

// StopAutoCheckpoint 停掉后台定时器（Close 里会调用，也可以单独调）。
func (s *Store) StopAutoCheckpoint() {
	if s.cpStop == nil {
		return
	}
	select {
	case <-s.cpStop:
		// 已经关了
	default:
		close(s.cpStop)
	}
	s.cpWG.Wait()
	s.cpStop = nil
}

// tickCheckpoint 由后台定时器每秒调用一次。
func (s *Store) tickCheckpoint() {
	secs := s.cpSeconds.Load()
	if secs <= 0 {
		return
	}
	// 先拿 schema 读锁：分段 checkpoint 会在 CommitMu 之外读行数据，
	// 后台这条路径没有语句的读锁兜底，得自己保证写盘期间 DDL 不会改行。
	// 加锁顺序仍然是 SchemaMu → CommitMu，不会和 DDL 互等。
	s.SchemaMu.RLock()
	defer s.SchemaMu.RUnlock()
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	// 没有改动就不写：checkpoint 会重写有改动的表，空转只会白做功。
	if s.cpSince.Load() == 0 {
		s.cpLastNs.Store(time.Now().UnixNano())
		return
	}
	last := s.cpLastNs.Load()
	if last > 0 && time.Since(time.Unix(0, last)) < time.Duration(secs)*time.Second {
		return
	}
	// 到点了但改动太少（大表上只改了几行）就先攒着，等 WAL 涨到阈值再全量落盘。
	if !s.worthWritingLocked() {
		return
	}
	if _, err := s.tryCheckpointLocked(CPReasonSeconds); err != nil {
		// 落盘失败不能让后台 goroutine 退出，也不能更新时间和计数：
		// 下一轮还会重试，同时把错误打到日志里。
		log.Printf("自动落盘失败: %v", err)
	}
}
