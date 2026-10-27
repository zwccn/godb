package exec

import (
	"strings"

	"godb/catalog"
)

// serialSeqSuffix 是自增序列名的后缀，和 PostgreSQL 的 SERIAL / IDENTITY
// 默认命名一致：<table>_<column>_seq。
const serialSeqSuffix = "_seq"

// serialTable 把序列名映射回自增列所在的表。
//
// 支持三种写法（godb 没有真正的 CREATE SEQUENCE，所以名字是算出来的）：
//
//	t_id_seq / public.t_id_seq / "public"."t_id_seq"  → 标准命名
//	t                                                 → 直接给表名，宽容处理
//
// 找不到返回 ok=false，调用方报 relation does not exist。
func serialTable(cat *catalog.Catalog, seq string) (string, bool) {
	if cat == nil {
		return "", false
	}
	name := strings.ToLower(strings.TrimSpace(seq))
	// 去掉 schema 前缀（可能带引号）
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(name, `"`)
	if name == "" {
		return "", false
	}

	// 先按标准命名精确匹配，再退回「直接给表名」。
	for _, t := range cat.Tables {
		for _, c := range t.Columns {
			if !c.Serial {
				continue
			}
			want := strings.ToLower(t.Name) + "_" + strings.ToLower(c.Name) + serialSeqSuffix
			if want == name {
				return t.Name, true
			}
		}
	}
	if t, ok := cat.Get(name); ok {
		if t.SerialIdx() >= 0 {
			return t.Name, true
		}
	}
	return "", false
}

// SerialSequenceName 返回某张表自增列的序列名（pg_get_serial_sequence 用）。
func SerialSequenceName(cat *catalog.Catalog, table, column string) (string, bool) {
	if cat == nil {
		return "", false
	}
	t, ok := cat.Get(table)
	if !ok {
		return "", false
	}
	col, _, ok := t.Col(column)
	if !ok || !col.Serial {
		return "", false
	}
	return strings.ToLower(t.Name) + "_" + strings.ToLower(col.Name) + serialSeqSuffix, true
}
