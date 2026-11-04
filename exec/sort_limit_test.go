package exec

import (
	"strings"
	"testing"

	"godb/types"
)

// rowSeqOp 产出 n 行单列整数，用来喂排序算子（真实表跑 200 万行太慢）。
type rowSeqOp struct {
	sc Schema
	n  int
	i  int
}

func (o *rowSeqOp) Schema() Schema { return o.sc }
func (o *rowSeqOp) Open() error    { o.i = 0; return nil }
func (o *rowSeqOp) Next() (types.Row, bool, error) {
	if o.i >= o.n {
		return nil, false, nil
	}
	o.i++
	return types.Row{types.Int(int64(o.i))}, true, nil
}
func (o *rowSeqOp) Close() error { return nil }

// 全排序没有 LIMIT 时，行数超限要立刻报错 —— 以前这里一条兜底都没有：
// `SELECT * FROM 大表 ORDER BY x` 会把整张表搬进内存，连接那边反而有保护。
func TestFullSortRefusesBeyondRowLimit(t *testing.T) {
	old := maxSortRows
	maxSortRows = 8
	defer func() { maxSortRows = old }()

	sc := Schema{{Name: "id", Type: types.Type{Kind: types.KInt, Name: "int"}}}
	newSort := func(n int) *sortOp {
		return &sortOp{
			child: &rowSeqOp{sc: sc, n: n},
			keys:  []sortKey{{eval: &colEval{idx: 0, t: sc[0].Type}}},
			sc:    sc,
		}
	}

	// 正好等于上限：允许（边界与连接那边一致 —— 拒绝的是「还想再放一行」）
	ok := newSort(int(maxSortRows))
	if err := ok.Open(); err != nil {
		t.Fatalf("等于上限不该报错：%v", err)
	}
	if got := len(ok.rows); got != int(maxSortRows) {
		t.Fatalf("缓存了 %d 行，期望 %d", got, maxSortRows)
	}

	// 多一行：拒绝，而且错误里要说清上限和该怎么办
	bad := newSort(int(maxSortRows) + 1)
	err := bad.Open()
	if err == nil {
		t.Fatal("超过上限应报错，实际成功")
	}
	msg := err.Error()
	if !strings.Contains(msg, "排序需要缓存超过 8 行") || !strings.Contains(msg, "LIMIT") {
		t.Fatalf("错误文案要给出上限与改法：%q", msg)
	}
}

// Top-N 不受这个上限影响：`ORDER BY … LIMIT k` 走的是大小为 k 的堆，
// 一百万行进来也只占 k 行。这条用来防止「一刀切」把正常分页也拦掉。
func TestTopNNotAffectedBySortLimit(t *testing.T) {
	old := maxSortRows
	maxSortRows = 4
	defer func() { maxSortRows = old }()

	sc := Schema{{Name: "id", Type: types.Type{Kind: types.KInt, Name: "int"}}}
	op := &sortOp{
		child: &rowSeqOp{sc: sc, n: 20},
		keys:  []sortKey{{eval: &colEval{idx: 0, t: sc[0].Type}}},
		sc:    sc,
		topN:  3,
	}
	if err := op.Open(); err != nil {
		t.Fatalf("Top-N 不该被全排序的上限拦住：%v", err)
	}
	if got := len(op.rows); got != 3 {
		t.Fatalf("Top-3 缓存了 %d 行，期望 3", got)
	}
}
