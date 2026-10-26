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
		for _, ref := range s.From {
			collectRef(ref, lock.Shared, out)
		}
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
	case *sql.TxStmt:
		// 事务控制语句不需要表锁
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
	}
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
