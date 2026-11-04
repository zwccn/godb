package db

import (
	"strconv"
	"testing"
)

// 示例数据里那张向量表：能被建出来、有数据、按距离能取到同一主题。
func TestSampleHasVectorTable(t *testing.T) {
	d := newTestDB(t)
	s := "sample"
	if err := d.LoadSample(d.Session(s)); err != nil {
		t.Fatalf("载入示例数据: %v", err)
	}

	// 9 篇文档，四类主题各 2～3 篇
	res := run(t, d, s, `SELECT count(*) FROM articles`)
	if got := fmtRow(rowsOf(t, res)[0]); got != "9" {
		t.Fatalf("articles 行数 = %s，期望 9", got)
	}

	// 用一篇「技术」文档的向量做查询：前 3 条应该都是 tech。
	// 断言主题而不是行序 —— 并列值的先后顺序 SQL 不保证。
	res = run(t, d, s, `SELECT topic FROM articles
		ORDER BY embedding <=> '[0.9,0.08,0.02,0,0.05,0.1,0.02,0.01]' LIMIT 3`)
	rows := rowsOf(t, res)
	if len(rows) != 3 {
		t.Fatalf("Top-3 返回 %d 行", len(rows))
	}
	for i, r := range rows {
		if r[0] != "tech" {
			t.Fatalf("第 %d 条主题是 %v，应为 tech（向量最近邻没落在同一主题上）", i+1, r[0])
		}
	}

	// 相似度分数：1 - 余弦距离，同一篇文档对它自己应当接近 1
	res = run(t, d, s, `SELECT 1 - (embedding <=> embedding) AS score FROM articles LIMIT 1`)
	got := fmtRow(rowsOf(t, res)[0])
	v, err := strconv.ParseFloat(got, 64)
	if err != nil || v < 0.9999 {
		t.Fatalf("自身相似度 = %s，期望 1（float32 精度内）", got)
	}

	// 向量列不能建索引 —— 示例表别埋一个「看起来能建」的坑
	if err := runErr(t, d, s, `CREATE INDEX articles_emb ON articles (embedding)`); err == nil {
		t.Fatal("向量列建索引应该被拒绝")
	}
}
