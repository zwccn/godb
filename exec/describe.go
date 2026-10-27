package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/syscat"
)

// Describe 只做“编译”不执行，返回 SELECT 的输出列定义。
// 用于 PostgreSQL 协议里的 Describe 消息：客户端在真正执行前需要先知道列名与类型。
func Describe(cat *catalog.Catalog, stmt sql.Statement) (Schema, error) {
	return DescribeWith(cat, nil, stmt)
}

// DescribeWith 多一个库信息参数：pg_database / current_database() 这类
// 依赖「当前是哪个库」的表达式，编译期就要能取到。
//
// 关键约束：**不能碰存储**。客户端（Npgsql / JDBC）在每次 Execute 前都会先发
// Describe，如果这里真的跑查询，代价不说，还会因为拿不到事务而崩
// —— 以前带表扫描的 CTE 就是这么把进程打死的（Planner.Tx 为 nil，
// materializeCTEs 却真去 Open，最后 nil 解引用）。
func DescribeWith(cat *catalog.Catalog, info *DBInfo, stmt sql.Statement) (Schema, error) {
	p := &Planner{Cat: cat, Info: info, compileOnly: true}
	switch s := stmt.(type) {
	case *sql.Select:
		plan, err := p.BuildSelect(s)
		if err != nil {
			return nil, err
		}
		return plan.Schema(), nil
	case *sql.Insert:
		return p.describeReturning(s.Table, "", s.Returning)
	case *sql.Update:
		return p.describeReturning(s.Table, s.Alias, s.Returning)
	case *sql.Delete:
		return p.describeReturning(s.Table, s.Alias, s.Returning)
	}
	return nil, nil
}

// describeReturning 推断 INSERT/UPDATE/DELETE 的 RETURNING 输出列。
//
// 不做的话，扩展协议里 Describe 会回 NoData、Execute 又照 RETURNING 发 DataRow，
// 客户端的协议状态机直接对不上（Npgsql 报 "Received unexpected backend message DataRow"）。
func (p *Planner) describeReturning(table, alias string, items []*sql.SelectItem) (Schema, error) {
	if len(items) == 0 {
		return nil, nil
	}
	t, ok := p.Cat.Get(table)
	if !ok {
		return nil, fmt.Errorf("relation %q does not exist", table)
	}
	sc := tableScopeOf(p.Cat, t, alias)
	expanded, err := expandStars(items, sc)
	if err != nil {
		return nil, err
	}
	out := make(Schema, 0, len(expanded))
	for _, it := range expanded {
		ev, err := p.compile(it.Expr, sc)
		if err != nil {
			return nil, err
		}
		ci := ColInfo{Name: returningName(it), Type: ev.Type()}
		// 与 buildSimple 一致：原样透出的表列带上 tableOID/attnum，
		// 客户端才知道这条结果能不能回写到源表。
		if ref, ok := it.Expr.(*sql.ColumnRef); ok {
			if idx, _, err := sc.Resolve(ref.Table, ref.Name); err == nil {
				ci.TableOID = sc[idx].TableOID
				ci.AttNum = sc[idx].AttNum
			}
		}
		out = append(out, ci)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// returningName 与 engine.compileReturning 的取名规则保持一致，
// 否则 Describe 报的列名和 Execute 实际返回的列名会不一样。
func returningName(it *sql.SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	if cr, ok := it.Expr.(*sql.ColumnRef); ok {
		return cr.Name
	}
	return "?column?"
}

// tableScopeOf 构造「一张表的列作用域」。engine.tableScope 也走这里，
// 保证 Describe 阶段推断出来的类型和执行阶段完全一致。
func tableScopeOf(cat *catalog.Catalog, t *catalog.Table, alias string) Schema {
	qual := alias
	if qual == "" {
		qual = t.Name
	}
	sc := make(Schema, len(t.Columns))
	oid, _ := syscat.RelationOID(cat, t.Name)
	for i, c := range t.Columns {
		sc[i] = ColInfo{
			Qualifier: strings.ToLower(qual),
			Name:      strings.ToLower(c.Name),
			Type:      c.Type(),
			TableOID:  oid,
			AttNum:    int16(i + 1),
		}
	}
	return sc
}
