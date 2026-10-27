package db

import (
	"fmt"
	"testing"

	"godb/syscat"
)

// pg_operator 以前是空表，客户端索引页里「操作符类 / 操作符」相关面板都是空的。
func TestOperatorCatalog(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	res := run(t, d, s, `SELECT count(*) FROM pg_catalog.pg_operator`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got == "0" {
		t.Fatal("pg_operator 不该是空表")
	}

	// int4 的等号：oid 96、oprcode int4eq，客户端靠这两个值做 join 与显示
	res = run(t, d, s, `
		SELECT o.oid, o.oprname, o.oprcode, l.typname, r.typname
		FROM pg_catalog.pg_operator o
		JOIN pg_catalog.pg_type l ON l.oid = o.oprleft
		JOIN pg_catalog.pg_type r ON r.oid = o.oprright
		WHERE o.oid = 96`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应查到 int4 等号，实际 %d 行", len(res[0].Rows))
	}
	row := res[0].Rows[0]
	if fmt.Sprint(row[1]) != "=" || fmt.Sprint(row[2]) != "int4eq" {
		t.Fatalf("操作符信息不对: %v", row)
	}
	if fmt.Sprint(row[3]) != "int4" || fmt.Sprint(row[4]) != "int4" {
		t.Fatalf("左右类型应是 int4: %v", row)
	}

	// 操作符类要能在 pg_operator 里找到对应类型的等号（索引页的常见 join）
	res = run(t, d, s, `
		SELECT count(*) FROM pg_catalog.pg_opclass oc
		JOIN pg_catalog.pg_operator op ON op.oprleft = oc.opcintype AND op.oprname = '='
		WHERE oc.opcdefault`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got == "0" {
		t.Fatal("默认操作符类应该都能对上等号操作符")
	}
}

// pg_stat_activity 以前是一行写死的假数据，现在是真实连接注册表。
func TestActivityTracksConnections(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	// 没有连接时是空集（PostgreSQL 也不给自己伪造一行）
	res := run(t, d, s, `SELECT count(*) FROM pg_catalog.pg_stat_activity`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("空库应有 0 条会话，实际 %s", got)
	}

	pid, unregister := syscat.RegisterActivity(syscat.Activity{User: "tester", DB: "godb"})
	defer unregister()

	res = run(t, d, s, `SELECT pid, usename, state, query FROM pg_catalog.pg_stat_activity`)
	if len(res[0].Rows) != 1 {
		t.Fatalf("应看到 1 条会话，实际 %d", len(res[0].Rows))
	}
	if got := fmt.Sprint(res[0].Rows[0][0]); got != fmt.Sprint(pid) {
		t.Fatalf("pid = %s，期望 %d", got, pid)
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "tester" {
		t.Fatalf("usename = %s，期望 tester", got)
	}

	// 状态更新后立刻可见
	syscat.SetActivityState(pid, "active", "SELECT 1")
	res = run(t, d, s, `SELECT state, query FROM pg_catalog.pg_stat_activity`)
	if fmt.Sprint(res[0].Rows[0][0]) != "active" || fmt.Sprint(res[0].Rows[0][1]) != "SELECT 1" {
		t.Fatalf("会话状态没更新: %v", res[0].Rows[0])
	}

	// 注销后消失
	unregister()
	res = run(t, d, s, `SELECT count(*) FROM pg_catalog.pg_stat_activity`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("注销后应为 0 条，实际 %s", got)
	}
}
