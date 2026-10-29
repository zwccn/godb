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
	case "VALUES":
		// `VALUES (1),(2)` 本身就是一条查询语句（PG 如此），走同一套 SELECT 解析
		return p.parseSelect()
	case "WITH":
		p.pos++
		return p.parseWith()
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
	case "VACUUM":
		p.pos++
		// VACUUM 后面可以带一个表名（PG 里还能带 FULL/ANALYZE 之类，
		// 这里只认表名，其它修饰词直接跳过 —— 别让客户端因此报语法错误）。
		v := &Vacuum{}
		for p.peek().Kind == Word {
			w := strings.ToUpper(p.peek().Text)
			if w == "FULL" || w == "FREEZE" || w == "VERBOSE" || w == "ANALYZE" {
				p.pos++
				continue
			}
			name, err := p.tableName()
			if err != nil {
				return nil, err
			}
			v.Table = name
			break
		}
		return v, nil
	case "ANALYZE":
		p.pos++
		// ANALYZE [VERBOSE] [tbl | (tbl, ...)]：我们没有统计信息，接受并忽略
		p.acceptKw("verbose")
		// 可选表名列表
		for p.peek().Kind != EOF && !p.peek().IsPunct(";") {
			p.advance()
		}
		return &Analyze{}, nil
	case "COPY":
		p.pos++
		return p.parseCopy()
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
		ts := &TxStmt{Kind: "begin"}
		if err := p.parseTxModes(ts); err != nil {
			return nil, err
		}
		return ts, nil
	case "START":
		p.pos++
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		ts := &TxStmt{Kind: "begin"}
		if err := p.parseTxModes(ts); err != nil {
			return nil, err
		}
		return ts, nil
	case "COMMIT", "END":
		p.pos++
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		p.skipTxChain() // COMMIT AND [NO] CHAIN
		return &TxStmt{Kind: "commit"}, nil
	case "ROLLBACK", "ABORT":
		p.pos++
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		// ROLLBACK TO [SAVEPOINT] name：回到保存点，事务继续
		if p.acceptKw("to") {
			p.acceptKw("savepoint")
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			return &SavepointStmt{Kind: "rollback_to", Name: name}, nil
		}
		p.skipTxChain() // ROLLBACK AND [NO] CHAIN
		return &TxStmt{Kind: "rollback"}, nil
	case "SAVEPOINT":
		p.pos++
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &SavepointStmt{Kind: "savepoint", Name: name}, nil
	case "RELEASE":
		p.pos++
		p.acceptKw("savepoint")
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &SavepointStmt{Kind: "release", Name: name}, nil
	case "EXPLAIN":
		p.pos++
		analyze := p.acceptKw("analyze") // ANALYZE 可选：带上就真的跑一遍
		p.acceptKw("verbose")            // VERBOSE 接受即忽略
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: st, Analyze: analyze}, nil
	case "COMMENT":
		p.pos++
		if err := p.expectKw("on"); err != nil {
			return nil, err
		}
		return p.parseCommentOn()
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
	case "DISCARD", "RESET", "DEALLOCATE", "CLOSE", "UNLISTEN", "LISTEN":
		// DISCARD { ALL | PLANS | SEQUENCES | TEMPORARY | TEMP }
		// RESET   { ALL | 参数名 }
		// DEALLOCATE [PREPARE] { ALL | 语句名 }
		// CLOSE   { ALL | 游标名 }
		// UNLISTEN { * | 频道名 } / LISTEN 频道名
		// 都是清理会话用的，godb 没有游标和 LISTEN，解析出来当 no-op 确认。
		// 注意必须吃掉剩余 token（UNLISTEN * 里的 * 是符号不是名字），
		// 否则同一条多语句批次里后面的语句会从 * 开始解析，直接语法错。
		cmd := strings.ToUpper(t.Text)
		p.pos++
		if cmd == "DEALLOCATE" {
			p.acceptKw("prepare")
		}
		var parts []string
		for p.peek().Kind != EOF && !p.peek().IsPunct(";") {
			parts = append(parts, p.advance().Text)
		}
		arg := strings.ToUpper(strings.Join(parts, " "))
		// 命令标签跟 PG 保持一致：DISCARD 带参数（DISCARD ALL），
		// RESET / DEALLOCATE / UNLISTEN 只回动词本身；CLOSE 的标签是 CLOSE CURSOR
		tag := cmd
		switch cmd {
		case "DISCARD":
			if arg != "" {
				tag = cmd + " " + arg
			}
		case "CLOSE":
			tag = "CLOSE CURSOR"
		}
		return &SessionCleanup{Command: cmd, Tag: tag}, nil
	}
	return nil, p.errAt(t, "syntax error at or near %q", t.String())
}

// parseCommentOn 解析 COMMENT ON <对象> <名字> IS {'文本' | NULL}。
//
//	COMMENT ON TABLE  t IS '…'
//	COMMENT ON COLUMN t.c IS '…'        （也可以写 schema.t.c）
//	COMMENT ON INDEX / SCHEMA / DATABASE / VIEW / CONSTRAINT …
//
// 名字先按 [a [. b [. c]]] 收齐，再按对象类型解释——不能直接用 tableName()，
// 它会把 COLUMN t.c 里的 t 当成 schema 吃掉。
func (p *parser) parseCommentOn() (Statement, error) {
	st := &CommentOn{}
	switch {
	case p.acceptKw("table"):
		st.Object = "table"
	case p.acceptKw("column"):
		st.Object = "column"
	case p.acceptKw("index"):
		st.Object = "index"
	case p.acceptKw("schema"):
		st.Object = "schema"
	case p.acceptKw("database"):
		st.Object = "database"
	case p.acceptKw("view"):
		st.Object = "view"
	case p.atKw("materialized"):
		p.pos++
		if err := p.expectKw("view"); err != nil {
			return nil, err
		}
		st.Object = "view"
	case p.acceptKw("constraint"):
		st.Object = "constraint"
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
		if err := p.expectKw("on"); err != nil {
			return nil, err
		}
		tbl, err := p.tableName()
		if err != nil {
			return nil, err
		}
		st.Table = tbl
	default:
		return nil, p.errAt(p.peek(), "unsupported COMMENT ON object %q", p.peek().String())
	}

	if st.Object != "constraint" {
		var parts []string
		for {
			id, err := p.ident()
			if err != nil {
				return nil, err
			}
			parts = append(parts, id)
			if !p.acceptPunct(".") {
				break
			}
		}
		switch st.Object {
		case "column":
			// [schema.]table.column：最后一段是列，倒数第二段是表
			if len(parts) < 2 {
				return nil, p.errAt(p.peek(),
					"COMMENT ON COLUMN needs a table name: COLUMN <table>.<column>")
			}
			st.Column = parts[len(parts)-1]
			st.Table = parts[len(parts)-2]
		default:
			// schema.table 时丢掉 schema（我们只有 public）
			st.Table = parts[len(parts)-1]
			st.Name = st.Table
		}
	}

	if err := p.expectKw("is"); err != nil {
		return nil, err
	}
	if p.acceptKw("null") {
		st.IsNull = true
		return st, nil
	}
	t := p.peek()
	if t.Kind != StringVal {
		return nil, p.errAt(t, "COMMENT ON … IS needs a string literal or NULL")
	}
	p.pos++
	st.Comment = t.Text
	return st, nil
}

// parseTxModes 解析 BEGIN / START TRANSACTION 后面的可选事务模式列表：
//
//	ISOLATION LEVEL { SERIALIZABLE | REPEATABLE READ | READ COMMITTED | READ UNCOMMITTED }
//	READ WRITE | READ ONLY
//	[ NOT ] DEFERRABLE
//
// 多个模式用逗号分隔、顺序不限。godb 恒为 SERIALIZABLE（表级 S/X 两阶段封锁，
// 锁持到提交），所以这里只记录客户端请求的模式，不做降级也不报错——
// 更强的隔离保证总是安全的，报错反而会让 Npgsql / Dapper 连不上。
func (p *parser) parseTxModes(ts *TxStmt) error {
	for {
		switch {
		case p.acceptKw("isolation"):
			if err := p.expectKw("level"); err != nil {
				return err
			}
			switch {
			case p.acceptKw("serializable"):
				ts.Isolation = "serializable"
			case p.atKw("repeatable"):
				p.pos++
				if err := p.expectKw("read"); err != nil {
					return err
				}
				ts.Isolation = "repeatable read"
			case p.acceptKw("read"):
				switch {
				case p.acceptKw("committed"):
					ts.Isolation = "read committed"
				case p.acceptKw("uncommitted"):
					ts.Isolation = "read uncommitted"
				default:
					return p.errAt(p.peek(), "expected COMMITTED or UNCOMMITTED")
				}
			default:
				return p.errAt(p.peek(),
					"expected SERIALIZABLE, REPEATABLE READ, READ COMMITTED or READ UNCOMMITTED")
			}
		case p.acceptKw("read"):
			switch {
			case p.acceptKw("only"):
				ts.ReadOnly = true
			case p.acceptKw("write"):
				ts.ReadOnly = false
			default:
				return p.errAt(p.peek(), "expected ONLY or WRITE")
			}
		case p.acceptKw("not"):
			if err := p.expectKw("deferrable"); err != nil {
				return err
			}
		case p.acceptKw("deferrable"):
			// DEFERRABLE 只影响 SET CONSTRAINTS 的默认时机，接受即忽略。
		default:
			return nil
		}
		// 模式之间的逗号可省：BEGIN READ ONLY DEFERRABLE 和
		// BEGIN READ ONLY, DEFERRABLE 都合法（PG 的 gram 允许连续写）。
		if p.acceptPunct(",") {
			continue
		}
		if !(p.atKw("isolation") || p.atKw("read") || p.atKw("not") || p.atKw("deferrable")) {
			return nil
		}
	}
}

// skipTxChain 吃掉 COMMIT / ROLLBACK 后面的 AND [NO] CHAIN
// （表示提交/回滚后立刻开新事务）。godb 不链式开启，客户端也不依赖。
func (p *parser) skipTxChain() {
	if !p.atKw("and") {
		return
	}
	p.pos++
	p.acceptKw("no")
	p.acceptKw("chain")
}

// ---------- DDL ----------

// parseDatabaseOptions 解析 CREATE DATABASE 尾部的选项列表（WITH 可省略）：
//
//	OWNER [=] u / TEMPLATE [=] t / ENCODING [=] e / STRATEGY [=] s
//	LOCALE [=] l / LC_COLLATE [=] c / LC_CTYPE [=] c / ICU_LOCALE [=] l
//	LOCALE_PROVIDER [=] p / COLLATION_VERSION [=] v / TABLESPACE [=] t
//	ALLOW_CONNECTIONS [=] b / CONNECTION LIMIT [=] n / IS_TEMPLATE [=] b
//
// 这些选项在 godb 里都没有对应的行为：每个库都是同一套 UTF-8 存储、
// 同一套权限模型、同一个数据目录。认得出、不报错就够了——各种 ORM 的
// CodeFirst 建库时都会带上 ENCODING / OWNER。
func (p *parser) parseDatabaseOptions(st *CreateDatabase) error {
	p.acceptKw("with")
	for {
		t := p.peek()
		if t.Kind == EOF || t.IsPunct(";") {
			return nil
		}
		var key string
		switch {
		case p.acceptKw("owner"):
			key = "owner"
		case p.acceptKw("template"):
			key = "template"
		case p.acceptKw("encoding"):
			key = "encoding"
		case p.acceptKw("strategy"):
			key = "strategy"
		case p.acceptKw("locale"):
			key = "locale"
		case p.acceptKw("lc_collate"):
			key = "lc_collate"
		case p.acceptKw("lc_ctype"):
			key = "lc_ctype"
		case p.acceptKw("icu_locale"):
			key = "icu_locale"
		case p.acceptKw("locale_provider"):
			key = "locale_provider"
		case p.acceptKw("collation_version"):
			key = "collation_version"
		case p.acceptKw("tablespace"):
			key = "tablespace"
		case p.acceptKw("allow_connections"):
			key = "allow_connections"
		case p.acceptKw("connection"):
			if err := p.expectKw("limit"); err != nil {
				return err
			}
			key = "connection_limit"
		case p.acceptKw("is_template"):
			key = "is_template"
		default:
			return p.errAt(t, "unrecognized CREATE DATABASE option %q", t.String())
		}
		p.acceptPunct("=")
		v := p.peek()
		if v.Kind != Word && v.Kind != QIdent && v.Kind != StringVal && v.Kind != NumberVal {
			return p.errAt(v, "expected a value for CREATE DATABASE option %s", strings.ToUpper(key))
		}
		p.pos++
		if st.Options == nil {
			st.Options = map[string]string{}
		}
		st.Options[key] = v.Text
		p.acceptPunct(",")
	}
}

func (p *parser) parseCreate() (Statement, error) {
	// CREATE DATABASE name [ [ WITH ] option [ ... ] ]
	if p.acceptKw("database") {
		if p.atKw("if") {
			// CREATE DATABASE IF NOT EXISTS 不是 PostgreSQL 语法，直接拒绝
			return nil, p.errAt(p.peek(), "CREATE DATABASE does not support IF NOT EXISTS")
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st := &CreateDatabase{Name: name}
		if err := p.parseDatabaseOptions(st); err != nil {
			return nil, err
		}
		return st, nil
	}
	// CREATE [UNIQUE] INDEX name ON table (cols)
	if p.atKw("index") || p.atKw("unique") {
		uniq := p.acceptKw("unique")
		if err := p.expectKw("index"); err != nil {
			return nil, err
		}
		return p.parseCreateIndex(uniq)
	}
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
	// CREATE TABLE ... AS SELECT
	if p.acceptKw("as") {
		st0, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		sel, ok := st0.(*Select)
		if !ok {
			return nil, p.errAt(p.peek(), "expected SELECT")
		}
		ct.As = sel
		return ct, nil
	}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	colPK := 0
	for {
		if p.atKw("primary") || p.atKw("unique") || p.atKw("check") || p.atKw("foreign") ||
			p.atKw("constraint") {
			if err := p.parseTableConstraint(ct); err != nil {
				return nil, err
			}
		} else {
			col, err := p.parseColumnDef()
			if err != nil {
				return nil, err
			}
			// 列级 PRIMARY KEY 只能出现一次；多列主键要走表级 PRIMARY KEY (a, b)。
			if col.PrimaryKey {
				colPK++
				if colPK > 1 {
					return nil, p.errAt(p.peek(), "multiple primary keys for table %q are not allowed", ct.Table)
				}
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
	// 建表语句尾部的存储参数：CREATE TABLE ... WITH (OIDS=FALSE) / WITHOUT OIDS。
	// FreeSql、EFCore 生成的 DDL 固定带这一段。godb 没有 OIDS 这个概念，
	// 整段吃掉即可（也不看 INHERITS / TABLESPACE 之外的其它子句）。
	if p.atKw("without") {
		p.pos++
		p.acceptKw("oids")
	} else if p.acceptKw("with") {
		if p.atPunct("(") {
			if err := p.skipParens(); err != nil {
				return nil, err
			}
		} else {
			// WITH OIDS 这种老写法
			p.acceptKw("oids")
		}
	}
	return ct, nil
}

// skipParens 吃掉一个配对完整的括号组（支持嵌套）。
// 用于忽略客户端 DDL 里我们不关心的选项列表，比如 WITH (fillfactor=70, ...)。
func (p *parser) skipParens() error {
	if err := p.expectPunct("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := p.peek()
		if t.Kind == EOF {
			return p.errAt(t, "unterminated option list")
		}
		if t.IsPunct("(") {
			depth++
		} else if t.IsPunct(")") {
			depth--
		}
		p.pos++
	}
	return nil
}

// parseReferences 解析 REFERENCES 后面的部分：表名 [(列)] [ON DELETE/UPDATE 动作]。
func (p *parser) parseReferences() (*FKDef, error) {
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	fk := &FKDef{RefTable: name}
	if p.atPunct("(") {
		p.pos++
		for {
			c, err := p.ident()
			if err != nil {
				return nil, err
			}
			fk.RefCols = append(fk.RefCols, c)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("on") {
		isDelete := false
		switch {
		case p.acceptKw("delete"):
			isDelete = true
		case p.acceptKw("update"):
		default:
			return nil, p.errAt(p.peek(), "expected DELETE or UPDATE")
		}
		action := ""
		switch {
		case p.acceptKw("cascade"):
			action = "cascade"
		case p.acceptKw("restrict"):
			action = "restrict"
		case p.acceptKw("no"):
			if err := p.expectKw("action"); err != nil {
				return nil, err
			}
			action = "no action"
		case p.acceptKw("set"):
			switch {
			case p.acceptKw("null"):
				action = "set null"
			case p.acceptKw("default"):
				action = "set default"
			default:
				return nil, p.errAt(p.peek(), "expected NULL or DEFAULT")
			}
		default:
			return nil, p.errAt(p.peek(), "unknown ON DELETE/UPDATE action")
		}
		if isDelete {
			fk.OnDelete = action
		} else {
			fk.OnUpdate = action
		}
	}
	return fk, nil
}

func (p *parser) parseTableConstraint(ct *CreateTable) error {
	// 表级约束允许带 CONSTRAINT <名字> 前缀（ORM 生成的建表语句都是这个形态）：
	//   CONSTRAINT "PK_xxx" PRIMARY KEY ("id")
	//   CONSTRAINT "FK_xxx" FOREIGN KEY ("a") REFERENCES ...
	// 名字本身不影响语义（主键/唯一/外键都由列标记承载），解析后丢弃，
	// 但必须吃掉 —— 否则 "constraint" 会被当成列名，紧接着报错。
	if p.acceptKw("constraint") {
		if _, err := p.ident(); err != nil {
			return err
		}
	}
	if p.acceptKw("primary") {
		if err := p.expectKw("key"); err != nil {
			return err
		}
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		// 多列主键：把这些列都打上 PrimaryKey（复合唯一性由引擎按「整组」判重）。
		for _, name := range cols {
			col := findColDef(ct.Cols, name)
			if col == nil {
				return p.errAt(p.peek(), "column %q named in key does not exist", name)
			}
			col.PrimaryKey = true
			col.NotNull = true
		}
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
	if p.acceptKw("foreign") {
		if err := p.expectKw("key"); err != nil {
			return err
		}
		cols, err := p.parseIdentList()
		if err != nil {
			return err
		}
		if err := p.expectKw("references"); err != nil {
			return err
		}
		ref, err := p.parseReferences()
		if err != nil {
			return err
		}
		ref.Cols = cols
		ct.FKs = append(ct.FKs, ref)
		return nil
	}
	if p.acceptKw("check") {
		// 表级 CHECK：按整行求值，可以引用任意列。
		if err := p.expectPunct("("); err != nil {
			return err
		}
		ce, err := p.parseExpr()
		if err != nil {
			return err
		}
		if err := p.expectPunct(")"); err != nil {
			return err
		}
		ct.Checks = append(ct.Checks, String(ce))
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
			// 列级外键：REFERENCES t [(col)] [ON DELETE action] [ON UPDATE action]
			p.pos++
			ref, err := p.parseReferences()
			if err != nil {
				return nil, err
			}
			col.RefTable = ref.RefTable
			if len(ref.RefCols) > 0 {
				col.RefCol = ref.RefCols[0]
			}
			col.RefOnDel = ref.OnDelete
		case p.atKw("check"):
			// 列级 CHECK：只检查本行，求值时和其他列一样按表的一行来算。
			// 表达式存成原文（catalog 里也存文本，重启后重新解析）。
			p.pos++
			if err := p.expectPunct("("); err != nil {
				return nil, err
			}
			ce, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			col.Check = String(ce)
		case p.atKw("generated"):
			if err := p.parseGenerated(col); err != nil {
				return nil, err
			}
		default:
			return col, nil
		}
	}
}

// parseGenerated 解析列定义里的 GENERATED 子句：
//
//	GENERATED { ALWAYS | BY DEFAULT } AS IDENTITY [ ( sequence_options ) ]
//	GENERATED ALWAYS AS ( expr ) STORED        —— 真正的生成列，未实现
//
// IDENTITY 按 SERIAL 执行（整数自增列），序列参数（START WITH / INCREMENT BY /
// NO MINVALUE / CACHE …）接受即忽略：godb 每张表一个计数器，语义上等价于
// 一个从 1 开始、步长 1 的序列。
func (p *parser) parseGenerated(col *ColumnDef) error {
	p.pos++ // GENERATED
	switch {
	case p.acceptKw("always"):
	case p.atKw("by"):
		p.pos++
		if err := p.expectKw("default"); err != nil {
			return err
		}
	default:
		return p.errAt(p.peek(), "expected ALWAYS or BY DEFAULT")
	}
	if err := p.expectKw("as"); err != nil {
		return err
	}
	if !p.atKw("identity") {
		// 走到这里说明是 GENERATED ALWAYS AS (expr) STORED（生成列）。
		return p.errAt(p.peek(), "generated columns (GENERATED ALWAYS AS (expr) STORED) are not supported")
	}
	p.pos++
	// 序列参数括号：整段吃掉，只跳过平衡括号，不求值。
	if p.atPunct("(") {
		depth := 0
		for {
			t := p.peek()
			if t.Kind == EOF {
				return p.errAt(t, "unterminated sequence options")
			}
			p.pos++
			if t.IsPunct("(") {
				depth++
			} else if t.IsPunct(")") {
				depth--
				if depth == 0 {
					break
				}
			}
		}
	}
	switch col.TypeName {
	case "smallint", "int", "int2", "int4", "integer", "bigint", "int8":
	default:
		return p.errAt(p.peek(),
			"identity column %q must be of type smallint, integer or bigint, not %q", col.Name, col.TypeName)
	}
	col.Serial = true
	col.Identity = true
	col.NotNull = true
	return nil
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
	if p.acceptKw("database") {
		st := &DropDatabase{}
		if p.atKw("if") {
			p.pos++
			if err := p.expectKw("exists"); err != nil {
				return nil, err
			}
			st.IfExists = true
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
		return st, nil
	}
	if p.acceptKw("index") {
		st := &DropIndex{}
		if p.atKw("if") {
			p.pos++
			if err := p.expectKw("exists"); err != nil {
				return nil, err
			}
			st.IfExists = true
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Name = name
		return st, nil
	}
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

// parseCopy 解析 COPY t [(列...)] FROM STDIN | TO STDOUT。
//
// 只支持标准输入输出：读写服务器本地文件是安全风险（任意文件读写），
// PostgreSQL 本身也建议用 psql 的 \copy 代替。
func (p *parser) parseCopy() (Statement, error) {
	name, err := p.tableName()
	if err != nil {
		return nil, err
	}
	c := &Copy{Table: name}
	if p.atPunct("(") {
		p.pos++
		for {
			col, err := p.ident()
			if err != nil {
				return nil, err
			}
			c.Cols = append(c.Cols, col)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	switch {
	case p.acceptKw("from"):
		c.To = false
	case p.acceptKw("to"):
		c.To = true
	default:
		return nil, p.errAt(p.peek(), "expected FROM or TO")
	}
	switch {
	case p.acceptKw("stdin"), p.acceptKw("stdout"):
		// 标准流，忽略后面的 WITH (...) 选项
	case p.acceptKw("strict"):
		return nil, p.errAt(p.peek(), "COPY from a file is not supported, use psql's \\copy")
	default:
		return nil, p.errAt(p.peek(), "COPY from/to a file is not supported, use psql's \\copy")
	}
	return c, nil
}

// parseCreateIndex 解析 CREATE INDEX 的剩余部分。
func (p *parser) parseCreateIndex(unique bool) (Statement, error) {
	st := &CreateIndex{Unique: unique}
	// CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON table (cols)
	if p.atKw("if") {
		p.pos++
		if err := p.expectKw("not"); err != nil {
			return nil, err
		}
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		st.IfNotExists = true
	}
	// 索引名可选（PostgreSQL 会自动取），为了简单要求显式给出
	if p.atKw("on") {
		return nil, p.errAt(p.peek(), "CREATE INDEX requires a name")
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st.Name = name
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	tbl, err := p.tableName()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	// 可选的 USING btree/hash：我们没有多种索引方法，忽略
	if p.acceptKw("using") {
		if _, err := p.ident(); err != nil {
			return nil, err
		}
	}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		st.Cols = append(st.Cols, col)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return st, nil
}

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
		oc, err := p.parseOnConflict()
		if err != nil {
			return nil, err
		}
		st.OnConflict = oc
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
	oc, err := p.parseOnConflict()
	if err != nil {
		return nil, err
	}
	st.OnConflict = oc
	ret, err := p.parseReturning()
	if err != nil {
		return nil, err
	}
	st.Returning = ret
	return st, nil
}

// parseAssignments 解析 `col = expr [, col = expr ...]`（UPDATE SET 与
// INSERT ... ON CONFLICT DO UPDATE SET 共用）。
func (p *parser) parseAssignments() ([]*Assignment, error) {
	var out []*Assignment
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
		out = append(out, &Assignment{Col: col, Value: v})
		if !p.acceptPunct(",") {
			return out, nil
		}
	}
}

// parseOnConflict 解析 INSERT 的 ON CONFLICT 子句（可省略）。
//
//	ON CONFLICT [(col, ...)] DO NOTHING
//	ON CONFLICT [(col, ...)] DO UPDATE SET a = excluded.a [, ...] [WHERE ...]
//	ON CONFLICT ON CONSTRAINT 名字 DO NOTHING
//
// 没写目标列时表示「任何唯一冲突都按这个策略处理」。
func (p *parser) parseOnConflict() (*OnConflict, error) {
	if !p.acceptKw("on") {
		return nil, nil
	}
	if err := p.expectKw("conflict"); err != nil {
		return nil, err
	}
	oc := &OnConflict{}
	if p.atPunct("(") {
		cols, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		oc.Target = cols
	} else if p.acceptKw("on") {
		if err := p.expectKw("constraint"); err != nil {
			return nil, err
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		oc.Constraint = name
	}
	if err := p.expectKw("do"); err != nil {
		return nil, err
	}
	switch {
	case p.acceptKw("nothing"):
		oc.DoNothing = true
	case p.acceptKw("update"):
		if err := p.expectKw("set"); err != nil {
			return nil, err
		}
		sets, err := p.parseAssignments()
		if err != nil {
			return nil, err
		}
		oc.Updates = sets
		if p.acceptKw("where") {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			oc.UpdateWhere = e
		}
	default:
		return nil, p.errAt(p.peek(), "expected NOTHING or UPDATE after ON CONFLICT DO")
	}
	return oc, nil
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
	sets, err := p.parseAssignments()
	if err != nil {
		return nil, err
	}
	st.Sets = sets
	// UPDATE ... FROM 额外数据源（关联更新）。和 SELECT 的 FROM 同一套解析：
	// 单表 / 逗号多表 / JOIN / 派生表 `(SELECT …) x` / `(VALUES …) x(a,b)`。
	if p.acceptKw("from") {
		ref, err := p.parseFromRefs()
		if err != nil {
			return nil, err
		}
		st.From = []TableRef{ref}
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
	// DELETE ... USING 额外数据源（按关联条件删行），解析规则同 UPDATE ... FROM
	if p.acceptKw("using") {
		ref, err := p.parseFromRefs()
		if err != nil {
			return nil, err
		}
		st.Using = []TableRef{ref}
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
// parseWith 解析 WITH [RECURSIVE] name [(cols)] AS ( SELECT ) [, ...] <select>。
//
// 只支持非递归的 CTE：物化一次、按名字引用。递归 CTE（WITH RECURSIVE 里
// 自引用）需要迭代求值，暂不支持，会给出明确错误。
func (p *parser) parseWith() (Statement, error) {
	if p.acceptKw("recursive") {
		return nil, p.errAt(p.peek(), "WITH RECURSIVE is not supported")
	}
	var ctes []*CTE
	for {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		ct := &CTE{Name: name}
		// 可选列名列表
		if p.atPunct("(") {
			p.pos++
			for {
				c, err := p.ident()
				if err != nil {
					return nil, err
				}
				ct.Cols = append(ct.Cols, c)
				if p.acceptPunct(",") {
					continue
				}
				break
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("as"); err != nil {
			return nil, err
		}
		// MATERIALIZED / NOT MATERIALIZED 只是优化提示，忽略
		if p.acceptKw("not") {
			p.acceptKw("materialized")
		} else {
			p.acceptKw("materialized")
		}
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
		ct.Select = sel
		ctes = append(ctes, ct)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if !p.atKw("select") {
		return nil, p.errAt(p.peek(), "only SELECT is supported after WITH")
	}
	st1, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	sel, ok := st1.(*Select)
	if !ok {
		return nil, p.errAt(p.peek(), "expected SELECT")
	}
	sel.CTEs = append(ctes, sel.CTEs...)
	return sel, nil
}

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
	// VALUES 也是一种查询源（PG 里 `VALUES (1),(2)` 就是查询，可以和 UNION 组合）
	if p.atKw("values") {
		return p.parseValuesQuery()
	}
	return p.parseSelectCore()
}

// parseValuesQuery 解析 `VALUES (…),(…)`：每行一组括号，行与行之间逗号分隔。
// 独立执行时的列名是 column1、column2……；当派生表用时由 `x(a,b)` 改名。
func (p *parser) parseValuesQuery() (*Select, error) {
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	var rows [][]Expr
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
		rows = append(rows, row)
		if !p.acceptPunct(",") {
			break
		}
	}
	return &Select{Values: rows}, nil
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

// parseFromRefs 解析 UPDATE ... FROM / DELETE ... USING 后面的数据源。
// 规则与 SELECT 的 FROM 完全一致（逗号分隔视为交叉连接、支持 JOIN 与派生表），
// 返回合并后的一棵引用树。
func (p *parser) parseFromRefs() (TableRef, error) {
	var refs []TableRef
	for {
		r, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		refs = append(refs, r)
		if !p.acceptPunct(",") {
			break
		}
	}
	merged := refs[0]
	for _, r := range refs[1:] {
		merged = &Join{Left: merged, Right: r, Type: JoinCross}
	}
	return merged, nil
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
		// schema.函数(...)：psql 会写 pg_catalog.unnest(stxkeys)
		if p.atPunct("(") {
			return p.parseFuncTable(second)
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
