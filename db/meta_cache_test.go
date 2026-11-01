package db

import (
	"fmt"
	"testing"
)

// 目录快照（syscat.cachedMeta）是按 Catalog.Gen 作废的。这里从外部行为验证：
// **任何一种 DDL 之后，pg_catalog 里必须立刻反映新结构**。
//
// 为什么值得单列一组用例：缓存失效错了不会报错，只会让客户端看到旧的表结构 ——
// Navicat 里列死活不出现、ORM 生成 DELETE 少一列，都从这里开始。
func TestMetadataCacheFollowsDDL(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	count := func(q string) int {
		t.Helper()
		res := run(t, d, s, q)
		var n int
		if _, err := fmt.Sscan(fmt.Sprint(res[0].Rows[0][0]), &n); err != nil {
			t.Fatalf("count 查询返回了非数字 %v", res[0].Rows[0][0])
		}
		return n
	}

	// 先查一次，把快照烧热 —— 不查的话缓存放不进东西，后面的断言验不到
	count(`SELECT count(*) FROM pg_class`)

	// 按名字查，不数总行数：pg_class 里索引也占行，一张带主键的表会多两行，
	// 数总数等于把「索引是否登记」也绑进这条断言里了。
	run(t, d, s, `CREATE TABLE mc (id int4 PRIMARY KEY, a text)`)
	if got := count(`SELECT count(*) FROM pg_class WHERE relname = 'mc'`); got != 1 {
		t.Fatalf("建表后 pg_class 里查不到 mc（%d 行），目录快照没失效", got)
	}

	cols := func() int {
		t.Helper()
		return count(`SELECT count(*) FROM pg_attribute WHERE attrelid = 'mc'::regclass`)
	}
	if got := cols(); got != 2 {
		t.Fatalf("新建表应有 2 列，实际 %d", got)
	}

	// ADD COLUMN：ALTER 是原地改 *catalog.Table，不看代次就会一直报 2 列
	run(t, d, s, `ALTER TABLE mc ADD COLUMN b int4`)
	if got := cols(); got != 3 {
		t.Fatalf("ADD COLUMN 后应有 3 列，实际 %d（快照没失效）", got)
	}

	// RENAME COLUMN：列名出现在 pg_attribute.attname 里
	run(t, d, s, `ALTER TABLE mc RENAME COLUMN b TO c`)
	res := run(t, d, s, `SELECT attname FROM pg_attribute WHERE attrelid = 'mc'::regclass ORDER BY attnum`)
	var names []string
	for _, r := range res[0].Rows {
		names = append(names, fmt.Sprint(r[0]))
	}
	if len(names) != 3 || names[2] != "c" {
		t.Fatalf("改名后列名 = %v，期望最后一列是 c", names)
	}

	// DROP TABLE：表得从 pg_class 里消失（不然客户端还会去连一张不存在的表）
	run(t, d, s, `DROP TABLE mc`)
	if got := count(`SELECT count(*) FROM pg_class WHERE relname = 'mc'`); got != 0 {
		t.Fatalf("删表后 pg_class 里仍有 mc（%d 行）", got)
	}
}
