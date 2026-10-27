package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse 把 SQL 文本解析成语句列表。
func Parse(input string) ([]Statement, error) {
	toks, err := Lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var out []Statement
	for {
		for p.peek().IsPunct(";") {
			p.pos++
		}
		if p.peek().Kind == EOF {
			break
		}
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		out = append(out, st)
		t := p.peek()
		if t.Kind == EOF {
			break
		}
		if !t.IsPunct(";") {
			return nil, p.errAt(t, "syntax error at or near %q", t.String())
		}
		p.pos++
	}
	if len(out) == 0 {
		return nil, &Error{Msg: "empty query"}
	}
	return out, nil
}

// ParseExpr 只解析一个表达式（用于列默认值等场景）。
func ParseExpr(src string) (Expr, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != EOF {
		return nil, p.errAt(p.peek(), "syntax error at or near %q", p.peek().String())
	}
	return e, nil
}

type parser struct {
	toks []Token
	pos  int
}

func (p *parser) peek() Token { return p.toks[p.pos] }

func (p *parser) peekN(n int) Token {
	if p.pos+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos+n]
}

func (p *parser) advance() Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) atKw(kw string) bool   { return p.peek().Is(kw) }
func (p *parser) atPunct(s string) bool { return p.peek().IsPunct(s) }

func (p *parser) acceptKw(kw string) bool {
	if p.atKw(kw) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) acceptPunct(s string) bool {
	if p.atPunct(s) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) errAt(t Token, format string, args ...interface{}) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Line: t.Line, Col: t.Col}
}

func (p *parser) expectKw(kw string) error {
	if p.acceptKw(kw) {
		return nil
	}
	t := p.peek()
	return p.errAt(t, "syntax error at or near %q, expected %s", t.String(), strings.ToUpper(kw))
}

func (p *parser) expectPunct(s string) error {
	if p.acceptPunct(s) {
		return nil
	}
	t := p.peek()
	return p.errAt(t, "syntax error at or near %q, expected %q", t.String(), s)
}

// ident 读取一个标识符（裸词会被折叠成小写，双引号标识符也统一折叠，保证解析一致）。
func (p *parser) ident() (string, error) {
	t := p.peek()
	switch t.Kind {
	case Word:
		if IsReserved(t.Text) {
			return "", p.errAt(t, "syntax error at or near %q", t.Text)
		}
		p.pos++
		return strings.ToLower(t.Text), nil
	case QIdent:
		p.pos++
		return strings.ToLower(t.Text), nil
	}
	return "", p.errAt(t, "syntax error at or near %q", t.String())
}

// tableName 解析可选的 schema 限定表名（"public"."t" / public.t / t），
// 返回表名本身。
//
// godb 只有 public 一个 schema，所以前缀被忽略；但客户端（Navicat、
// pgAdmin、各类 ORM）几乎都会带上它，不认就会在建表时直接语法错误。
func (p *parser) tableName() (string, error) {
	first, err := p.ident()
	if err != nil {
		return "", err
	}
	if p.atPunct(".") {
		p.pos++
		second, err := p.ident()
		if err != nil {
			return "", err
		}
		return second, nil
	}
	return first, nil
}

func (p *parser) parseStatement() (Statement, error) {
	t := p.peek()
	if t.Kind != Word {
		return nil, p.errAt(t, "syntax error at or near %q", t.String())
	}
	switch strings.ToUpper(t.Text) {
	case "SELECT":
		return p.parseSelect()
	case "CREATE":
		p.pos++
		return p.parseCreate()
	case "DROP":
		p.pos++
		return p.parseDrop()
	case "ALTER":
		p.pos++
		return p.parseAlter()
	case "TRUNCATE":
		p.pos++
		return p.parseTruncate()
	case "CHECKPOINT", "FLUSH":
		p.pos++
		return &Checkpoint{}, nil
	case "INSERT":
		p.pos++
		return p.parseInsert()
	case "UPDATE":
		p.pos++
		return p.parseUpdate()
	case "DELETE":
		p.pos++
		return p.parseDelete()
	case "BEGIN":
		p.pos++
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		return &TxStmt{Kind: "begin"}, nil
	case "START":
		p.pos++
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		return &TxStmt{Kind: "begin"}, nil
	case "COMMIT", "END":
		p.pos++
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		return &TxStmt{Kind: "commit"}, nil
	case "ROLLBACK", "ABORT":
		p.pos++
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		return &TxStmt{Kind: "rollback"}, nil
	case "EXPLAIN":
		p.pos++
		p.acceptKw("analyze")
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: st}, nil
	case "SHOW":
		p.pos++
		// 参数名原样收下：可能是保留字或带空格（SHOW time zone、SHOW ALL）。
		var parts []string
		for p.peek().Kind != EOF && !p.peek().IsPunct(";") {
			parts = append(parts, p.advance().Text)
		}
		if len(parts) == 0 {
			return nil, p.errAt(p.peek(), "SHOW needs a parameter name")
		}
		return &Show{Name: strings.Join(parts, " ")}, nil
	case "SET":
		p.pos++
		// SET 只是会话参数，引擎不保存状态：整条吃掉，执行层回一个确认。
		var parts []string
		for p.peek().Kind != EOF && !p.peek().IsPunct(";") {
			parts = append(parts, p.advance().Text)
		}
		return &SetStmt{Raw: "SET " + strings.Join(parts, " ")}, nil
	}
	return nil, p.errAt(t, "syntax error at or near %q", t.String())
}

// ---------- DDL ----------

func (p *parser) parseCreate() (Statement, error) {
	if !p.acceptKw("table") {
		return nil, p.errAt(p.peek(), "only CREATE TABLE is supported")
	}
	ct := &CreateTable{}
	if p.atKw("if") {
		p.pos++
		if err := p.expectKw("not"); err != nil {
			return nil, err
		}
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		ct.IfNotExists = true
	}
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	ct.Table = name
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		if p.atKw("primary") || p.atKw("unique") {
			if err := p.parseTableConstraint(ct); err != nil {
				return nil, err
			}
		} else {
			col, err := p.parseColumnDef()
			if err != nil {
				return nil, err
			}
			ct.Cols = append(ct.Cols, col)
		}
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return ct, nil
}

func (p *parser) parseTableConstraint(ct *CreateTable) error {
	if p.acceptKw("primary") {
		if err := p.expectKw("key"); err != nil {
			return err
		}
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		if len(cols) != 1 {
			return p.errAt(p.peek(), "composite primary key is not supported")
		}
		col := findColDef(ct.Cols, cols[0])
		if col == nil {
			return p.errAt(p.peek(), "column %q named in key does not exist", cols[0])
		}
		col.PrimaryKey = true
		col.NotNull = true
		return nil
	}
	if p.acceptKw("unique") {
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		if len(cols) != 1 {
			return p.errAt(p.peek(), "composite unique constraint is not supported")
		}
		col := findColDef(ct.Cols, cols[0])
		if col == nil {
			return p.errAt(p.peek(), "column %q named in key does not exist", cols[0])
		}
		col.Unique = true
		return nil
	}
	return p.errAt(p.peek(), "unsupported table constraint")
}

func findColDef(cols []*ColumnDef, name string) *ColumnDef {
	for _, c := range cols {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (p *parser) parseIdentList() ([]string, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseColumnDef() (*ColumnDef, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	col := &ColumnDef{Name: name}
	tn, ta, serial, err := p.parseTypeRef()
	if err != nil {
		return nil, err
	}
	col.TypeName, col.TypeArgs, col.Serial = tn, ta, serial
	// COLLATE 只用来指定排序规则；我们只有一种（等价于 C/default），
	// 接受但忽略。Navicat 导出的建表语句每列都带
	// "pg_catalog"."default"，不认这个子句整条 CREATE 就失败。
	if err := p.skipCollation(); err != nil {
		return nil, err
	}
	for {
		switch {
		case p.atKw("not"):
			p.pos++
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			col.NotNull = true
		case p.acceptKw("null"):
			col.NotNull = false
		case p.atKw("primary"):
			p.pos++
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			col.PrimaryKey = true
			col.NotNull = true
		case p.acceptKw("unique"):
			col.Unique = true
		case p.acceptKw("default"):
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			col.Default = e
		case p.atKw("references"):
			return nil, p.errAt(p.peek(), "foreign key constraints are not supported")
		case p.atKw("check"):
			return nil, p.errAt(p.peek(), "check constraints are not supported")
		default:
			return col, nil
		}
	}
}

// parseTypeRef 解析类型名（可能带括号参数），返回 serial 标记。
func (p *parser) parseTypeRef() (string, []int, bool, error) {
	t := p.peek()
	// 类型名也允许带引号：客户端会写 'r'::"char"、"char"、"text" 之类
	// （"char" 是 PG 里真实存在的单字节内部类型）。
	if t.Kind != Word && t.Kind != QIdent {
		return "", nil, false, p.errAt(t, "syntax error at or near %q", t.String())
	}
	p.pos++
	name := strings.ToLower(t.Text)
	// 类型名可以带 schema 限定：information_schema.sql_identifier、
	// pg_catalog.int4、pg_catalog."char"。我们只有一套内建类型，
	// 所以丢掉 schema 只取最后一段。
	if p.atPunct(".") && (p.peekN(1).Kind == Word || p.peekN(1).Kind == QIdent) {
		p.pos++
		name = strings.ToLower(p.peek().Text)
		p.pos++
	}
	serial := false
	switch name {
	case "double":
		if p.acceptKw("precision") {
			name = "double precision"
		}
	case "character":
		if p.acceptKw("varying") {
			name = "character varying"
		}
	case "char":
		if p.acceptKw("varying") {
			name = "character varying"
		}
	case "timestamp":
		if p.atKw("without") {
			p.pos++
			if err := p.expectKw("time"); err != nil {
				return "", nil, false, err
			}
			if err := p.expectKw("zone"); err != nil {
				return "", nil, false, err
			}
			name = "timestamp"
		} else if p.atKw("with") {
			p.pos++
			if err := p.expectKw("time"); err != nil {
				return "", nil, false, err
			}
			if err := p.expectKw("zone"); err != nil {
				return "", nil, false, err
			}
			name = "timestamptz"
		}
	case "serial", "serial4":
		name, serial = "integer", true
	case "bigserial", "serial8":
		name, serial = "bigint", true
	}
	var args []int
	if p.acceptPunct("(") {
		for {
			nt := p.peek()
			if nt.Kind != NumberVal {
				return "", nil, false, p.errAt(nt, "syntax error at or near %q", nt.String())
			}
			p.pos++
			n, err := strconv.Atoi(nt.Text)
			if err != nil {
				return "", nil, false, p.errAt(nt, "invalid type modifier %q", nt.Text)
			}
			args = append(args, n)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return "", nil, false, err
		}
	}
	// 数组类型后缀：char[]、text[]、int4[]，客户端会写 '{r,v,m}'::char[]。
	// 我们没有真正的数组类型，统一按文本处理（见 types.ParseType）。
	for p.atPunct("[") {
		p.pos++
		if p.peek().Kind == NumberVal {
			p.pos++
		}
		if err := p.expectPunct("]"); err != nil {
			return "", nil, false, err
		}
		name += "[]"
	}
	return name, args, serial, nil
}

// skipCollation 吃掉可选的 COLLATE [schema.]名字。
//
// 我们没有真正的排序规则实现（只有一种），所以这里只做语法兼容。
func (p *parser) skipCollation() error {
	if !p.acceptKw("collate") {
		return nil
	}
	if _, err := p.ident(); err != nil {
		return err
	}
	if p.atPunct(".") {
		p.pos++
		if _, err := p.ident(); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) parseDrop() (Statement, error) {
	if !p.acceptKw("table") {
		return nil, p.errAt(p.peek(), "only DROP TABLE is supported")
	}
	st := &DropTable{}
	if p.atKw("if") {
		p.pos++
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		st.IfExists = true
	}
	for {
		n, err := p.tableName()
		if err != nil {
			return nil, err
		}
		st.Tables = append(st.Tables, n)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if !p.acceptKw("cascade") {
		p.acceptKw("restrict")
	}
	return st, nil
}

func (p *parser) parseAlter() (Statement, error) {
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	st := &AlterTable{Table: name}
	act := &AlterAction{}
	switch {
	case p.acceptKw("add"):
		p.acceptKw("column")
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		act.Kind = "add_column"
		act.Col = col
	case p.acceptKw("drop"):
		p.acceptKw("column")
		if p.atKw("if") {
			p.pos++
			if err := p.expectKw("exists"); err != nil {
				return nil, err
			}
		}
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		act.Kind = "drop_column"
		act.Name = n
		if !p.acceptKw("cascade") {
			p.acceptKw("restrict")
		}
	case p.acceptKw("rename"):
		if p.acceptKw("to") {
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			act.Kind = "rename_table"
			act.NewName = n
		} else {
			p.acceptKw("column")
			old, err := p.ident()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("to"); err != nil {
				return nil, err
			}
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			act.Kind = "rename_column"
			act.Name = old
			act.NewName = n
		}
	case p.acceptKw("alter"):
		p.acceptKw("column")
		n, err := p.ident()
		if err != nil {
			return nil, err
		}
		if !p.acceptKw("type") {
			if err := p.expectKw("set"); err != nil {
				return nil, err
			}
			if err := p.expectKw("data"); err != nil {
				return nil, err
			}
			if err := p.expectKw("type"); err != nil {
				return nil, err
			}
		}
		tn, ta, serial, err := p.parseTypeRef()
		if err != nil {
			return nil, err
		}
		if serial {
			return nil, p.errAt(p.peek(), "cannot alter column to serial")
		}
		act.Kind = "alter_type"
		act.Name = n
		act.TypeName, act.TypeArgs = tn, ta
	default:
		return nil, p.errAt(p.peek(), "unsupported ALTER TABLE action")
	}
	st.Action = act
	return st, nil
}

func (p *parser) parseTruncate() (Statement, error) {
	p.acceptKw("table")
	n, err := p.tableName()
	if err != nil {
		return nil, err
	}
	p.acceptKw("restart")
	if p.atKw("identity") {
		p.pos++
	}
	return &Truncate{Table: n}, nil
}

// ---------- DML ----------

func (p *parser) parseInsert() (Statement, error) {
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	st := &Insert{Table: name}
	if p.atPunct("(") {
		cols, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		st.Columns = cols
	}
	// INSERT ... SELECT：数据来自一条查询。PG 也允许用括号包住那条查询。
	if p.atKw("select") || (p.atPunct("(") && p.startsQuery()) {
		st0, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		sel, ok := st0.(*Select)
		if !ok {
			return nil, p.errAt(p.peek(), "expected SELECT")
		}
		st.Select = sel
		ret, err := p.parseReturning()
		if err != nil {
			return nil, err
		}
		st.Returning = ret
		return st, nil
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		st.Rows = append(st.Rows, row)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	ret, err := p.parseReturning()
	if err != nil {
		return nil, err
	}
	st.Returning = ret
	return st, nil
}

// parseReturning 解析可选的 RETURNING 输出项列表。
func (p *parser) parseReturning() ([]*SelectItem, error) {
	if !p.acceptKw("returning") {
		return nil, nil
	}
	var items []*SelectItem
	for {
		it, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		items = append(items, it)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	return items, nil
}

func (p *parser) parseUpdate() (Statement, error) {
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	st := &Update{Table: name}
	if alias, ok, err := p.parseOptAlias(); err != nil {
		return nil, err
	} else if ok {
		st.Alias = alias
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("="); err != nil {
			return nil, err
		}
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Sets = append(st.Sets, &Assignment{Col: col, Value: v})
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if p.acceptKw("where") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	ret, err := p.parseReturning()
	if err != nil {
		return nil, err
	}
	st.Returning = ret
	return st, nil
}

func (p *parser) parseDelete() (Statement, error) {
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	st := &Delete{Table: name}
	if alias, ok, err := p.parseOptAlias(); err != nil {
		return nil, err
	} else if ok {
		st.Alias = alias
	}
	if p.acceptKw("where") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Where = e
	}
	ret, err := p.parseReturning()
	if err != nil {
		return nil, err
	}
	st.Returning = ret
	return st, nil
}

// ---------- SELECT ----------

// parseSelect 解析一条完整查询：核心 SELECT + 集合运算 + ORDER BY/LIMIT。
func (p *parser) parseSelect() (Statement, error) {
	s, err := p.parseSetExpr()
	if err != nil {
		return nil, err
	}
	if err := p.parseSelectSuffix(s); err != nil {
		return nil, err
	}
	return s, nil
}

// parseSetExpr 解析 UNION / INTERSECT / EXCEPT 链（左结合）。
//
// 集合运算节点本身是个空壳 Select：SetLeft 与 SetRight 才是两个操作数，
// 它自己的 Items/From 都是空的。这样左边不需要被改造成「半个查询」。
func (p *parser) parseSetExpr() (*Select, error) {
	left, err := p.parseSetOperand()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.atKw("union"):
			op = "union"
		case p.atKw("intersect"):
			op = "intersect"
		case p.atKw("except"):
			op = "except"
		default:
			return left, nil
		}
		p.pos++
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		right, err := p.parseSetOperand()
		if err != nil {
			return nil, err
		}
		left = &Select{SetOp: op, SetAll: all, SetLeft: left, SetRight: right}
	}
}

// parseSetOperand 解析集合运算的一个操作数：可以是括号里的子查询，
// 也可以是裸的 SELECT 核心。
func (p *parser) parseSetOperand() (*Select, error) {
	if p.atPunct("(") {
		p.pos++
		inner, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		sel, ok := inner.(*Select)
		if !ok {
			return nil, p.errAt(p.peek(), "expected SELECT")
		}
		return sel, nil
	}
	return p.parseSelectCore()
}

// parseSelectCore 解析 SELECT ... [GROUP BY] [HAVING]，不含集合运算与
// ORDER BY/LIMIT（那些属于整条查询，见 parseSelectSuffix）。
func (p *parser) parseSelectCore() (*Select, error) {
	if err := p.expectKw("select"); err != nil {
		return nil, err
	}
	s := &Select{}
	if p.acceptKw("distinct") {
		s.Distinct = true
	} else {
		p.acceptKw("all")
	}
	for {
		it, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, it)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if p.acceptKw("from") {
		for {
			ref, err := p.parseTableRef()
			if err != nil {
				return nil, err
			}
			s.From = append(s.From, ref)
			if p.acceptPunct(",") {
				s.From = append(s.From, nil)
				continue
			}
			break
		}
		// 逗号分隔的 FROM 视为交叉连接
		var merged TableRef
		for _, r := range s.From {
			if r == nil {
				continue
			}
			if merged == nil {
				merged = r
				continue
			}
			merged = &Join{Left: merged, Right: r, Type: JoinCross}
		}
		s.From = []TableRef{merged}
	}
	if p.acceptKw("where") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Where = e
	}
	if p.atKw("group") {
		p.pos++
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.GroupBy = append(s.GroupBy, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if p.acceptKw("having") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.Having = e
	}
	return s, nil
}

// parseSelectSuffix 解析 ORDER BY / LIMIT / OFFSET，作用在整条查询上。
func (p *parser) parseSelectSuffix(s *Select) error {
	if p.atKw("order") {
		p.pos++
		if err := p.expectKw("by"); err != nil {
			return err
		}
		for {
			oi := &OrderItem{}
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			oi.Expr = e
			if p.acceptKw("desc") {
				oi.Desc = true
			} else {
				p.acceptKw("asc")
			}
			if p.acceptKw("nulls") {
				var first bool
				if p.acceptKw("first") {
					first = true
				} else if err := p.expectKw("last"); err != nil {
					return err
				}
				oi.NullsFirst = &first
			}
			s.OrderBy = append(s.OrderBy, oi)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	for {
		if p.acceptKw("limit") {
			if p.acceptKw("all") {
				continue
			}
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			s.Limit = e
			continue
		}
		if p.acceptKw("offset") {
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			s.Offset = e
			continue
		}
		break
	}
	return nil
}

func (p *parser) parseSelectItem() (*SelectItem, error) {
	if p.acceptPunct("*") {
		return &SelectItem{Star: true}, nil
	}
	if (p.peek().Kind == Word || p.peek().Kind == QIdent) && p.peekN(1).IsPunct(".") && p.peekN(2).IsPunct("*") {
		t, err := p.ident()
		if err != nil {
			return nil, err
		}
		p.pos += 2
		return &SelectItem{Star: true, StarTable: t}, nil
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	it := &SelectItem{Expr: e}
	if alias, ok, err := p.parseOptAlias(); err != nil {
		return nil, err
	} else if ok {
		it.Alias = alias
	}
	return it, nil
}

// parseOptAlias 解析可选别名（[AS] name）。裸词别名不能是保留字。
func (p *parser) parseOptAlias() (string, bool, error) {
	if p.acceptKw("as") {
		// AS 后面的别名允许是保留字：PostgreSQL 的 ColLabel 规则里别名可以
		// 用任何关键字，客户端会写 `pg_get_expr(...) AS default`、
		// `... AS user`、`... AS comment` 这类。裸别名（不带 AS）仍然
		// 只接受非保留字，否则 `FROM t JOIN u` 会把 JOIN 当成别名。
		t := p.peek()
		if t.Kind == Word || t.Kind == QIdent {
			p.pos++
			return strings.ToLower(t.Text), true, nil
		}
		return "", false, p.errAt(t, "syntax error at or near %q", t.String())
	}
	t := p.peek()
	if t.Kind == QIdent {
		p.pos++
		return strings.ToLower(t.Text), true, nil
	}
	if t.Kind == Word && !IsReserved(t.Text) {
		p.pos++
		return strings.ToLower(t.Text), true, nil
	}
	return "", false, nil
}

func (p *parser) parseTableRef() (TableRef, error) {
	base, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	var ref TableRef = base
	for {
		jt := JoinInner
		explicit := false
		switch {
		case p.acceptKw("join"):
			explicit = true
		case p.acceptKw("inner"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			explicit = true
		case p.acceptKw("left"):
			p.acceptKw("outer")
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			jt, explicit = JoinLeft, true
		case p.acceptKw("right"):
			p.acceptKw("outer")
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			jt, explicit = JoinRight, true
		case p.atKw("full"):
			return nil, p.errAt(p.peek(), "FULL JOIN is not supported")
		case p.acceptKw("cross"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			jt, explicit = JoinCross, true
		case p.atKw("natural"):
			return nil, p.errAt(p.peek(), "NATURAL JOIN is not supported")
		}
		if !explicit {
			break
		}
		right, err := p.parseTablePrimary()
		if err != nil {
			return nil, err
		}
		var on Expr
		if jt == JoinCross {
			if p.atKw("on") {
				return nil, p.errAt(p.peek(), "CROSS JOIN cannot have an ON clause")
			}
		} else {
			if err := p.expectKw("on"); err != nil {
				return nil, err
			}
			on, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
		}
		ref = &Join{Left: ref, Right: right, Type: jt, On: on}
	}
	return ref, nil
}

// parseTablePrimary 解析 FROM 里的一项：裸表、schema.table、集合函数、
// 括号子查询，或括号联接组。
func (p *parser) parseTablePrimary() (TableRef, error) {
	if p.atPunct("(") {
		if p.startsQuery() {
			p.pos++
			st, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			sel, ok := st.(*Select)
			if !ok {
				return nil, p.errAt(p.peek(), "expected SELECT")
			}
			alias, cols, err := p.parseAliasCols()
			if err != nil {
				return nil, err
			}
			return &SubTable{Select: sel, Alias: alias, Cols: cols}, nil
		}
		p.pos++
		inner, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		alias, _, err := p.parseOptAlias()
		if err != nil {
			return nil, err
		}
		return &ParenJoin{Inner: inner, Alias: alias}, nil
	}
	return p.parseBaseTable()
}

// startsQuery 判断当前（跳过左括号后）是不是一个查询。
//
// 用它来区分 FROM 里的两种括号：括号子查询 ((SELECT ...) UNION ...) 和
// 括号联接组 ((a JOIN b ON ...))。跳过连续的 "(" 后第一个词决定一切，
// 这样嵌套的括号集合运算也能正确识别，而且子查询里的语法错误不会被
// 当成「括号联接组」的解析失败而丢掉。
func (p *parser) startsQuery() bool {
	for i := p.pos; i < len(p.toks); i++ {
		t := p.toks[i]
		if t.IsPunct("(") {
			continue
		}
		return t.Is("select") || t.Is("with") || t.Is("values")
	}
	return false
}

// parseAliasCols 解析 [AS] alias [(col, ...)]。
// 列定义列表在 PG 里既可以给列改名（FROM 子查询、集合函数都用这个写法），
// 也可以写类型；我们只关心名字，类型留给来源自己。
func (p *parser) parseAliasCols() (string, []string, error) {
	alias, ok, err := p.parseOptAlias()
	if err != nil {
		return "", nil, err
	}
	if alias == "" && !ok {
		alias = ""
	}
	var cols []string
	if p.atPunct("(") {
		p.pos++
		for {
			c, err := p.ident()
			if err != nil {
				return "", nil, err
			}
			cols = append(cols, strings.ToLower(c))
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return "", nil, err
		}
	}
	return alias, cols, nil
}

// parseBaseTable 解析 [schema.]表名 [alias]，或集合函数 func(...) [alias] [(col,...)]。
func (p *parser) parseBaseTable() (TableRef, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if p.atPunct("(") {
		return p.parseFuncTable(name)
	}
	bt := &BaseTable{Table: name}
	// 支持限定名 schema.table（pg_catalog.pg_class、information_schema.tables）
	if p.atPunct(".") {
		p.pos++
		second, err := p.ident()
		if err != nil {
			return nil, err
		}
		bt.Schema = name
		bt.Table = second
	}
	if alias, ok, err := p.parseOptAlias(); err != nil {
		return nil, err
	} else if ok {
		bt.Alias = alias
	}
	return bt, nil
}

// parseFuncTable 解析 FROM 里的集合函数：name(args) [AS] alias [(col, ...)]。
func (p *parser) parseFuncTable(name string) (TableRef, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	ft := &FuncTable{Name: strings.ToLower(name)}
	if !p.atPunct(")") {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ft.Args = append(ft.Args, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	alias, cols, err := p.parseAliasCols()
	if err != nil {
		return nil, err
	}
	ft.Alias, ft.Cols = alias, cols
	return ft, nil
}
