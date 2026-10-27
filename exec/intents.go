package exec

import (
	"strings"

	"godb/lock"
	"godb/sql"
	"godb/syscat"
)

// TableIntents 静态分析一条语句会访问哪些表、各需要什么锁。
// 读表要 S，写表（含 DDL）要 X。分析只依赖 AST，不访问 catalog，因此可以在
// 真正执行（持 schema 读锁）之前完成，避免拿着 schema 锁去等表锁。
func TableIntents(st sql.Statement) (map[string]lock.Mode, error) {
	out := map[string]lock.Mode{}
	switch s := st.(type) {
	case *sql.Select:
		collectSelect(s, lock.Shared, out)
	case *sql.Explain:
		return TableIntents(s.Stmt)
	case *sql.Insert:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.Update:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.Delete:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.Truncate:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.CreateTable:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.DropTable:
		for _, n := range s.Tables {
			addIntent(out, n, lock.Exclusive)
		}
	case *sql.AlterTable:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.CreateIndex:
		addIntent(out, s.Table, lock.Exclusive)
	case *sql.CommentOn:
		// 表/列/约束的注释改的是 catalog，要 X 锁；INDEX / SCHEMA / DATABASE /
		// VIEW 的注释接受即忽略，不碰任何表（不然会给一个叫 public 的表加锁）。
		switch s.Object {
		case "table", "column", "constraint":
			if s.Table != "" {
				addIntent(out, s.Table, lock.Exclusive)
			}
		}
	case *sql.TxStmt, *sql.Checkpoint, *sql.Show, *sql.SetStmt, *sql.SessionCleanup:
		// 事务控制、CHECKPOINT、SHOW/SET、DISCARD/RESET/DEALLOCATE 都不需要表锁
	}
	return out, nil
}

// MergeIntents 把 src 合并进 dst，同名表取更强的模式。
func MergeIntents(dst, src map[string]lock.Mode) {
	for name, mode := range src {
		addIntent(dst, name, mode)
	}
}

func collectRef(ref sql.TableRef, mode lock.Mode, out map[string]lock.Mode) {
	switch r := ref.(type) {
	case *sql.BaseTable:
		// 系统目录是只读虚拟表，不参与加锁
		if syscat.IsSystem(r.Schema, r.Table) {
			return
		}
		addIntent(out, r.Table, mode)
	case *sql.Join:
		collectRef(r.Left, mode, out)
		collectRef(r.Right, mode, out)
		collectExpr(r.On, mode, out)
	case *sql.ParenJoin:
		collectRef(r.Inner, mode, out)
	case *sql.SubTable:
		collectSelect(r.Select, mode, out)
	case *sql.FuncTable:
		// 集合函数自己就是数据源（generate_series 之类），没有表可锁
	}
}

// collectSelect 收集一条查询（含集合运算两侧、子查询）用到的业务表。
// 括号联接组与表达式里的标量子查询以前都漏掉了，会读不加锁。
func collectSelect(s *sql.Select, mode lock.Mode, out map[string]lock.Mode) {
	if s == nil {
		return
	}
	if s.SetLeft != nil {
		collectSelect(s.SetLeft, mode, out)
	}
	if s.SetRight != nil {
		collectSelect(s.SetRight, mode, out)
	}
	// WITH 里的子查询也读表，必须一起加锁
	for _, c := range s.CTEs {
		collectSelect(c.Select, mode, out)
	}
	for _, ref := range s.From {
		collectRef(ref, mode, out)
	}
	collectExpr(s.Where, mode, out)
	collectExpr(s.Having, mode, out)
	for _, it := range s.Items {
		collectExpr(it.Expr, mode, out)
	}
	for _, oi := range s.OrderBy {
		collectExpr(oi.Expr, mode, out)
	}
}

// collectExpr 找表达式里的标量子查询，把它们引用的表也算上。
func collectExpr(e sql.Expr, mode lock.Mode, out map[string]lock.Mode) {
	sql.Walk(e, func(n sql.Expr) bool {
		if sq, ok := n.(*sql.Subquery); ok && sq != nil {
			collectSelect(sq.Select, mode, out)
		}
		return true
	})
}

func addIntent(out map[string]lock.Mode, table string, mode lock.Mode) {
	name := strings.ToLower(strings.TrimSpace(table))
	if name == "" {
		return
	}
	if cur, ok := out[name]; ok && cur >= mode {
		return
	}
	out[name] = mode
}
