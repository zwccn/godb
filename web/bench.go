package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"godb/db"
	"godb/syscat"
	"godb/txn"
)

// 基准测试：把「单条/批量插入、批量更新/删除、点查/范围扫描/聚合」跑到一个
// 可重复的数字，作为后续性能的参照，也喂给执行计划做对照。
//
// 设计上几条硬约定（直接决定数字有没有可比性）：
//
//  1. 每个场景**明确提交粒度**：单条插入是「一个事务里逐条 INSERT」，批量插入是
//     「一条多行 VALUES」。这两者吞吐差一个数量级都正常，不写清就是废数字。
//  2. 预热：每个场景先跑一次丢弃，首轮的表加载 / 索引冷启动不计入。
//  3. 每场景 3 轮，报**中位数**不是均值 —— 一次后台 checkpoint 鼓包就能把均值带偏。
//  4. 墙钟时间算吞吐（行数 ÷ 墙钟），另报服务端 tookNs 累计做交叉验证，
//     能看出瓶颈在引擎还是网络往返。
//  5. 基准表 __bench_items 测完即删（用户选了「下次现建」），不污染真实数据。

// benchScale 是基准数据规模（用户定 100 万）。
const benchScale = 1000000

// benchTable 是专用的基准表，前缀 __ 表示「工具表，测完删」。
const benchTable = "__bench_items"

// 单条插入的批大小档位：批量插入吞吐对批大小极敏感，扫档位能看出「阈值在哪」。
var benchBatchSizes = []int{100, 500, 1000, 5000}

// benchScaleOverride 只在测试里改小，避免真灌 100 万行；生产恒为 0（走默认）。
var benchScaleOverride int

// scale 返回本次要用的规模。
func scale() int {
	if benchScaleOverride > 0 {
		return benchScaleOverride
	}
	return benchScale
}

// benchState 是基准任务的运行时状态，start 时创建、status 时读出。
type benchState struct {
	mu      sync.Mutex
	running bool
	cancel  bool
	step    string // 当前正在做哪一步（喂给前端进度条）
	done    int    // 已完成的场景数
	total   int    // 场景总数
	result  *benchResult
	err     string
}

type benchResult struct {
	StartedAt string       `json:"startedAt"`
	Finished  bool         `json:"finished"`
	Scale     int          `json:"scale"`
	Version   string       `json:"version"`
	Scenarios []benchScene `json:"scenarios"`
	Batches   []benchBatch `json:"batches,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type benchScene struct {
	Name       string  `json:"name"`
	Commit     string  `json:"commit"`     // 提交粒度说明
	Rows       int     `json:"rows"`       // 这轮动了多少行
	PerSec     float64 `json:"perSec"`     // 中位数：行 / 秒（墙钟）
	SrvNsPerOp int64   `json:"srvNsPerOp"` // 中位数：服务端单次操作耗时
	Checkpoint bool    `json:"checkpoint"` // 是否撞上后台落盘（数字偏保守）
	Error      string  `json:"error,omitempty"`
}

type benchBatch struct {
	Size   int     `json:"size"`
	PerSec float64 `json:"perSec"`
}

// benchRunner 由 Server 持有，保证同一时刻只跑一个基准。
type benchRunner struct {
	mu  sync.Mutex
	cur *benchState
}

func (r *benchRunner) start() (*benchState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.cur.running {
		return r.cur, false // 已有任务在跑
	}
	s := &benchState{running: true, result: &benchResult{StartedAt: time.Now().Format("2006-01-02 15:04:05")}}
	r.cur = s
	return s, true
}

func (r *benchRunner) status() *benchState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// median 返回 float64 切片的中位数；切片会被原地排序（调用方不要再用原顺序）。
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// runBench 在后台 goroutine 里跑完整套场景，边跑边更新 state.step / done。
func (s *Server) runBench(st *benchState) {
	defer func() {
		st.mu.Lock()
		st.running = false
		if st.result != nil {
			st.result.Finished = true
		}
		st.mu.Unlock()
	}()
	d := s.currentBenchDB()
	if d == nil {
		st.mu.Lock()
		st.err = "没有可用的数据库"
		st.mu.Unlock()
		return
	}

	// 场景清单：7 项，覆盖插入/更新/删除/查询四大类。
	// total 用于进度；下面按顺序逐个跑，跑一个 done++。
	scenes := []struct {
		name string
		run  func(sess *benchSess) benchScene
	}{
		{"单条插入", func(x *benchSess) benchScene { return s.sceneInsertOne(x) }},
		{"批量插入", func(x *benchSess) benchScene { return s.sceneInsertBatch(x) }},
		{"批量更新", func(x *benchSess) benchScene { return s.sceneUpdate(x) }},
		{"批量删除", func(x *benchSess) benchScene { return s.sceneDelete(x) }},
		{"点查（主键）", func(x *benchSess) benchScene { return s.scenePointQuery(x) }},
		{"范围扫描", func(x *benchSess) benchScene { return s.sceneRangeScan(x) }},
		{"聚合", func(x *benchSess) benchScene { return s.sceneAggregate(x) }},
	}

	st.mu.Lock()
	st.total = len(scenes)
	st.result.Scale = scale()
	st.mu.Unlock()

	sess := &benchSess{d: d}

	// 无论正常跑完、取消、还是中途出错，基准表都要清掉（用户选了「测完即删」）。
	// defer 保证取消路径也不会留一张 100 万行的 __bench_items 在库里。
	defer func() {
		if sess.s != nil {
			_, _ = d.Exec(sess.s, "DROP TABLE IF EXISTS "+benchTable)
		}
	}()

	// 准备基准表（建表 + 灌 100 万行）。这一步耗时不小，进度单独标「准备数据」。
	st.setStep("建表并写入基准数据")
	if err := s.benchSeed(d, sess); err != nil {
		st.mu.Lock()
		st.err = "准备基准数据失败：" + err.Error()
		st.mu.Unlock()
		return
	}

	for i, sc := range scenes {
		if st.cancelled() {
			st.setStep("已取消")
			return
		}
		st.setStep(sc.name)
		res := sc.run(sess)
		st.mu.Lock()
		st.result.Scenarios = append(st.result.Scenarios, res)
		st.done = i + 1
		st.mu.Unlock()
	}

	// 批量插入的「批大小 → 吞吐」曲线单独归集。
	st.mu.Lock()
	st.result.Batches = sess.batches
	st.mu.Unlock()

	// 全部跑完且没取消，落盘一份历史，供下次「本次 vs 上次」对比。
	if !st.cancelled() {
		st.mu.Lock()
		res := st.result
		st.mu.Unlock()
		if res != nil {
			res.Version = syscat.GodbVersion
			s.saveBenchHistory(res)
		}
	}
}

func (s *benchState) setStep(step string) {
	s.mu.Lock()
	s.step = step
	s.mu.Unlock()
}

func (s *benchState) cancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel
}

// currentBenchDB 返回基准要打的库：多库模式用默认库（同其它管理接口）。
func (s *Server) currentBenchDB() *db.DB {
	if s.DBs != nil {
		return s.DBs.Default()
	}
	return s.DB
}

// benchSess 包一层会话，顺带攒「批大小 → 吞吐」曲线。
type benchSess struct {
	d       *db.DB
	s       *txn.Session
	batches []benchBatch
}

// exec 执行一条 SQL，返回结果。错误直接向上抛。
func (x *benchSess) exec(sqlText string) error {
	_, err := x.d.Exec(x.s, sqlText)
	return err
}

// benchSeed 建基准表并灌满 benchScale 行。用多行 VALUES 分批写入（一条 5000 行），
// 这样 100 万行也只要 200 条语句，几秒能完成；逐条 INSERT 在这里纯属浪费。
func (s *Server) benchSeed(d *db.DB, x *benchSess) error {
	x.s = d.Session("bench-" + newID())
	if err := x.exec(`DROP TABLE IF EXISTS ` + benchTable); err != nil {
		return err
	}
	// payload 塞中等长度文本，让行宽接近真实表，别拿「全空行」测出一个虚高的数。
	if err := x.exec(`CREATE TABLE ` + benchTable +
		` (id serial PRIMARY KEY, grp int4, v text, payload text)`); err != nil {
		return err
	}

	const chunk = 5000
	for base := 0; base < scale(); base += chunk {
		n := chunk
		if scale()-base < n {
			n = scale() - base
		}
		var b strings.Builder
		b.WriteString(`INSERT INTO ` + benchTable + ` (grp, v, payload) VALUES `)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(`,`)
			}
			id := base + i
			fmt.Fprintf(&b, `(%d, 'v_%d', 'payload-%d-`+benchPayload+`')`, id%1000, id, id)
		}
		if err := x.exec(b.String()); err != nil {
			return err
		}
	}
	return nil
}

// benchPayload 是每行 payload 里重复的一段文本，让行宽稳定在一个可复现的量级。
const benchPayload = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz"

// timed 包一层「预热 + 3 轮计时」：warmup 与每轮都调 fn，返回 3 轮墙钟秒。
// 每轮之间不做 sleep，靠 nextCheckpoint 探测是否撞上后台落盘。
func (s *Server) timed(x *benchSess, warmup func() error, fn func() error) ([]float64, error) {
	if err := warmup(); err != nil {
		return nil, err
	}
	out := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			return nil, err
		}
		out = append(out, time.Since(start).Seconds())
	}
	return out, nil
}

// insertPerRound 是「单条插入 / 批量插入」每个档位/每轮要写的行数。
// 生产 100 万行时取 2 万（够让逐条路径稳定）；测试规模小就跟着缩小，
// 否则单测里「单事务逐条插 8 万行」会跑一分钟。
func insertPerRound() int {
	if benchScaleOverride > 0 && benchScaleOverride < 20000 {
		return benchScaleOverride
	}
	return 20000
}

// sceneInsertOne 单条插入：一个事务里逐条 INSERT，测「引擎单条写入上限」。
// 提交粒度写死在结果里 —— 这是吞吐最大的变量，不说清数字没法比。
func (s *Server) sceneInsertOne(x *benchSess) benchScene {
	n := insertPerRound()
	warmup := func() error { return x.exec(`INSERT INTO ` + benchTable + ` (grp, v, payload) VALUES (1,'w','x')`) }
	fn := func() error {
		if err := x.exec(`BEGIN`); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if err := x.exec(fmt.Sprintf(`INSERT INTO %s (grp,v,payload) VALUES (%d,'i','x')`, benchTable, i)); err != nil {
				return err
			}
		}
		return x.exec(`COMMIT`)
	}
	secs, err := s.timed(x, warmup, fn)
	if err != nil {
		return benchScene{Name: "单条插入", Commit: "单事务逐条", Error: err.Error()}
	}
	return benchScene{Name: "单条插入", Commit: "单事务逐条", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

// sceneInsertBatch 批量插入：一条多行 VALUES，扫批大小档位。曲线单独归集到 batches。
func (s *Server) sceneInsertBatch(x *benchSess) benchScene {
	perRound := insertPerRound()
	var best float64
	for _, bs := range benchBatchSizes {
		warmup := func() error {
			return x.exec(fmt.Sprintf(`INSERT INTO %s (grp,v,payload) VALUES (1,'w','x')`, benchTable))
		}
		fn := func() error {
			for done := 0; done < perRound; done += bs {
				var b strings.Builder
				b.WriteString(`INSERT INTO ` + benchTable + ` (grp,v,payload) VALUES `)
				for i := 0; i < bs; i++ {
					if i > 0 {
						b.WriteString(`,`)
					}
					fmt.Fprintf(&b, `(%d,'b','x')`, done+i)
				}
				if err := x.exec(b.String()); err != nil {
					return err
				}
			}
			return nil
		}
		secs, err := s.timed(x, warmup, fn)
		if err != nil {
			return benchScene{Name: "批量插入", Commit: "多行 VALUES", Error: err.Error()}
		}
		per := float64(perRound) / median(secs)
		x.batches = append(x.batches, benchBatch{Size: bs, PerSec: round3(per)})
		if per > best {
			best = per
		}
	}
	return benchScene{Name: "批量插入", Commit: "多行 VALUES（最优档位）", Rows: perRound, PerSec: round3(best)}
}

// sceneUpdate 批量更新：整表 100 万行都 UPDATE 一次，按 id 区间分几段跑，
// 避免一条 UPDATE 锁住整张表太久。固定「每行 +1」，各轮可重复。
func (s *Server) sceneUpdate(x *benchSess) benchScene {
	const rows = 0 // 占位，真正取值在下面（scale() 不能被 const 调用）
	_ = rows
	rowsV := scale()
	warmup := func() error { return x.exec(`UPDATE ` + benchTable + ` SET v = v WHERE id = 1`) }
	fn := func() error {
		const seg = 100000
		for lo := 1; lo <= rowsV; lo += seg {
			if err := x.exec(fmt.Sprintf(`UPDATE %s SET v = v WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+seg-1)); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, warmup, fn)
	if err != nil {
		return benchScene{Name: "批量更新", Commit: "按 id 区间分段", Error: err.Error()}
	}
	return benchScene{Name: "批量更新", Commit: "按 id 区间分段", Rows: rowsV, PerSec: round3(float64(rowsV) / median(secs))}
}

// sceneDelete 批量删除：删光 + 重灌。用户选的是「删光重灌」（A），所以这里
// 删除吞吐**不含**重灌时间。
//
// 删除场景和别的不同：它不可重复 —— 删光之后表就空了，下一轮删的是空表
// （实测第二、三轮 timed 全是 0 秒，median 变 0）。所以每轮的结构是
// 「重灌（不计时）→ 删光（计时）」，这样三轮的删除代价才可比。
func (s *Server) sceneDelete(x *benchSess) benchScene {
	rowsV := scale()
	del := func() error {
		const seg = 100000
		for lo := 1; lo <= rowsV; lo += seg {
			if err := x.exec(fmt.Sprintf(`DELETE FROM %s WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+seg-1)); err != nil {
				return err
			}
		}
		return nil
	}

	// 预热一轮：重灌 + 删一次，让表加载 / 索引冷启动都过去
	if err := s.benchSeed(x.d, x); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "重灌失败：" + err.Error()}
	}
	if err := del(); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: err.Error()}
	}

	secs := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		if err := s.benchSeed(x.d, x); err != nil { // 重灌，不计时
			return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "重灌失败：" + err.Error()}
		}
		start := time.Now()
		if err := del(); err != nil {
			return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: err.Error()}
		}
		secs = append(secs, time.Since(start).Seconds())
	}
	// 最后一轮删光后重灌，供后面的点查/范围扫描/聚合继续用
	if err := s.benchSeed(x.d, x); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "收尾重灌失败：" + err.Error()}
	}
	return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光，重灌不计入）", Rows: rowsV, PerSec: round3(float64(rowsV) / median(secs))}
}

// scenePointQuery 点查：按主键查单行，跑多次测索引点查吞吐。
func (s *Server) scenePointQuery(x *benchSess) benchScene {
	const n = 5000
	warmup := func() error { return x.exec(`SELECT id FROM ` + benchTable + ` WHERE id = 1`) }
	fn := func() error {
		for i := 0; i < n; i++ {
			if err := x.exec(fmt.Sprintf(`SELECT id FROM %s WHERE id = %d`, benchTable, (i%scale())+1)); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, warmup, fn)
	if err != nil {
		return benchScene{Name: "点查（主键）", Commit: "单条 SELECT by PK", Error: err.Error()}
	}
	return benchScene{Name: "点查（主键）", Commit: "单条 SELECT by PK", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

// sceneRangeScan 范围扫描：一次扫一个 id 区间并聚合，测顺序/索引范围扫描。
func (s *Server) sceneRangeScan(x *benchSess) benchScene {
	const n = 2000 // 每轮扫 2000 个区间
	warmup := func() error { return x.exec(`SELECT count(*) FROM ` + benchTable + ` WHERE id BETWEEN 1 AND 100`) }
	fn := func() error {
		for i := 0; i < n; i++ {
			lo := (i*100)%(scale()-100) + 1
			if err := x.exec(fmt.Sprintf(`SELECT count(*) FROM %s WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+99)); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, warmup, fn)
	if err != nil {
		return benchScene{Name: "范围扫描", Commit: "区间 SELECT + count", Error: err.Error()}
	}
	return benchScene{Name: "范围扫描", Commit: "区间 SELECT + count", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

// sceneAggregate 聚合：全表 count + group by，测物化内存与哈希聚合。
func (s *Server) sceneAggregate(x *benchSess) benchScene {
	const n = 20
	warmup := func() error { return x.exec(`SELECT count(*) FROM ` + benchTable) }
	fn := func() error {
		for i := 0; i < n; i++ {
			if err := x.exec(`SELECT grp, count(*) FROM ` + benchTable + ` GROUP BY grp`); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, warmup, fn)
	if err != nil {
		return benchScene{Name: "聚合", Commit: "GROUP BY 全表", Error: err.Error()}
	}
	return benchScene{Name: "聚合", Commit: "GROUP BY 全表", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

func round3(v float64) float64 {
	// 防御：分母为 0 或计时异常时会得到 +Inf/NaN，int 转换是未定义行为
	// （实测会翻成 math.MinInt = -9.2e15，前端显示成一个天文负数）。
	if v <= 0 || v != v || v > 1e12 {
		return 0
	}
	return float64(int(v*1000+0.5)) / 1000
}

// benchHistoryPath 是历史结果落盘的位置（相对数据目录）。
const benchHistoryPath = "bench-history.json"

// handleBenchStart 启动一次基准，秒回。已有任务在跑时返回 409。
func (s *Server) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st, ok := s.bench.start()
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "已有基准在跑，等它结束"})
		return
	}
	go s.runBench(st)
	writeJSON(w, http.StatusOK, map[string]interface{}{"started": true, "scale": scale()})
}

// handleBenchStatus 返回当前任务进度；没有任务时返回空 result。
func (s *Server) handleBenchStatus(w http.ResponseWriter, r *http.Request) {
	st := s.bench.status()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"result": nil})
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	resp := map[string]interface{}{
		"running": st.running,
		"step":    st.step,
		"done":    st.done,
		"total":   st.total,
		"result":  st.result,
	}
	if st.err != "" {
		resp["error"] = st.err
		if st.result != nil {
			st.result.Error = st.err
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBenchCancel 请求取消当前任务。取消是协作式的：当前场景跑完就停。
func (s *Server) handleBenchCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := s.bench.status()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	st.mu.Lock()
	st.cancel = true
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBenchHistory 返回上一次完整跑完的结果（用于「本次 vs 上次」对比）。
func (s *Server) handleBenchHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": s.loadBenchHistory()})
}

// loadBenchHistory 从数据目录读上次结果；没有 / 读坏就返回 nil（历史是锦上添花）。
func (s *Server) loadBenchHistory() *benchResult {
	d := s.currentBenchDB()
	if d == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(d.Dir(), benchHistoryPath))
	if err != nil {
		return nil
	}
	var out benchResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

// saveBenchHistory 把这次结果写进数据目录（覆盖上次）。失败不致命，只是没对比。
func (s *Server) saveBenchHistory(res *benchResult) {
	d := s.currentBenchDB()
	if d == nil {
		return
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(d.Dir(), benchHistoryPath), data, 0o644)
}
