package exec

import (
	"fmt"

	"godb/types"
)

// 向量不能参与「顺序」和「等值身份」这两类判断，这里把它们统一挡在规划期。
//
// 为什么必须在规划期挡，而不是等运行时：
//   - 空表上 `ORDER BY v` 一行都不用比较，运行时的报错永远不会触发，
//     同一句 SQL 于是「有数据就报错、没数据就返回空」，比不支持更难解释；
//   - 等值/分组的分歧在于「行里的值只是一个 .vec 槽位号」，槽位每次写入、
//     每次更新都会换（删除后还会被别的行复用）。拿它当身份，结果取决于
//     物理时机而不是内容 —— 这正是「看起来正常、其实错的」那类答案。
//
// 报错要给出能照做的替代方案：向量检索的正确写法是用距离算子排序。

// checkVectorOrder 挡住「给向量排大小」的地方（ORDER BY / MIN / MAX）。
func checkVectorOrder(what string, ty types.Type) error {
	if ty.Kind != types.KVector {
		return nil
	}
	return fmt.Errorf("%s cannot be applied to vector values: a vector has no comparable order; "+
		"rank by distance instead, e.g. ORDER BY v <-> '[0.1,0.2]'", what)
}

// checkVectorKey 挡住「把向量当等值/分组键」的地方（DISTINCT / GROUP BY / 连接键）。
func checkVectorKey(what string, ty types.Type) error {
	if ty.Kind != types.KVector {
		return nil
	}
	return fmt.Errorf("%s cannot be applied to vector values: rows only store a .vec slot reference, "+
		"so there is no content-based identity to compare; use a distance operator (<->, <=> or <#>) instead", what)
}

// checkVectorText 挡住「把向量当文本用」的地方（lower / concat / || / length / string_agg …）。
//
// 这些函数的工作方式就是把参数转成文本，而存盘的向量**没有**文本形式（`v::text` 在
// types.Cast 里也被拒）。跑到 TextForm() 会拿到 `[vector:0]` 这种内部引用：
// 结果看起来像字符串，其实是槽位号 —— lower(v) 返回 "[vector:0]"、length(v) 返回 10。
func checkVectorText(what string, ty types.Type) error {
	if ty.Kind != types.KVector {
		return nil
	}
	return fmt.Errorf("%s cannot be applied to vector values: a stored vector has no text form "+
		"(the row only holds a .vec slot reference); select the vector column on its own to get '[0.1,0.2,…]' text", what)
}

// checkSortKeys 检查一组排序键里没有向量列。
func checkSortKeys(what string, keys []sortKey) error {
	for _, k := range keys {
		if err := checkVectorOrder(what, k.eval.Type()); err != nil {
			return err
		}
	}
	return nil
}

// checkSchemaKeyable 检查一份输出 schema 里没有向量列（去重/集合运算要用）。
func checkSchemaKeyable(what string, sc Schema) error {
	for _, c := range sc {
		if err := checkVectorKey(what, c.Type); err != nil {
			return err
		}
	}
	return nil
}
