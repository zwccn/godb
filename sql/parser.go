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
	if t.Kind != Word {
		return "", nil, false, p.errAt(t, "syntax error at or near %q", t.String())
	}
	p.pos++
	name := strings.ToLower(t.Text)
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
	return name, args, serial, nil
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
	if p.atKw("select") {
		return nil, p.errAt(p.peek(), "INSERT ... SELECT is not supported")
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
	return st, nil
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
	return st, nil
}

// ---------- SELECT ----------

func (p *parser) parseSelect() (Statement, error) {
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
	if p.atKw("order") {
		p.pos++
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			oi := &OrderItem{}
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
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
					return nil, err
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
				return nil, err
			}
			s.Limit = e
			continue
		}
		if p.acceptKw("offset") {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.Offset = e
			continue
		}
		break
	}
	return s, nil
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
		n, err := p.ident()
		if err != nil {
			return "", false, err
		}
		return n, true, nil
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
	base, err := p.parseBaseTable()
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
		right, err := p.parseBaseTable()
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

func (p *parser) parseBaseTable() (*BaseTable, error) {
	name, err := p.ident()
	if err != nil {
		return nil, err
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
