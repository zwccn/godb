package sql

import (
	"fmt"
	"strconv"
	"strings"

	"godb/types"
)

// String 把表达式还原成可读的 SQL 片段（用于 EXPLAIN 与错误提示）。
func String(e Expr) string {
	var b strings.Builder
	writeExpr(&b, e)
	return b.String()
}

func writeExpr(b *strings.Builder, e Expr) {
	if e == nil {
		b.WriteString("NULL")
		return
	}
	switch n := e.(type) {
	case *Literal:
		switch n.Val.K {
		case types.KText:
			b.WriteString("'")
			b.WriteString(strings.ReplaceAll(n.Val.S, "'", "''"))
			b.WriteString("'")
		case types.KBool:
			b.WriteString(strings.ToUpper(n.Val.TextForm()))
		case types.KTime:
			b.WriteString("'")
			b.WriteString(n.Val.TextForm())
			b.WriteString("'")
		case types.KNull:
			b.WriteString("NULL")
		default:
			b.WriteString(n.Val.TextForm())
		}
	case *ColumnRef:
		if n.Table != "" {
			b.WriteString(n.Table)
			b.WriteString(".")
		}
		b.WriteString(n.Name)
	case *Binary:
		b.WriteString("(")
		writeExpr(b, n.L)
		op := n.Op
		if op == "and" || op == "or" {
			op = strings.ToUpper(op)
		}
		b.WriteString(" " + op + " ")
		writeExpr(b, n.R)
		b.WriteString(")")
	case *Unary:
		if n.Op == "not" {
			b.WriteString("NOT ")
		} else {
			b.WriteString(n.Op)
		}
		writeExpr(b, n.X)
	case *FuncCall:
		b.WriteString(strings.ToLower(n.Name))
		b.WriteString("(")
		if n.Distinct {
			b.WriteString("DISTINCT ")
		}
		if n.Star {
			b.WriteString("*")
		}
		for i, a := range n.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, a)
		}
		b.WriteString(")")
	case *Case:
		b.WriteString("CASE")
		if n.Operand != nil {
			b.WriteString(" ")
			writeExpr(b, n.Operand)
		}
		for _, w := range n.Whens {
			b.WriteString(" WHEN ")
			writeExpr(b, w.Cond)
			b.WriteString(" THEN ")
			writeExpr(b, w.Result)
		}
		if n.Else != nil {
			b.WriteString(" ELSE ")
			writeExpr(b, n.Else)
		}
		b.WriteString(" END")
	case *Subscript:
		writeExpr(b, n.X)
		b.WriteString("[")
		writeExpr(b, n.Index)
		b.WriteString("]")
	case *Cast:
		b.WriteString("CAST(")
		writeExpr(b, n.X)
		b.WriteString(" AS ")
		b.WriteString(strings.ToUpper(n.TypeName))
		if len(n.TypeArgs) > 0 {
			parts := make([]string, len(n.TypeArgs))
			for i, a := range n.TypeArgs {
				parts[i] = strconv.Itoa(a)
			}
			b.WriteString("(" + strings.Join(parts, ",") + ")")
		}
		b.WriteString(")")
	case *IsNull:
		writeExpr(b, n.X)
		if n.Not {
			b.WriteString(" IS NOT NULL")
		} else {
			b.WriteString(" IS NULL")
		}
	case *In:
		writeExpr(b, n.X)
		if n.Not {
			b.WriteString(" NOT")
		}
		b.WriteString(" IN (")
		if n.Sub != nil {
			b.WriteString("SELECT ...")
		}
		for i, a := range n.List {
			if i > 0 || n.Sub != nil {
				b.WriteString(", ")
			}
			writeExpr(b, a)
		}
		b.WriteString(")")
	case *Exists:
		if n.Not {
			b.WriteString("NOT ")
		}
		b.WriteString("EXISTS (SELECT ...)")
	case *Subquery:
		// 不做完整反解析：子查询只用于内部计划与错误信息，
		// 完整 SQL 文本对客户端查询没有意义。
		b.WriteString("(SELECT ...)")
	case *AnyOp:
		writeExpr(b, n.X)
		b.WriteString(" " + n.Op + " ")
		if n.All {
			b.WriteString("ALL (")
		} else {
			b.WriteString("ANY (")
		}
		writeExpr(b, n.Arr)
		b.WriteString(")")
	case *Between:
		writeExpr(b, n.X)
		if n.Not {
			b.WriteString(" NOT")
		}
		b.WriteString(" BETWEEN ")
		writeExpr(b, n.Lo)
		b.WriteString(" AND ")
		writeExpr(b, n.Hi)
	case *Like:
		writeExpr(b, n.X)
		if n.Not {
			b.WriteString(" NOT")
		}
		if n.IgnoreCase {
			b.WriteString(" ILIKE ")
		} else {
			b.WriteString(" LIKE ")
		}
		writeExpr(b, n.Pattern)
	case *DefaultVal:
		b.WriteString("DEFAULT")
	default:
		fmt.Fprintf(b, "?")
	}
}
