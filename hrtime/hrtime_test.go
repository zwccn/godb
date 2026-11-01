package hrtime

import (
	"testing"
	"time"
)

// waitActive 等后台标定完成（最多 8 秒），返回状态。
func waitActive(t *testing.T) Status {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st := StatusOf()
		if st.Active || (st.Reason != "" && st.Reason != "正在标定 TSC 频率…") {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	return StatusOf()
}

// 标定完成后：连续读数必须单调、不能倒退，也不能给出负数。
func TestHighResMonotonic(t *testing.T) {
	if !platformSupported() {
		t.Skip("当前平台没有 TSC 实现")
	}
	Init(true)
	defer Init(false)

	st := waitActive(t)
	if !st.Active {
		t.Fatalf("标定没成功：%s", st.Reason)
	}
	if st.FreqHz < 1e9 || st.FreqHz > 1e11 {
		t.Fatalf("频率不合理：%d Hz", st.FreqHz)
	}

	s := Now()
	if !s.tsc {
		t.Fatal("激活后 Now() 应该返回 TSC 采样")
	}
	prev := s.tick
	for i := 0; i < 100000; i++ {
		cur, ok := readTSC()
		if !ok {
			t.Fatal("读不到 TSC")
		}
		if cur < prev {
			t.Fatalf("TSC 倒退了 %d -> %d", prev, cur)
		}
		prev = cur
	}
	if Since(s) < 0 {
		t.Fatal("Since 不该返回负数")
	}
}

// 精度：100ms 的等待，用 TSC 量出来的误差应该远小于旧的 219µs 刻度。
func TestHighResAccuracy(t *testing.T) {
	if !platformSupported() {
		t.Skip("当前平台没有 TSC 实现")
	}
	Init(true)
	defer Init(false)
	if st := waitActive(t); !st.Active {
		t.Skipf("标定没成功：%s", st.Reason)
	}

	var worst int64
	for i := 0; i < 5; i++ {
		s := Now()
		time.Sleep(100 * time.Millisecond)
		d := Since(s)
		off := d - int64(100*time.Millisecond)
		if off < 0 {
			off = -off
		}
		if off > worst {
			worst = off
		}
	}
	// 允许 5ms：time.Sleep 本身就不准，这里验的是「标定没有离谱」，
	// 不是「sleep 精确」。旧的 219µs 刻度下，100ms 的测量误差也在这一档，
	// 所以真正能区分的是下面的 Granularity。
	if worst > 5*int64(time.Millisecond) {
		t.Fatalf("100ms 测量偏差 %v，标定可能不对", time.Duration(worst))
	}
	if g := Granularity(); g != time.Nanosecond {
		t.Fatalf("激活后分辨率应为 1ns，实际 %v", g)
	}
}

// 关掉之后必须干净地退回 time.Now，而不是留着半套状态。
func TestDisableFallsBack(t *testing.T) {
	Init(false)
	st := StatusOf()
	if st.Active {
		t.Fatal("关掉开关后不该还在用 TSC")
	}
	if st.Reason == "" {
		t.Fatal("关掉后应给出原因，设置页要显示")
	}
	s := Now()
	if s.tsc {
		t.Fatal("关掉后 Now() 不该返回 TSC 采样")
	}
	time.Sleep(20 * time.Millisecond)
	if d := Since(s); d <= 0 {
		t.Fatalf("回退到 time.Now 后 Since 应该正常，实际 %d", d)
	}
}

// 中途从 TSC 切回 time.Now：切换前拿到的 Stamp 仍按 TSC 结算，不能算错。
func TestStampKeepsItsSource(t *testing.T) {
	if !platformSupported() {
		t.Skip("当前平台没有 TSC 实现")
	}
	Init(true)
	defer Init(false)
	if st := waitActive(t); !st.Active {
		t.Skipf("标定没成功：%s", st.Reason)
	}

	old := Now()
	if !old.tsc {
		t.Fatal("应为 TSC 采样")
	}
	SetEnabled(false)
	time.Sleep(50 * time.Millisecond)
	// 切换后新采样走墙钟
	if Now().tsc {
		t.Fatal("关掉后不该再产生 TSC 采样")
	}
	// 但旧的那个仍按 TSC 结算，且不能是离谱值
	d := Since(old)
	if d < 40*int64(time.Millisecond) || d > 200*int64(time.Millisecond) {
		t.Fatalf("混用两种时钟：旧 Stamp 结算出 %v", time.Duration(d))
	}
}
