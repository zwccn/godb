package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 取消要真的让长查询停下来，并且**把表锁还回去**。
//
// 这条链本来是断的：`sess.interrupt()` 会检查 cancel 标志，存储层游标每 1024 行
// 也会问一次，但 HTTP 请求被取消（关页面 / 前端 AbortController / 断网）时没人调
// `sess.Cancel()`。于是长查询一直跑到自然结束，而表级 2PL 意味着这期间整张表的
// 读写全堵着 —— 客户端能做的只有等。
func TestCancelStopsLongQueryAndReleasesLock(t *testing.T) {
	d := newTestDB(t)
	s := "cancel-setup"
	run(t, d, s, "CREATE TABLE big (id int4 primary key, v int4)")

	// 10 万行：够让下面那条「小数乘两次再求和」跑到几百毫秒；分批插，避免单条太大。
	const batch = 5000
	for base := 0; base < 100000; base += batch {
		var b strings.Builder
		b.WriteString("INSERT INTO big VALUES ")
		for i := 0; i < batch; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,%d)", base+i+1, base+i)
		}
		run(t, d, s, b.String())
	}

	// A 跑一条慢查询：每行两次 numeric 乘法 + 求和（10 万行约几百毫秒）。
	// 不用自连接 —— 那会先撞上「中间结果超过 200 万行」的兜底，测不到取消。
	sessA := d.Session("cancel-a")
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec(sessA, "SELECT sum(id * 1.5 * v + 1) FROM big")
		done <- err
	}()

	// 给它一点时间真正跑起来（游标已经推进若干批），再取消
	time.Sleep(60 * time.Millisecond)
	sessA.Cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("取消后应返回 ErrCanceled，实际: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 5 秒仍未停止 —— interrupt 没接上，长查询还在跑")
	}

	// 锁必须已经释放：另一个会话写同一张表不该等
	sessB := d.Session("cancel-b")
	start := time.Now()
	if _, err := d.Exec(sessB, "UPDATE big SET v = v WHERE id = 1"); err != nil {
		t.Fatalf("取消后另一个会话应能立刻写，实际: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("取消后写操作等了 %v —— 表锁没随取消释放", took)
	}

	// 会话不能因此变成废人：取消只针对那一次执行
	if _, err := d.Exec(sessA, "SELECT count(*) FROM big WHERE id = 1"); err != nil {
		t.Errorf("取消之后同一会话应能继续用，实际: %v", err)
	}
}
