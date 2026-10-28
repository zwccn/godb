package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 启动门：未就绪时回启动页，就绪后转发给真路由；/api/boot-status 始终可用。
func TestBootGate(t *testing.T) {
	g := NewBootGate()
	g.SetPhase("WAL 重放", "已重放 1200 条")

	// 未就绪：任意路径都是启动页
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("启动页 HTTP %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "WAL 重放") || !strings.Contains(body, "已重放 1200 条") {
		t.Fatalf("启动页缺少阶段文案: %s", body[:200])
	}
	if !strings.Contains(body, "/api/boot-status") {
		t.Fatal("启动页应轮询 /api/boot-status")
	}

	// 状态接口：未就绪时 503，带阶段
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/api/boot-status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("boot-status HTTP %d, want 503", rec.Code)
	}
	var st struct {
		Ready  bool   `json:"ready"`
		Phase  string `json:"phase"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("boot-status JSON: %v", err)
	}
	if st.Ready || st.Phase != "WAL 重放" {
		t.Fatalf("boot-status = %+v", st)
	}

	// 就绪后：转给真路由
	real := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("REAL:" + r.URL.Path))
	})
	g.SetReady(real)

	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/api/schema", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "REAL:") {
		t.Fatalf("就绪后应转发真路由，实际 %d %q", rec.Code, rec.Body.String())
	}

	// 就绪后 boot-status 仍由启动门处理（返回 ready:true，不 404）
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest("GET", "/api/boot-status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("就绪后 boot-status JSON: %v", err)
	}
	if !st.Ready {
		t.Fatalf("就绪后 boot-status 应 ready:true, got %+v", st)
	}
}
