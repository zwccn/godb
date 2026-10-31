package storage

import "testing"

// blockStep：块数必须压到 pageMapMaxBlocks 以内，且步长只增不减。
func TestBlockStep(t *testing.T) {
	cases := []struct {
		pages int
		want  int
	}{
		{1, blockStepBase},        // 页很少也按 256 一块（无所谓，反正一块）
		{256, blockStepBase},      // 正好一块
		{1000000, blockStepBase},  // 1000000/256 = 3907 块，还在上限内
		{2000000, 512},            // 2000000/256 = 7813 超了 → 翻倍
		{10000000, 4096},          // 需要翻到 4096 才压到 2442 块
	}
	for _, c := range cases {
		got := blockStep(c.pages)
		if got != c.want {
			t.Errorf("blockStep(%d) = %d，期望 %d", c.pages, got, c.want)
		}
		if n := (c.pages + got - 1) / got; n > pageMapMaxBlocks {
			t.Errorf("blockStep(%d) 得到 %d 块，超过上限 %d", c.pages, n, pageMapMaxBlocks)
		}
	}
}

// 「这块值不值得整理一次」的判据必须和 tablepages.go 的 needsVacuum 同口径：
// 洞超过容量 1/4 才算，且小表（< 8 页）不折腾。
// 两处不一样就会出现「存储页说整库不用整理、空间地图说这块该整理」的矛盾。
func TestPageBlockAdvice(t *testing.T) {
	cases := []struct {
		name  string
		pages int
		hole  int
		want  string
	}{
		{"256 页、洞正好 1/4", 256, PageSize * 256 / 4, ""},
		{"256 页、洞多 1 字节就超阈值", 256, PageSize*256/4 + 1, "vacuum"},
		{"256 页、几乎全是洞", 256, PageSize * 250, "vacuum"},
		{"4 页、全是洞也算了（小表不折腾）", 4, PageSize * 4, ""},
		{"8 页、全是洞", 8, PageSize * 8, "vacuum"},
		{"256 页、没有洞", 256, 0, ""},
	}
	for _, c := range cases {
		b := PageBlock{Pages: c.pages, HoleBytes: c.hole}
		b.finish()
		if b.Advice != c.want {
			t.Errorf("%s：advice=%q，期望 %q（洞 %d B / 容量 %d B）",
				c.name, b.Advice, c.want, c.hole, c.pages*PageSize)
		}
	}
}
