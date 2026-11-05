// Package netio 统计网络连接上实际收发的字节数。
//
// 存在的理由只有一个：总览大屏要显示「带宽」。磁盘那边 storage/ioprofile.go
// 早就在记 WAL 写放大和 fsync 延迟，网络这边一直是空白 —— 所以「查一个大
// 结果集把链路打满」这种事，只能靠看客户端卡不卡来猜。
//
// 口径：**在 listener 层统计，也就是在 TLS 之外**。加密连接记的是上线缆的
// 字节数，那才是「带宽」；记明文长度会少算 TLS 记录头和 padding。
package netio

import (
	"net"
	"sync/atomic"
)

// Counter 是一对收发累计值。
//
// 用 atomic.Int64 结构体而不是裸 int64：它是数据面（每次 Read/Write）写、
// 监控面每秒读一次，加锁不值当。同步代价必须落到「几个原子加」的量级，
// 否则统计本身会拖慢查询。
type Counter struct {
	in  atomic.Int64
	out atomic.Int64
}

// AddIn 计入收到的字节数。
func (c *Counter) AddIn(n int) {
	if n > 0 {
		c.in.Add(int64(n))
	}
}

// AddOut 计入发出的字节数。
func (c *Counter) AddOut(n int) {
	if n > 0 {
		c.out.Add(int64(n))
	}
}

// Totals 返回累计收到、发出的字节数。只增不减：调用方靠两次采样求差
// 得到速率，中途清零会让速率算出一个负数。
func (c *Counter) Totals() (in, out int64) {
	return c.in.Load(), c.out.Load()
}

// PG 是 PostgreSQL 端口上的累计收发字节（进程级）。
//
// 为什么是包级全局而不是挂在某个 DB 上：带宽是「这台机器在往外吐多少数据」，
// 多库共用一个监听端口，分摊到库没有意义。也因为 db 包要读它、netsrv 要写它，
// 而 netsrv 已经 import db 了，只能挪到一个两边都依赖的叶包。
//
// 只统计 PG，**不统计 HTTP**：控制台自己每两秒轮询一次 /api/monitor，
// 算进去会得到一个恒定底噪，把真正在做的事埋掉。
var PG Counter

// countingConn 在每次读写返回时累计实际字节数。
//
// 只包 Read/Write/Close 这几件事，其余方法靠嵌入的 net.Conn 直通；
// 不实现 CloseWrite —— 现在没有 copy 两端半关闭的用法，多实现一个方法
// 就多一处要跟着协议交互验证的东西。
type countingConn struct {
	net.Conn
	c *Counter
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.c.AddIn(n)
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.c.AddOut(n)
	return n, err
}

type countingListener struct {
	net.Listener
	c *Counter
}

// Accept 返回包好的连接。包装失败不存在 —— 只是嵌一层接口。
func (l *countingListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		// 关监听时会走到这里（net.ErrClosed），原样返回，别包装一个半成品
		return nc, err
	}
	return &countingConn{Conn: nc, c: l.c}, nil
}

// Wrap 让从 ln 接受的所有连接都计入 c。
//
// 返回的是新 listener，Close 会透传到原来的那个 —— 热切换端口时
// netsrv 关的还是同一份底层监听，行为不变。
func Wrap(ln net.Listener, c *Counter) net.Listener {
	if ln == nil || c == nil {
		return ln
	}
	return &countingListener{Listener: ln, c: c}
}
