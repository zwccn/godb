package db

import (
	"fmt"
	"strings"
	"testing"
)

// P5：pg_database_size 不再是假常数；pg_size_pretty 按参数渲染。
func TestP5DatabaseSize(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sz (id int PRIMARY KEY, v text)`)
	for i := 0; i < 50; i++ {
		run(t, d, s, fmt.Sprintf(`INSERT INTO sz VALUES (%d, 'pad-pad-pad-pad-pad')`, i))
	}
	r := rowsOf(t, run(t, d, s, `SELECT pg_database_size(current_database())`))
	if len(r) != 1 {
		t.Fatalf("pg_database_size 行数 = %d", len(r))
	}
	got := strings.TrimSpace(fmt.Sprint(r[0][0]))
	if got == "0" || got == "8192" {
		t.Fatalf("pg_database_size = %s，应是真实占用", got)
	}
	p := rowsOf(t, run(t, d, s, `SELECT pg_size_pretty(8192)`))
	if fmt.Sprint(p[0][0]) != "8192 bytes" {
		t.Fatalf("pg_size_pretty(8192) = %q", fmt.Sprint(p[0][0]))
	}
	p2 := rowsOf(t, run(t, d, s, `SELECT pg_size_pretty(16777216)`))
	if !strings.Contains(fmt.Sprint(p2[0][0]), "MB") {
		t.Fatalf("pg_size_pretty(16MB) = %q", fmt.Sprint(p2[0][0]))
	}
}

// P6：current_user / session_user 跟随登录名（会话 User）。
func TestP6CurrentUserFollowsLogin(t *testing.T) {
	d := newTestDB(t)
	sess := d.Session("u1")
	sess.User = "alice"
	out, err := d.Exec(sess, `SELECT current_user`)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(out) == 0 || len(out[0].Rows) != 1 {
		t.Fatalf("结果异常: %+v", out)
	}
	if got := fmt.Sprint(out[0].Rows[0][0]); got != "alice" {
		t.Fatalf("current_user = %q, want alice", got)
	}
	// 未设置 User 的会话回退到默认角色名
	assertRows(t, run(t, d, "s", `SELECT current_user`), []string{"godb"})
}
