package sql

import (
	"strconv"
	"strings"

	"godb/types"
)

// parseExpr 是表达式解析入口（OR 优先级最低）。
func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &Binary{Op: "or", L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("and") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &Binary{Op: "and", L: left, R: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.acceptKw("not") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &Unary{Op: "not", X: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind == Punct {
			op := ""
			switch t.Text {
			case "=", "<", ">", "<=", ">=", "<>", "!=", "~", "~*", "!~", "!~*":
				op = t.Text
			}
			if op != "" {
				p.pos++
				if op == "!=" {
					op = "<>"
				}
				// x <op> ANY (arr) / x <op> ALL (arr)
				if p.atKw("any") || p.atKw("all") {
					all := p.atKw("all")
					p.pos++
					if err := p.expectPunct("("); err != nil {
						return nil, err
					}
					arr, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					if err := p.expectPunct(")"); err != nil {
						return nil, err
					}
					left = &AnyOp{Op: op, All: all, X: left, Arr: arr}
					continue
				}
				right, err := p.parseAdditive()
				if err != nil {
					return nil, err
				}
				left = &Binary{Op: op, L: left, R: right}
				continue
			}
		}
		if t.Is("is") {
			p.pos++
			not := p.acceptKw("not")
			if p.acceptKw("null") {
				left = &IsNull{X: left, Not: not}
				continue
			}
			if p.acceptKw("true") {
				left = &Binary{Op: "is", L: left, R: &Literal{Val: types.Bool(true)}}
				if not {
					left = &Unary{Op: "not", X: left}
				}
				continue
			}
			if p.acceptKw("false") {
				left = &Binary{Op: "is", L: left, R: &Literal{Val: types.Bool(false)}}
				if not {
					left = &Unary{Op: "not", X: left}
				}
				continue
			}
			nt := p.peek()
			return nil, p.errAt(nt, "syntax error at or near %q", nt.String())
		}

		not := false
		save := p.pos
		if t.Is("not") {
			p.pos++
			not = true
			t = p.peek()
		}
		switch {
		case t.Is("in"):
			p.pos++
			if p.atKw("select") {
				return nil, p.errAt(p.peek(), "subqueries are not supported")
			}
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			var list []Expr
			if !p.atPunct(")") {
				for {
					e, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					list = append(list, e)
					if p.acceptPunct(",") {
						continue
					}
					break
				}
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			left = &In{X: left, List: list, Not: not}
		case t.Is("between"):
			p.pos++
			lo, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("and"); err != nil {
				return nil, err
			}
			hi, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &Between{X: left, Lo: lo, Hi: hi, Not: not}
		case t.Is("like"), t.Is("ilike"):
			ignoreCase := t.Is("ilike")
			p.pos++
			pat, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &Like{X: left, Pattern: pat, Not: not, IgnoreCase: ignoreCase}
		default:
			p.pos = save
			return left, nil
		}
	}
}

func (p *parser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind == Punct && (t.Text == "+" || t.Text == "-" || t.Text == "||") {
			p.pos++
			right, err := p.parseMultiplicative()
			if err != nil {
				return nil, err
			}
			left = &Binary{Op: t.Text, L: left, R: right}
			continue
		}
		return left, nil
	}
}

func (p *parser) parseMultiplicative() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind == Punct && (t.Text == "*" || t.Text == "/" || t.Text == "%") {
			p.pos++
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = &Binary{Op: t.Text, L: left, R: right}
			continue
		}
		return left, nil
	}
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if t.Kind == Punct && (t.Text == "-" || t.Text == "+") {
		p.pos++
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if t.Text == "-" {
			return &Unary{Op: "-", X: x}, nil
		}
		return &Unary{Op: "+", X: x}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	e, err := p.parsePrimaryBase()
	if err != nil {
		return nil, err
	}
	for p.acceptPunct("::") {
		tn, ta, serial, err := p.parseTypeRef()
		if err != nil {
			return nil, err
		}
		if serial {
			return nil, p.errAt(p.peek(), "cannot cast to serial")
		}
		e = &Cast{X: e, TypeName: tn, TypeArgs: ta}
	}
	return e, nil
}

func (p *parser) parsePrimaryBase() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case NumberVal:
		p.pos++
		return numberLiteral(t)
	case StringVal:
		p.pos++
		return &Literal{Val: types.Text(t.Text)}, nil
	case Punct:
		if t.Text == "(" {
			p.pos++
			if p.atKw("select") {
				return nil, p.errAt(p.peek(), "subqueries are not supported")
			}
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
		return nil, p.errAt(t, "syntax error at or near %q", t.String())
	case Word:
		switch strings.ToUpper(t.Text) {
		case "NULL":
			p.pos++
			return &Literal{Val: types.Null}, nil
		case "TRUE":
			p.pos++
			return &Literal{Val: types.Bool(true)}, nil
		case "FALSE":
			p.pos++
			return &Literal{Val: types.Bool(false)}, nil
		case "DEFAULT":
			p.pos++
			return &DefaultVal{}, nil
		case "CAST":
			p.pos++
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("as"); err != nil {
				return nil, err
			}
			tn, ta, serial, err := p.parseTypeRef()
			if err != nil {
				return nil, err
			}
			if serial {
				return nil, p.errAt(p.peek(), "cannot cast to serial")
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return &Cast{X: x, TypeName: tn, TypeArgs: ta}, nil
		case "CASE":
			return p.parseCase()
		case "ARRAY":
			return p.parseArrayLiteral()
		}
	}

	// 标识符：函数调用 或 列引用
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	// 限定名：
	//   schema.func(...)      —— 客户端大量使用 pg_catalog.format_type(...)
	//   table.column          —— 常规列引用
	if p.atPunct(".") {
		save := p.pos
		p.pos++
		if second, e2 := p.ident(); e2 == nil {
			if p.atPunct("(") {
				return p.parseCallArgs(second)
			}
			return &ColumnRef{Table: name, Name: second}, nil
		}
		p.pos = save
	}
	if p.atPunct("(") {
		return p.parseCallArgs(name)
	}
	return &ColumnRef{Name: name}, nil
}

// parseCallArgs 解析 name(...) 形式的函数调用（当前位置在 '('）。
func (p *parser) parseCallArgs(name string) (Expr, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	fc := &FuncCall{Name: name}
	if p.acceptKw("distinct") {
		fc.Distinct = true
	}
	if p.acceptPunct("*") {
		fc.Star = true
	} else if !p.atPunct(")") {
		for {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			fc.Args = append(fc.Args, a)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return fc, nil
}

// parseArrayLiteral 解析 ARRAY[...]，内部表示成一个 array(...) 函数调用。
func (p *parser) parseArrayLiteral() (Expr, error) {
	p.pos++ // ARRAY
	if err := p.expectPunct("["); err != nil {
		return nil, err
	}
	fc := &FuncCall{Name: "array"}
	if !p.atPunct("]") {
		for {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			fc.Args = append(fc.Args, a)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if err := p.expectPunct("]"); err != nil {
		return nil, err
	}
	return fc, nil
}

func (p *parser) parseCase() (Expr, error) {
	if err := p.expectKw("case"); err != nil {
		return nil, err
	}
	c := &Case{}
	if !p.atKw("when") {
		op, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Operand = op
	}
	for p.acceptKw("when") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		res, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, &When{Cond: cond, Result: res})
	}
	if len(c.Whens) == 0 {
		return nil, p.errAt(p.peek(), "CASE requires at least one WHEN clause")
	}
	if p.acceptKw("else") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Else = e
	}
	if err := p.expectKw("end"); err != nil {
		return nil, err
	}
	return c, nil
}

func numberLiteral(t Token) (Expr, error) {
	text := t.Text
	if strings.ContainsAny(text, ".eE") {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, &Error{Msg: "invalid number literal " + text, Line: t.Line, Col: t.Col}
		}
		return &Literal{Val: types.Float(f)}, nil
	}
	i, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, &Error{Msg: "invalid number literal " + text, Line: t.Line, Col: t.Col}
	}
	return &Literal{Val: types.Int(i)}, nil
}
