package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 杀会话（/api/monitor/kill）：监控页「活动」列表里那个「停止」按钮。
//
// 这条以前完全没测。它会回滚事务、打断等锁的语句 —— 弄错了轻则杀不掉，
// 重则把整个库卡死（WakeAll 没唤醒，等锁的 goroutine 会一直睡着）。
func TestMonitorKillContract(t *testing.T) {
	s, _, _ := newDBServer(t)
	h := s.Routes()
	local := "127.0.0.1:5000"

	// 只收 POST
	if rec := call(t, h, http.MethodGet, "/api/monitor/kill", local, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，实际 %d", rec.Code)
	}
	// 没给会话 ID
	rec := call(t, h, http.MethodPost, "/api/monitor/kill", local, `{}`, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "会话 ID") {
		t.Fatalf("缺 session 应 400 且点名，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 不存在的会话 → 404（不能 500）
	rec = call(t, h, http.MethodPost, "/api/monitor/kill", local, `{"session":"nope"}`, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "不存在") {
		t.Fatalf("不存在的会话应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	// "self" 但没有 godb_sid cookie → 400，而不是把空 id 丢给数据库
	rec = call(t, h, http.MethodPost, "/api/monitor/kill", local, `{"session":"self"}`, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "没有会话") {
		t.Fatalf("self 无 cookie 应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 真跑一遍：先发一条查询拿到会话 cookie，再 kill self，然后这个会话还能继续用。
//
// 「杀完还活着」是关键：Cancel 只置标志，KillSession 处理完会 ClearCancel；
// 如果忘了清，这个会话后面的每条 SQL 都会被判成「已取消」。
func TestMonitorKillSelfKeepsWorking(t *testing.T) {
	s, _, _ := newDBServer(t)
	h := s.Routes()
	local := "127.0.0.1:5000"

	// 1) 一条查询，顺手拿到服务端发的 godb_sid
	rec := call(t, h, http.MethodPost, "/api/query", local, `{"sql":"SELECT 1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("query = %d %s", rec.Code, rec.Body.String())
	}
	var sid string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "godb_sid" {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatalf("没拿到 godb_sid cookie")
	}
	ck := &http.Cookie{Name: "godb_sid", Value: sid}

	// 2) 杀这个会话
	rec = call(t, h, http.MethodPost, "/api/monitor/kill", local, `{"session":"self"}`, ck)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("kill self = %d %s", rec.Code, rec.Body.String())
	}

	// 3) 计数器要记一笔（监控页的「累计 Kill」读的就是它）
	rec = call(t, h, http.MethodGet, "/api/monitor", local, "", ck)
	var mon map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &mon); err != nil {
		t.Fatalf("monitor 解析失败: %v", err)
	}
	kills := 0.0
	if c, ok := mon["counters"].(map[string]interface{}); ok {
		if v, ok := c["kills"].(float64); ok {
			kills = v
		}
	}
	if kills < 1 {
		t.Fatalf("counters.kills 应 ≥1，实际 %v（%s）", kills, head(rec.Body.String(), 200))
	}

	// 4) 被杀过之后，同一个会话继续执行 —— 不能被「已取消」黏住
	rec = call(t, h, http.MethodPost, "/api/query", local, `{"sql":"SELECT 42 AS n"}`, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("kill 之后 query = %d %s", rec.Code, rec.Body.String())
	}
	var out queryResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error != nil {
		t.Fatalf("kill 之后不该报错: %s", out.Error.Message)
	}
	rows := rowsOf(t, &out)
	if len(rows) != 1 || asStr(rows[0][0]) != "42" {
		t.Fatalf("kill 之后结果不对: %v", rows)
	}
}

var _ = httptest.NewRecorder
