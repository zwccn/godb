package sql

import (
	"fmt"
	"strconv"
	"strings"

	"godb/types"
)

// AggregateFuncs 是本库支持的聚合函数。
var AggregateFuncs = map[string]bool{
	"count": true,
	"sum":   true,
	"avg":   true,
	"min":   true,
	"max":   true,
}

// IsAggregate 判断函数名是否为聚合函数。
func IsAggregate(name string) bool { return AggregateFuncs[strings.ToLower(name)] }

// ContainsAggregate 判断表达式里是否出现聚合函数。
func ContainsAggregate(e Expr) bool {
	found := false
	Walk(e, func(n Expr) bool {
		if fc, ok := n.(*FuncCall); ok && IsAggregate(fc.Name) {
			found = true
			return false
		}
		return true
	})
	return found
}

// Walk 深度优先遍历表达式；visit 返回 false 时不再深入该节点。
func Walk(e Expr, visit func(Expr) bool) {
	if e == nil {
		return
	}
	if !visit(e) {
		return
	}
	switch n := e.(type) {
	case *Binary:
		Walk(n.L, visit)
		Walk(n.R, visit)
	case *Unary:
		Walk(n.X, visit)
	case *FuncCall:
		for _, a := range n.Args {
			Walk(a, visit)
		}
	case *Case:
		Walk(n.Operand, visit)
		for _, w := range n.Whens {
			Walk(w.Cond, visit)
			Walk(w.Result, visit)
		}
		Walk(n.Else, visit)
	case *Cast:
		Walk(n.X, visit)
	case *Subscript:
		Walk(n.X, visit)
		Walk(n.Index, visit)
	case *AnyOp:
		Walk(n.X, visit)
		Walk(n.Arr, visit)
	case *Subquery:
		// 子查询整体是一个叶子节点：不展开内部表达式（避免和外层
		// GROUP BY 的规范化冲突），canon 用它的指针做稳定 key。
	case *IsNull:
		Walk(n.X, visit)
	case *In:
		Walk(n.X, visit)
		for _, a := range n.List {
			Walk(a, visit)
		}
	case *Between:
		Walk(n.X, visit)
		Walk(n.Lo, visit)
		Walk(n.Hi, visit)
	case *Like:
		Walk(n.X, visit)
		Walk(n.Pattern, visit)
	}
}

// Canonical 生成表达式的规范文本，用于 GROUP BY / 聚合去重比较。
func Canonical(e Expr) string {
	var b strings.Builder
	canon(&b, e)
	return b.String()
}

func canon(b *strings.Builder, e Expr) {
	if e == nil {
		b.WriteString("_")
		return
	}
	switch n := e.(type) {
	case *Literal:
		b.WriteString("L")
		b.WriteString(types.Key([]types.Value{n.Val}))
	case *ColumnRef:
		b.WriteString("C")
		b.WriteString(strings.ToLower(n.Table))
		b.WriteByte('.')
		b.WriteString(strings.ToLower(n.Name))
	case *Binary:
		b.WriteString("B")
		b.WriteString(n.Op)
		b.WriteByte('(')
		canon(b, n.L)
		b.WriteByte(',')
		canon(b, n.R)
		b.WriteByte(')')
	case *Unary:
		b.WriteString("U")
		b.WriteString(n.Op)
		b.WriteByte('(')
		canon(b, n.X)
		b.WriteByte(')')
	case *FuncCall:
		b.WriteString("F")
		b.WriteString(strings.ToLower(n.Name))
		if n.Distinct {
			b.WriteString("D")
		}
		b.WriteByte('(')
		if n.Star {
			b.WriteByte('*')
		}
		for i, a := range n.Args {
			if i > 0 {
				b.WriteByte(',')
			}
			canon(b, a)
		}
		b.WriteByte(')')
	case *Case:
		b.WriteString("K(")
		canon(b, n.Operand)
		for _, w := range n.Whens {
			b.WriteString(";")
			canon(b, w.Cond)
			b.WriteString(":")
			canon(b, w.Result)
		}
		b.WriteString("|")
		canon(b, n.Else)
		b.WriteByte(')')
	case *Subscript:
		b.WriteString("S(")
		canon(b, n.X)
		b.WriteString(":")
		canon(b, n.Index)
		b.WriteByte(')')
	case *Cast:
		b.WriteString("T(")
		canon(b, n.X)
		b.WriteString(":")
		b.WriteString(n.TypeName)
		for _, a := range n.TypeArgs {
			b.WriteString(",")
			b.WriteString(strconv.Itoa(a))
		}
		b.WriteByte(')')
	case *IsNull:
		b.WriteString("N(")
		if n.Not {
			b.WriteString("!")
		}
		canon(b, n.X)
		b.WriteByte(')')
	case *In:
		b.WriteString("I(")
		if n.Not {
			b.WriteString("!")
		}
		canon(b, n.X)
		for _, a := range n.List {
			b.WriteString(",")
			canon(b, a)
		}
		b.WriteByte(')')
	case *Subquery:
		// 同一次解析出来的 AST 指针稳定，足够作为分组/去重的 key
		b.WriteString("S(")
		b.WriteString(fmt.Sprintf("%p", n))
		b.WriteByte(')')
	case *AnyOp:
		b.WriteString("A(")
		b.WriteString(n.Op)
		if n.All {
			b.WriteString("ALL")
		} else {
			b.WriteString("ANY")
		}
		b.WriteString(",")
		canon(b, n.X)
		b.WriteString(",")
		canon(b, n.Arr)
		b.WriteByte(')')
	case *Between:
		b.WriteString("W(")
		if n.Not {
			b.WriteString("!")
		}
		canon(b, n.X)
		b.WriteString(",")
		canon(b, n.Lo)
		b.WriteString(",")
		canon(b, n.Hi)
		b.WriteByte(')')
	case *Like:
		b.WriteString("P(")
		if n.Not {
			b.WriteString("!")
		}
		if n.IgnoreCase {
			b.WriteString("i")
		}
		canon(b, n.X)
		b.WriteString(",")
		canon(b, n.Pattern)
		b.WriteByte(')')
	case *DefaultVal:
		b.WriteString("DEFAULT")
	default:
		b.WriteString("?")
	}
}
