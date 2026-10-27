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
	on   bool
	rows int64
	ns   int64
}

// Start 在句子的开头调一次；没开统计时返回零值，调用方照样往下走。
type timer struct {
	p  *OpProfile
	t0 hrtime.Stamp
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
	p.rows, p.ns = 0, 0
}

func (p *OpProfile) profile() (int64, int64) { return p.rows, p.ns }

// begin 开始计时。没开统计时返回的 timer 什么都不做（零开销的分支预测）。
func (p *OpProfile) begin() timer {
	if p == nil || !p.on {
		return timer{}
	}
	return timer{p: p, t0: hrtime.Now()}
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
}

// setProfiling 递归打开/关闭整棵树的统计。
func setProfiling(op Operator, on bool) {
	if p, ok := op.(profiler); ok {
		p.enableProfile(on)
	}
	if pn, ok := op.(planNode); ok {
		for _, c := range pn.Children() {
			setProfiling(c, on)
		}
	}
}

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
func ExplainAnalyze(op Operator) ([]string, error) {
	setProfiling(op, true)
	defer setProfiling(op, false)
	if err := op.Open(); err != nil {
		return nil, err
	}
	for {
		_, ok, err := op.Next()
		if err != nil {
			op.Close()
			return nil, err
		}
		if !ok {
			break
		}
	}
	if err := op.Close(); err != nil {
		return nil, err
	}
	return renderPlan(op, true), nil
}

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
