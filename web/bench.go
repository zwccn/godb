package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// benchScale 是基准数据规模，默认 10 万行。
//
// 曾经是 100 万：那个量级能看出「精确扫描开始难受」，但整套跑一次要四五分钟，
// 其中一多半时间花在灌数据和 DROP 512MB 的 .vec 上 —— 改一行代码想验一次性能，
// 等不起。10 万行的量级关系与 100 万一致（都是线性放大的区间），整轮约一分钟。
//
// 真要出对外数字时把这里改回 1000000 再跑一次即可：所有场景的规模都走 scale()，
// 没有第二处写死行数。对比历史数据时记得确认两次的规模相同 —— 吞吐是按行算的，
// 但「独占时间的绝对值」会随规模线性变化。
const benchScale = 100000

// benchTable 是专用的基准表，前缀 __ 表示「工具表，测完删」。
const benchTable = "__bench_items"

// benchCommitTable 是「逐条提交插入」专用的独立小表。
// 不复用主表：多出的几千行会让后面的全表扫描 / 聚合行数基准变掉，两次结果就不可比了。
const benchCommitTable = "__bench_commit"

// 向量基准用独立的表，规模跟主表一致（默认 10 万行 × 128 维）。
//
// 128 维是常见的轻量 embedding 尺寸（OpenAI 的 ada-002/text-embedding-3-small 的
// 短维度档也在这个量级）。这组数字是 **ANN 索引的对照基线**：将来做近似索引，
// 拿它比快了多少、召回掉了多少。
//
// 规模降到 10 万后：.vec 约 51MB，灌一次几秒，Top-K 单次几十毫秒 —— 仍然能看出
// 「全表扫 × 逐行算距离」的形状，但整套基准回到一分钟级。
// 要对外报 100 万档的数字时改 benchScale 即可（改完记得两边的 .vec 都删掉重建）。
const (
	benchVecTable = "__bench_vec"
	benchVecDim   = 128
)

// benchVecN 返回这次要灌多少行：单测把规模改小的时候跟着变小。
func benchVecN() int { return scale() }

// benchVecFrac 是 "0.00"…"0.99" 的查表。
//
// 10 万行 × 128 维 = 1280 万次格式化，每次走一遍 fmt 会把灌数据拖成十几秒；
// 查表写字符串只要一次 append。
var benchVecFrac = func() [100]string {
	var t [100]string
	for i := range t {
		t[i] = fmt.Sprintf("0.%02d", i)
	}
	return t
}()

// benchVecLiteral 生成第 seed 个确定性向量。
//
// 不用 math/rand：基准的数字要能横着比，两次跑的数据必须一模一样。
// 取值固定两位小数，文本长度稳定，导入耗时不会因为格式抖动。
func benchVecLiteral(dim, seed int) string {
	var b strings.Builder
	b.Grow(dim*5 + 2)
	b.WriteByte('[')
	for i := 0; i < dim; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(benchVecFrac[(seed*31+i*17)%100])
	}
	b.WriteByte(']')
	return b.String()
}

// 单条插入的批大小档位：批量插入吞吐对批大小极敏感，扫档位能看出「阈值在哪」。
var benchBatchSizes = []int{100, 500, 1000, 5000}

// benchScaleOverride 只在测试里改小，避免真灌满整表；生产恒为 0（走默认）。
var benchScaleOverride int

// scale 返回本次要用的规模。
func scale() int {
	if benchScaleOverride > 0 {
		return benchScaleOverride
	}
	return benchScale
}

// benchState 是基准任务的运行时状态，start 时创建、status 时读出。
//
// 进度分三层，缺一层就会出现「长时间没反应」：
//   - step   ：在跑哪个场景（准备数据 / 批量删除 …）
//   - detail ：场景内部到哪了（预热、第 2/3 轮、灌数据 62%、删除 4/10 段）
//   - frac   ：场景内部的比例 0~1，和 done/total 一起算出整体百分比
//
// 只报 done/total 是不够的：批量删除光一个场景就要重灌 5 次整张表 + 删 4 次，
// 三分钟里数字一动不动，看着就像卡死了。
type benchState struct {
	mu        sync.Mutex
	running   bool
	cancel    bool
	step      string  // 当前正在做哪一步（喂给前端进度条）
	detail    string  // 这一步内部的细分说明
	frac      float64 // 当前这一步内部完成到几成（0~1）
	done      int     // 已完成的「步数」（准备数据也算一步）
	total     int     // 总步数
	startedAt time.Time
	stepAt    time.Time
	sceneSecs []float64 // 每个已完成场景的耗时，用来估剩余时间
	result    *benchResult
	err       string
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
	s := &benchState{
		running:   true,
		startedAt: time.Now(),
		stepAt:    time.Now(),
		result:    &benchResult{StartedAt: time.Now().Format("2006-01-02 15:04:05")},
	}
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

	// 场景清单：8 项，覆盖插入/更新/删除/查询四大类，最后一项是纯磁盘提交对照。
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
		// 向量另用一张表（主表没有向量列），跑完一起删。
		{"向量 Top-K", func(x *benchSess) benchScene { return s.sceneVectorTopK(x) }},
		// 放最后：它用自己的小表，跑完即删，不打断前面「主表行数恒定」的那几个场景。
		{"逐条提交插入", func(x *benchSess) benchScene { return s.sceneInsertCommit(x) }},
	}

	st.mu.Lock()
	// 「准备数据」也算一步：灌满整表本身就要十几秒，不算进去的话进度条
	// 会在 0% 上杵着，最容易被当成卡死。
	st.total = len(scenes) + 1
	st.result.Scale = scale()
	st.mu.Unlock()

	sess := &benchSess{d: d, st: st}

	// 无论正常跑完、取消、还是中途出错，基准表都要清掉（用户选了「测完即删」）。
	// defer 保证取消路径也不会留一张灌满的 __bench_items 在库里。
	defer func() {
		if sess.s == nil {
			return
		}
		// 取消时可能正卡在 BEGIN … COMMIT 中间：事务还没结束，表上的 X 锁还在，
		// DROP 会被自己挡住 —— 而这里的错误是被忽略的，结果就是「取消了，但库里
		// 多出一张灌满的表」。先强制回滚把锁放掉。
		sess.s.ForceRollback()
		_, _ = d.Exec(sess.s, "DROP TABLE IF EXISTS "+benchTable)
		_, _ = d.Exec(sess.s, "DROP TABLE IF EXISTS "+benchCommitTable)
		_, _ = d.Exec(sess.s, "DROP TABLE IF EXISTS "+benchVecTable)
		// 向量基准是 512MB 的表，DROP 会按常规路径备份进回收站（.dat + .vec
		// 两份拷贝，实测 1002MB）。那是给用户误删兜底的，不该为一次基准留着。
		// 这里只删「全部是 __bench_ 表」的备份，用户的备份一条不动。
		d.Store.PurgeTrashForBench()
	}()

	// 准备基准表（建表 + 灌满 scale() 行）。这一步耗时不小，进度按「写了多少行」走。
	st.setStep("准备数据")
	if err := s.benchSeed(d, sess, progPart{span: 1, label: "建表并写入"}); err != nil {
		if errors.Is(err, errBenchCancel) {
			st.canceledNow()
			return
		}
		st.mu.Lock()
		st.err = "准备基准数据失败：" + err.Error()
		st.mu.Unlock()
		return
	}
	st.mu.Lock()
	st.done = 1
	st.mu.Unlock()

	for i, sc := range scenes {
		if st.cancelled() {
			st.canceledNow()
			return
		}
		st.setStep(sc.name)
		t0 := time.Now()
		res := sc.run(sess)
		// 取消是协作式的：场景内部每轮 / 每段都会检查，这里发现被取消就把
		// 这一趟的残值丢掉 —— 半轮的数字既不能比也不能存。
		if st.cancelled() {
			st.canceledNow()
			return
		}
		took := time.Since(t0).Seconds()
		st.mu.Lock()
		st.result.Scenarios = append(st.result.Scenarios, res)
		st.sceneSecs = append(st.sceneSecs, took)
		st.done = i + 2 // +1 是「准备数据」那一步，+1 是 i 从 0 开始
		st.frac = 0
		st.detail = ""
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
		// finished 必须在写盘之前置上。原来只在 defer 里置，于是磁盘上永远写着
		// "finished": false —— 而「这份基线是不是完整跑完的」正是下次决定要不要
		// 拿它对比的依据，写反了等于每份历史都不可信。
		if res != nil {
			res.Finished = true
		}
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
	s.detail = ""
	s.frac = 0
	s.stepAt = time.Now()
	s.mu.Unlock()
}

// setProgress 汇报「当前这一步内部」的进度。frac 收在 0~1，detail 是给人看的一句话。
func (s *benchState) setProgress(frac float64, detail string) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	s.mu.Lock()
	s.frac = frac
	s.detail = detail
	s.mu.Unlock()
}

// canceledNow 把状态切成「已取消」：进度停在原地，不再往前走。
// 顺手在结果里记一句 —— 半途而废的数字不能拿去和完整跑的一轮比。
func (s *benchState) canceledNow() {
	s.mu.Lock()
	s.step = "已取消"
	s.detail = ""
	s.frac = 0
	if s.result != nil {
		s.result.Error = "已取消：结果不完整，没有写进历史记录"
	}
	s.mu.Unlock()
}

// etaLocked 估剩余时间：拿已完成场景的平均耗时乘剩余步数，再扣掉当前这一步
// 已经走的部分。少于两个场景就是「还说不准」—— 一个样本估出来的数只会误导人。
// 调用方必须已持锁。
func (s *benchState) etaLocked() (int64, bool) {
	if len(s.sceneSecs) < 2 || s.total <= 0 {
		return 0, false
	}
	sum := 0.0
	for _, v := range s.sceneSecs {
		sum += v
	}
	avg := sum / float64(len(s.sceneSecs))
	rem := float64(s.total-s.done) - s.frac
	if rem <= 0 {
		return 0, false
	}
	return int64(avg * rem * 1000), true
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

// errBenchCancel 是「用户点了停止」。用它区分「真出错」和「主动中断」：
// 后者不该被记成一条失败场景。
var errBenchCancel = errors.New("bench: canceled")

// progPart 是「这一趟占整个场景多大比例」。一个场景里常跑好几趟（批量插入扫
// 4 个批大小档位、批量删除每轮都要先重灌一次整张表），各趟各占一段，
// 进度条才走得匀，不会一趟顶到底。
type progPart struct {
	base, span float64
	label      string // 非空时当前缀，例如 "batch=5000"
}

func (p progPart) pre() string {
	if p.label == "" {
		return ""
	}
	return p.label + " · "
}

// benchSess 包一层会话，顺带攒「批大小 → 吞吐」曲线和进度。
type benchSess struct {
	d       *db.DB
	s       *txn.Session
	batches []benchBatch
	st      *benchState // 进度回报；单测里可能为空
	part    progPart    // 当前这一趟占场景内多大一段（timed 里设）
	round   int         // 当前第几轮（0 起）
	pre     string      // 当前轮的说明前缀，例如 "batch=5000 · 第 2/3 轮"
}

// exec 执行一条 SQL，返回结果。错误直接向上抛。
func (x *benchSess) exec(sqlText string) error {
	_, err := x.d.Exec(x.s, sqlText)
	return err
}

// stopped 是「用户点了停止」。大循环每轮 / 每一段都查一次，
// 否则点完停止要等整个场景跑完才生效（批量删除那种要三分钟）。
func (x *benchSess) stopped() bool {
	return x != nil && x.st != nil && x.st.cancelled()
}

func (x *benchSess) note(frac float64, detail string) {
	if x == nil || x.st == nil {
		return
	}
	x.st.setProgress(frac, detail)
}

// within 汇报「某一轮内部」的进度（f 是这一轮自己的 0~1），顺带看一眼要不要停。
// 大循环（十万行一段的更新/删除、五千行一批的灌数据）走到一半时，百分比
// 至少在动 —— 这就是「长时间没反应」和「慢但在动」的区别。
//
// 进度和取消检查绑在一起是有意的：它们本来就发生在同一批循环边界上，
// 分开写只会让人忘掉其中一件（忘了查取消，点了停止要等整轮跑完才生效）。
func (x *benchSess) within(f float64, detail string) error {
	if x.stopped() {
		return errBenchCancel
	}
	if x == nil || x.st == nil {
		return nil
	}
	p := x.part
	frac := p.base + p.span*(float64(x.round)+f)/3
	x.note(frac, x.pre+" · "+detail)
	return nil
}

// segPart 是「一串按行数分段的大循环」里的进度：拿已处理行数除以总行数。
func segPart(done, total int) float64 {
	if total <= 0 {
		return 1
	}
	return float64(done) / float64(total)
}

// benchComma 给数字加千分位：进度里「62,000 / 100,000 行」比一长串 0 好读。
func benchComma(n int) string {
	s := strconv.Itoa(n)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// benchCap 取两者较小值（最后一批常常不满）。Go 1.20 没有内建 min。
func benchCap(v, hi int) int {
	if v > hi {
		return hi
	}
	return v
}

// benchSeed 建基准表并灌满 benchScale 行。用多行 VALUES 分批写入（一条 5000 行），
// 这样 10 万行只要 20 条语句；逐条 INSERT 在这里纯属浪费。
//
// p 是这一趟在整个场景里占的比例：批量删除每轮都要重灌，不能让它把进度条
// 一次顶到底。进度按「已完成行数」报，长表上也能几秒动一下。
func (s *Server) benchSeed(d *db.DB, x *benchSess, p progPart) error {
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
	total := scale()
	for base := 0; base < total; base += chunk {
		if x.stopped() {
			return errBenchCancel
		}
		n := chunk
		if total-base < n {
			n = total - base
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
		x.note(p.base+p.span*segPart(base+n, total),
			fmt.Sprintf("%s灌数据 %s / %s 行", p.pre(), benchComma(base+n), benchComma(total)))
	}
	return nil
}

// benchPayload 是每行 payload 里重复的一段文本，让行宽稳定在一个可复现的量级。
const benchPayload = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz"

// timed 包一层「预热 + 3 轮计时」：warmup 与每轮都调 fn，返回 3 轮墙钟秒。
// 每轮之间不做 sleep，靠 nextCheckpoint 探测是否撞上后台落盘。
//
// p 说明这一趟占场景内多大一段（单个场景就是整段，批量插入是 1/4）。
// 每轮开始前都会把「第 i/3 轮」推到前端 —— 一轮可能就是十几秒，
// 这中间什么都不说的话，进度条看着就是死的。
func (s *Server) timed(x *benchSess, p progPart, warmup, fn func() error) ([]float64, error) {
	x.part = p
	pre := p.pre()
	x.round = -1
	x.pre = pre + "预热"
	x.note(p.base, x.pre)
	if err := warmup(); err != nil {
		return nil, err
	}
	out := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		if x.stopped() {
			return nil, errBenchCancel
		}
		x.round = i
		x.pre = fmt.Sprintf("%s第 %d/3 轮", pre, i+1)
		x.note(p.base+p.span*float64(i)/3, x.pre)
		start := time.Now()
		if err := fn(); err != nil {
			return nil, err
		}
		out = append(out, time.Since(start).Seconds())
	}
	return out, nil
}

// insertPerRound 是「单条插入 / 批量插入」每个档位/每轮要写的行数。
//
// 跟着规模按比例走（约 1/20，夹在 [5000, 20000] 之间）：10 万行时 5000 行、
// 100 万行时 2 万行。写死 2 万在 10 万行的库里就是「每轮重写五分之一张表」，
// 单条插入这一项会从几秒变成十几秒，而它测的是「每秒多少条」，和写多少行无关 ——
// 5000 行已经足够把首次编译、连接建立这些一次性开销摊掉。
func insertPerRound() int {
	n := scale() / 20
	if n < 5000 {
		n = 5000
	}
	if n > 20000 {
		n = 20000
	}
	if benchScaleOverride > 0 && benchScaleOverride < n {
		return benchScaleOverride
	}
	return n
}

// commitPerRound 是「逐条提交插入」每轮的行数。
// 一行一个事务，所以这个数字同时也是一轮的 fsync 次数：5000 行在 1.3ms/fsync 的盘上
// 就是六七秒一轮，够把 p50 稳定下来；再往上加只是把场景拖长，不会让数字更可信。
func commitPerRound() int {
	n := insertPerRound() / 4
	if n < 1 {
		n = 1
	}
	return n
}

// sceneInsertOne 单条插入：一个事务里逐条 INSERT，测「引擎单条写入上限」。
// 提交粒度写死在结果里 —— 这是吞吐最大的变量，不说清数字没法比。
func (s *Server) sceneInsertOne(x *benchSess) benchScene {
	n := insertPerRound()
	p := progPart{span: 1}
	warmup := func() error { return x.exec(`INSERT INTO ` + benchTable + ` (grp, v, payload) VALUES (1,'w','x')`) }
	fn := func() error {
		if err := x.exec(`BEGIN`); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if err := x.exec(fmt.Sprintf(`INSERT INTO %s (grp,v,payload) VALUES (%d,'i','x')`, benchTable, i)); err != nil {
				return err
			}
			if i%2000 == 1999 {
				if err := x.within(segPart(i+1, n), fmt.Sprintf("%s / %s 行", benchComma(i+1), benchComma(n))); err != nil {
					return err
				}
			}
		}
		return x.exec(`COMMIT`)
	}
	secs, err := s.timed(x, p, warmup, fn)
	if err != nil {
		return benchScene{Name: "单条插入", Commit: "单事务逐条", Error: err.Error()}
	}
	return benchScene{Name: "单条插入", Commit: "单事务逐条", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

// sceneInsertBatch 批量插入：一条多行 VALUES，扫批大小档位。曲线单独归集到 batches。
func (s *Server) sceneInsertBatch(x *benchSess) benchScene {
	perRound := insertPerRound()
	var best float64
	for bi, bs := range benchBatchSizes {
		// 四个档位各占四分之一：不然扫档位这段时间里进度条一直贴在同一处。
		p := progPart{base: float64(bi) / 4, span: 0.25, label: fmt.Sprintf("batch=%d", bs)}
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
				if err := x.within(segPart(done+bs, perRound),
					fmt.Sprintf("%s / %s 行", benchComma(benchCap(done+bs, perRound)), benchComma(perRound))); err != nil {
					return err
				}
			}
			return nil
		}
		secs, err := s.timed(x, p, warmup, fn)
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

// sceneInsertCommit 逐条提交插入：每行一个事务，每行都等一次 fsync 返回。
//
// 全套里只有这一项把「落盘」放在关键路径上，所以它的吞吐基本就是磁盘提交上限
// （约等于 fsync p50 的倒数）：
//   - 单条插入是「2 万行一个事务」，整个事务只 fsync 一次；
//   - 批量插入是每条多行 VALUES 一次；
//   - 这里是每条 INSERT 一次。
//
// 表故意不建主键：只测「提交 + 堆追加」，不掺索引页维护；也用独立小表，
// 避免多出的几千行改变前面全表扫描 / 聚合的行数基准。
func (s *Server) sceneInsertCommit(x *benchSess) benchScene {
	const name = "逐条提交插入"
	n := commitPerRound()
	_ = x.exec("DROP TABLE IF EXISTS " + benchCommitTable)
	if err := x.exec("CREATE TABLE " + benchCommitTable + " (id INT, v TEXT)"); err != nil {
		return benchScene{Name: name, Commit: "每行一个事务（含 fsync）", Error: err.Error()}
	}
	// 计时前后取 fsync 计数：「每行一次」不是嘴上说说，是真数出来的。
	syncsBefore := x.d.Store.Info().WALSyncs

	warmup := func() error {
		return x.exec(fmt.Sprintf(`INSERT INTO %s (id,v) VALUES (-1,'w')`, benchCommitTable))
	}
	fn := func() error {
		for i := 0; i < n; i++ {
			if err := x.exec(fmt.Sprintf(`INSERT INTO %s (id,v) VALUES (%d,'c')`, benchCommitTable, i)); err != nil {
				return err
			}
			if i%500 == 499 {
				if err := x.within(segPart(i+1, n),
					fmt.Sprintf("%s / %s 行 · 每行一次 fsync", benchComma(i+1), benchComma(n))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	secs, err := s.timed(x, progPart{span: 1}, warmup, fn)
	syncs := x.d.Store.Info().WALSyncs - syncsBefore
	_ = x.exec("DROP TABLE IF EXISTS " + benchCommitTable)
	if err != nil {
		return benchScene{Name: name, Commit: "每行一个事务（含 fsync）", Error: err.Error()}
	}
	// 提交次数 = 预热 1 次 + 3 轮各 n 次。fsync 次数往往**少于**提交次数：
	// 组提交 / 后台 checkpoint 的 SyncTo 会顺带把后面的记录刷掉，那些提交不必自己刷 ——
	// 数据同样已经落盘，只是不是各自独立的一次 fsync。两个数并排写出来，
	// 一眼能看出「合并率」，也不会被误读成「有几条没落盘」。
	commits := 3*n + 1
	return benchScene{
		Name:   name,
		Commit: fmt.Sprintf("每行一事务 · %s fsync / %s 提交", benchComma(int(syncs)), benchComma(commits)),
		Rows:   n,
		PerSec: round3(float64(n) / median(secs)),
	}
}

// sceneUpdate 批量更新：整表每行都 UPDATE 一次，按 id 区间分几段跑，
// 避免一条 UPDATE 锁住整张表太久。固定「每行 +1」，各轮可重复。
func (s *Server) sceneUpdate(x *benchSess) benchScene {
	rowsV := scale()
	p := progPart{span: 1}
	warmup := func() error { return x.exec(`UPDATE ` + benchTable + ` SET v = v WHERE id = 1`) }
	fn := func() error {
		const seg = 100000
		for lo := 1; lo <= rowsV; lo += seg {
			if err := x.exec(fmt.Sprintf(`UPDATE %s SET v = v WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+seg-1)); err != nil {
				return err
			}
			// 一段十万行，几秒钟；报「已更新到第几行」比只报轮次实在
			if err := x.within(segPart(benchCap(lo+seg-1, rowsV), rowsV),
				fmt.Sprintf("已更新 %s / %s 行", benchComma(benchCap(lo+seg-1, rowsV)), benchComma(rowsV))); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, p, warmup, fn)
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

	// 这个场景一共 9 趟：预热（重灌 + 删）、3 轮（重灌 + 删）、收尾重灌。
	// 每趟各占九分之一 —— 重灌整张表就要几秒，不分段的话这个场景
	// 三分钟里进度条一动不动，正是「看着像卡死」的元凶。
	const phases = 9
	phase := 0
	next := func(label string) progPart {
		p := progPart{base: float64(phase) / phases, span: 1.0 / phases, label: label}
		phase++
		return p
	}
	del := func(p progPart) error {
		const seg = 100000
		for lo := 1; lo <= rowsV; lo += seg {
			if err := x.exec(fmt.Sprintf(`DELETE FROM %s WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+seg-1)); err != nil {
				return err
			}
			done := benchCap(lo+seg-1, rowsV)
			x.note(p.base+p.span*segPart(done, rowsV),
				fmt.Sprintf("%s已删除 %s / %s 行", p.pre(), benchComma(done), benchComma(rowsV)))
		}
		return nil
	}

	// 预热一轮：重灌 + 删一次，让表加载 / 索引冷启动都过去
	if err := s.benchSeed(x.d, x, next("预热 · 重灌")); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "重灌失败：" + err.Error()}
	}
	if err := del(next("预热 · 删光")); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: err.Error()}
	}

	secs := make([]float64, 0, 3)
	for i := 0; i < 3; i++ {
		if x.stopped() {
			return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: errBenchCancel.Error()}
		}
		if err := s.benchSeed(x.d, x, next(fmt.Sprintf("第 %d/3 轮 · 重灌", i+1))); err != nil { // 重灌，不计时
			return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "重灌失败：" + err.Error()}
		}
		start := time.Now()
		if err := del(next(fmt.Sprintf("第 %d/3 轮 · 删光", i+1))); err != nil {
			return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: err.Error()}
		}
		secs = append(secs, time.Since(start).Seconds())
	}
	// 最后一轮删光后重灌，供后面的点查/范围扫描/聚合继续用
	if err := s.benchSeed(x.d, x, next("收尾 · 重灌")); err != nil {
		return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光）", Error: "收尾重灌失败：" + err.Error()}
	}
	return benchScene{Name: "批量删除", Commit: "按 id 区间分段（删光，重灌不计入）", Rows: rowsV, PerSec: round3(float64(rowsV) / median(secs))}
}

// scenePointQuery 点查：按主键查单行，跑多次测索引点查吞吐。
func (s *Server) scenePointQuery(x *benchSess) benchScene {
	const n = 5000
	p := progPart{span: 1}
	warmup := func() error { return x.exec(`SELECT id FROM ` + benchTable + ` WHERE id = 1`) }
	fn := func() error {
		for i := 0; i < n; i++ {
			if err := x.exec(fmt.Sprintf(`SELECT id FROM %s WHERE id = %d`, benchTable, (i%scale())+1)); err != nil {
				return err
			}
			if i%1000 == 999 {
				if err := x.within(segPart(i+1, n), fmt.Sprintf("%s / %s 次", benchComma(i+1), benchComma(n))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	secs, err := s.timed(x, p, warmup, fn)
	if err != nil {
		return benchScene{Name: "点查（主键）", Commit: "单条 SELECT by PK", Error: err.Error()}
	}
	return benchScene{Name: "点查（主键）", Commit: "单条 SELECT by PK", Rows: n, PerSec: round3(float64(n) / median(secs))}
}

// sceneRangeScan 范围扫描：一次扫一个 id 区间并聚合，测顺序/索引范围扫描。
func (s *Server) sceneRangeScan(x *benchSess) benchScene {
	const n = 2000    // 每轮扫 2000 个区间
	const width = 100 // 每个区间 100 行（闭区间，所以是 lo+99）
	// 报「处理过多少行」而不是「跑了几次」：表头那一列是行数，2000 个区间实际
	// 命中 20 万行，只写 2000 会让人以为一次只碰一行。
	rows := n * width
	commit := fmt.Sprintf("区间 SELECT + count · 每段 %d 行", width)
	p := progPart{span: 1}
	warmup := func() error { return x.exec(`SELECT count(*) FROM ` + benchTable + ` WHERE id BETWEEN 1 AND 100`) }
	fn := func() error {
		for i := 0; i < n; i++ {
			lo := (i*width)%(scale()-width) + 1
			if err := x.exec(fmt.Sprintf(`SELECT count(*) FROM %s WHERE id BETWEEN %d AND %d`, benchTable, lo, lo+width-1)); err != nil {
				return err
			}
			if i%250 == 249 {
				if err := x.within(segPart(i+1, n), fmt.Sprintf("%s / %s 段", benchComma(i+1), benchComma(n))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	secs, err := s.timed(x, p, warmup, fn)
	if err != nil {
		return benchScene{Name: "范围扫描", Commit: commit, Error: err.Error()}
	}
	return benchScene{Name: "范围扫描", Commit: commit, Rows: rows, PerSec: round3(float64(rows) / median(secs))}
}

// sceneAggregate 聚合：全表 count + group by，测物化内存与哈希聚合。
//
// 口径和别的场景一致，报「处理过多少行 / 秒」：每次 GROUP BY 都要把整张表扫一遍，
// 跑 20 次就是 20 × 表行数。以前这里报的是「20 次查询 / 秒」，于是结果表里
// 「动了多少行」那一列显示 20、「条/秒」显示 61.8 —— 同一张表里两种单位，
// 看的人只能猜。现在换成行，和批量更新、向量 Top-K 这些能直接比。
func (s *Server) sceneAggregate(x *benchSess) benchScene {
	const times = 20
	rows := times * scale()
	commit := fmt.Sprintf("GROUP BY 全表 × %d 次", times)
	p := progPart{span: 1}
	warmup := func() error { return x.exec(`SELECT count(*) FROM ` + benchTable) }
	fn := func() error {
		for i := 0; i < times; i++ {
			if err := x.exec(`SELECT grp, count(*) FROM ` + benchTable + ` GROUP BY grp`); err != nil {
				return err
			}
			// 每次都要扫全表，每两次报一下
			if i%2 == 1 {
				if err := x.within(segPart(i+1, times), fmt.Sprintf("%s / %s 次", benchComma(i+1), benchComma(times))); err != nil {
					return err
				}
			}
		}
		return nil
	}
	secs, err := s.timed(x, p, warmup, fn)
	if err != nil {
		return benchScene{Name: "聚合", Commit: commit, Error: err.Error()}
	}
	return benchScene{Name: "聚合", Commit: commit, Rows: rows, PerSec: round3(float64(rows) / median(secs))}
}

// sceneVectorTopK 是「向量 Top-K」：目前没有近似索引，只能全表扫 + 逐行算距离。
//
// 报的口径和别的场景一致：每秒扫过多少行（墙钟）。它衡量的是「距离计算 + 扫描」
// 的合速度，所以做近似索引（ANN）之后，这个数会成倍上涨 —— 这就是它的用途。
// 建表 / 灌数据不计时（那是准备），和批量插入场景的口径一致。
//
// 默认 10 万行 × 128 维（约 51MB 的 .vec，灌一次几秒）。规模跟主表一致；要对外报
// 100 万档的数字时改 benchScale —— 见上面 benchVecTable 的注释。
func (s *Server) sceneVectorTopK(x *benchSess) benchScene {
	name := "向量 Top-K"
	n := benchVecN()
	commit := fmt.Sprintf("全表扫 %s 行 × %d 维 / 次，取最近 10 条", benchComma(n), benchVecDim)
	fail := func(err error) benchScene {
		return benchScene{Name: name, Commit: commit, Error: err.Error()}
	}

	// 上一次取消可能留下半张表；先清掉再建。
	if err := x.exec(`DROP TABLE IF EXISTS ` + benchVecTable); err != nil {
		return fail(err)
	}
	if err := x.exec(fmt.Sprintf(`CREATE TABLE %s (id int PRIMARY KEY, v vector(%d))`, benchVecTable, benchVecDim)); err != nil {
		return fail(err)
	}

	// 分批灌：一条 INSERT 1000 行（约 700KB）。再大只会让单条语句的解析时间
	// 喧宾夺主，再小则是把往返开销算进准备时间。
	//
	// 进度前缀要自己设：x.pre 还留着上一个场景的「第 3/3 轮」，灌数据时会出现
	// 「第 3/3 轮 · 灌数据 8,100 / 100,000 行」这种自相矛盾的说明。
	x.part = progPart{span: 1}
	x.round = -1
	x.pre = "准备向量表"
	const batch = 1000
	for start := 1; start <= n; start += batch {
		end := start + batch - 1
		if end > n {
			end = n
		}
		var b strings.Builder
		fmt.Fprintf(&b, "INSERT INTO %s VALUES ", benchVecTable)
		for i := start; i <= end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,'%s')", i, benchVecLiteral(benchVecDim, i))
		}
		if err := x.exec(b.String()); err != nil {
			return fail(err)
		}
		if err := x.within(float64(end)/float64(n),
			fmt.Sprintf("灌数据 %s / %s 行", benchComma(end), benchComma(n))); err != nil {
			return fail(err)
		}
	}

	// 每次查询取最近 10 条；每轮 5 次，够摊掉单次的抖动又不至于让场景变成几分钟。
	//
	// 计时前先 CHECKPOINT：没落盘时所有行还在 memtable 里（`td.rows` 直接命中），
	// 落盘后走「页 → 解码」这条路，同样的查询要慢好几倍（100 万行时 118ms vs 350ms）。
	// 基准要测的是**稳态**（真实使用里数据早就在页里），不是刚导入时的假快。
	if err := x.exec(`CHECKPOINT`); err != nil {
		return fail(err)
	}
	const perRound = 5
	query := fmt.Sprintf(`SELECT id FROM %s ORDER BY v <=> '%s' LIMIT 10`,
		benchVecTable, benchVecLiteral(benchVecDim, 7))
	p := progPart{span: 1}
	warmup := func() error { return x.exec(query) }
	fn := func() error {
		for i := 0; i < perRound; i++ {
			if err := x.exec(query); err != nil {
				return err
			}
			if err := x.within(float64(i+1)/perRound, fmt.Sprintf("%s / %d 次查询", benchComma(i+1), perRound)); err != nil {
				return err
			}
		}
		return nil
	}
	secs, err := s.timed(x, p, warmup, fn)
	if err != nil {
		return fail(err)
	}
	rows := n * perRound
	return benchScene{Name: name, Commit: commit, Rows: rows, PerSec: round3(float64(rows) / median(secs))}
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
		// 进程起来之后还没跑过基准：也要明确给出 running=false，
		// 前端和巡检脚本都是按这个字段判断的，少一个字段就变成 undefined。
		writeJSON(w, http.StatusOK, map[string]interface{}{"running": false, "result": nil})
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// 百分比 = (已完成的步数 + 这一步内部的比例) / 总步数。
	// 只按 done/total 算的话，一个三分钟的场景会让进度条整段不动。
	pct := 0.0
	if st.total > 0 {
		pct = (float64(st.done) + st.frac) / float64(st.total) * 100
		if pct > 100 {
			pct = 100
		}
	}
	resp := map[string]interface{}{
		"running":   st.running,
		"step":      st.step,
		"detail":    st.detail,
		"done":      st.done,
		"total":     st.total,
		"frac":      st.frac,
		"percent":   math.Round(pct*10) / 10,
		"elapsedMs": time.Since(st.startedAt).Milliseconds(),
		"result":    st.result,
	}
	if eta, ok := st.etaLocked(); ok {
		resp["etaMs"] = eta
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
