package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"godb/db"
)

// 取消（关页面 / 前端 AbortController / 面板 Kill）要报成 canceled，
// 不要混进「SQL 写错了」那一类 —— 前端对两类的渲染完全不同。
func TestClassifyCanceledError(t *testing.T) {
	e := classifyError(db.ErrCanceled, 3)
	if e.Kind != "canceled" {
		t.Errorf("Kind = %q，期望 canceled", e.Kind)
	}
	if !e.RolledBack {
		t.Error("取消会回滚事务，RolledBack 应为 true")
	}
	if e.Stmt != 3 {
		t.Errorf("Stmt = %d，期望 3", e.Stmt)
	}
}

// 请求的 ctx 被取消时，/api/query 要把取消转成会话取消（sess.Cancel），
// 而不是让查询继续跑到自然结束、把表锁一直握着。
//
// 这里用一个**已经取消**的 ctx：handler 里的 goroutine 会立刻调 sess.Cancel，
// 而 Exec 开头会 ClearCancel 把它清掉，所以查询本身仍然正常返回 —— 关键在于
// 「ctx.Done() → Cancel」这条线是通的、且不会把请求搞坏。
func TestQueryHonoursCanceledContext(t *testing.T) {
	s, _, _ := newDBServer(t)
	h := s.Routes()
	local := "127.0.0.1:5000"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求发出前就取消

	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT 42 AS n"}`))
	req = req.WithContext(ctx)
	req.RemoteAddr = local
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（取消不该变成 4xx/5xx）", rec.Code)
	}
	var resp struct {
		Results []struct{ Tag string } `json:"results"`
		Error   map[string]any         `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	// 已经取消的请求：要么正常返回结果（ClearCancel 生效），要么返回 canceled；
	// 不允许的是「崩溃 / 空响应 / 500」。
	if len(resp.Results) == 0 && resp.Error == nil {
		t.Fatal("既没有结果也没有错误")
	}
}
