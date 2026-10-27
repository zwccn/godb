package exec

import (
	"godb/catalog"
	"godb/sql"
)

// Describe 只做“编译”不执行，返回 SELECT 的输出列定义。
// 用于 PostgreSQL 协议里的 Describe 消息：客户端在真正执行前需要先知道列名与类型。
func Describe(cat *catalog.Catalog, stmt sql.Statement) (Schema, error) {
	return DescribeWith(cat, nil, stmt)
}

// DescribeWith 多一个库信息参数：pg_database / current_database() 这类
// 依赖「当前是哪个库」的表达式，编译期就要能取到。
func DescribeWith(cat *catalog.Catalog, info *DBInfo, stmt sql.Statement) (Schema, error) {
	sel, ok := stmt.(*sql.Select)
	if !ok {
		return nil, nil
	}
	// 只构建计划、不 Open，因此不需要真实事务
	p := &Planner{Cat: cat, Info: info}
	plan, err := p.BuildSelect(sel)
	if err != nil {
		return nil, err
	}
	return plan.Schema(), nil
}
