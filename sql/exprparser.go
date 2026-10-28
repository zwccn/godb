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
	left, err := p.parseBitwise()
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
				right, err := p.parseBitwise()
				if err != nil {
					return nil, err
				}
				left = &Binary{Op: op, L: left, R: right}
				continue
			}
		}
		// PostgreSQL 的 OPERATOR(schema.op) 显式写法：psql 的 \d 会发
		//   c.relname OPERATOR(pg_catalog.~) '^(employees)$'
		// 我们只有一套内建操作符，忽略 schema 与 OPERATOR 外壳，
		// 直接把最后那个符号当成普通二元操作符。
		if t.Is("operator") {
			p.pos++
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			name := ""
			for !p.atPunct(")") {
				if !p.acceptPunct(".") {
					name = p.peek().Text
					p.pos++
				}
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			right, err := p.parseBitwise()
			if err != nil {
				return nil, err
			}
			if name == "!=" {
				name = "<>"
			}
			left = &Binary{Op: name, L: left, R: right}
			continue
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
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			// IN (SELECT ...)：子查询形式，和 IN (1, 2, 3) 靠括号内的
			// 第一个词区分（startsQuery 会跳过嵌套括号）。
			if p.startsQuery() {
				st0, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				sel, ok := st0.(*Select)
				if !ok {
					return nil, p.errAt(p.peek(), "expected SELECT")
				}
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
				left = &In{X: left, Sub: &Subquery{Select: sel}, Not: not}
				continue
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

// parseBitwise 解析位运算符（& | #）。PostgreSQL 把这三个和大部分
// "other operator" 放在同一优先级：低于加减、高于比较，左结合。
// 客户端查触发器时会写 (t.tgtype)::integer & 1。
func (p *parser) parseBitwise() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind == Punct && (t.Text == "&" || t.Text == "|" || t.Text == "#") {
			p.pos++
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &Binary{Op: t.Text, L: left, R: right}
			continue
		}
		return left, nil
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
	for {
		if p.acceptPunct("::") {
			tn, ta, serial, err := p.parseTypeRef()
			if err != nil {
				return nil, err
			}
			if serial {
				return nil, p.errAt(p.peek(), "cannot cast to serial")
			}
			e = &Cast{X: e, TypeName: tn, TypeArgs: ta}
			continue
		}
		// x COLLATE "C"：只是排序规则，忽略
		if err := p.skipCollation(); err != nil {
			return nil, err
		}
		// 数组下标 e[i]（PG 的下标从 1 开始）
		if p.acceptPunct("[") {
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			e = &Subscript{X: e, Index: idx}
			continue
		}
		break
	}
	return e, nil
}

func (p *parser) parsePrimaryBase() (Expr, error) {
	t := p.peek()
	// EXISTS (SELECT ...)：只看子查询有没有行。NOT EXISTS 由外层的
	// NOT 包一层 Unary，这里只认 EXISTS 本身。
	if t.Is("exists") {
		p.pos++
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		st0, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		sel, ok := st0.(*Select)
		if !ok {
			return nil, p.errAt(p.peek(), "expected SELECT")
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &Exists{Sub: &Subquery{Select: sel}}, nil
	}
	// 不带括号的「关键字常量」：PostgreSQL 允许直接写 current_user、
	// current_date 之类，它们等价于同名函数调用。
	if t.Kind == Word && !p.peekN(1).IsPunct("(") {
		switch strings.ToLower(t.Text) {
		case "current_user", "session_user", "current_role", "current_catalog",
			"current_schema", "current_date", "current_time", "current_timestamp",
			"localtime", "localtimestamp", "user":
			p.pos++
			return &FuncCall{Name: strings.ToLower(t.Text)}, nil
		}
	}
	// 带类型前缀的字面量：DATE '2024-03-05'、TIMESTAMP '…'、TIME '…'、INTERVAL '…'。
	// 这是 PG 的标准写法，ORM 生成的 SQL 里到处都是（`WHERE ts > TIMESTAMP '2019-01-01'`）。
	// 只在「类型名紧跟字符串」时生效，所以 `SELECT date FROM t` 里的列名不受影响。
	if t.Kind == Word && p.peekN(1).Kind == StringVal {
		switch strings.ToLower(t.Text) {
		case "date", "time", "timestamp", "timestamptz", "timetz", "interval":
			lit := p.peekN(1).Text
			tn := strings.ToLower(t.Text)
			p.pos += 2
			return &Cast{X: &Literal{Val: types.Text(lit)}, TypeName: tn}, nil
		}
	}
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
				st, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				sel, ok := st.(*Select)
				if !ok {
					return nil, p.errAt(p.peek(), "expected SELECT")
				}
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
				return &Subquery{Select: sel}, nil
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
				return p.finishCall(second)
			}
			return &ColumnRef{Table: name, Name: second}, nil
		}
		p.pos = save
	}
	if p.atPunct("(") {
		return p.finishCall(name)
	}
	return &ColumnRef{Name: name}, nil
}

// finishCall 解析完 name(...) 之后检查它是不是窗口函数。
//
// row_number() OVER (...)、sum(x) OVER (...) 这类现在完全没实现，
// 与其让它掉到更深的地方报一个含糊的 42601（客户端只看到 syntax error），
// 不如在这里明确说「不支持」——测试报告里 P2 的最后一条就是指这个。
func (p *parser) finishCall(name string) (Expr, error) {
	e, err := p.parseCallArgs(name)
	if err != nil {
		return nil, err
	}
	if p.atKw("over") {
		return nil, p.errAt(p.peek(), "window functions (%s(...) OVER ...) are not supported", strings.ToLower(name))
	}
	return e, nil
}

// parseCallArgs 解析 name(...) 形式的函数调用（当前位置在 '('）。
func (p *parser) parseCallArgs(name string) (Expr, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	// EXTRACT(field FROM source)：field 是关键字不是表达式，不能当列引用求值
	// （否则 col 层报 column "year" does not exist）。归一成 extract('year', src)。
	if strings.EqualFold(name, "extract") &&
		(p.peek().Kind == Word || p.peek().Kind == QIdent) && p.peekN(1).Is("from") {
		fld := strings.ToLower(p.advance().Text)
		p.pos++ // FROM
		src, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &FuncCall{Name: "extract", Args: []Expr{
			&Literal{Val: types.Text(fld)}, src,
		}}, nil
	}
	fc := &FuncCall{Name: name}
	if p.acceptKw("distinct") {
		fc.Distinct = true
	}
	if p.acceptPunct("*") {
		fc.Star = true
	} else if !p.atPunct(")") {
		// SQL 标准的 substring 写法用关键字而非逗号：
		//   substring(s FROM 2 FOR 3) / substring(s FOR 128)
		// psql 的 \d、pg_dump 都这么写，按普通三参数函数归一化。
		first, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args := []Expr{first}
		if p.acceptKw("from") {
			b, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, b)
		}
		if p.acceptKw("for") {
			c, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if len(args) == 1 {
				// substring(x FOR n) 等价于从第 1 个字符起取 n 个
				args = append(args, &Literal{Val: types.Int(1)})
			}
			args = append(args, c)
		}
		fc.Args = args
		for p.acceptPunct(",") {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			fc.Args = append(fc.Args, a)
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
	// SQL 标准的 ARRAY(SELECT ...)：把子查询的每一行收成数组。
	// 和 ARRAY[...] 一样归一成 array(...) 调用，求值时按参数类型分辨。
	if p.acceptPunct("(") {
		if p.atKw("select") {
			st, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			sel, ok := st.(*Select)
			if !ok {
				return nil, p.errAt(p.peek(), "expected SELECT")
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return &FuncCall{Name: "array", Args: []Expr{&Subquery{Select: sel}}}, nil
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &FuncCall{Name: "array", Args: []Expr{e}}, nil
	}
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
