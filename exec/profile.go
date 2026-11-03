package exec

import (
	"fmt"
	"strings"

	"godb/hrtime"
	"godb/types"
)

// 这个文件是「为什么慢」的观测口子。
//
// 加它不是因为现在慢，而是因为慢的时候得能定位到**哪个算子、多少行、多少时间**，
// 以及规划器当初**为什么**做了这个选择。没有这两样，优化就只能靠猜。
//
// 分两层：
//   - OpProfile：算子自己记「吐了多少行、花了多少时间」。默认关闭，
//     只有 EXPLAIN ANALYZE 打开 —— 计时本身有成本（每行一次 hrtime.Now），
//     不能让正常查询背着它跑。
//   - Detail：算子用文字讲清自己的决策（走不走索引、用了哪种连接算法、估算多少行）。
//     不用开关，EXPLAIN 就直接打出来。
type OpProfile struct {
	on    bool
	rows  int64
	loops int64
	ns    int64
	// io 由 setProfiling 注入（storage.Store 实现）。它为 nil 时所有 I/O 记账跳过。
	io      ioStat
	ioNs    int64
	ioPages int64
	ioBytes int64
}

// ioStat 是「读页阻塞」的计口。exec 不关心它怎么实现，只要能在任意时刻取快照 ——
// 算子在自己每次调用前后各取一次，差值就是这次调用等页文件等了多久。
type ioStat interface {
	IOSnapshot() (ns, pages, bytes int64)
}

// Start 在句子的开头调一次；没开统计时返回零值，调用方照样往下走。
type timer struct {
	p   *OpProfile
	t0  hrtime.Stamp
	io0 int64
	pg0 int64
	b0  int64
}

// profiler 是算子可选的统计能力。实现了它，EXPLAIN ANALYZE 才能看到行数与耗时。
type profiler interface {
	enableProfile(bool)
	profile() (rows, ns int64)
}

// detailer 是算子可选的「决策说明」。实现它，EXPLAIN 会把说明接在节点行后面。
type detailer interface {
	ExplainDetail() []string
}

func (p *OpProfile) enableProfile(on bool) {
	p.on = on
	p.rows, p.loops, p.ns = 0, 0, 0
	p.ioNs, p.ioPages, p.ioBytes = 0, 0, 0
}

func (p *OpProfile) profile() (int64, int64) { return p.rows, p.ns }

// loop 记一次「重新启动子节点」。
//
// 只有嵌套循环类算子用得上：哈希连接把右表建一次哈希表就算完，而索引连接
// 是「外表每来一行就点查一次内表」—— 那个「跑了几轮」不记下来，就只能看到
// 一个总行数，看不出代价是线性堆起来的。profile 关着时是一次空指针判断。
func (p *OpProfile) loop() {
	if p != nil && p.on {
		p.loops++
	}
}

// begin 开始计时。没开统计时返回的 timer 什么都不做（零开销的分支预测）。
func (p *OpProfile) begin() timer {
	if p == nil || !p.on {
		return timer{}
	}
	t := timer{p: p, t0: hrtime.Now()}
	if p.io != nil {
		t.io0, t.pg0, t.b0 = p.io.IOSnapshot()
	}
	return t
}

// done 结算一次调用：ok 表示这次真的吐了一行。
func (t timer) done(ok bool) {
	if t.p == nil {
		return
	}
	t.p.ns += int64(hrtime.Since(t.t0))
	if ok {
		t.p.rows++
	}
	if t.p.io != nil {
		ns, pages, bytes := t.p.io.IOSnapshot()
		t.p.ioNs += ns - t.io0
		t.p.ioPages += pages - t.pg0
		t.p.ioBytes += bytes - t.b0
	}
}

// setProfiling 递归打开/关闭整棵树的统计。
func setProfiling(op Operator, on bool, io ioStat) {
	if p, ok := op.(profiler); ok {
		p.enableProfile(on)
		if pp, ok := op.(interface{ setIO(ioStat) }); ok {
			pp.setIO(io)
		}
	}
	if pn, ok := op.(planNode); ok {
		for _, c := range pn.Children() {
			setProfiling(c, on, io)
		}
	}
}

// setIO 由 OpProfile 提供，setProfiling 通过类型断言调用：
// 每个算子都嵌了 OpProfile，所以只要嵌了就有这个方法。
func (p *OpProfile) setIO(io ioStat) { p.io = io }

// ExplainPlan 渲染执行计划。
//
// 每个节点一行，节点自己的决策说明接在 `|` 后面（用 `;` 分隔），这样既有的
// 「计划里包含某段文字」的断言仍然成立，同时能一眼看到为什么走/不走索引。
func ExplainPlan(op Operator) []string {
	return renderPlan(op, false)
}

// ExplainAnalyze 跑一遍计划，返回带「实际行数 + 耗时」的计划。
//
// 结果集会被丢弃（EXPLAIN ANALYZE 只关心统计），所以只用于 SELECT。
// 第二个返回值是同一份统计的结构化版本（给 Web 的「探测执行」用）；
// 文本版一个字都没改 —— PG 客户端和既有断言都还依赖那段格式。
func ExplainAnalyze(op Operator, io ioStat) ([]string, *PlanNode, error) {
	setProfiling(op, true, io)
	defer setProfiling(op, false, nil)
	if err := op.Open(); err != nil {
		return nil, nil, err
	}
	for {
		_, ok, err := op.Next()
		if err != nil {
			op.Close()
			return nil, nil, err
		}
		if !ok {
			break
		}
	}
	if err := op.Close(); err != nil {
		return nil, nil, err
	}
	return renderPlan(op, true), ExplainTree(op), nil
}

// PlanNode 是执行计划的结构化版本：一台「每步花了多少」的账。
//
// 只看 TotalNs 会误判：父节点的耗时里含子节点，越往上数字越大，看起来永远
// 是根节点最贵。真正要回答「时间花在哪」的是 SelfNs（独占时间）：父的总时长
// 减掉所有子节点的总时长。火山模型里父算子的每次调用都包住子调用，这个减法
// 是成立的（父子时钟严格嵌套），不需要在每次调用时做指针记账。
type PlanNode struct {
	Name string `json:"name"`
	// Detail 是算子自己写的决策说明（走没走索引、为什么没走、哪种连接算法）。
	Detail []string `json:"detail,omitempty"`
	// Rows 是「往上吐了多少行」。
	Rows int64 `json:"rows"`
	// Loops 是子节点被重新启动的次数（嵌套循环跑了几轮），0 表示不适用。
	Loops int64 `json:"loops,omitempty"`
	// Scanned 是从堆表里真读了多少行 —— 只对全表扫描有意义（读了 5 万行才吐 1 行）。
	Scanned int64 `json:"scanned,omitempty"`
	// MemRows / MemBytes 是「这一步在内存里攒着多少」。
	//
	// 只有把输入整份物化的算子才有值：Sort、Distinct、HashAggregate、哈希连接。
	// 流式算子（扫描、Filter、Project）边拉边吐，不在这里报。
	//
	// 为什么不用 Rows：物化算子的输出往往比它攒的少得多 —— HashAggregate 吃 30 万行
	// 只吐 20 个分组。只看 Rows 的话，「这一步占了 100MB」这件事永远不会显示。
	//
	// MemBytes 是**下限**：只算行本身（slice 头 + 每列一个 Value），不含排序键、
	// 哈希桶、map 的桶数组。前端必须显示成「约」。
	MemRows  int64 `json:"memRows,omitempty"`
	MemBytes int64 `json:"memBytes,omitempty"`
	// TotalNs 含所有子节点；SelfNs 是独占时间。
	TotalNs int64 `json:"totalNs"`
	SelfNs  int64 `json:"selfNs"`
	// IONs / IOPages 是这次调用里等页文件的墙钟与页数（同样含子节点）；
	// SelfIONs 是独占的那部分。默认配置下多半是 0：数据全在内存，根本没读盘。
	IONs        int64 `json:"ioNs,omitempty"`
	IOPages     int64 `json:"ioPages,omitempty"`
	IOBytes     int64 `json:"ioBytes,omitempty"`
	SelfIONs    int64 `json:"selfIoNs,omitempty"`
	SelfIOPages int64 `json:"selfIoPages,omitempty"`
	SelfIOBytes int64 `json:"selfIoBytes,omitempty"`

	Children []*PlanNode `json:"children,omitempty"`
}

// scannedReporter 是「我从堆表读了多少行」的口子。
// 只有全表扫描实现它：索引扫描吐多少行就读多少行，两者没有差额。
type scannedReporter interface {
	ScanRows() int64
}

// memReporter 是「这一步内存里攒着多少行」的口子。
//
// 各算子报**行数**，字节数由 ExplainTree 统一折算（见 memRowBytes）。不让算子
// 自己算字节：口径一分散，探测面板上的数字就没法横向比了 —— 同一个「12 MB」，
// 一个算的是行、一个算的是字节缓冲，读的人没法判断哪个更危险。
//
// 只报「整份物化」的算子：Sort / Distinct / HashAggregate / 哈希连接。
// 扫描、Filter、Project 是边拉边吐的，报了只会让人以为它们也占内存。
type memReporter interface {
	MemRows() int64
}

// memRowBytes 是一行在堆上的下限：slice 头 24 字节 + 每列一个 types.Value（32 字节）。
//
// 这是**下限**，不是估算的「差不多」：排序键、哈希桶、map 的桶数组、以及行里
// 字符串的实际内容都不在内。真实占用只会更高，所以展示时必须说「约」。
// 基准来自实测（unsafe.Sizeof: Value=32、Row=24），不是拍脑袋的系数。
const memRowHead = 24
const memValueBytes = 32

func memRowBytes(cols int) int64 {
	if cols < 0 {
		cols = 0
	}
	return int64(memRowHead + memValueBytes*cols)
}

// ExplainTree 把算子树的统计摊成结构化的树。
//
// 统计没开时（普通执行也会调一次来填 Result.Plan）行数与耗时全是 0，
// 前端据此只当成「计划长什么样」，不会拿 0 去算命。
func ExplainTree(op Operator) *PlanNode {
	var walk func(o Operator) *PlanNode
	walk = func(o Operator) *PlanNode {
		n := &PlanNode{Name: fmt.Sprintf("%T", o)}
		kids := []Operator(nil)
		if pn, ok := o.(planNode); ok {
			n.Name = pn.Describe()
			kids = pn.Children()
		}
		if p, ok := o.(profiler); ok {
			rows, ns := p.profile()
			n.Rows, n.TotalNs = rows, ns
			if pl, ok := o.(loopProfiler); ok {
				n.Loops = pl.profileLoops()
			}
			if ip, ok := o.(ioProfiler); ok {
				n.IONs, n.IOPages, n.IOBytes = ip.profileIO()
			}
		}
		if sr, ok := o.(scannedReporter); ok {
			n.Scanned = sr.ScanRows()
		}
		// 内存里攒了多少。只问「行数」，字节在这里统一折算 —— 口径放在一处，
		// 探测面板上各行的数字才能横向比（见 memReporter / memRowBytes 的注释）。
		if mr, ok := o.(memReporter); ok {
			if rows := mr.MemRows(); rows > 0 {
				n.MemRows = rows
				n.MemBytes = rows * memRowBytes(len(o.Schema()))
			}
		}
		if d, ok := o.(detailer); ok {
			n.Detail = d.ExplainDetail()
		}
		var kidNs, kidIo, kidPages, kidBytes int64
		for _, k := range kids {
			kn := walk(k)
			if kn == nil {
				continue
			}
			kidNs += kn.TotalNs
			kidIo += kn.IONs
			kidPages += kn.IOPages
			kidBytes += kn.IOBytes
			n.Children = append(n.Children, kn)
		}
		n.SelfIONs = n.IONs - kidIo
		if n.SelfIONs < 0 {
			n.SelfIONs = 0
		}
		n.SelfIOPages = n.IOPages - kidPages
		if n.SelfIOPages < 0 {
			n.SelfIOPages = 0
		}
		n.SelfIOBytes = n.IOBytes - kidBytes
		if n.SelfIOBytes < 0 {
			n.SelfIOBytes = 0
		}
		n.SelfNs = n.TotalNs - kidNs
		if n.SelfNs < 0 {
			// 子节点比父节点还久只可能是「父没计时、子计了」或者时钟回退，
			// 与其报个负数让人猜，不如认成 0。
			n.SelfNs = 0
		}
		return n
	}
	return walk(op)
}

// loopProfiler 是轮次统计的口子（只有嵌套循环实现）。
type loopProfiler interface {
	profileLoops() int64
}

func (p *OpProfile) profileLoops() int64 { return p.loops }

// ioProfiler 是「等页文件等了多久」的口子。字节数只对大字段有意义（不计页数）。
type ioProfiler interface {
	profileIO() (ns, pages, bytes int64)
}

func (p *OpProfile) profileIO() (int64, int64, int64) { return p.ioNs, p.ioPages, p.ioBytes }

func renderPlan(op Operator, withStats bool) []string {
	var out []string
	var walk func(o Operator, depth int)
	walk = func(o Operator, depth int) {
		label := fmt.Sprintf("%T", o)
		var kids []Operator
		if pn, ok := o.(planNode); ok {
			label = pn.Describe()
			kids = pn.Children()
		}
		if withStats {
			if p, ok := o.(profiler); ok {
				rows, ns := p.profile()
				label += fmt.Sprintf("  (rows=%d time=%s)", rows, fmtTook(ns))
			}
		}
		if d, ok := o.(detailer); ok {
			if items := d.ExplainDetail(); len(items) > 0 {
				label += "  | " + strings.Join(items, "; ")
			}
		}
		out = append(out, strings.Repeat("  ", depth)+label)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	walk(op, 0)
	return out
}

// fmtTook 把纳秒渲染成人能读的时长。小于时钟步进时给上界，不假装精确。
func fmtTook(ns int64) string {
	switch {
	case ns <= 0:
		return "0"
	case ns < 1000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.1fµs", float64(ns)/1e3)
	default:
		return fmt.Sprintf("%.2fms", float64(ns)/1e6)
	}
}

// keyDigest 把索引键值渲染成一行短文本，只给 EXPLAIN 看。
// 值多了会截断（0xFF 拼接后的真实键没法读，也没必要读）。
func keyDigest(vals []types.Value) string {
	const maxShow = 3
	parts := make([]string, 0, len(vals))
	for i, v := range vals {
		if i >= maxShow {
			parts = append(parts, "…")
			break
		}
		if v.IsNull() {
			parts = append(parts, "NULL")
			continue
		}
		parts = append(parts, fmt.Sprint(v.Interface()))
	}
	return strings.Join(parts, ", ")
}
