package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// Planner 负责把 SELECT 语句变成算子树。
type Planner struct {
	Tx  *storage.Tx
	Cat *catalog.Catalog

	// Info 是当前库的名字与库列表；nil 表示单库模式（pg_database 只报默认库）。
	// 只影响系统目录与 current_database()，不影响查询计划。
	Info *DBInfo

	// pushdown 是「只涉及单张表的 WHERE 条件」，按表的限定符分组。
	// 多表查询时由 BuildSelect 算好，buildTableRef 里在扫表时就先用它过滤
	// （能走索引就走），避免把整张表灌进连接。nil 表示没有可下推的条件。
	pushdown map[string][]sql.Expr

	// outerScopes 是编译子查询时压入的外层作用域（见 buildSubquery）。
	outerScopes []outerScope

	// ctes 是 WITH 物化后的结果（名字 → 行集）。子查询与 FROM 子查询
	// 复用同一个 Planner，所以 CTE 在内层也可见，和 PostgreSQL 一致。
	ctes map[string]*cteData
}

// cteData 是一个物化好的 CTE。
type cteData struct {
	cols  []string
	types []types.Type
	rows  [][]types.Value
}

// BuildSelect 构建查询计划。
func (p *Planner) BuildSelect(s *sql.Select) (Operator, error) {
	// WITH：先把 CTE 物化，后面的 FROM 才能按名字取到
	if len(s.CTEs) > 0 {
		if err := p.materializeCTEs(s.CTEs); err != nil {
			return nil, err
		}
	}
	// 集合运算节点是个空壳：SetLeft/SetRight 才是两个操作数。
	if s.SetOp != "" {
		return p.buildSetOp(s)
	}
	// 多表连接：先把「只涉及一张表」的条件下推下去，让连接之前就把行数降下来。
	// 不这么做的话，join 的两侧都会被完整物化，WHERE 只能在最后过滤。
	p.planPushdown(s.From, s.Where)

	op, sc, err := p.buildFrom(s.From)
	if err != nil {
		return nil, err
	}

	if s.Where != nil {
		// 单表查询才尝试走索引：多表 JOIN 的谓词分不清是连接条件还是过滤条件，
		// 那属于连接顺序重排的范畴，这个引擎没有优化器。
		if len(s.From) == 1 {
			if bt, ok := s.From[0].(*sql.BaseTable); ok {
				if is, ok2 := p.tryIndexScan(bt, s.Where); ok2 {
					op = is
				}
			}
		}
		pred, err := p.compile(s.Where, sc)
		if err != nil {
			return nil, err
		}
		op = &filterOp{child: op, pred: pred, label: sql.String(s.Where)}
	}

	items, err := expandStars(s.Items, sc)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("SELECT must have at least one output column")
	}

	aggMode := len(s.GroupBy) > 0 || s.Having != nil
	for _, it := range items {
		if sql.ContainsAggregate(it.Expr) {
			aggMode = true
		}
	}
	for _, oi := range s.OrderBy {
		if sql.ContainsAggregate(oi.Expr) {
			aggMode = true
		}
	}

	if aggMode {
		return p.buildAggregate(s, items, op, sc)
	}
	return p.buildSimple(s, items, op, sc)
}

// columnIndexOf 把「纯列引用」翻成它在某个 schema 里的位置。
// 不是列引用（表达式、常量）就返回 -1，调用方据此放弃索引连接。
func columnIndexOf(e sql.Expr, sc Schema) (int, bool) {
	ref, ok := e.(*sql.ColumnRef)
	if !ok {
		return -1, false
	}
	for i, c := range sc {
		if !strings.EqualFold(c.Name, ref.Name) {
			continue
		}
		if ref.Table != "" && !strings.EqualFold(c.Qualifier, ref.Table) {
			continue
		}
		return i, true
	}
	return -1, false
}

// indexForJoinCols 找「连接列恰好是索引全部列」的索引（顺序也要一致）。
//
// 为什么要求完整列：IndexLookup 用的是完整键，只给前缀查不出来。前缀形式
// 要走键区间（另一条路径），而实际关联 99% 是完整主键/唯一键，先不做。
func indexForJoinCols(t *catalog.Table, cols []int) (string, []int, bool) {
	if len(cols) == 0 {
		return "", nil, false
	}
	same := func(ix []int) bool {
		if len(ix) != len(cols) {
			return false
		}
		for i := range cols {
			if ix[i] != cols[i] {
				return false
			}
		}
		return true
	}
	if pk := t.PKIdxes(); same(pk) {
		return "", pk, true
	}
	for _, ux := range t.Indexes {
		ix := indexColIdxes(t, ux)
		if same(ix) {
			return ux.Name, ix, true
		}
	}
	return "", nil, false
}

// tryIndexJoin 决定要不要把哈希连接换成索引连接，能换就返回新算子。
//
// 判断依据（都是估算，没有统计信息）：
//   - 「有下推条件」的那侧算小（条件已经把行数压下来了），score 记 0；
//     没有下推条件的一侧 score = 表行数。
//   - 内表取 score 更大的那侧 —— 大表按索引点查才有意义，小表直接哈希更省。
//   - LEFT JOIN 不能交换（内表必须是右表），INNER 可以。
//
// 判断不了或者没有合适的索引，就返回 nil 让它老实用哈希连接。
func (p *Planner) tryIndexJoin(n *sql.Join, j *joinOp, lop, rop Operator, lcols, rcols []int) Operator {
	if p.Tx == nil || len(lcols) == 0 || len(lcols) != len(rcols) {
		return nil
	}
	if j.typ != sql.JoinInner && j.typ != sql.JoinLeft {
		return nil
	}
	if j.residual == nil && len(j.leftKeys) != len(lcols) {
		return nil
	}
	if p.hasNonColumns(j.leftKeys) || p.hasNonColumns(j.rightKeys) {
		return nil
	}
	lt := p.baseTableOf(n.Left)
	rt := p.baseTableOf(n.Right)
	if lt == nil || rt == nil {
		return nil // 有一侧是子查询/CTE/连接组，估算不了
	}

	lscore := p.joinSideScore(lt.Name, n.Left)
	rscore := p.joinSideScore(rt.Name, n.Right)

	// 内表取「更重」的那侧
	if rscore >= lscore {
		// 内表 = 右表，左表驱动（LEFT JOIN 也只能这样）
		if ixName, ixCols, ok := indexForJoinCols(rt, rcols); ok {
			return p.newIndexJoin(j, lop, rop, rt, ixName, ixCols, rcols, false)
		}
		return nil
	}
	if j.typ == sql.JoinLeft {
		// 左表更重，但 LEFT JOIN 的内表只能是右表；换不了就回去哈希
		return nil
	}
	// 内表 = 左表，右表驱动（交换）
	if ixName, ixCols, ok := indexForJoinCols(lt, lcols); ok {
		return p.newIndexJoin(j, lop, rop, lt, ixName, ixCols, lcols, true)
	}
	return nil
}

// hasNonColumns 判断连接键里有没有「不是纯列引用」的表达式。
// 有的话拿不到列位置，也做不了索引查找。
func (p *Planner) hasNonColumns(keys []Eval) bool {
	for _, k := range keys {
		if _, ok := k.(*colEval); !ok {
			return true
		}
	}
	return false
}

// baseTableOf 从 FROM 项里取出实体表；子查询 / CTE / 函数表返回 nil。
func (p *Planner) baseTableOf(r sql.TableRef) *catalog.Table {
	bt, ok := r.(*sql.BaseTable)
	if !ok {
		return nil
	}
	if p.lookupCTE(bt.Table) != nil {
		return nil
	}
	t, ok := p.Cat.Get(bt.Table)
	if !ok {
		return nil
	}
	return t
}

// joinSideScore 估算一侧的「体量」：有下推条件的算 0（已经被压小了），
// 否则用表行数。没有统计信息，只能这么粗估 —— 决策理由会写进 EXPLAIN。
func (p *Planner) joinSideScore(table string, ref sql.TableRef) int {
	if bt, ok := ref.(*sql.BaseTable); ok {
		qual := bt.Alias
		if qual == "" {
			qual = table
		}
		if len(p.pushdown[strings.ToLower(qual)]) > 0 {
			return 0
		}
	}
	if st := p.Tx.Store(); st != nil {
		return st.RowCount(table)
	}
	return 0
}

func (p *Planner) newIndexJoin(j *joinOp, lop, rop Operator, inner *catalog.Table,
	ixName string, ixCols, innerCols []int, swap bool) Operator {
	o := &indexJoinOp{
		tx: p.Tx, inner: inner, typ: j.typ,
		osc: j.rsc, isc: j.lsc, sc: j.sc, swap: swap,
		ixName: ixName, ixCols: ixCols, ixNames: colNames(inner, ixCols),
		residual: j.residual, cond: j.cond,
		details: []string{fmt.Sprintf("选索引连接：内表 %s 的 %s 有完整匹配索引",
			inner.Name, ixNameOrPkey(ixName))},
	}
	if swap {
		o.outer, o.osc, o.isc = rop, j.rsc, j.lsc
		o.outerKeys = j.rightKeys // 在右表行上求值，值就是左表的键
	} else {
		o.outer, o.osc, o.isc = lop, j.lsc, j.rsc
		o.outerKeys = j.leftKeys
	}
	return o
}

func ixNameOrPkey(name string) string {
	if name == "" {
		return "主键"
	}
	return name
}

// pushFilter 在扫描之上就地过滤（能走索引就走）。
//
// 上层的 WHERE **仍然保留**，会再判一遍。所以这里即使判断失误，最多是「没省到」，
// 不会返回错的行 —— 和索引扫描那条既有设计是同一个取舍。
func (p *Planner) pushFilter(t *catalog.Table, qual string, sc Schema, base Operator, conds []sql.Expr) Operator {
	where := andAll(conds)
	pred, err := p.compile(where, sc)
	if err != nil {
		return base // 编译不了就老实走原路
	}
	op := base
	if p.Tx != nil {
		bt := &sql.BaseTable{Table: t.Name, Alias: qual}
		if is, ok := p.tryIndexScan(bt, where); ok {
			op = is
		} else if so, ok := base.(*scanOp); ok {
			so.notes = append(so.notes, "下推条件没走索引："+indexHint(t))
		}
	}
	return &filterOp{child: op, pred: pred, label: "pushed: " + sql.String(where)}
}

// indexHint 说清这张表有哪些索引 —— 查「为什么没走索引」时第一件要看的东西。
func indexHint(t *catalog.Table) string {
	var parts []string
	if pk := t.PKIdxes(); len(pk) > 0 {
		names := make([]string, len(pk))
		for i, ci := range pk {
			names[i] = t.Columns[ci].Name
		}
		parts = append(parts, "主键("+strings.Join(names, ",")+")")
	}
	for _, ix := range t.Indexes {
		parts = append(parts, ix.Name+"("+strings.Join(ix.Columns, ",")+")")
	}
	if len(parts) == 0 {
		return "该表没有任何索引"
	}
	return "现有索引 " + strings.Join(parts, "、") + "，条件不在索引首列的等值/区间形式上"
}

// pushdownTarget 是一张可以独立下推条件的基表。
type pushdownTarget struct {
	qual  string
	table *catalog.Table
}

// planPushdown 把 WHERE 里「只碰一张表」的条件挑出来，按表分组。
//
// 关键点是**外连接的可空侧不参与下推**：`a LEFT JOIN b ON … WHERE b.x IS NULL`
// 里如果把 b.x IS NULL 推到 b 上，会先滤掉 b 再把不匹配的 a 补 NULL，语义就变了。
// 所以只会沿着 INNER / CROSS 的两侧、LEFT 的左侧、RIGHT 的右侧往下走。
//
// 判断归属时也保守：条件里只要出现子查询、或者列名在两张表里都有（有歧义），
// 就整条不下推。漏下推只是慢一点，下推错了是结果不对。
func (p *Planner) planPushdown(from []sql.TableRef, where sql.Expr) {
	p.pushdown = nil
	if where == nil || len(from) == 0 {
		return
	}
	if _, ok := from[0].(*sql.Join); !ok {
		return // 单表查询走既有的 tryIndexScan，不需要这套
	}
	var targets []pushdownTarget
	p.collectPushdownTargets(from[0], &targets)
	if len(targets) == 0 {
		return
	}
	// 注意这里**不要求** targets >= 2：LEFT JOIN 只有左侧可下推，右侧为空，
	// 但左侧的条件照样应该推下去。condOwner 会拒掉指不到任何目标的条件。
	out := map[string][]sql.Expr{}
	for _, c := range splitConjuncts(where) {
		if q, ok := condOwner(c, targets); ok {
			out[q] = append(out[q], c)
		}
	}
	if len(out) > 0 {
		p.pushdown = out
	}
}

func (p *Planner) collectPushdownTargets(ref sql.TableRef, out *[]pushdownTarget) {
	switch n := ref.(type) {
	case *sql.BaseTable:
		if p.lookupCTE(n.Table) != nil {
			return // CTE 是物化好的结果集，条件推不进去
		}
		t, ok := p.Cat.Get(n.Table)
		if !ok {
			return // 系统目录的虚拟表
		}
		qual := n.Alias
		if qual == "" {
			qual = t.Name
		}
		*out = append(*out, pushdownTarget{qual: strings.ToLower(qual), table: t})
	case *sql.Join:
		switch n.Type {
		case sql.JoinLeft:
			p.collectPushdownTargets(n.Left, out)
		case sql.JoinRight:
			p.collectPushdownTargets(n.Right, out)
		default:
			p.collectPushdownTargets(n.Left, out)
			p.collectPushdownTargets(n.Right, out)
		}
	}
	// ParenJoin / SubTable / FuncTable 一律不下推：前者的别名会覆盖内部限定符，
	// 后两者不是实体表。
}

// condOwner 判断一个条件能不能整块下推到某一张表，返回那张表的限定符。
func condOwner(e sql.Expr, targets []pushdownTarget) (string, bool) {
	owner := ""
	ok := true
	sql.Walk(e, func(x sql.Expr) bool {
		if !ok {
			return false
		}
		switch n := x.(type) {
		case *sql.Subquery:
			ok = false // 子查询里的列可能是外表的，判断不了就别推
			return false
		case *sql.ColumnRef:
			q := strings.ToLower(n.Table)
			if q == "" {
				// 没写限定符：列名必须只属于一张表，否则有歧义
				name := strings.ToLower(n.Name)
				for _, t := range targets {
					if _, has := t.table.ColumnIdx(name); has {
						if q != "" && q != t.qual {
							ok = false
							return false
						}
						q = t.qual
					}
				}
				if q == "" {
					ok = false
					return false
				}
			} else {
				found := false
				for _, t := range targets {
					if t.qual == q {
						found = true
						break
					}
				}
				if !found {
					ok = false
					return false
				}
			}
			switch {
			case owner == "":
				owner = q
			case owner != q:
				ok = false
				return false
			}
		}
		return true
	})
	if !ok || owner == "" {
		return "", false
	}
	return owner, true
}

// buildFrom 构建 FROM 子树。
func (p *Planner) buildFrom(from []sql.TableRef) (Operator, Schema, error) {
	if len(from) == 0 || from[0] == nil {
		return &oneRowOp{}, nil, nil
	}
	return p.buildTableRef(from[0])
}

func (p *Planner) buildTableRef(r sql.TableRef) (Operator, Schema, error) {
	switch n := r.(type) {
	case *sql.BaseTable:
		if d := p.lookupCTE(n.Table); d != nil {
			qual := n.Alias
			if qual == "" {
				qual = n.Table
			}
			op := newCTEScan(d, qual)
			return op, op.Schema(), nil
		}
		t, ok := p.Cat.Get(n.Table)
		if !ok {
			// 不是业务表就看看是不是系统目录（pg_catalog / information_schema）
			if op, sc, vok := p.virtualTable(n.Schema, n.Table, n.Alias); vok {
				return op, sc, nil
			}
			return nil, nil, fmt.Errorf("relation %q does not exist", n.Table)
		}
		qual := n.Alias
		if qual == "" {
			qual = t.Name
		}
		var op Operator = newScan(p.Tx, t, qual, p.Cat)
		sc := op.Schema()
		// 多表查询：这张表上的条件就地过滤掉，别整表灌进连接
		if conds := p.pushdown[strings.ToLower(qual)]; len(conds) > 0 {
			op = p.pushFilter(t, qual, sc, op, conds)
		}
		return op, sc, nil

	case *sql.SubTable:
		return p.buildSubTable(n)

	case *sql.FuncTable:
		return p.buildFuncTable(n)

	case *sql.ParenJoin:
		op, sc, err := p.buildTableRef(n.Inner)
		if err != nil {
			return nil, nil, err
		}
		if n.Alias == "" {
			return op, sc, nil
		}
		// 别名覆盖内部所有列的限定符：外面写 x.col 就能取到内部任意列，
		// 而内部别名（a、b）在括号外不再可见。
		ns := make(Schema, len(sc))
		copy(ns, sc)
		for i := range ns {
			ns[i].Qualifier = n.Alias
		}
		return &aliasOp{inner: op, sc: ns}, ns, nil

	case *sql.Join:
		lop, lsc, err := p.buildTableRef(n.Left)
		if err != nil {
			return nil, nil, err
		}
		rop, rsc, err := p.buildTableRef(n.Right)
		if err != nil {
			return nil, nil, err
		}
		sc := make(Schema, 0, len(lsc)+len(rsc))
		sc = append(sc, lsc...)
		sc = append(sc, rsc...)
		j := &joinOp{left: lop, right: rop, typ: n.Type, lsc: lsc, rsc: rsc, sc: sc}
		// 连接键的列位置：索引连接要靠它判断「内表的哪几列被等值约束」
		var lcols, rcols []int
		if n.On != nil {
			j.cond = sql.String(n.On)
			conjuncts := splitConjuncts(n.On)
			var residual []sql.Expr
			for _, c := range conjuncts {
				matched := false
				if b, ok := c.(*sql.Binary); ok && b.Op == "=" {
					ll, lr, err1 := sideOf(b.L, lsc, rsc)
					rl, rr, err2 := sideOf(b.R, lsc, rsc)
					if err1 == nil && err2 == nil {
						if ll && rr {
							le, err := p.compile(b.L, lsc)
							if err != nil {
								return nil, nil, err
							}
							re, err := p.compile(b.R, rsc)
							if err != nil {
								return nil, nil, err
							}
							j.leftKeys = append(j.leftKeys, le)
							j.rightKeys = append(j.rightKeys, re)
							lc, _ := columnIndexOf(b.L, lsc)
							rc, _ := columnIndexOf(b.R, rsc)
							lcols = append(lcols, lc)
							rcols = append(rcols, rc)
							matched = true
						} else if rl && lr {
							le, err := p.compile(b.R, lsc)
							if err != nil {
								return nil, nil, err
							}
							re, err := p.compile(b.L, rsc)
							if err != nil {
								return nil, nil, err
							}
							j.leftKeys = append(j.leftKeys, le)
							j.rightKeys = append(j.rightKeys, re)
							lc, _ := columnIndexOf(b.L, rsc)
							rc, _ := columnIndexOf(b.R, lsc)
							lcols = append(lcols, rc)
							rcols = append(rcols, lc)
							matched = true
						}
					}
				}
				if !matched {
					residual = append(residual, c)
				}
			}
			if len(residual) > 0 {
				re, err := p.compile(andAll(residual), sc)
				if err != nil {
					return nil, nil, err
				}
				j.residual = re
			}
		}
		// 索引连接：把「大表」当内表按索引点查，小表驱动。
		// 走不了就还是原来的「物化两侧 + 哈希连接」。
		if iop := p.tryIndexJoin(n, j, lop, rop, lcols, rcols); iop != nil {
			return iop, sc, nil
		}
		return j, sc, nil
	}
	return nil, nil, fmt.Errorf("unsupported FROM clause")
}

// tryIndexScan 尝试把「整表扫描 + WHERE」改写成索引查找。
//
// 只认最稳的一种情形：索引的每一列都被 `列 = 常量` 约束住（完整键等值）。
// 前缀匹配、范围条件、IN 列表都留到以后 —— 它们需要的是有序索引，而这里的
// 索引是哈希表，只有完整键才能查。主键也走这条路径（它有常驻的哈希索引，
// 之前一直只用来判重、查数据还在全表扫）。
func (p *Planner) tryIndexScan(bt *sql.BaseTable, where sql.Expr) (Operator, bool) {
	if p.Tx == nil || p.lookupCTE(bt.Table) != nil {
		return nil, false
	}
	t, ok := p.Cat.Get(bt.Table)
	if !ok {
		return nil, false
	}
	qual := bt.Alias
	if qual == "" {
		qual = t.Name
	}
	qual = strings.ToLower(qual)
	plan, ok := pickIndex(t, where, qual)
	if !ok {
		return nil, false
	}
	if plan.isRange {
		return newIndexRangeScan(p.Tx, t, plan.ixName, plan.cols, plan.ixCols, plan.rs, qual, p.Cat), true
	}
	return newIndexScan(p.Tx, t, plan.ixName, plan.cols, plan.ixCols, plan.eq, qual, p.Cat), true
}

// indexPlan 描述一次索引访问。
//
//	isRange        → 扫 rs 里的键区间（OR 合并后可能多于一个）
//	否则 eq 非空    → 完整键等值
type indexPlan struct {
	ixName  string // 空表示主键
	cols    []int  // 参与条件的列（等值按索引顺序；区间只有首列）
	ixCols  []int  // 索引的完整列（按索引定义顺序），ORDER BY 前缀匹配要看它
	eq      []types.Value
	rs      []storage.KeyRange
	isRange bool
}

// ranges 把计划统一成键区间列表（OR 合并与算子构造都走它）。
func (p indexPlan) ranges() []storage.KeyRange {
	if p.isRange {
		return p.rs
	}
	k := storage.OrderKey(p.eq)
	return []storage.KeyRange{{Lo: k, Hi: k, HasLo: true, HasHi: true, LoInc: true, HiInc: true}}
}

// pickIndex 从 WHERE 里挑一条能用的索引：OR 合并 → 等值 → IN 列表 → 首列区间。
//
// UPDATE / DELETE 也用同一套选择逻辑：它们的 WHERE 和 SELECT 是一回事，
// 没道理查询能走索引、按主键改一行还要全表扫。
func pickIndex(t *catalog.Table, where sql.Expr, qual string) (indexPlan, bool) {
	// 1) 顶层是 OR：每个分支各自能走索引，就合并成一次多区间扫描
	if p, ok := pickOrIndex(t, where, qual); ok {
		return p, true
	}
	// 2) 完整键等值：覆盖的列最多、命中最少
	if p, ok := pickEqualityIndex(t, where, qual); ok {
		return p, true
	}
	// 3) IN 列表：一堆等值条件换个写法，合并成多区间
	if p, ok := pickInIndex(t, where, qual); ok {
		return p, true
	}
	// 4) 首列的范围条件
	return pickRangeIndex(t, where, qual)
}

// pickInIndex 把 `col IN (v1, v2, ...)` 变成同一索引上的多个等值区间。
//
// 和 OR 合并走同一条路 —— IN 本来就是「等值条件的缩写」。每个值一个前缀区间
// （`[enc(v), enc(v)+0xFF…]`），多列索引 `(a,b)` 上也能用（覆盖整个 a=v 的分组）。
//
// 三种情况不走索引，交给 filter：`NOT IN`（语义是「不在集合里」，索引帮不上）、
// `IN (SELECT ...)`（子查询要先算出来，那是另一条路径）、列表里全是 NULL
// （等于空集，扫不扫都一样，索性别为它绕开全表扫）。
func pickInIndex(t *catalog.Table, where sql.Expr, qual string) (indexPlan, bool) {
	for _, e := range splitConjuncts(where) {
		in, ok := e.(*sql.In)
		if !ok || in.Not || in.Sub != nil || len(in.List) == 0 {
			continue
		}
		ref, ok := in.X.(*sql.ColumnRef)
		if !ok {
			continue
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, qual) {
			continue
		}
		name := strings.ToLower(ref.Name)
		ci, exists := t.ColumnIdx(name)
		if !exists {
			continue
		}
		ixName, ixCols, found := indexOnFirstColumn(t, ci)
		if !found {
			continue // 该列不是任何索引的首列，用不上
		}
		colType := t.Columns[ci].Type()
		var rs []storage.KeyRange
		bad := false
		for _, item := range in.List {
			cv, err := CompileConst(item)
			if err != nil {
				bad = true
				break
			}
			cv, err = types.Cast(cv, colType)
			if err != nil {
				bad = true
				break
			}
			if cv.IsNull() {
				continue // `IN (1, NULL)` 里的 NULL 恒不成立，跳过
			}
			k := storage.OrderKey([]types.Value{cv})
			rs = append(rs, storage.KeyRange{
				Lo: k, Hi: k + keyTail, HasLo: true, HasHi: true, LoInc: true,
			})
		}
		if bad || len(rs) == 0 {
			continue
		}
		return indexPlan{ixName: ixName, cols: ixCols, ixCols: ixCols, rs: rs, isRange: true}, true
	}
	return indexPlan{}, false
}

// indexOnFirstColumn 找「首列是第 ci 列」的索引，主键优先。
// 返回的列位置是索引的**完整**列（区间校验和排序都靠它）。
func indexOnFirstColumn(t *catalog.Table, ci int) (string, []int, bool) {
	if pk := t.PKIdxes(); len(pk) > 0 && pk[0] == ci {
		return "", pk, true
	}
	want := strings.ToLower(t.Columns[ci].Name)
	for _, ux := range t.Indexes {
		if len(ux.Columns) == 0 || strings.ToLower(ux.Columns[0]) != want {
			continue
		}
		cols := indexColIdxes(t, ux)
		if len(cols) == 0 {
			continue
		}
		return ux.Name, cols, true
	}
	return "", nil, false
}

// pickOrIndex 把顶层 OR 的每个分支翻译成键区间，合并成一次索引扫描。
//
// 条件很严：**每个**分支都得落在同一个索引上，否则退回全表扫 —— 部分命中
// 就等于漏行。分支内部只认等值和单列区间，嵌套 OR 交给外层全表扫。
func pickOrIndex(t *catalog.Table, where sql.Expr, qual string) (indexPlan, bool) {
	brs := splitDisjuncts(where)
	if len(brs) < 2 {
		return indexPlan{}, false
	}
	var plan indexPlan
	var rs []storage.KeyRange
	for i, b := range brs {
		p, ok := pickBranchIndex(t, b, qual)
		if !ok {
			return indexPlan{}, false
		}
		if i == 0 {
			plan = p
		} else if p.ixName != plan.ixName || !sameCols(p.ixCols, plan.ixCols) {
			// 两个分支落在不同索引上：没法一次扫完，整条 OR 放弃
			return indexPlan{}, false
		}
		rs = append(rs, p.ranges()...)
	}
	plan.isRange = true
	plan.eq = nil
	plan.rs = rs
	// 校验与排序都换成索引**完整列**的键：各分支约束的列未必一样（一个只卡首列、
	// 一个卡满复合索引），拿「第一个分支的约束列」算键会把另一个分支的行判成
	// 不在区间 —— 静默漏行。区间本身是按索引完整键编码的，用完整键判断才是对的。
	if len(plan.ixCols) > 0 {
		plan.cols = plan.ixCols
	}
	return plan, true
}

// pickBranchIndex 单个 OR 分支：只认完整键等值、IN 列表或单列区间。
func pickBranchIndex(t *catalog.Table, e sql.Expr, qual string) (indexPlan, bool) {
	if len(splitDisjuncts(e)) > 1 {
		return indexPlan{}, false // 嵌套 OR：不处理
	}
	if p, ok := pickEqualityIndex(t, e, qual); ok {
		for _, v := range p.eq {
			if v.IsNull() {
				return indexPlan{}, false // `col = NULL` 恒不成立，交给全表扫
			}
		}
		return p, true
	}
	// `a IN (1,2) OR a = 3`：IN 分支本身就能展开成多段区间，由外层一起合并
	if p, ok := pickInIndex(t, e, qual); ok {
		return p, true
	}
	return pickRangeIndex(t, e, qual)
}

// sameCols 判断两组列位置是否相同（OR 各分支必须落在同一个索引上）。
func sameCols(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitDisjuncts 拆开 OR。只有顶层就是 OR 时才返回多于一个分支。
func splitDisjuncts(e sql.Expr) []sql.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*sql.Binary); ok && b.Op == "or" {
		return append(splitDisjuncts(b.L), splitDisjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

// indexColIdxes 把索引定义的列名翻成列位置。翻不出来（索引过期）就返回 nil，
// 调用方会当成「没有可用的列信息」，只影响 ORDER BY 免排序，不影响查得对不对。
func indexColIdxes(t *catalog.Table, ix *catalog.Index) []int {
	out := make([]int, 0, len(ix.Columns))
	for _, n := range ix.Columns {
		ci, ok := t.ColumnIdx(strings.ToLower(n))
		if !ok {
			return nil
		}
		out = append(out, ci)
	}
	return out
}

// pickEqualityIndex 找一个「每一列都被 `= 常量` 约束住」的索引，主键优先。
func pickEqualityIndex(t *catalog.Table, where sql.Expr, qual string) (indexPlan, bool) {
	eq := equalities(where, qual)
	if len(eq) == 0 {
		return indexPlan{}, false
	}
	if pk := t.PKIdxes(); len(pk) > 0 {
		if vals, ok := constrainByPos(t, pk, eq); ok {
			return indexPlan{cols: pk, ixCols: pk, eq: vals}, true
		}
	}
	// 多个索引都能用时选列最多的那个：命中的行最少。
	var best *catalog.Index
	var bestCols []int
	var bestVals []types.Value
	for _, ux := range t.Indexes {
		cols, vals, ok := constrainByIndex(t, ux, eq)
		if !ok {
			continue
		}
		if best == nil || len(cols) > len(bestCols) {
			best, bestCols, bestVals = ux, cols, vals
		}
	}
	if best == nil {
		return indexPlan{}, false
	}
	return indexPlan{ixName: best.Name, cols: bestCols, ixCols: indexColIdxes(t, best), eq: bestVals}, true
}

// pickRangeIndex 用索引的**首列**做区间扫描。
//
// 多列索引 (a,b) 的键是 enc(a)+enc(b)，所以「a 的区间」在键上表现为一个前缀区间：
// 任何 a>5 的键都大于 enc(5)+0xFF（因为下一列的编码从类型标签开始，远小于 0xFF）。
// 单列索引同理，所以下面的边界扩展规则两种情况共用。
func pickRangeIndex(t *catalog.Table, where sql.Expr, qual string) (indexPlan, bool) {
	ranges := rangeConds(t, where, qual)
	if len(ranges) == 0 {
		return indexPlan{}, false
	}
	// 主键优先
	if pk := t.PKIdxes(); len(pk) > 0 {
		if cr, ok := ranges[strings.ToLower(t.Columns[pk[0]].Name)]; ok {
			return indexPlan{cols: pk, ixCols: pk, rs: []storage.KeyRange{cr.toKeyRange()}, isRange: true}, true
		}
	}
	var best *catalog.Index
	var bestCols []int
	var bestRange storage.KeyRange
	for _, ux := range t.Indexes {
		if len(ux.Columns) == 0 {
			continue
		}
		first := strings.ToLower(ux.Columns[0])
		cr, ok := ranges[first]
		if !ok {
			continue
		}
		ci, exists := t.ColumnIdx(first)
		if !exists {
			continue
		}
		if best == nil {
			best, bestCols, bestRange = ux, []int{ci}, cr.toKeyRange()
		}
	}
	if best == nil {
		return indexPlan{}, false
	}
	ixCols := indexColIdxes(t, best)
	if len(ixCols) > 0 {
		// 键是「索引完整列」拼出来的，所以拿完整键判断在不在区间里；
		// bestCols 只记录「条件打在哪一列」。
		bestCols = ixCols
	}
	return indexPlan{ixName: best.Name, cols: bestCols, ixCols: ixCols,
		rs: []storage.KeyRange{bestRange}, isRange: true}, true
}

// colRange 是一列上的区间约束，边界直接存成**保序键**（所以能和其它键比较大小）。
type colRange struct {
	loKey, hiKey string
	hasLo, hasHi bool
	loInc, hiInc bool
}

// toKeyRange 把「某列的区间」翻译成索引上的键区间。
//
// 含下界（>=）：enc(v) 本身就在区间里，直接当下界。
// 不含下界（>）：要跳过所有以 enc(v) 开头的键，下界抬到 enc(v)+0xFF。
// 上界反过来。
func (c *colRange) toKeyRange() storage.KeyRange {
	r := storage.KeyRange{}
	if c.hasLo {
		if c.loInc {
			r.Lo, r.LoInc = c.loKey, true
		} else {
			r.Lo, r.LoInc = c.loKey+keyTail, true
		}
		r.HasLo = true
	}
	if c.hasHi {
		if c.hiInc {
			r.Hi, r.HiInc = c.hiKey+keyTail, false
		} else {
			r.Hi, r.HiInc = c.hiKey, false
		}
		r.HasHi = true
	}
	return r
}

// keyTail 是「比任何真实键都大的尾巴」：文本/数值编码里都不会出现 0xFF
// （UTF-8 不用 0xFF；数值编码最后一个字节可以是 0xFF，但那时键已经比边界长了）。
const keyTail = "\xff\xff\xff\xff\xff\xff\xff\xff"

// addLo 收紧下界：更大的键更紧，同键时开区间更紧。
func (c *colRange) addLo(key string, inc bool) {
	if !c.hasLo || key > c.loKey || (key == c.loKey && !inc) {
		c.loKey, c.loInc, c.hasLo = key, inc, true
	}
}

// addHi 收紧上界：更小的键更紧，同键时开区间更紧。
func (c *colRange) addHi(key string, inc bool) {
	if !c.hasHi || key < c.hiKey || (key == c.hiKey && !inc) {
		c.hiKey, c.hiInc, c.hasHi = key, inc, true
	}
}

// rangeConds 从 WHERE 的 AND 合取里收集每列的区间约束。
//
// 值会先 Cast 到列类型再编码，和索引里的键保持一致；Cast 不了就忽略这条
// （宁可退化成全表扫描，也不能拿错类型的界去查索引）。
func rangeConds(t *catalog.Table, where sql.Expr, qual string) map[string]*colRange {
	out := map[string]*colRange{}
	get := func(name string) *colRange {
		c := out[name]
		if c == nil {
			c = &colRange{}
			out[name] = c
		}
		return c
	}
	// 解析出一个「列 + 常量值」的条件
	addCol := func(ref *sql.ColumnRef, v types.Value, setLo, setHi bool, inc bool) {
		if ref.Table != "" && !strings.EqualFold(ref.Table, qual) {
			return
		}
		name := strings.ToLower(ref.Name)
		ci, ok := t.ColumnIdx(name)
		if !ok {
			return
		}
		cv, err := types.Cast(v, t.Columns[ci].Type())
		if err != nil || cv.IsNull() {
			return
		}
		key := storage.OrderKey([]types.Value{cv})
		c := get(name)
		if setLo {
			c.addLo(key, inc)
		}
		if setHi {
			c.addHi(key, inc)
		}
	}
	for _, e := range splitConjuncts(where) {
		switch c := e.(type) {
		case *sql.Binary:
			ref, val, flipped, ok := colConstPair(c)
			if !ok {
				continue
			}
			op := c.Op
			if flipped {
				op = flipOp(op)
			}
			switch op {
			case "=":
				addCol(ref, val, true, true, true)
			case ">":
				addCol(ref, val, true, false, false)
			case ">=":
				addCol(ref, val, true, false, true)
			case "<":
				addCol(ref, val, false, true, false)
			case "<=":
				addCol(ref, val, false, true, true)
			}
		case *sql.Between:
			if c.Not {
				continue
			}
			ref, ok := c.X.(*sql.ColumnRef)
			if !ok {
				continue
			}
			lo, err1 := CompileConst(c.Lo)
			hi, err2 := CompileConst(c.Hi)
			if err1 != nil || err2 != nil {
				continue
			}
			addCol(ref, lo, true, false, true)
			addCol(ref, hi, false, true, true)
		case *sql.Like:
			// 只认 `LIKE 'abc%'`：前缀之后没有通配符，大小写敏感
			if c.Not || c.IgnoreCase {
				continue
			}
			ref, ok := c.X.(*sql.ColumnRef)
			if !ok {
				continue
			}
			pat, err := CompileConst(c.Pattern)
			if err != nil || pat.K != types.KText {
				continue
			}
			prefix, ok := likePrefix(pat.S)
			if !ok {
				continue
			}
			if ref.Table != "" && !strings.EqualFold(ref.Table, qual) {
				continue
			}
			name := strings.ToLower(ref.Name)
			ci, exists := t.ColumnIdx(name)
			if !exists || t.Columns[ci].Type().Kind != types.KText {
				continue
			}
			lo := storage.OrderKey([]types.Value{types.Text(prefix)})
			_, hiKey := storage.TextPrefixRange(prefix)
			c2 := get(name)
			c2.addLo(lo, true)
			c2.addHi(hiKey, false)
		}
	}
	return out
}

// colConstPair 把 `列 op 常量` / `常量 op 列` 拆成（列引用、常量、是否左右翻转）。
func colConstPair(b *sql.Binary) (*sql.ColumnRef, types.Value, bool, bool) {
	if ref, ok := b.L.(*sql.ColumnRef); ok {
		if _, isRef := b.R.(*sql.ColumnRef); !isRef {
			if v, err := CompileConst(b.R); err == nil {
				return ref, v, false, true
			}
		}
	}
	if ref, ok := b.R.(*sql.ColumnRef); ok {
		if _, isRef := b.L.(*sql.ColumnRef); !isRef {
			if v, err := CompileConst(b.L); err == nil {
				return ref, v, true, true
			}
		}
	}
	return nil, types.Null, false, false
}

// flipOp 把 `5 < col` 翻成 `col > 5`。
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// likePrefix 从 LIKE 模式里取出前缀：必须只有结尾一个 `%`，中间不能有通配符。
func likePrefix(p string) (string, bool) {
	if !strings.HasSuffix(p, "%") {
		return "", false
	}
	body := p[:len(p)-1]
	if strings.ContainsAny(body, "%_") {
		return "", false
	}
	if body == "" {
		return "", false // LIKE '%' 等于没条件
	}
	return body, true
}

// equalities 从 WHERE 里收集「列 = 常量」形式的等值约束。
//
// 同名列出多次（`a = 1 AND a = 2`）时留第一个：候选集可能偏大，但上面的
// filterOp 会把这些行全过滤掉，结果仍然正确 —— 这里宁可多取也不能少取。
func equalities(where sql.Expr, qual string) map[string]types.Value {
	out := map[string]types.Value{}
	for _, c := range splitConjuncts(where) {
		b, ok := c.(*sql.Binary)
		if !ok || b.Op != "=" {
			continue
		}
		lref, lok := b.L.(*sql.ColumnRef)
		rref, rok := b.R.(*sql.ColumnRef)
		switch {
		case lok && !rok:
			addEquality(out, lref, b.R, qual)
		case rok && !lok:
			addEquality(out, rref, b.L, qual)
		}
	}
	return out
}

func addEquality(dst map[string]types.Value, ref *sql.ColumnRef, val sql.Expr, qual string) {
	if ref.Table != "" && !strings.EqualFold(ref.Table, qual) {
		return
	}
	v, err := CompileConst(val)
	if err != nil {
		return
	}
	name := strings.ToLower(ref.Name)
	if _, exists := dst[name]; !exists {
		dst[name] = v
	}
}

// constrainByPos 检查一组列位置是否都被等值常量约束住，并给出归一后的值。
func constrainByPos(t *catalog.Table, cols []int, eq map[string]types.Value) ([]types.Value, bool) {
	vals := make([]types.Value, 0, len(cols))
	for _, ci := range cols {
		if ci < 0 || ci >= len(t.Columns) {
			return nil, false
		}
		v, ok := eq[strings.ToLower(t.Columns[ci].Name)]
		if !ok {
			return nil, false
		}
		cv, err := types.Cast(v, t.Columns[ci].Type())
		if err != nil {
			return nil, false
		}
		vals = append(vals, cv)
	}
	return vals, len(vals) > 0
}

func constrainByIndex(t *catalog.Table, ux *catalog.Index, eq map[string]types.Value) ([]int, []types.Value, bool) {
	cols := make([]int, 0, len(ux.Columns))
	for _, cn := range ux.Columns {
		ci, ok := t.ColumnIdx(cn)
		if !ok {
			return nil, nil, false
		}
		cols = append(cols, ci)
	}
	vals, ok := constrainByPos(t, cols, eq)
	if !ok {
		return nil, nil, false
	}
	return cols, vals, true
}

// expandStars 展开 * 与 table.*。
func expandStars(items []*sql.SelectItem, sc Schema) ([]*sql.SelectItem, error) {
	var out []*sql.SelectItem
	for _, it := range items {
		if !it.Star {
			out = append(out, it)
			continue
		}
		matched := false
		for _, c := range sc {
			if it.StarTable != "" && c.Qualifier != strings.ToLower(it.StarTable) {
				continue
			}
			matched = true
			out = append(out, &sql.SelectItem{Expr: &sql.ColumnRef{Table: c.Qualifier, Name: c.Name}})
		}
		if !matched {
			if it.StarTable != "" {
				return nil, fmt.Errorf("table %q does not exist in FROM clause", it.StarTable)
			}
			return nil, fmt.Errorf("SELECT * requires a FROM clause")
		}
	}
	return out, nil
}

func itemName(it *sql.SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	switch e := it.Expr.(type) {
	case *sql.ColumnRef:
		return strings.ToLower(e.Name)
	case *sql.FuncCall:
		return strings.ToLower(e.Name)
	case *sql.Cast:
		return itemName(&sql.SelectItem{Expr: e.X})
	}
	return "?column?"
}

// orderedScan 表示算子已经按某个索引的键序输出行。
type orderedScan interface {
	orderedColumns() []string // 索引列，按索引定义顺序
	isReversed() bool
	setReversed(bool)
}

// orderCovered 判断下层算子是否已经按 ORDER BY 要求的顺序输出行。
//
// 三个前提缺一不可：
//  1. 隔着 filter 看下去是索引扫描 —— filter 是流式的，不打乱顺序；
//  2. ORDER BY 的列正好是索引列的**前缀**、顺序一致（`(a,b)` 索引上只认 `a` 或 `a,b`；
//     单独 `b` 不行，那得全表按 a 扫一遍才能排）；
//  3. 所有排序列方向一致 —— 索引只能整体正扫或反扫，`a ASC, b DESC` 做不到。
//
// NULL 不用特殊处理：索引里不含 NULL 键，而能命中索引的条件本身就排除了 NULL 行。
func orderCovered(op Operator, orderBy []*sql.OrderItem) bool {
	src := op
	for {
		if f, ok := src.(*filterOp); ok {
			src = f.child
			continue
		}
		break
	}
	os, ok := src.(orderedScan)
	if !ok {
		return false
	}
	cols := os.orderedColumns()
	if len(cols) == 0 || len(orderBy) == 0 || len(orderBy) > len(cols) {
		return false
	}
	desc := orderBy[0].Desc
	for i, oi := range orderBy {
		if oi.Desc != desc {
			return false // 混合方向
		}
		ref, ok := oi.Expr.(*sql.ColumnRef)
		if !ok {
			return false // 表达式排序用不上索引序
		}
		if !strings.EqualFold(ref.Name, cols[i]) {
			return false
		}
	}
	if desc && !os.isReversed() {
		os.setReversed(true)
	}
	return true
}

// buildSimple 处理不含聚合的 SELECT。
func (p *Planner) buildSimple(s *sql.Select, items []*sql.SelectItem, op Operator, sc Schema) (Operator, error) {
	exprs := make([]Eval, len(items))
	out := make(Schema, len(items))
	for i, it := range items {
		ev, err := p.compile(it.Expr, sc)
		if err != nil {
			return nil, err
		}
		exprs[i] = ev
		out[i] = ColInfo{Name: itemName(it), Type: ev.Type()}
		// 投影项就是表列时，把「来自哪张表哪一列」一起透出来。
		// 不填的话 PG 协议里 tableOID/attnum 全是 0，Navicat 会把结果网格设成只读
		//（数据能看不能改），因为它没法把结果行回写定位到源表。
		if ref, ok := it.Expr.(*sql.ColumnRef); ok {
			if idx, _, err := sc.Resolve(ref.Table, ref.Name); err == nil {
				out[i].TableOID = sc[idx].TableOID
				out[i].AttNum = sc[idx].AttNum
			}
		}
	}

	if len(s.OrderBy) > 0 {
		// 下层是索引扫描、并且 ORDER BY 正好是索引列的前缀时，行已经有序，
		// 不必再排序（索引扫描按键升序输出，整体反向扫就是降序）。
		if !orderCovered(op, s.OrderBy) {
			keys := make([]sortKey, 0, len(s.OrderBy))
			for _, oi := range s.OrderBy {
				ev, err := p.orderKey(oi, items, exprs, out, &compileCtx{sc: sc})
				if err != nil {
					return nil, err
				}
				nullsFirst := oi.Desc
				if oi.NullsFirst != nil {
					nullsFirst = *oi.NullsFirst
				}
				keys = append(keys, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
			}
			op = &sortOp{child: op, keys: keys, sc: sc}
		}
	}

	op = &projectOp{child: op, exprs: exprs, sc: out}
	if s.Distinct {
		op = &distinctOp{child: op, sc: out}
	}
	return p.applyLimit(s, op)
}

// buildSetOp 构建 UNION / INTERSECT / EXCEPT。
func (p *Planner) buildSetOp(s *sql.Select) (Operator, error) {
	left, err := p.BuildSelect(s.SetLeft)
	if err != nil {
		return nil, err
	}
	right, err := p.BuildSelect(s.SetRight)
	if err != nil {
		return nil, err
	}
	if s.SetAll && s.SetOp != "union" {
		return nil, fmt.Errorf("%s ALL is not supported", strings.ToUpper(s.SetOp))
	}
	sc := left.Schema()
	var op Operator = &setOp{left: left, right: right, op: s.SetOp, all: s.SetAll, sc: sc}

	// ORDER BY / LIMIT 属于整条查询，作用在集合运算的结果上。
	// 结果列名取自左操作数，所以排序键按这份 schema 编译。
	if len(s.OrderBy) > 0 {
		keys := make([]sortKey, 0, len(s.OrderBy))
		for _, oi := range s.OrderBy {
			ev, err := p.compile(oi.Expr, sc)
			if err != nil {
				return nil, err
			}
			nullsFirst := oi.Desc
			if oi.NullsFirst != nil {
				nullsFirst = *oi.NullsFirst
			}
			keys = append(keys, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
		}
		op = &sortOp{child: op, keys: keys, sc: sc}
	}
	return p.applyLimit(s, op)
}

// buildAggregate 处理含聚合/分组/HAVING 的 SELECT。
func (p *Planner) buildAggregate(s *sql.Select, items []*sql.SelectItem, op Operator, sc Schema) (Operator, error) {
	groupEvals := make([]Eval, len(s.GroupBy))
	groupTypes := make([]types.Type, len(s.GroupBy))
	for i, ge := range s.GroupBy {
		ev, err := p.compile(ge, sc)
		if err != nil {
			return nil, err
		}
		groupEvals[i] = ev
		groupTypes[i] = ev.Type()
	}

	col := &aggCollector{byKey: map[string]int{}, sc: sc,
		ctx: &compileCtx{sc: sc, planner: p, outerScopes: p.outerScopes}}
	for _, it := range items {
		if err := col.collect(it.Expr); err != nil {
			return nil, err
		}
	}
	if s.Having != nil {
		if err := col.collect(s.Having); err != nil {
			return nil, err
		}
	}
	for _, oi := range s.OrderBy {
		if err := col.collect(oi.Expr); err != nil {
			return nil, err
		}
	}

	outSc := make(Schema, 0, len(groupEvals)+len(col.aggs))
	keys := map[string]int{}
	for i, ge := range s.GroupBy {
		name := "?column?"
		qual := ""
		if cr, ok := ge.(*sql.ColumnRef); ok {
			name = strings.ToLower(cr.Name)
			qual = strings.ToLower(cr.Table)
		}
		keys[sql.Canonical(ge)] = len(outSc)
		outSc = append(outSc, ColInfo{Qualifier: qual, Name: name, Type: groupTypes[i]})
	}
	aggIdx := map[string]int{}
	for i, spec := range col.aggs {
		aggIdx[spec.key] = len(outSc)
		outSc = append(outSc, ColInfo{Name: fmt.Sprintf("__agg_%d", i), Type: spec.Type})
	}

	agg := &aggOp{child: op, groupEvals: groupEvals, groupTypes: groupTypes, aggs: col.aggs, sc: outSc}
	// planner 必须带上：聚合后的 select 列表里也可能有 current_database()
	// 这类要查「当前是哪个库」的兼容函数。
	ctx := &compileCtx{postAgg: true, keys: keys, aggs: aggIdx, groupN: len(groupEvals),
		scOut: outSc, sc: outSc, planner: p}

	if s.Having != nil {
		pred, err := compile(s.Having, ctx)
		if err != nil {
			return nil, err
		}
		op = &filterOp{child: agg, pred: pred, label: sql.String(s.Having)}
	} else {
		op = agg
	}

	exprs := make([]Eval, len(items))
	finalSc := make(Schema, len(items))
	for i, it := range items {
		ev, err := compile(it.Expr, ctx)
		if err != nil {
			return nil, err
		}
		exprs[i] = ev
		finalSc[i] = ColInfo{Name: itemName(it), Type: ev.Type()}
	}

	if len(s.OrderBy) > 0 {
		keysOut := make([]sortKey, 0, len(s.OrderBy))
		for _, oi := range s.OrderBy {
			ev, err := p.orderKey(oi, items, exprs, finalSc, ctx)
			if err != nil {
				return nil, err
			}
			nullsFirst := oi.Desc
			if oi.NullsFirst != nil {
				nullsFirst = *oi.NullsFirst
			}
			keysOut = append(keysOut, sortKey{eval: ev, desc: oi.Desc, nullsFirst: nullsFirst})
		}
		op = &sortOp{child: op, keys: keysOut, sc: outSc}
	}

	op = &projectOp{child: op, exprs: exprs, sc: finalSc}
	if s.Distinct {
		op = &distinctOp{child: op, sc: finalSc}
	}
	return p.applyLimit(s, op)
}

// orderKey 解析 ORDER BY 的一项：支持序号、输出别名、普通表达式。
func (p *Planner) orderKey(oi *sql.OrderItem, items []*sql.SelectItem, targetExprs []Eval, out Schema, ctx *compileCtx) (Eval, error) {
	// 序号：ORDER BY 2
	if lit, ok := oi.Expr.(*sql.Literal); ok && lit.Val.K == types.KInt {
		n := lit.Val.I
		if n >= 1 && int(n) <= len(items) {
			return targetExprs[n-1], nil
		}
		return nil, fmt.Errorf("ORDER BY position %d is not in select list", n)
	}
	// 输出列名优先于来源列 —— 这是 PG 的规矩（ORDER BY 文档原话：裸名字既像输出列
	// 又像输入列时，按输出列解释）。例：
	//   SELECT pg_type.oid, enumlabel FROM pg_enum JOIN pg_type ON … ORDER BY oid
	// 两张表都有 oid，但输出里那个 oid 是第 1 列，所以按它排，不该报歧义。
	// Npgsql 加载类型目录的第 4 条查询就卡在这里。
	if cr, ok := oi.Expr.(*sql.ColumnRef); ok && cr.Table == "" {
		hit := -1
		for i, it := range items {
			// 比对的是「输出列名」而不是「Alias」：没写别名的 SELECT a, b 也要能按 a/b 排
			if !strings.EqualFold(itemName(it), cr.Name) {
				continue
			}
			if hit >= 0 {
				// 输出里有两个同名列（SELECT a.id, b.id … ORDER BY id）：PG 也报歧义
				return nil, fmt.Errorf("ORDER BY %q is ambiguous", cr.Name)
			}
			hit = i
		}
		if hit >= 0 {
			return targetExprs[hit], nil
		}
	}
	// 走到这里说明不是输出列名（或本来就带表限定/是表达式），回落到来源列解析
	return compile(oi.Expr, ctx)
}

func (p *Planner) applyLimit(s *sql.Select, op Operator) (Operator, error) {
	if s.Limit == nil && s.Offset == nil {
		return op, nil
	}
	limit := int64(-1)
	offset := int64(0)
	if s.Limit != nil {
		v, err := CompileConst(s.Limit)
		if err != nil {
			return nil, err
		}
		n, err := toInt(v)
		if err != nil || v.IsNull() {
			return nil, fmt.Errorf("LIMIT must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("LIMIT must not be negative")
		}
		limit = n
	}
	if s.Offset != nil {
		v, err := CompileConst(s.Offset)
		if err != nil {
			return nil, err
		}
		if v.IsNull() {
			return nil, fmt.Errorf("OFFSET must be an integer")
		}
		n, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf("OFFSET must be an integer")
		}
		if n < 0 {
			return nil, fmt.Errorf("OFFSET must not be negative")
		}
		offset = n
	}
	if limit < 0 {
		limit = int64(1) << 62
	}
	return &limitOp{child: op, limit: limit, offset: offset}, nil
}

// aggCollector 收集查询中出现的聚合调用（同一聚合只保留一份）。
type aggCollector struct {
	aggs  []*aggSpec
	byKey map[string]int
	sc    Schema
	// ctx 用于把 planner / 外层作用域传给聚合参数的编译（子查询要用）。
	ctx *compileCtx
}

func (c *aggCollector) collect(e sql.Expr) error {
	var firstErr error
	sql.Walk(e, func(n sql.Expr) bool {
		if firstErr != nil {
			return false
		}
		fc, ok := n.(*sql.FuncCall)
		if !ok || !sql.IsAggregate(fc.Name) {
			return true
		}
		key := sql.Canonical(fc)
		if _, exists := c.byKey[key]; exists {
			return false
		}
		spec, err := buildAggSpec(fc, c.sc, c.ctx)
		if err != nil {
			firstErr = err
			return false
		}
		spec.key = key
		c.byKey[key] = len(c.aggs)
		c.aggs = append(c.aggs, spec)
		return false
	})
	return firstErr
}

func buildAggSpec(fc *sql.FuncCall, sc Schema, parent *compileCtx) (*aggSpec, error) {
	name := strings.ToLower(fc.Name)
	spec := &aggSpec{Fn: name, Distinct: fc.Distinct, Label: sql.String(fc)}
	if fc.Star {
		if name != "count" {
			return nil, fmt.Errorf("%s(*) is not supported", name)
		}
		spec.Star = true
		spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
		return spec, nil
	}
	if name == "string_agg" {
		if len(fc.Args) != 2 {
			return nil, fmt.Errorf("string_agg requires two arguments")
		}
		sep, err := CompileConst(fc.Args[1])
		if err != nil {
			return nil, fmt.Errorf("string_agg delimiter must be a constant")
		}
		spec.Separator = sep.TextForm()
	} else if len(fc.Args) != 1 {
		return nil, fmt.Errorf("aggregate %s requires exactly one argument", name)
	}
	sub := &compileCtx{sc: sc, postAgg: parent.postAgg, keys: parent.keys, aggs: parent.aggs,
		groupN: parent.groupN, scOut: parent.scOut, planner: parent.planner, outerScopes: parent.outerScopes}
	arg, err := compile(fc.Args[0], sub)
	if err != nil {
		return nil, err
	}
	spec.Arg = arg
	switch name {
	case "string_agg":
		spec.Type = types.Type{Kind: types.KText, Name: "text"}
	case "count":
		spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
	case "sum":
		if arg.Type().Kind == types.KInt {
			spec.Type = types.Type{Kind: types.KInt, Name: "bigint"}
		} else {
			spec.Type = types.Type{Kind: types.KFloat, Name: "double precision"}
		}
	case "avg":
		spec.Type = types.Type{Kind: types.KFloat, Name: "double precision"}
	case "min", "max":
		spec.Type = arg.Type()
	}
	return spec, nil
}

// aliasOp 给整棵子树的列换一个限定符，用于括号联接组起别名。
type aliasOp struct {
	inner Operator
	sc    Schema
}

func (o *aliasOp) Schema() Schema                 { return o.sc }
func (o *aliasOp) Open() error                    { return o.inner.Open() }
func (o *aliasOp) Next() (types.Row, bool, error) { return o.inner.Next() }
func (o *aliasOp) Close() error                   { return o.inner.Close() }
func (o *aliasOp) Describe() string               { return "Alias(" + o.sc[0].Qualifier + ")" }
func (o *aliasOp) Children() []Operator           { return []Operator{o.inner} }

// compile 编译表达式，并允许其中出现子查询（标量子查询）。
func (p *Planner) compile(e sql.Expr, sc Schema) (Eval, error) {
	return compile(e, &compileCtx{sc: sc, planner: p, outerScopes: p.outerScopes})
}

// buildSubquery 在「当前外层作用域」下构建子查询计划。
//
// outerRow 指向调用方 subqueryEval 里的当前行，编译出来的 outerRef
// 会持有这个指针，从而支持相关子查询（例如 WHERE inhparent = c.oid）。
func (p *Planner) buildSubquery(s *sql.Select, outer Schema, outerRow *types.Row) (Operator, error) {
	p.outerScopes = append(p.outerScopes, outerScope{sc: outer, row: outerRow})
	defer func() {
		p.outerScopes = p.outerScopes[:len(p.outerScopes)-1]
	}()
	return p.BuildSelect(s)
}

// ---------- WITH（公用表表达式） ----------

// materializeCTEs 依次物化 WITH 里的每个 CTE。
//
// 物化 = 完整跑一遍子查询把结果收进内存。后续按名字引用时不再重算，
// 这与 PostgreSQL 默认的 MATERIALIZED 行为一致（也省掉了引用两次跑两遍）。
func (p *Planner) materializeCTEs(list []*sql.CTE) error {
	if p.ctes == nil {
		p.ctes = map[string]*cteData{}
	}
	for _, c := range list {
		plan, err := p.BuildSelect(c.Select)
		if err != nil {
			return err
		}
		if err := plan.Open(); err != nil {
			return err
		}
		sc := plan.Schema()
		d := &cteData{
			cols:  make([]string, len(sc)),
			types: make([]types.Type, len(sc)),
		}
		for i, ci := range sc {
			// WITH x(a,b) 里给的名字优先，否则用查询的输出列名
			if i < len(c.Cols) {
				d.cols[i] = strings.ToLower(c.Cols[i])
			} else {
				d.cols[i] = strings.ToLower(ci.Name)
			}
			d.types[i] = ci.Type
		}
		for {
			r, ok, err := plan.Next()
			if err != nil {
				plan.Close()
				return err
			}
			if !ok {
				break
			}
			cp := make([]types.Value, len(r))
			copy(cp, r)
			d.rows = append(d.rows, cp)
		}
		plan.Close()
		p.ctes[strings.ToLower(c.Name)] = d
	}
	return nil
}

// lookupCTE 按名字找已物化的 CTE。
func (p *Planner) lookupCTE(name string) *cteData {
	if p.ctes == nil {
		return nil
	}
	return p.ctes[strings.ToLower(name)]
}

// cteScan 把一个物化好的 CTE 当作表来读。
type cteScan struct {
	sc   Schema
	rows []types.Row
	i    int
}

func newCTEScan(d *cteData, qual string) *cteScan {
	sc := make(Schema, len(d.cols))
	for i := range d.cols {
		sc[i] = ColInfo{Qualifier: strings.ToLower(qual), Name: d.cols[i], Type: d.types[i]}
	}
	rows := make([]types.Row, len(d.rows))
	for i, r := range d.rows {
		rows[i] = r
	}
	return &cteScan{sc: sc, rows: rows}
}

func (o *cteScan) Schema() Schema { return o.sc }

func (o *cteScan) Open() error {
	o.i = 0
	return nil
}

func (o *cteScan) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

func (o *cteScan) Close() error {
	o.i = 0
	return nil
}

func (o *cteScan) Describe() string {
	return fmt.Sprintf("CTE Scan (%d rows)", len(o.rows))
}

func (o *cteScan) Children() []Operator { return nil }
