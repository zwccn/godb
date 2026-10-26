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

// noteCheckpointLocked 在 checkpointLocked 成功后调用（已持 CommitMu）。
func (s *Store) noteCheckpointLocked() {
	s.cpLastNs.Store(time.Now().UnixNano())
	s.cpSince.Store(0)
	s.cpRuns.Add(1)
}

// needCheckpointLocked 判断「提交次数 / WAL 大小」是否已经该落盘了。
//
// 时间条件不在这里判断：它由后台 loop 负责，这样提交路径上不会因为
// 「刚好到时间」而突然多出一次几十毫秒的快照写。
func (s *Store) needCheckpointLocked() bool {
	if n := s.cpCommits.Load(); n > 0 && s.cpSince.Load() >= n {
		return true
	}
	if b := s.cpWALBytes.Load(); b > 0 && s.wal != nil && s.wal.LiveBytes() >= b {
		return true
	}
	return false
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
	if err := s.checkpointLocked(); err != nil {
		// 落盘失败不能让后台 goroutine 退出，也不能更新时间和计数：
		// 下一轮还会重试，同时把错误打到日志里。
		log.Printf("自动落盘失败: %v", err)
		return
	}
	// 必须记账：否则 cpLastNs 不前进、cpSince 不清零，
	// 后台会每秒都判定「已经到点了」并重复写快照。
	s.noteCheckpointLocked()
}
