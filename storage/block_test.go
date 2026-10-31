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
