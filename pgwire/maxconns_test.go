package pgwire

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"godb/db"
	"godb/lock"
)

// startLimited 起一个带连接上限的 Server。
func startLimited(t *testing.T, limit int) string {
	t.Helper()
	d, err := db.Open(t.TempDir(), db.Options{Lock: lock.Options{
		Timeout: 500 * time.Millisecond, Retry: 10 * time.Millisecond,
	}})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{
		DB:       d,
		Logf:     func(string, ...interface{}) {},
		MaxConns: func() int { return limit },
	}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// tryDial 连上去并读第一条消息。返回 (sawAuthOK, 错误文本)。
// 被拒时服务端回 FATAL 53300，客户端拿到的是 'E' 而不是 AuthenticationOk。
func tryDial(t *testing.T, addr string) (net.Conn, bool, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	body := binary.BigEndian.AppendUint32(nil, protoVersion3)
	body = append(body, []byte("user\x00zwccn\x00")...)
	body = append(body, 0)
	startup := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	startup = append(startup, body...)
	if _, err := conn.Write(startup); err != nil {
		conn.Close()
		t.Fatalf("write startup: %v", err)
	}

	var hdr [5]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		conn.Close()
		return nil, false, "read: " + err.Error()
	}
	n := int(binary.BigEndian.Uint32(hdr[1:]))
	msgBody := make([]byte, n-4)
	if _, err := io.ReadFull(conn, msgBody); err != nil {
		conn.Close()
		return nil, false, "read body: " + err.Error()
	}
	return conn, hdr[0] == 'R', string(msgBody)
}

// 连接数到上限后，新连接必须被 53300 拒掉；关掉一条之后又能连上
// （计数要真的回退，不能只增不减 —— 那会越用越少）。
func TestMaxConnsRejectsExtra(t *testing.T) {
	addr := startLimited(t, 2)

	c1, ok1, e1 := tryDial(t, addr)
	if !ok1 {
		t.Fatalf("第 1 条连接应当成功: %s", e1)
	}
	defer c1.Close()

	c2, ok2, e2 := tryDial(t, addr)
	if !ok2 {
		t.Fatalf("第 2 条连接应当成功: %s", e2)
	}
	defer c2.Close()

	c3, ok3, e3 := tryDial(t, addr)
	if ok3 {
		c3.Close()
		t.Fatal("上限是 2，第 3 条连接不该通过认证")
	}
	if c3 != nil {
		c3.Close()
	}
	// 必须是 53300（too_many_connections），不是别的错误码 ——
	// 客户端靠这个码判断「服务端忙、该退避重试」，而不是「连错地方了」。
	if !strings.Contains(e3, "53300") {
		t.Errorf("第 3 条连接的错误里应当有 53300，实际: %q", e3)
	}
	if !strings.Contains(e3, "too many clients") {
		t.Errorf("错误文案应当是 too many clients，实际: %q", e3)
	}

	// 关掉一条 → 让出一个槽位 → 新连接应当能进来。
	// handle() 的 defer 里减计数，是异步的，所以轮询等一会儿（最多 2 秒）。
	c2.Close()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr string
	for time.Now().Before(deadline) {
		c4, ok, e := tryDial(t, addr)
		if ok {
			c4.Close()
			return
		}
		lastErr = e
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("关掉一条连接后仍然连不上（计数没回退？）: %s", lastErr)
}

// 上限为 0（或不设 MaxConns）应当完全不限制 —— 这是热切换关掉限制的路径，
// 不能因为「0」被当成「不许连」而把所有人都挡在外面。
func TestMaxConnsZeroMeansUnlimited(t *testing.T) {
	addr := startLimited(t, 0)
	conns := make([]net.Conn, 0, 20)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 20; i++ {
		c, ok, e := tryDial(t, addr)
		if !ok {
			t.Fatalf("第 %d 条连接被拒了（上限为 0 应当是不限）: %s", i+1, e)
		}
		conns = append(conns, c)
	}
}
