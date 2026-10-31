package db

import (
	"fmt"
	"strings"
	"testing"
)

// 页密度 = 1 - 页里可复用的空闲字节 / 页容量。
//
// 它的语义要说清楚：**洞不算空闲**。所以「删了一大批行但还没 VACUUM」时密度不会掉
// （那些字节还复用不起来）；VACUUM 把洞收成真实空闲之后密度才掉下来，于是这个数字
// 正好回答「现在有多少页是白占的」，而 VACUUM FULL 之后页数降下来、密度回升。
func TestSpaceFill(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 64, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d, "s", `CREATE TABLE sp (id INTEGER PRIMARY KEY, pad TEXT)`)
	pad := strings.Repeat("p", 200)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO sp VALUES `)
	for i := 1; i <= 20000; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'%s')", i, pad)
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `CHECKPOINT`)

	full := d.Store.Space()
	if full.Pages == 0 || full.DataBytes == 0 {
		t.Fatalf("2 万行应该已经落盘：%+v", full)
	}
	if full.Fill < 0.85 {
		t.Fatalf("刚批量插入的表应该很紧凑，页密度只有 %.2f（%+v）", full.Fill, full)
	}
	// 明细与汇总要自洽
	if ts := d.Store.TableSizes(); len(ts) != 1 || ts[0].Fill != full.Fill {
		t.Fatalf("单表密度应与全库一致：%+v vs %+v", ts, full)
	}

	// 删掉九成再整理：页数不变，但空闲变成「可复用」，密度掉下来
	run(t, d, "s", `DELETE FROM sp WHERE id % 10 <> 0`)
	run(t, d, "s", `CHECKPOINT`)
	run(t, d, "s", `VACUUM sp`)
	run(t, d, "s", `CHECKPOINT`)
	sparse := d.Store.Space()
	if sparse.Pages != full.Pages {
		t.Fatalf("VACUUM 只整理页内，页数不该变：%d → %d", full.Pages, sparse.Pages)
	}
	if sparse.Fill > 0.5 {
		t.Fatalf("整理后应该明显稀疏，页密度 %.2f", sparse.Fill)
	}

	// VACUUM FULL 整份重写：页数降下来，密度回到接近满
	res, err := d.Exec(d.Session("s"), `VACUUM FULL sp`)
	if err != nil {
		t.Fatalf("vacuum full: %v", err)
	}
	_ = res
	run(t, d, "s", `CHECKPOINT`)
	tight := d.Store.Space()
	if tight.Pages >= sparse.Pages {
		t.Fatalf("VACUUM FULL 之后页数应该降：%d → %d", sparse.Pages, tight.Pages)
	}
	// 单表密度不受「小表不参与全库统计」的阈值影响，用它看紧凑程度
	tsTight := d.Store.TableSizes()[0]
	if tsTight.Fill < 0.8 {
		t.Fatalf("重写之后应该很紧凑，单表页密度 %.2f（%+v）", tsTight.Fill, tsTight)
	}

	// 未加载的表也要报得出来（密度来自 meta 里记的空闲字节）
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	d2, err := Open(dir, Options{BufferPoolPages: 64, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	loaded := d2.Store.Space()
	if loaded.Pages != tight.Pages {
		t.Fatalf("重启后页数对不上：%d vs %d", loaded.Pages, tight.Pages)
	}
	if diff := loaded.Fill - tight.Fill; diff > 0.02 || diff < -0.02 {
		t.Fatalf("重启后（未加载表的 meta 值）密度漂了：%.3f vs %.3f", loaded.Fill, tight.Fill)
	}
}
