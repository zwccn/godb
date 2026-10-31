package db

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// 页浏览：地图、页内结构、原始字节、文本串。
//
// 这些接口是「打开黑箱」用的，读的是**磁盘上的页**（直接读文件、不进缓冲池），
// 所以要先把数据 checkpoint 下去再断言。
func TestPageBrowse(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{BufferPoolPages: 64, Lock: testOptions().Lock})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	run(t, d, "s", `CREATE TABLE br (id INTEGER PRIMARY KEY, sku TEXT, qty INTEGER)`)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO br VALUES `)
	for i := 1; i <= 500; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'SKU-%06d-观察-文本',%d)", i, i, i%9)
	}
	run(t, d, "s", sb.String())
	run(t, d, "s", `CREATE INDEX idx_br_sku ON br (sku)`)
	run(t, d, "s", `CHECKPOINT`)

	// 1) 数据页地图：页数对得上，存活元组数等于行数
	pm, err := d.Store.PageMap("br", "data", "", 0, 0)
	if err != nil {
		t.Fatalf("page map: %v", err)
	}
	if pm.Total == 0 || len(pm.Pages) != pm.Total {
		t.Fatalf("页地图不完整：total=%d pages=%d", pm.Total, len(pm.Pages))
	}
	live := 0
	for _, p := range pm.Pages {
		if p.Bad {
			t.Fatalf("第 %d 页被判成坏页", p.No)
		}
		live += p.Live
	}
	if live != 500 {
		t.Fatalf("页里存活元组合计 %d，期望 500", live)
	}
	if pm.Bytes != int64(pm.Total)*8192 {
		t.Fatalf("扫描字节数 %d 和页数 %d 对不上", pm.Bytes, pm.Total)
	}

	// 2) 分页：limit 小的时候 Next 要指到下一页
	if len(pm.Pages) > 2 {
		first, err := d.Store.PageMap("br", "data", "", 0, 2)
		if err != nil {
			t.Fatalf("page map limit: %v", err)
		}
		if len(first.Pages) != 2 || first.Next != 2 {
			t.Fatalf("分页不对：%d 页，next=%d", len(first.Pages), first.Next)
		}
		tail, err := d.Store.PageMap("br", "data", "", first.Next, 2)
		if err != nil || len(tail.Pages) == 0 || tail.Pages[0].No != 2 {
			t.Fatalf("续扫不对：%+v (%v)", tail.Pages, err)
		}
	}

	// 3) 索引页地图
	im, err := d.Store.PageMap("br", "index", "idx_br_sku", 0, 0)
	if err != nil {
		t.Fatalf("index map: %v", err)
	}
	if im.Total == 0 || len(im.Pages) == 0 {
		t.Fatalf("索引页地图是空的：%+v", im)
	}
	// 第 0 页是头页，不是树节点 —— 不分开的话它会显示成一个「空叶子」
	if im.Pages[0].Role != "header" {
		t.Fatalf("索引第 0 页应该是头页，实际 %q", im.Pages[0].Role)
	}
	hdr, err := d.Store.PageDetail("br", "index", "idx_br_sku", 0)
	if err != nil || hdr.Hdr == nil || hdr.Hdr.Count != 500 || hdr.Hdr.Height < 1 {
		t.Fatalf("索引头页不对：%+v (%v)", hdr, err)
	}

	// 4) 单页详情：页头 + 元组解码 + 原始字节 + 文本串
	det, err := d.Store.PageDetail("br", "data", "", 0)
	if err != nil {
		t.Fatalf("page detail: %v", err)
	}
	if det.CRC != "ok" || det.Data == nil || len(det.Data.Entries) == 0 {
		t.Fatalf("页详情不完整：%+v", det)
	}
	if raw, err := base64.StdEncoding.DecodeString(det.Raw); err != nil || len(raw) != 8192 {
		t.Fatalf("原始字节不是 8KB：%d (%v)", len(raw), err)
	}
	// 第一行的三列应该解出来：id、sku、qty
	var sawSKU bool
	for _, e := range det.Data.Entries {
		if !e.Live || len(e.Cols) < 3 {
			continue
		}
		if strings.HasPrefix(e.Cols[1], "SKU-") {
			sawSKU = true
			break
		}
	}
	if !sawSKU {
		t.Fatalf("元组里的 sku 没解出来：%+v", det.Data.Entries[:1])
	}
	// 文本视图里应该能看到业务文本
	var sawText bool
	for _, tr := range det.Texts {
		if strings.Contains(tr.Text, "观察") {
			sawText = true
			break
		}
	}
	if !sawText {
		t.Fatalf("页里的中文文本没被识别出来（共 %d 段）：%+v", len(det.Texts), det.Texts)
	}

	// 5) 索引页详情：键能解码，叶子项的值是 RID
	ileaf := -1
	for _, p := range im.Pages {
		if p.Role == "leaf" && p.Live > 0 {
			ileaf = int(p.No)
			break
		}
	}
	if ileaf < 0 {
		t.Fatal("索引里没有非空页")
	}
	id, err := d.Store.PageDetail("br", "index", "idx_br_sku", uint32(ileaf))
	if err != nil {
		t.Fatalf("index page detail: %v", err)
	}
	if id.Idx == nil || len(id.Idx.Entries) == 0 {
		t.Fatalf("索引页没有条目：%+v", id)
	}
	e0 := id.Idx.Entries[0]
	if len(e0.Vals) != 1 || !strings.HasPrefix(e0.Vals[0], "SKU-") {
		t.Fatalf("索引键没解对：%+v", e0)
	}
	if int(e0.Val) >= 500 {
		t.Fatalf("叶子项的 RID 越界：%d", e0.Val)
	}

	// 6) 错误路径
	if _, err := d.Store.PageDetail("br", "data", "", uint32(pm.Total)); err == nil {
		t.Fatal("越界页号应该报错")
	}
	if _, err := d.Store.PageMap("br", "index", "不存在", 0, 0); err == nil {
		t.Fatal("不存在的索引应该报错")
	}
}
