// 启动门：在数据库还没打开完之前，HTTP 端口就已经在服务了，
// 用户看到的是「正在恢复」进度页，而不是浏览器一直转圈 / 连接被拒绝。
//
// 打开完成（WAL 重放、索引重建都结束）后，调用 SetReady 换上真正的控制台路由，
// 同一个监听无缝切换，不需要重启端口。
package web

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// BootGate 是一个 http.Handler：
//   - 未就绪 → 返回启动页（HTML）或启动状态（/api/boot-status JSON）
//   - 已就绪 → 转发给真正的控制台路由
//
// 就绪后 /api/boot-status 仍由它处理（永远回 ready:true），
// 这样启动页的轮询脚本不会因为路由消失而 404。
type BootGate struct {
	start time.Time

	// ready 一旦为 true 就不再变回 false。
	ready atomic.Bool
	// real 是就绪后接管请求的路由（通常就是 Server.Routes()）。
	real atomic.Value // http.Handler
	// phase / detail 是当前阶段的简短描述（中文），启动页显示用。
	phase  atomic.Value // string
	detail atomic.Value // string
}

// NewBootGate 创建启动门。从这一刻起端口就可以对外服务了。
func NewBootGate() *BootGate {
	g := &BootGate{start: time.Now()}
	g.phase.Store("正在启动")
	g.detail.Store("初始化")
	return g
}

// SetPhase 更新启动阶段（线程安全，随便从哪个 goroutine 调）。
func (g *BootGate) SetPhase(phase, detail string) {
	if phase != "" {
		g.phase.Store(phase)
	}
	if detail != "" {
		g.detail.Store(detail)
	}
}

// SetReady 交出控制权：之后所有请求都走真正的控制台路由。
// 重复调用只有第一次生效。
func (g *BootGate) SetReady(h http.Handler) {
	if h == nil {
		return
	}
	g.real.Store(h)
	g.ready.Store(true)
}

// ServeHTTP 实现 http.Handler。
func (g *BootGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 启动状态接口：任何时候都能查（启动页轮询用）
	if r.URL.Path == "/api/boot-status" {
		g.serveStatus(w, r)
		return
	}
	if g.ready.Load() {
		if h, ok := g.real.Load().(http.Handler); ok && h != nil {
			h.ServeHTTP(w, r)
			return
		}
	}
	g.serveBootPage(w, r)
}

func (g *BootGate) loadPhase() (phase, detail string) {
	if v, ok := g.phase.Load().(string); ok {
		phase = v
	}
	if v, ok := g.detail.Load().(string); ok {
		detail = v
	}
	return
}

func (g *BootGate) serveStatus(w http.ResponseWriter, r *http.Request) {
	phase, detail := g.loadPhase()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !g.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ready":     g.ready.Load(),
		"phase":     phase,
		"detail":    detail,
		"elapsedMs": time.Since(g.start).Milliseconds(),
	})
}

func (g *BootGate) serveBootPage(w http.ResponseWriter, r *http.Request) {
	phase, detail := g.loadPhase()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bootPageHTML(phase, detail)))
}

// bootPageHTML 是自包含的启动页（不依赖任何静态资源 —— 那些要等路由就绪才拿得到）。
// 样式取自控制台的深色语义色，保持观感一致。
func bootPageHTML(phase, detail string) string {
	return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>godb · 正在启动</title>
<style>
  :root {
    --bg: #0b0f14; --bg-1: #11161d; --bg-2: #161d26;
    --border: #232c38; --text: #e6edf3; --text-2: #9aa7b4; --text-3: #6b7785;
    --accent: #4c9ef0; --accent-soft-bg: #14293c; --accent-soft-border: #22425c;
    --ok: #3fb950;
    --mono: "JetBrains Mono", "Cascadia Mono", Consolas, ui-monospace, monospace;
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", "PingFang SC", sans-serif;
  }
  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    margin: 0; background: var(--bg); color: var(--text);
    font-family: var(--sans); font-size: 15px;
    display: flex; align-items: center; justify-content: center;
    padding: 24px;
  }
  .card {
    width: 100%; max-width: 440px;
    background: var(--bg-1); border: 1px solid var(--border);
    border-radius: 12px; padding: 32px 28px;
  }
  .brand {
    display: flex; align-items: center; gap: 12px; margin-bottom: 22px;
  }
  .logo {
    width: 40px; height: 40px; border-radius: 10px; flex: none;
    background: linear-gradient(135deg, #1e6fbf, #4c9ef0);
    color: #fff; font-family: var(--mono); font-weight: 700; font-size: 15px;
    display: flex; align-items: center; justify-content: center;
    letter-spacing: .5px;
  }
  .brand h1 { margin: 0; font-size: 20px; font-weight: 650; }
  .brand .sub { margin: 2px 0 0; color: var(--text-3); font-size: 12.5px; }

  .phase {
    font-family: var(--mono); font-size: 14px;
    background: var(--accent-soft-bg); border: 1px solid var(--accent-soft-border);
    color: var(--text); border-radius: 8px; padding: 12px 14px;
    margin-bottom: 10px;
  }
  .phase .label { color: var(--text-3); font-size: 11.5px; margin-bottom: 4px; }
  .phase .value { color: var(--text); }
  .phase .detail { color: var(--text-2); font-size: 12px; margin-top: 3px; }

  .bar {
    height: 6px; border-radius: 99px; background: var(--bg-2);
    overflow: hidden; margin: 18px 0 12px;
  }
  .bar i {
    display: block; height: 100%; width: 40%; border-radius: 99px;
    background: linear-gradient(90deg, transparent, var(--accent), transparent);
    animation: slide 1.2s ease-in-out infinite;
  }
  @keyframes slide {
    0%   { transform: translateX(-100%); }
    100% { transform: translateX(280%); }
  }

  .meta {
    display: flex; justify-content: space-between; align-items: center;
    color: var(--text-3); font-size: 12px;
  }
  .meta .t { font-family: var(--mono); }
  .dot {
    display: inline-block; width: 7px; height: 7px; border-radius: 50%;
    background: var(--ok); margin-right: 6px; vertical-align: 1px;
    animation: blink 1.4s ease-in-out infinite;
  }
  @keyframes blink { 0%,100% { opacity: 1; } 50% { opacity: .35; } }

  .tip {
    margin-top: 18px; padding-top: 14px; border-top: 1px solid var(--border);
    color: var(--text-3); font-size: 12px; line-height: 1.7;
  }
</style>
</head>
<body>
  <div class="card">
    <div class="brand">
      <div class="logo">db</div>
      <div>
        <h1>godb</h1>
        <p class="sub">正在启动 · 数据恢复中</p>
      </div>
    </div>

    <div class="phase">
      <div class="label">当前阶段</div>
      <div class="value" id="phase">` + phase + `</div>
      <div class="detail" id="detail">` + detail + `</div>
    </div>

    <div class="bar"><i></i></div>

    <div class="meta">
      <span><span class="dot"></span>服务已在监听，恢复完成后自动进入控制台</span>
      <span class="t" id="elapsed">0s</span>
    </div>

    <div class="tip">
      WAL 重放 / 索引重建期间页面会停留在这里，属于正常现象。<br>
      数据量越大恢复时间越长；完成后本页会自动跳转，无需手动刷新。
    </div>
  </div>

<script>
(function () {
  var phaseEl = document.getElementById('phase');
  var detailEl = document.getElementById('detail');
  var elapsedEl = document.getElementById('elapsed');
  var start = Date.now();

  function fmt(ms) {
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    return m + 'm ' + (s % 60) + 's';
  }

  function tick() {
    elapsedEl.textContent = fmt(Date.now() - start);
    fetch('/api/boot-status', { cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d && d.ready) {
          location.reload();
          return;
        }
        if (d && d.phase) phaseEl.textContent = d.phase;
        if (d && d.detail) detailEl.textContent = d.detail;
        if (d && typeof d.elapsedMs === 'number') elapsedEl.textContent = fmt(d.elapsedMs);
      })
      .catch(function () { /* 还没起来 / 瞬断，下一轮再试 */ });
  }

  setInterval(tick, 600);
  tick();
})();
</script>
</body>
</html>`
}
