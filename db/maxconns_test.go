package db

import (
	"fmt"
	"testing"

	"godb/config"
	"godb/syscat"
)

// 连接上限是**推导值**：并发数 + 排队上限。
//
// 为什么不允许单独配一个数：一个连接同一时刻只执行一条 SQL，所以第
// (并发 + 排队 + 1) 个连接必然既拿不到执行槽、又排不进队。单独配就会出现
// 「排队 1 万、连接 100」——那 1 万里的一大截永远用不到。
func TestMaxConnsDerived(t *testing.T) {
	cases := []struct {
		conc, queued, want int
		why                string
	}{
		{32, 10000, 10032, "出厂默认"},
		{0, 10000, 0, "并发不限 → 连接也不限"},
		{32, 0, 0, "排队不限 → 连接也不限"},
		{0, 0, 0, "两个都不限"},
		{8, 8, 16, "小配置"},
	}
	for _, c := range cases {
		p := config.Performance{MaxConcurrent: c.conc, MaxQueued: c.queued}
		if got := p.MaxConns(); got != c.want {
			t.Errorf("MaxConcurrent=%d MaxQueued=%d → MaxConns()=%d，期望 %d（%s）",
				c.conc, c.queued, got, c.want, c.why)
		}
	}
}

// pg_settings 里的 max_connections 必须报真实值。
//
// 以前这里是硬编码的 "100"，而服务端根本不限 —— 实测连 150+ 都能成功。
// 客户端（psql 的 psqlrc、JDBC 连接池、Navicat）读到 100 会按 100 配池子，
// 声明的参数和真实行为对不上，这种错不报错、只让人按错的数去规划。
func TestMaxConnectionsReported(t *testing.T) {
	d := newTestDB(t)
	s := "maxconn"

	read := func() string {
		res := run(t, d, s, `SELECT setting FROM pg_settings WHERE name = 'max_connections'`)
		if len(res) == 0 || len(res[0].Rows) == 0 {
			t.Fatalf("pg_settings 里没有 max_connections")
		}
		return fmt.Sprint(res[0].Rows[0][0])
	}

	// 未设置时是 0（= 不限），和本库其它配置的约定一致
	syscat.SetMaxConns(0)
	if got := read(); got != "0" {
		t.Errorf("未限制时 max_connections = %q，期望 \"0\"", got)
	}

	// 设了就跟上（启动时 main、保存配置时 web 都会调这个）
	syscat.SetMaxConns(10032)
	if got := read(); got != "10032" {
		t.Errorf("限制 10032 时 max_connections = %q，期望 \"10032\"", got)
	}

	// 关掉（0）要能还原，不能只增不减
	syscat.SetMaxConns(0)
	if got := read(); got != "0" {
		t.Errorf("改回不限后 max_connections = %q，期望 \"0\"", got)
	}
}

// SHOW max_connections 和 pg_settings 必须是**同一个数**。
//
// 这两条走的是完全不同的两条路：SHOW 在 exec/compat.go 的 settingValue，
// pg_settings 在 syscat/rows.go 的 settingRows。以前两处各写死一个 "100"，
// 改了一处忘了另一处，客户端就会看到「表里 10032、SHOW 里 100」——
// 信哪个都可能错，而且这种不一致不会报任何错。
func TestMaxConnectionsSameInShowAndTable(t *testing.T) {
	d := newTestDB(t)
	s := "maxconn-show"

	syscat.SetMaxConns(4321)
	defer syscat.SetMaxConns(0)

	show := run(t, d, s, `SHOW max_connections`)
	fromTable := run(t, d, s, `SELECT setting FROM pg_settings WHERE name = 'max_connections'`)

	gotShow := fmt.Sprint(show[0].Rows[0][0])
	gotTable := fmt.Sprint(fromTable[0].Rows[0][0])
	if gotShow != "4321" || gotTable != "4321" {
		t.Errorf("SHOW = %q、pg_settings = %q，两者都该是 \"4321\"", gotShow, gotTable)
	}
}
