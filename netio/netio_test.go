package netio

import (
	"errors"
	"io"
	"net"
	"testing"
)

// 收和发分开算：查大结果集时发远大于收，收写入时反过来。
// 合成一个数就看不出「是在拖数据还是在灌数据」了。
// 用 net.Pipe 两端直接对接，注意 net.Pipe 的读端是同步的：这里不做并发，
// 所以只测「包一层之后字节数对不对」，走的仍是 Read/Write 两个方法。
func TestCountingConn(t *testing.T) {
	var c Counter
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wrapped := &countingConn{Conn: a, c: &c}

	// 对端写 → 这端读，算「收」
	go func() { b.Write([]byte("hello")) }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if in, out := c.Totals(); in != 5 || out != 0 {
		t.Fatalf("读一次之后 in=%d out=%d，期望 5 / 0", in, out)
	}

	// 这端写 → 对端读，算「发」。对端要先读，Pipe 的写才会返回。
	go func() { io.Copy(io.Discard, b) }()
	if _, err := wrapped.Write([]byte("worlds")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if in, out := c.Totals(); in != 5 || out != 6 {
		t.Fatalf("写一次之后 in=%d out=%d，期望 5 / 6", in, out)
	}
}

// listener 包装之后：Accept 出来的连接自动计数，Close 透传到原监听。
//
// 热切换端口时 netsrv 关的是返回的这个 listener，所以 Close 必须能传到
// 底层 —— 传不到就是「换了地址但旧端口还开着」。
func TestWrapListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var c Counter
	wln := Wrap(ln, &c)

	done := make(chan error, 1)
	go func() {
		nc, aerr := wln.Accept()
		if aerr != nil {
			done <- aerr
			return
		}
		defer nc.Close()
		buf := make([]byte, 4)
		if _, rerr := io.ReadFull(nc, buf); rerr != nil {
			done <- rerr
			return
		}
		nc.Write([]byte("pong"))
		done <- nil
	}()

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()
	nc.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(nc, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("accept 侧: %v", err)
	}

	// 客户端发了 4 字节、收到 4 字节；服务端相反。
	if in, out := c.Totals(); in != 4 || out != 4 {
		t.Fatalf("in=%d out=%d，期望 4 / 4", in, out)
	}

	// Close 必须透传：关掉包装层之后原监听也跟着关。
	if err := wln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("底层监听还活着：%v", err)
	}
}

// Accept 失败时原样返回，不能包装一个半成品连接。
func TestWrapListenerAcceptError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	wln := Wrap(ln, &Counter{})
	ln.Close()
	nc, err := wln.Accept()
	if err == nil {
		if nc != nil {
			nc.Close()
		}
		t.Fatal("监听已关闭，Accept 应当报错")
	}
	if nc != nil {
		t.Fatalf("出错时不该返回连接，拿到 %v", nc)
	}
}

// 计数器只增不减：调用方靠两次采样求差算速率，中途清零会算出负数。
func TestCounterMonotonic(t *testing.T) {
	var c Counter
	for i := 0; i < 100; i++ {
		c.AddIn(7)
		c.AddOut(3)
	}
	in1, out1 := c.Totals()
	if in1 != 700 || out1 != 300 {
		t.Fatalf("in=%d out=%d，期望 700 / 300", in1, out1)
	}
	// 负数和零不该动累计值（Read/Write 返回 -1 是不可能的，但接口没约束）
	c.AddIn(-5)
	c.AddOut(0)
	if in2, out2 := c.Totals(); in2 != in1 || out2 != out1 {
		t.Fatalf("负数/零被计进去了：%d / %d", in2, out2)
	}
}
