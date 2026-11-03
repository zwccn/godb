package exec

import (
	"testing"

	"godb/types"
)

// dummyOp 是最小的 Operator：只提供 schema，不产出任何行。
// 用来给物化算子当 child —— ExplainTree 不调 Open/Next，只问 Schema 和 Describe。
type dummyOp struct{ sc Schema }

func (d *dummyOp) Schema() Schema                 { return d.sc }
func (d *dummyOp) Open() error                    { return nil }
func (d *dummyOp) Next() (types.Row, bool, error) { return nil, false, nil }
func (d *dummyOp) Close() error                   { return nil }

// TestMemRowBytesMatchesValueLayout 钉住折算公式的两个常数。
//
// 这两个数字不是拍的：types.Value 实测 32 字节（K 1 + z 2 + 对齐 + I 8 + S 16），
// 一个 []Value 的 slice 头 24 字节。将来有人把 Value 改大（加字段）却忘了改这里，
// 探测面板上的内存就会系统性偏低 —— 所以让它在测试里红，而不是在线上悄悄错。
func TestMemRowBytesMatchesValueLayout(t *testing.T) {
	if got := memRowBytes(0); got != memRowHead {
		t.Fatalf("0 列的行应该是 slice 头 %d，得到 %d", memRowHead, got)
	}
	for cols := 1; cols <= 8; cols++ {
		want := int64(memRowHead + memValueBytes*cols)
		if got := memRowBytes(cols); got != want {
			t.Fatalf("%d 列的行应该是 %d，得到 %d", cols, want, got)
		}
	}
	// 负数列数不该算出比行头还小的值（防御性：schema 为空的边界）
	if got := memRowBytes(-3); got != memRowHead {
		t.Fatalf("负数列数应退化成行头，得到 %d", got)
	}
}

// TestMemRowsMaterializingOps 验证「谁报、报多少」。
//
// 报的是**攒下的行数**，不是输出行数：HashAggregate 吃 30 万行吐 20 组，
// 内存按 20 组算；Sort / 连接则是全量。
func TestMemRowsMaterializingOps(t *testing.T) {
	sc := Schema{{Name: "a"}, {Name: "b"}}

	t.Run("排序报物化的行数", func(t *testing.T) {
		op := &sortOp{child: &dummyOp{sc: sc}, sc: sc, rows: make([]sortEntry, 300)}
		n := ExplainTree(op)
		if n.MemRows != 300 {
			t.Fatalf("MemRows 应为 300，得到 %d", n.MemRows)
		}
		if want := int64(300) * memRowBytes(2); n.MemBytes != want {
			t.Fatalf("MemBytes 应为 %d，得到 %d", want, n.MemBytes)
		}
	})

	t.Run("去重报缓冲行数", func(t *testing.T) {
		op := &distinctOp{child: &dummyOp{sc: sc}, sc: sc, buf: make([]types.Row, 42)}
		n := ExplainTree(op)
		if n.MemRows != 42 {
			t.Fatalf("MemRows 应为 42，得到 %d", n.MemRows)
		}
	})

	t.Run("聚合报分组数而不是输入行数", func(t *testing.T) {
		// rows 是输出（分组结果）；输入行数不进内存估算
		op := &aggOp{child: &dummyOp{sc: sc}, sc: sc, rows: make([]types.Row, 7)}
		n := ExplainTree(op)
		if n.MemRows != 7 {
			t.Fatalf("MemRows 应为 7（分组数），得到 %d", n.MemRows)
		}
	})

	t.Run("连接报两侧之和", func(t *testing.T) {
		op := &joinOp{
			left:      &dummyOp{sc: sc},
			right:     &dummyOp{sc: sc},
			lsc:       sc,
			rsc:       sc,
			leftRows:  make([]types.Row, 12),
			rightRows: make([]types.Row, 4),
		}
		n := ExplainTree(op)
		if n.MemRows != 16 {
			t.Fatalf("MemRows 应为 16（12+4），得到 %d", n.MemRows)
		}
	})

	t.Run("流式算子不报", func(t *testing.T) {
		// 边拉边吐的算子不该出现在内存清单里，否则读者会以为它们也占内存
		n := ExplainTree(&dummyOp{sc: sc})
		if n.MemRows != 0 || n.MemBytes != 0 {
			t.Fatalf("流式算子不应报内存，得到 rows=%d bytes=%d", n.MemRows, n.MemBytes)
		}
	})
}
