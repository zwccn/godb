package netsrv

import (
	"net"
	"strconv"
	"testing"
)

// 客户端把 localhost 解析成 ::1 时，如果只监听 127.0.0.1，Windows 上要等
// 约 2 秒才回落到 IPv4 —— Navicat 每个操作新开连接，于是处处慢 2~3 秒。
// 这里锁住「本机回环地址要同时占 IPv4 和 IPv6」这个行为。
func TestListenLoopbackCoversBothFamilies(t *testing.T) {
	// 拿一个空闲端口：先绑 127.0.0.1:0，读出端口后立刻释放，再交给被测函数
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("拿空闲端口失败: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	lns, err := listenLoopback(addr)
	if err != nil {
		t.Fatalf("listenLoopback(%s): %v", addr, err)
	}
	defer closeAll(lns)

	if len(lns) < 2 {
		t.Skipf("这台机器没有 IPv6 回环，跳过（拿到 %d 个 listener）", len(lns))
	}

	// 两个族都要能连上：客户端先试 ::1 时必须直接成功，而不是等超时
	for _, target := range []string{"127.0.0.1", "::1"} {
		c, err := net.Dial("tcp", net.JoinHostPort(target, strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("连 %s 失败: %v", target, err)
		}
		c.Close()
	}
}

// 通配地址（0.0.0.0 / 空主机）不该被当成回环再加一个 listener：
// Go 的 tcp 监听本身已经是双栈的，重复绑定会失败。
func TestListenLoopbackWildcardSingle(t *testing.T) {
	if isLoopbackHost("") || isLoopbackHost("0.0.0.0") || isLoopbackHost("192.168.1.5") {
		t.Fatal("通配/普通地址不该被判成回环")
	}
	for _, h := range []string{"127.0.0.1", "localhost", "::1", "127.0.0.2"} {
		if !isLoopbackHost(h) {
			t.Fatalf("%s 应该判成回环", h)
		}
	}
}
