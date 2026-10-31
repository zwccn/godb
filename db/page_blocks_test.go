package db

import (
	"fmt"
	"strings"
	"testing"
)

// 块级页地图：几十万页的表不能「一格一页」交给前端（20 万个 DOM 节点布局就卡死），
// 所以先按 step 页合成一块，只给统计量。这里要保证两件事：
//
//  1. 聚合不丢数、不重复：块内各字段之和 == 逐页之和，块内页数之和 == 总页数；
//  2. 自适应分档对：小表逐页，大表按块，块数永远不超过 pageMapMaxBlocks。
func TestPageMapBlocks(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 64, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	run(t, d, "s", `CREATE TABLE blk (id INTEGER PRIMARY KEY, v TEXT)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO blk VALUES `)
	for i := 1; i <= 30000; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'v-%05d')", i, i)
	}
	run(t, d, "s", sb.String())
	// 删一段，制造洞：块视图要能标出「哪一块里有洞」才有的下钻
	run(t, d, "s", `DELETE FROM blk WHERE id % 7 = 0`)
	run(t, d, "s", `CHECKPOINT`)

	// ---- 1) 自适应：页数在上限内 → 逐页，Step 保持 0 ----
	pm, err := d.Store.PageMap("blk", "data", "", 0, 0, 0)
	if err != nil {
		t.Fatalf("PageMap: %v", err)
	}
	if pm.Total == 0 {
		t.Fatal("表已 checkpoint，应该有页")
	}
	if pm.Step != 0 || len(pm.Blocks) != 0 {
		t.Fatalf("%d 页（未超上限）应该逐页返回，实际 Step=%d Blocks=%d",
			pm.Total, pm.Step, len(pm.Blocks))
	}
	// 逐页视图一次最多 pageMapMaxPages；不够就继续翻
	var pageSum struct{ live, used, free, slots, holes int }
	for {
		for _, p := range pm.Pages {
			pageSum.live += p.Live
			pageSum.used += p.Used
			pageSum.free += p.Free
			pageSum.slots += p.Slots
			if p.Hole > 0 {
				pageSum.holes++
			}
		}
		if pm.Next == 0 {
			break
		}
		pm, err = d.Store.PageMap("blk", "data", "", pm.Next, 0, 0)
		if err != nil {
			t.Fatalf("PageMap(next): %v", err)
		}
	}

	// ---- 2) 强制块级：块内合计必须和逐页合计一致 ----
	bm, err := d.Store.PageMap("blk", "data", "", 0, 0, 64)
	if err != nil {
		t.Fatalf("PageMap(step=64): %v", err)
	}
	if bm.Step != 64 || len(bm.Blocks) == 0 {
		t.Fatalf("step=64 应该返回块视图，实际 Step=%d Blocks=%d", bm.Step, len(bm.Blocks))
	}
	if got, want := len(bm.Blocks), (bm.Total+63)/64; got != want {
		t.Fatalf("块数 %d，期望 %d（共 %d 页）", got, want, bm.Total)
	}
	var blkSum struct{ pages, live, used, free, slots, holes, bad int }
	for i, b := range bm.Blocks {
		if int(b.From) != i*64 {
			t.Fatalf("第 %d 块的起始页是 %d", i, b.From)
		}
		if int(b.To)-int(b.From) != b.Pages || b.Pages > 64 || b.Pages <= 0 {
			t.Fatalf("第 %d 块页数不对：%d..%d pages=%d", i, b.From, b.To, b.Pages)
		}
		if b.Fill < 0 || b.Fill > 100 {
			t.Fatalf("第 %d 块 Fill=%d，应在 0..100", i, b.Fill)
		}
		if b.Holes+b.Empty+b.Bad > b.Pages {
			t.Fatalf("第 %d 块分类页数超过总页数：holes=%d empty=%d bad=%d pages=%d",
				i, b.Holes, b.Empty, b.Bad, b.Pages)
		}
		blkSum.pages += b.Pages
		blkSum.live += b.Live
		blkSum.used += b.Used
		blkSum.free += b.Free
		blkSum.slots += b.Slots
		blkSum.holes += b.Holes
		blkSum.bad += b.Bad
	}
	if blkSum.pages != bm.Total {
		t.Fatalf("块内页数之和 %d ≠ 总页数 %d", blkSum.pages, bm.Total)
	}
	if blkSum.live != pageSum.live || blkSum.used != pageSum.used ||
		blkSum.free != pageSum.free || blkSum.slots != pageSum.slots {
		t.Fatalf("块聚合与逐页不一致：块 live/used/free/slots = %d/%d/%d/%d，逐页 = %d/%d/%d/%d",
			blkSum.live, blkSum.used, blkSum.free, blkSum.slots,
			pageSum.live, pageSum.used, pageSum.free, pageSum.slots)
	}
	// 「有洞的页数」是最该拿来下钻的那个信号，块级必须和逐页对得上。
	// 注意不能断言「一定有洞」：checkpoint 落盘时页会被紧凑重写，删掉的槽位直接消失，
	// 洞只在「页内改短但没整理」时才留得下 —— 所以这里只比一致性。
	if blkSum.holes != pageSum.holes {
		t.Fatalf("块视图标出的洞页数 %d ≠ 逐页统计 %d", blkSum.holes, pageSum.holes)
	}
	if blkSum.bad != 0 {
		t.Fatalf("页头不该坏，实际 %d 块有坏页", blkSum.bad)
	}
	// 块视图一次给完：Next 必须是 0，否则前端还得多点一次
	if bm.Next != 0 {
		t.Fatalf("块视图应该一次给完，Next=%d", bm.Next)
	}
}
