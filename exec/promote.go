package exec

import (
	"strings"

	"godb/sql"
)

/* 把 WHERE 里写成 `a.x = b.y` 的连接条件提升成真正的连接条件。

   背景：`SELECT … FROM a JOIN b ON a.id = b.id` 会被拆成连接键（能走哈希 / 索引
   连接）；但同样的条件写在 WHERE 里、表用逗号列出来：

       SELECT … FROM a, b WHERE a.id = b.id AND a.id < 50

   就只会变成「笛卡尔积 + 事后过滤」——5 万 × 5 万 = 25 亿行，先撞上 200 万行的
   保护才报错。这不是保护太严，是计划本身在做一件不该做的事。

   PostgreSQL 两种写法给同一个计划，因为它会从 WHERE 里提取等值条件做连接谓词。
   这个 pass 做的是同一件事，规则保守：

     · 只认 `列 = 列`（两侧都是带限定符的纯列引用）；
     · 两侧必须落在不同的表上，且分属这个连接节点的左右子树；
     · 只挂到内连接 / 交叉连接上 —— 外连接的可空侧不能吸收条件（`a LEFT JOIN b
       WHERE b.x = c.y` 里的条件如果在连接前生效，语义就变了）；
     · 挂在「覆盖两侧的最低那个节点」上，所以三张表、混合写法都能对上。

   保守的代价只是「没提升、慢一点」，提升错了是结果不对 —— 这条线一直按这个尺度划。
*/

// promoteImplicitJoins 返回改写后的 FROM 与剩余 WHERE。
// 没有可提升的条件时返回 (nil, nil)，调用方保持原样。
func (p *Planner) promoteImplicitJoins(from []sql.TableRef, where sql.Expr) ([]sql.TableRef, sql.Expr) {
	if where == nil || len(from) != 1 {
		return nil, nil
	}
	root, ok := from[0].(*sql.Join)
	if !ok {
		return nil, nil // 单表、FROM 子查询、函数表：这里没有连接可挂
	}
	var cands, rest []sql.Expr
	for _, c := range splitConjuncts(where) {
		if isJoinEquality(c) {
			cands = append(cands, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	newRoot, left := promoteInto(root, cands)
	if len(left) == len(cands) {
		return nil, nil // 一个都没挂上去（比如两侧都在同一子树里）
	}
	return []sql.TableRef{newRoot}, andAll(rest)
}

// isJoinEquality 判断一条条件是不是「两张不同表的列相等」。
//
// 要求两侧都写了限定符：`WHERE id = user_id` 这种裸列名要靠 Schema 才能判归属，
// 在这里判不准就不碰 —— 判错比不判危险得多。
func isJoinEquality(e sql.Expr) bool {
	b, ok := e.(*sql.Binary)
	if !ok || b.Op != "=" {
		return false
	}
	l, ok := b.L.(*sql.ColumnRef)
	if !ok || l.Table == "" {
		return false
	}
	r, ok := b.R.(*sql.ColumnRef)
	if !ok || r.Table == "" {
		return false
	}
	return !strings.EqualFold(l.Table, r.Table)
}

// promoteInto 后序遍历连接树：先让子树吸收各自的条件，再看本节点能不能吸收。
// 返回改写后的节点和「还没被吸收」的条件。
func promoteInto(ref sql.TableRef, cands []sql.Expr) (sql.TableRef, []sql.Expr) {
	j, ok := ref.(*sql.Join)
	if !ok {
		// BaseTable / ParenJoin / 子查询：内部没有连接节点可挂。
		// 括号组（ParenJoin）外面只能用它的别名引用，条件挂不进去，留给上层。
		return ref, cands
	}
	leftRef, rest := promoteInto(j.Left, cands)
	rightRef, rest := promoteInto(j.Right, rest)
	node := &sql.Join{Left: leftRef, Right: rightRef, Type: j.Type, On: j.On}

	// 外连接：条件不能进到可空侧，原样留给上层（它会在连接之后作为过滤生效）
	if j.Type != sql.JoinInner && j.Type != sql.JoinCross {
		return node, rest
	}
	ls, rs := refQuals(leftRef), refQuals(rightRef)
	if len(ls) == 0 || len(rs) == 0 {
		return node, rest
	}
	var take, keep []sql.Expr
	for _, c := range rest {
		b := c.(*sql.Binary)
		lq := strings.ToLower(b.L.(*sql.ColumnRef).Table)
		rq := strings.ToLower(b.R.(*sql.ColumnRef).Table)
		if (ls[lq] && rs[rq]) || (ls[rq] && rs[lq]) {
			take = append(take, c)
		} else {
			keep = append(keep, c)
		}
	}
	if len(take) == 0 {
		return node, keep
	}
	// 交叉连接吸收条件后就是内连接：不改成 Inner 的话，joinOp 会当它是笛卡尔积，
	// 就算有关键字也不走哈希/索引连接（见 join.go 里的 equi 判断）。
	if node.Type == sql.JoinCross {
		node.Type = sql.JoinInner
	}
	cond := take
	if node.On != nil {
		cond = append([]sql.Expr{node.On}, take...)
	}
	node.On = andAll(cond)
	return node, keep
}

// refQuals 是这个 FROM 项从外面可以用哪些限定符引用。
// 括号组有别名时只有别名可见（内部的 a、b 在外面不可见）。
func refQuals(ref sql.TableRef) map[string]bool {
	out := map[string]bool{}
	switch n := ref.(type) {
	case *sql.BaseTable:
		q := n.Alias
		if q == "" {
			q = n.Table
		}
		out[strings.ToLower(q)] = true
	case *sql.SubTable:
		if n.Alias != "" {
			out[strings.ToLower(n.Alias)] = true
		}
	case *sql.FuncTable:
		q := n.Alias
		if q == "" {
			q = n.Name
		}
		out[strings.ToLower(q)] = true
	case *sql.ParenJoin:
		if n.Alias != "" {
			out[strings.ToLower(n.Alias)] = true
			return out
		}
		return refQuals(n.Inner)
	case *sql.Join:
		for q := range refQuals(n.Left) {
			out[q] = true
		}
		for q := range refQuals(n.Right) {
			out[q] = true
		}
	}
	return out
}
