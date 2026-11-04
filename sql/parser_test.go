package sql

// 解析器的表驱动用例。
//
// 为什么要有这个文件：sql 包长期是「零测试」——4 千多行词法 + 语法，全靠 db 包
// 的端到端用例间接覆盖。端到端失败时只能看到「结果不对」，分不清是解析错了、
// 规划错了还是算子错了；而且只有「恰好被某条查询走到」的语法才有保护。
//
// 这里的断言方式：把 AST 折成一行「形状」（见 shapeStmt），与期望字符串逐字比对。
// 不深比较 AST 结构体，是因为结构体字段多、以后再加工字段会让所有用例一起改；
// 形状串只写「这句话被理解成了什么」，读失败信息时一眼能看出偏差。
//
// 表达式单独用 ParseExpr + String() 做「解析→打印」往返，覆盖优先级与结合性。

import (
	"fmt"
	"strings"
	"testing"
)

// ---------- 形状描述 ----------

func joinStrs(ss []string, sep string) string { return strings.Join(ss, sep) }

func shapeInts(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprint(n)
	}
	return joinStrs(out, ",")
}

func shapeCol(c *ColumnDef) string {
	typ := c.TypeName
	if len(c.TypeArgs) > 0 {
		typ += "(" + shapeInts(c.TypeArgs) + ")"
	}
	parts := []string{c.Name, typ}
	if c.NotNull {
		parts = append(parts, "NOT NULL")
	}
	if c.PrimaryKey {
		parts = append(parts, "PRIMARY KEY")
	}
	if c.Unique {
		parts = append(parts, "UNIQUE")
	}
	if c.Serial {
		parts = append(parts, "SERIAL")
	}
	if c.Identity {
		parts = append(parts, "IDENTITY")
	}
	if c.Default != nil {
		parts = append(parts, "DEFAULT "+String(c.Default))
	}
	if c.Check != "" {
		parts = append(parts, "CHECK("+c.Check+")")
	}
	if c.RefTable != "" {
		r := "REFERENCES " + c.RefTable
		if c.RefCol != "" {
			r += "(" + c.RefCol + ")"
		}
		if c.RefOnDel != "" {
			r += " ON DELETE " + c.RefOnDel
		}
		parts = append(parts, r)
	}
	return joinStrs(parts, " ")
}

func shapeFK(fk *FKDef) string {
	s := "FOREIGN KEY(" + joinStrs(fk.Cols, ",") + ") REFERENCES " + fk.RefTable
	if len(fk.RefCols) > 0 {
		s += "(" + joinStrs(fk.RefCols, ",") + ")"
	}
	if fk.OnDelete != "" {
		s += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" {
		s += " ON UPDATE " + fk.OnUpdate
	}
	return s
}

func shapeItems(items []*SelectItem) string {
	out := make([]string, len(items))
	for i, it := range items {
		switch {
		case it.Star && it.StarTable != "":
			out[i] = it.StarTable + ".*"
		case it.Star:
			out[i] = "*"
		default:
			out[i] = String(it.Expr)
			if it.Alias != "" {
				out[i] += " AS " + it.Alias
			}
		}
	}
	return joinStrs(out, ", ")
}

func shapeExprList(es []Expr) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = String(e)
	}
	return joinStrs(out, ", ")
}

func shapeAssignments(as []*Assignment) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Col + " = " + String(a.Value)
	}
	return joinStrs(out, ", ")
}

func shapeRef(r TableRef) string {
	switch n := r.(type) {
	case nil:
		return "<nil>"
	case *BaseTable:
		s := n.Table
		if n.Schema != "" {
			s = n.Schema + "." + n.Table
		}
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		return s
	case *Join:
		op := map[JoinType]string{
			JoinInner: "JOIN", JoinLeft: "LEFT JOIN",
			JoinRight: "RIGHT JOIN", JoinCross: "CROSS JOIN",
		}[n.Type]
		s := shapeRef(n.Left) + " " + op + " " + shapeRef(n.Right)
		if n.On != nil {
			s += " ON " + String(n.On)
		}
		return s
	case *ParenJoin:
		s := "(" + shapeRef(n.Inner) + ")"
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		return s
	case *SubTable:
		s := "(" + shapeSelect(n.Select) + ")"
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		if len(n.Cols) > 0 {
			s += "(" + joinStrs(n.Cols, ",") + ")"
		}
		return s
	case *FuncTable:
		s := n.Name + "(" + shapeExprList(n.Args) + ")"
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		if len(n.Cols) > 0 {
			s += "(" + joinStrs(n.Cols, ",") + ")"
		}
		return s
	}
	return fmt.Sprintf("<%T>", r)
}

func shapeSelect(s *Select) string {
	if s == nil {
		return "<nil>"
	}
	var b strings.Builder
	if len(s.CTEs) > 0 {
		b.WriteString("WITH ")
		for i, c := range s.CTEs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.Name)
			if len(c.Cols) > 0 {
				b.WriteString("(" + joinStrs(c.Cols, ",") + ")")
			}
			b.WriteString(" AS " + shapeSelect(c.Select))
		}
		b.WriteString(" ")
	}
	switch {
	case s.SetOp != "":
		b.WriteString("(" + shapeSelect(s.SetLeft) + " " + strings.ToUpper(s.SetOp))
		if s.SetAll {
			b.WriteString(" ALL")
		}
		b.WriteString(" " + shapeSelect(s.SetRight) + ")")
	case len(s.Values) > 0:
		b.WriteString("VALUES ")
		rows := make([]string, len(s.Values))
		for i, row := range s.Values {
			rows[i] = "(" + shapeExprList(row) + ")"
		}
		b.WriteString(joinStrs(rows, ", "))
	default:
		b.WriteString("SELECT ")
		if s.Distinct {
			b.WriteString("DISTINCT ")
		}
		b.WriteString(shapeItems(s.Items))
		if len(s.From) > 0 {
			refs := make([]string, len(s.From))
			for i, r := range s.From {
				refs[i] = shapeRef(r)
			}
			b.WriteString(" FROM " + joinStrs(refs, ", "))
		}
		if s.Where != nil {
			b.WriteString(" WHERE " + String(s.Where))
		}
		if len(s.GroupBy) > 0 {
			b.WriteString(" GROUP BY " + shapeExprList(s.GroupBy))
		}
		if s.Having != nil {
			b.WriteString(" HAVING " + String(s.Having))
		}
	}
	if len(s.OrderBy) > 0 {
		outs := make([]string, len(s.OrderBy))
		for i, o := range s.OrderBy {
			outs[i] = String(o.Expr)
			if o.Desc {
				outs[i] += " DESC"
			}
			if o.NullsFirst != nil {
				if *o.NullsFirst {
					outs[i] += " NULLS FIRST"
				} else {
					outs[i] += " NULLS LAST"
				}
			}
		}
		b.WriteString(" ORDER BY " + joinStrs(outs, ", "))
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT " + String(s.Limit))
	}
	if s.Offset != nil {
		b.WriteString(" OFFSET " + String(s.Offset))
	}
	return b.String()
}

func shapeOnConflict(oc *OnConflict) string {
	if len(oc.Target) > 0 {
		s := " ON CONFLICT(" + joinStrs(oc.Target, ",") + ")"
		if oc.Constraint != "" {
			s += " ON CONSTRAINT " + oc.Constraint
		}
		return s + shapeConflictAction(oc)
	}
	if oc.Constraint != "" {
		return " ON CONFLICT ON CONSTRAINT " + oc.Constraint + shapeConflictAction(oc)
	}
	return " ON CONFLICT" + shapeConflictAction(oc)
}

func shapeConflictAction(oc *OnConflict) string {
	if oc.DoNothing {
		return " DO NOTHING"
	}
	s := " DO UPDATE SET " + shapeAssignments(oc.Updates)
	if oc.UpdateWhere != nil {
		s += " WHERE " + String(oc.UpdateWhere)
	}
	return s
}

func shapeStmt(st Statement) string {
	switch n := st.(type) {
	case *CreateTable:
		var b strings.Builder
		b.WriteString("CREATE TABLE ")
		if n.IfNotExists {
			b.WriteString("IF NOT EXISTS ")
		}
		b.WriteString(n.Table)
		if n.As != nil {
			b.WriteString(" AS " + shapeSelect(n.As))
			return b.String()
		}
		var parts []string
		for _, c := range n.Cols {
			parts = append(parts, shapeCol(c))
		}
		for _, fk := range n.FKs {
			parts = append(parts, shapeFK(fk))
		}
		for _, ck := range n.Checks {
			parts = append(parts, "CHECK("+ck+")")
		}
		b.WriteString("(" + joinStrs(parts, ", ") + ")")
		return b.String()

	case *DropTable:
		s := "DROP TABLE "
		if n.IfExists {
			s += "IF EXISTS "
		}
		return s + joinStrs(n.Tables, ", ")

	case *CreateIndex:
		s := "CREATE "
		if n.Unique {
			s += "UNIQUE "
		}
		s += "INDEX "
		if n.IfNotExists {
			s += "IF NOT EXISTS "
		}
		return s + n.Name + " ON " + n.Table + "(" + joinStrs(n.Cols, ", ") + ")"

	case *DropIndex:
		s := "DROP INDEX "
		if n.IfExists {
			s += "IF EXISTS "
		}
		return s + n.Name

	case *AlterTable:
		a := n.Action
		switch a.Kind {
		case "add_column":
			return "ALTER TABLE " + n.Table + " ADD COLUMN " + shapeCol(a.Col)
		case "drop_column":
			return "ALTER TABLE " + n.Table + " DROP COLUMN " + a.Name
		case "rename_column":
			return "ALTER TABLE " + n.Table + " RENAME COLUMN " + a.Name + " TO " + a.NewName
		case "rename_table":
			return "ALTER TABLE " + n.Table + " RENAME TO " + a.NewName
		case "alter_type":
			typ := a.TypeName
			if len(a.TypeArgs) > 0 {
				typ += "(" + shapeInts(a.TypeArgs) + ")"
			}
			return "ALTER TABLE " + n.Table + " ALTER COLUMN " + a.Name + " TYPE " + typ
		case "add_fk":
			ref := a.FK.RefTable
			if len(a.FK.RefCols) > 0 {
				ref += "(" + joinStrs(a.FK.RefCols, ", ") + ")"
			}
			s := "ALTER TABLE " + n.Table + " ADD "
			if a.Name != "" {
				s += "CONSTRAINT " + a.Name + " "
			}
			s += "FOREIGN KEY (" + joinStrs(a.FK.Cols, ", ") + ") REFERENCES " + ref
			if a.FK.OnDelete != "" {
				s += " ON DELETE " + a.FK.OnDelete
			}
			if a.FK.OnUpdate != "" {
				s += " ON UPDATE " + a.FK.OnUpdate
			}
			return s
		case "drop_constraint":
			return "ALTER TABLE " + n.Table + " DROP CONSTRAINT " + a.Name
		}
		return "ALTER TABLE " + n.Table + " <" + a.Kind + ">"

	case *Truncate:
		return "TRUNCATE " + n.Table

	case *Insert:
		var b strings.Builder
		b.WriteString("INSERT INTO " + n.Table)
		if len(n.Columns) > 0 {
			b.WriteString("(" + joinStrs(n.Columns, ", ") + ")")
		}
		if n.Select != nil {
			b.WriteString(" " + shapeSelect(n.Select))
		} else {
			rows := make([]string, len(n.Rows))
			for i, row := range n.Rows {
				rows[i] = "(" + shapeExprList(row) + ")"
			}
			b.WriteString(" VALUES " + joinStrs(rows, ", "))
		}
		if n.OnConflict != nil {
			b.WriteString(shapeOnConflict(n.OnConflict))
		}
		if len(n.Returning) > 0 {
			b.WriteString(" RETURNING " + shapeItems(n.Returning))
		}
		return b.String()

	case *Update:
		s := "UPDATE " + n.Table
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		s += " SET " + shapeAssignments(n.Sets)
		if len(n.From) > 0 {
			refs := make([]string, len(n.From))
			for i, r := range n.From {
				refs[i] = shapeRef(r)
			}
			s += " FROM " + joinStrs(refs, ", ")
		}
		if n.Where != nil {
			s += " WHERE " + String(n.Where)
		}
		if len(n.Returning) > 0 {
			s += " RETURNING " + shapeItems(n.Returning)
		}
		return s

	case *Delete:
		s := "DELETE FROM " + n.Table
		if n.Alias != "" {
			s += " AS " + n.Alias
		}
		if len(n.Using) > 0 {
			refs := make([]string, len(n.Using))
			for i, r := range n.Using {
				refs[i] = shapeRef(r)
			}
			s += " USING " + joinStrs(refs, ", ")
		}
		if n.Where != nil {
			s += " WHERE " + String(n.Where)
		}
		if len(n.Returning) > 0 {
			s += " RETURNING " + shapeItems(n.Returning)
		}
		return s

	case *Select:
		return shapeSelect(n)

	case *TxStmt:
		s := strings.ToUpper(n.Kind)
		if n.Isolation != "" {
			s += " " + n.Isolation
		}
		if n.ReadOnly {
			s += " READ ONLY"
		}
		return s

	case *SavepointStmt:
		switch n.Kind {
		case "rollback_to":
			return "ROLLBACK TO " + n.Name
		case "release":
			return "RELEASE " + n.Name
		}
		return "SAVEPOINT " + n.Name

	case *Explain:
		if n.Analyze {
			return "EXPLAIN ANALYZE " + shapeStmt(n.Stmt)
		}
		return "EXPLAIN " + shapeStmt(n.Stmt)

	case *Show:
		return "SHOW " + n.Name

	case *SetStmt:
		return n.Raw

	case *SessionCleanup:
		return "SESSION " + n.Tag

	case *CommentOn:
		s := "COMMENT ON " + n.Object + " "
		switch n.Object {
		case "column":
			s += n.Table + "." + n.Column
		case "constraint":
			s += n.Name + " ON " + n.Table
		case "table":
			s += n.Table
		default:
			s += n.Name
		}
		s += " IS "
		if n.IsNull {
			return s + "NULL"
		}
		return s + "'" + n.Comment + "'"

	case *Copy:
		s := "COPY " + n.Table
		if len(n.Cols) > 0 {
			s += "(" + joinStrs(n.Cols, ", ") + ")"
		}
		if n.To {
			return s + " TO STDOUT"
		}
		return s + " FROM STDIN"

	case *Vacuum:
		s := "VACUUM"
		if n.Full {
			s += " FULL"
		}
		if n.Analyze {
			s += " ANALYZE"
		}
		if n.Table != "" {
			s += " " + n.Table
		}
		return s

	case *Analyze:
		return "ANALYZE"

	case *Checkpoint:
		return "CHECKPOINT"

	case *CreateDatabase:
		s := "CREATE DATABASE " + n.Name
		if len(n.Options) > 0 {
			keys := make([]string, 0, len(n.Options))
			for k := range n.Options {
				keys = append(keys, k)
			}
			// map 遍历无序，排序保证形状稳定
			sortStrings(keys)
			opts := make([]string, len(keys))
			for i, k := range keys {
				opts[i] = k + "=" + n.Options[k]
			}
			s += " WITH " + joinStrs(opts, " ")
		}
		return s

	case *DropDatabase:
		s := "DROP DATABASE "
		if n.IfExists {
			s += "IF EXISTS "
		}
		return s + n.Name
	}
	return fmt.Sprintf("<%T>", st)
}

// sortStrings 是一个 3 行插入排序：只在此处给 map 的 key 排序用，
// 不想为了它去 import "sort" 让整个测试文件多一行依赖。
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// ---------- 语句用例 ----------

func TestParseStatements(t *testing.T) {
	cases := []struct {
		sql  string
		want string
	}{
		// ---- DDL：建表 ----
		// 注意：解析层**不归一化类型名**（`int` 仍是 int，`varchar(32)` 仍是 varchar），
		// 归一化（int→integer、varchar→text …）是 types.ParseType 的职责。
		// 这里把「解析层只做语法」这条契约固定下来。
		{`CREATE TABLE t (id int, name text)`,
			`CREATE TABLE t(id int, name text)`},
		{`CREATE TABLE IF NOT EXISTS s.t (id int)`,
			`CREATE TABLE IF NOT EXISTS t(id int)`},
		{`create table t (a varchar(32), b numeric(10,2), c timestamp(3))`,
			`CREATE TABLE t(a varchar(32), b numeric(10,2), c timestamp(3))`},
		{`CREATE TABLE t (id serial PRIMARY KEY, n text NOT NULL, u text UNIQUE)`,
			`CREATE TABLE t(id integer NOT NULL PRIMARY KEY SERIAL, n text NOT NULL, u text UNIQUE)`},
		// IDENTITY 在引擎里按 SERIAL 执行，所以两个标记都会被置上（另加 NOT NULL）
		{`CREATE TABLE t (id int GENERATED ALWAYS AS IDENTITY)`,
			`CREATE TABLE t(id int NOT NULL SERIAL IDENTITY)`},
		{`CREATE TABLE t (flag bool DEFAULT true, ts timestamp DEFAULT now())`,
			`CREATE TABLE t(flag bool DEFAULT TRUE, ts timestamp DEFAULT now())`},
		{`CREATE TABLE t (n int CHECK (n > 0))`,
			`CREATE TABLE t(n int CHECK((n > 0)))`},
		{`CREATE TABLE t (uid int REFERENCES users(id) ON DELETE CASCADE)`,
			`CREATE TABLE t(uid int REFERENCES users(id) ON DELETE cascade)`},
		{`CREATE TABLE t (a int, b int, PRIMARY KEY (a, b))`,
			`CREATE TABLE t(a int NOT NULL PRIMARY KEY, b int NOT NULL PRIMARY KEY)`},
		{`CREATE TABLE t (a int, b int, CONSTRAINT fk FOREIGN KEY (a, b) REFERENCES p(x, y) ON DELETE SET NULL)`,
			`CREATE TABLE t(a int, b int, FOREIGN KEY(a,b) REFERENCES p(x,y) ON DELETE set null)`},
		{`CREATE TABLE t (a int, CHECK (a > 0), CHECK (a < 100))`,
			`CREATE TABLE t(a int, CHECK((a > 0)), CHECK((a < 100)))`},
		{`CREATE TABLE t (a int) WITH (oids = false)`,
			`CREATE TABLE t(a int)`},
		{`CREATE TABLE "Foo" ("Id" int)`,
			`CREATE TABLE foo(id int)`},
		{`CREATE TABLE t AS SELECT a, b FROM u`,
			`CREATE TABLE t AS SELECT a, b FROM u`},

		// ---- DDL：索引 / 删表 / 改表 ----
		{`CREATE INDEX ix ON t (a, b)`, `CREATE INDEX ix ON t(a, b)`},
		{`CREATE UNIQUE INDEX IF NOT EXISTS ix ON t USING btree (a)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS ix ON t(a)`},
		{`DROP INDEX IF EXISTS ix`, `DROP INDEX IF EXISTS ix`},
		{`DROP TABLE IF EXISTS a, b CASCADE`, `DROP TABLE IF EXISTS a, b`},
		{`ALTER TABLE t ADD COLUMN c text NOT NULL DEFAULT 'x'`,
			`ALTER TABLE t ADD COLUMN c text NOT NULL DEFAULT 'x'`},
		{`ALTER TABLE t DROP COLUMN IF EXISTS c`, `ALTER TABLE t DROP COLUMN c`},
		{`ALTER TABLE t RENAME COLUMN a TO b`, `ALTER TABLE t RENAME COLUMN a TO b`},
		{`ALTER TABLE t RENAME TO t2`, `ALTER TABLE t RENAME TO t2`},
		{`ALTER TABLE t ALTER COLUMN c SET DATA TYPE varchar(64)`,
			`ALTER TABLE t ALTER COLUMN c TYPE varchar(64)`},
		// 给已有表补外键（表设计器的「引用」列走这条）：带名与不带名都要能解析
		{`ALTER TABLE c ADD CONSTRAINT fk_c_p FOREIGN KEY (p_id) REFERENCES p(id)`,
			`ALTER TABLE c ADD CONSTRAINT fk_c_p FOREIGN KEY (p_id) REFERENCES p(id)`},
		{`ALTER TABLE c ADD FOREIGN KEY (p_id) REFERENCES p (id) ON DELETE CASCADE`,
			`ALTER TABLE c ADD FOREIGN KEY (p_id) REFERENCES p(id) ON DELETE cascade`},
		{`ALTER TABLE c ADD CONSTRAINT fk2 FOREIGN KEY (a, b) REFERENCES p (x, y)`,
			`ALTER TABLE c ADD CONSTRAINT fk2 FOREIGN KEY (a, b) REFERENCES p(x, y)`},
		{`ALTER TABLE c DROP CONSTRAINT IF EXISTS fk_c_p`,
			`ALTER TABLE c DROP CONSTRAINT fk_c_p`},
		{`TRUNCATE TABLE t`, `TRUNCATE t`},
		{`COMMENT ON TABLE t IS '用户表'`, `COMMENT ON table t IS '用户表'`},
		{`COMMENT ON COLUMN t.c IS NULL`, `COMMENT ON column t.c IS NULL`},
		{`CREATE DATABASE app WITH ENCODING 'UTF8' OWNER godb`,
			`CREATE DATABASE app WITH encoding=UTF8 owner=godb`},
		{`DROP DATABASE IF EXISTS app`, `DROP DATABASE IF EXISTS app`},

		// ---- DML ----
		{`INSERT INTO t (a, b) VALUES (1, 'x')`, `INSERT INTO t(a, b) VALUES (1, 'x')`},
		{`INSERT INTO t VALUES (1, 2), (3, 4)`, `INSERT INTO t VALUES (1, 2), (3, 4)`},
		{`INSERT INTO t (a) SELECT b FROM u WHERE b > 0`,
			`INSERT INTO t(a) SELECT b FROM u WHERE (b > 0)`},
		{`INSERT INTO t (a) VALUES (1) ON CONFLICT DO NOTHING`,
			`INSERT INTO t(a) VALUES (1) ON CONFLICT DO NOTHING`},
		{`INSERT INTO t (a, b) VALUES (1, 2) ON CONFLICT (a) DO UPDATE SET b = excluded.b WHERE t.b IS NULL`,
			`INSERT INTO t(a, b) VALUES (1, 2) ON CONFLICT(a) DO UPDATE SET b = excluded.b WHERE t.b IS NULL`},
		{`INSERT INTO t (a) VALUES (1) ON CONFLICT ON CONSTRAINT t_pkey DO NOTHING`,
			`INSERT INTO t(a) VALUES (1) ON CONFLICT ON CONSTRAINT t_pkey DO NOTHING`},
		{`INSERT INTO t (a) VALUES (DEFAULT) RETURNING *`,
			`INSERT INTO t(a) VALUES (DEFAULT) RETURNING *`},
		{`UPDATE t SET a = 1, b = b + 1 WHERE id = 2 RETURNING a`,
			`UPDATE t SET a = 1, b = (b + 1) WHERE (id = 2) RETURNING a`},
		{`UPDATE t AS x SET a = u.a FROM u WHERE u.id = x.id`,
			`UPDATE t AS x SET a = u.a FROM u WHERE (u.id = x.id)`},
		{`DELETE FROM t WHERE id = 3`, `DELETE FROM t WHERE (id = 3)`},
		{`DELETE FROM t USING u WHERE u.id = t.id RETURNING t.id`,
			`DELETE FROM t USING u WHERE (u.id = t.id) RETURNING t.id`},
		{`COPY t (a, b) FROM STDIN`, `COPY t(a, b) FROM STDIN`},
		{`COPY t TO STDOUT`, `COPY t TO STDOUT`},

		// ---- 查询：选择列表与子句 ----
		{`SELECT 1`, `SELECT 1`},
		{`SELECT * FROM t`, `SELECT * FROM t`},
		{`SELECT t.*, u.a FROM t, u`, `SELECT t.*, u.a FROM t CROSS JOIN u`},
		{`SELECT DISTINCT a AS x, b FROM t`, `SELECT DISTINCT a AS x, b FROM t`},
		{`SELECT a FROM t WHERE a > 1 AND b < 2`,
			`SELECT a FROM t WHERE ((a > 1) AND (b < 2))`},
		{`SELECT g, count(*) AS n FROM t GROUP BY g HAVING count(*) > 1 ORDER BY n DESC, g NULLS FIRST LIMIT 10 OFFSET 5`,
			`SELECT g, count(*) AS n FROM t GROUP BY g HAVING (count(*) > 1) ORDER BY n DESC, g NULLS FIRST LIMIT 10 OFFSET 5`},
		{`SELECT a FROM t LIMIT ALL`, `SELECT a FROM t`},

		// ---- 查询：FROM 里各种来源 ----
		{`SELECT * FROM a JOIN b ON a.id = b.id`,
			`SELECT * FROM a JOIN b ON (a.id = b.id)`},
		{`SELECT * FROM a LEFT OUTER JOIN b ON a.id = b.id`,
			`SELECT * FROM a LEFT JOIN b ON (a.id = b.id)`},
		{`SELECT * FROM a RIGHT JOIN b ON a.id = b.id`,
			`SELECT * FROM a RIGHT JOIN b ON (a.id = b.id)`},
		{`SELECT * FROM a CROSS JOIN b`, `SELECT * FROM a CROSS JOIN b`},
		{`SELECT * FROM (SELECT 1 AS x) s`, `SELECT * FROM (SELECT 1 AS x) AS s`},
		{`SELECT * FROM (VALUES (1, 'a'), (2, 'b')) v (id, name)`,
			`SELECT * FROM (VALUES (1, 'a'), (2, 'b')) AS v(id,name)`},
		{`SELECT * FROM generate_series(1, 5) AS g(n)`,
			`SELECT * FROM generate_series(1, 5) AS g(n)`},
		{`SELECT * FROM (a JOIN b ON a.id = b.id) x`,
			`SELECT * FROM (a JOIN b ON (a.id = b.id)) AS x`},
		{`SELECT * FROM pg_catalog.pg_class`, `SELECT * FROM pg_catalog.pg_class`},

		// ---- 查询：CTE 与集合运算 ----
		{`WITH c AS (SELECT 1 AS x) SELECT * FROM c`,
			`WITH c AS SELECT 1 AS x SELECT * FROM c`},
		{`WITH c (v) AS (SELECT 1), d AS (SELECT 2) SELECT * FROM c, d`,
			`WITH c(v) AS SELECT 1, d AS SELECT 2 SELECT * FROM c CROSS JOIN d`},
		{`SELECT 1 UNION SELECT 2`, `(SELECT 1 UNION SELECT 2)`},
		{`SELECT 1 UNION ALL SELECT 2 ORDER BY 1 LIMIT 3`,
			`(SELECT 1 UNION ALL SELECT 2) ORDER BY 1 LIMIT 3`},
		{`SELECT 1 INTERSECT SELECT 2`, `(SELECT 1 INTERSECT SELECT 2)`},
		{`SELECT 1 EXCEPT SELECT 2`, `(SELECT 1 EXCEPT SELECT 2)`},
		{`VALUES (1, 2), (3, 4)`, `VALUES (1, 2), (3, 4)`},

		// ---- 表达式：子查询与谓词（放在语句里验证嵌套） ----
		{`SELECT (SELECT count(*) FROM u) AS n`, `SELECT (SELECT ...) AS n`},
		{`SELECT a FROM t WHERE a IN (1, 2, 3)`, `SELECT a FROM t WHERE a IN (1, 2, 3)`},
		{`SELECT a FROM t WHERE a NOT IN (SELECT b FROM u)`,
			`SELECT a FROM t WHERE a NOT IN (SELECT ...)`},
		{`SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u)`,
			`SELECT a FROM t WHERE EXISTS (SELECT ...)`},
		{`SELECT a FROM t WHERE a IS NOT NULL`,
			`SELECT a FROM t WHERE a IS NOT NULL`},
		{`SELECT CAST(a AS bigint), b::text FROM t`,
			`SELECT CAST(a AS BIGINT), CAST(b AS TEXT) FROM t`},
		{`SELECT CASE WHEN a > 0 THEN 'p' ELSE 'n' END FROM t`,
			`SELECT CASE WHEN (a > 0) THEN 'p' ELSE 'n' END FROM t`},
		{`SELECT CASE a WHEN 1 THEN 'one' END FROM t`,
			`SELECT CASE a WHEN 1 THEN 'one' END FROM t`},
		{`SELECT * FROM t WHERE relkind = ANY (ARRAY['r', 'p'])`,
			`SELECT * FROM t WHERE relkind = ANY (array('r', 'p'))`},

		// ---- 兼容语句：客户端连上来会发的一堆 ----
		{`BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY`,
			`BEGIN serializable READ ONLY`},
		{`START TRANSACTION`, `BEGIN`},
		{`COMMIT`, `COMMIT`},
		{`ROLLBACK`, `ROLLBACK`},
		{`SAVEPOINT s1`, `SAVEPOINT s1`},
		{`ROLLBACK TO SAVEPOINT s1`, `ROLLBACK TO s1`},
		{`RELEASE SAVEPOINT s1`, `RELEASE s1`},
		{`EXPLAIN SELECT 1`, `EXPLAIN SELECT 1`},
		{`EXPLAIN ANALYZE SELECT 1 FROM t`, `EXPLAIN ANALYZE SELECT 1 FROM t`},
		{`EXPLAIN VERBOSE SELECT 1`, `EXPLAIN SELECT 1`},
		{`SHOW time zone`, `SHOW time zone`},
		{`SET statement_timeout = 0`, `SET statement_timeout = 0`},
		{`SET client_encoding TO UTF8`, `SET client_encoding TO UTF8`},
		{`DISCARD ALL`, `SESSION DISCARD ALL`},
		{`RESET ALL`, `SESSION RESET`},
		{`DEALLOCATE ALL`, `SESSION DEALLOCATE`},
		{`CHECKPOINT`, `CHECKPOINT`},
		{`FLUSH`, `CHECKPOINT`},
		{`VACUUM FULL ANALYZE t`, `VACUUM FULL ANALYZE t`},
		{`VACUUM`, `VACUUM`},
		{`ANALYZE t`, `ANALYZE`},
	}

	for _, c := range cases {
		sts, err := Parse(c.sql)
		if err != nil {
			t.Errorf("%s\n  解析失败: %v", c.sql, err)
			continue
		}
		if len(sts) != 1 {
			t.Errorf("%s\n  得到 %d 条语句，期望 1 条", c.sql, len(sts))
			continue
		}
		if got := shapeStmt(sts[0]); got != c.want {
			t.Errorf("%s\n  得到: %s\n  期望: %s", c.sql, got, c.want)
		}
	}
}

// 多语句与分号：空语句要跳过，末尾多余分号不算错。
func TestParseMultiStatement(t *testing.T) {
	sts, err := Parse("SELECT 1; SELECT 2;;  ;")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(sts) != 2 {
		t.Fatalf("得到 %d 条语句，期望 2 条（空语句要跳过）", len(sts))
	}
	if got := shapeStmt(sts[1]); got != "SELECT 2" {
		t.Fatalf("第二条 = %q，期望 SELECT 2", got)
	}
}

// 双引号标识符：godb 统一折叠成小写（见 parser.ident 的注释），
// 这里把它固定成契约——将来若要支持大小写敏感标识符，改这里会立刻红。
func TestParseQuotedIdentFoldsToLower(t *testing.T) {
	sts, err := Parse(`SELECT "Id" FROM "T"`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := shapeStmt(sts[0]); got != "SELECT id FROM t" {
		t.Fatalf("得到 %q，期望 SELECT id FROM t", got)
	}
}

// ---------- 表达式用例 ----------

func TestParseExpressions(t *testing.T) {
	cases := []struct{ src, want string }{
		{`1`, `1`},
		{`1.5`, `1.5`},
		{`'it''s'`, `'it''s'`},
		{`NULL`, `NULL`},
		{`true`, `TRUE`},
		{`a`, `a`},
		{`t.a`, `t.a`},
		{`1 + 2 * 3`, `(1 + (2 * 3))`},
		{`(1 + 2) * 3`, `((1 + 2) * 3)`},
		{`10 / 4 % 3`, `((10 / 4) % 3)`},
		{`-1`, `-1`},
		{`a = 1 AND b = 2 OR c = 3`, `(((a = 1) AND (b = 2)) OR (c = 3))`},
		{`a = 1 AND (b = 2 OR c = 3)`, `((a = 1) AND ((b = 2) OR (c = 3)))`},
		{`NOT a = 1`, `NOT (a = 1)`},
		{`a != 1`, `(a <> 1)`},
		{`a || b`, `(a || b)`},
		{`count(*)`, `count(*)`},
		{`count(DISTINCT a)`, `count(DISTINCT a)`},
		{`sum(a + b)`, `sum((a + b))`},
		{`a IS NULL`, `a IS NULL`},
		{`a IS NOT NULL`, `a IS NOT NULL`},
		{`a BETWEEN 1 AND 2`, `a BETWEEN 1 AND 2`},
		{`a NOT BETWEEN 1 AND 2`, `a NOT BETWEEN 1 AND 2`},
		{`a LIKE 'x%'`, `a LIKE 'x%'`},
		{`a NOT ILIKE 'x%'`, `a NOT ILIKE 'x%'`},
		{`a IN (1, 2)`, `a IN (1, 2)`},
		{`b::int4`, `CAST(b AS INT4)`},
		{`CAST(b AS varchar(10))`, `CAST(b AS VARCHAR(10))`},
		{`rm.m[1]`, `rm.m[1]`},
		// ARRAY[...] 被解析成名为 array 的函数调用（godb 没有真数组类型，按文本处理）
		{`a = ANY (ARRAY['r', 'p'])`, `a = ANY (array('r', 'p'))`},
		{`DATE '2024-03-05'`, `CAST('2024-03-05' AS DATE)`},
		{`TIMESTAMP '2024-03-05 01:02:03'`, `CAST('2024-03-05 01:02:03' AS TIMESTAMP)`},
		{`current_date`, `current_date()`},
		{`coalesce(a, 0)`, `coalesce(a, 0)`},
	}

	for _, c := range cases {
		e, err := ParseExpr(c.src)
		if err != nil {
			t.Errorf("%s\n  解析失败: %v", c.src, err)
			continue
		}
		if got := String(e); got != c.want {
			t.Errorf("%s\n  得到: %s\n  期望: %s", c.src, got, c.want)
		}
	}
}

// BETWEEN 里的 AND 不能把外面的 AND 吃进去：`a BETWEEN 1 AND 5 AND b` 必须解析成
// And(Between(a,1,5), b)，而不是 Between(a, 1, And(5, b))。
//
// 这条只能查结构，不能比 String()：BETWEEN 打印时不加括号，所以打印结果
// `a BETWEEN 1 AND 5 AND b` 看起来是歧义的。String() 只用于 EXPLAIN 与错误提示，
// 不是可往返的 SQL 序列化（见 print.go 开头）。
func TestBetweenBindsLooserThanAnd(t *testing.T) {
	e, err := ParseExpr(`a BETWEEN 1 AND 5 AND b = 2`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	and, ok := e.(*Binary)
	if !ok || and.Op != "and" {
		t.Fatalf("顶层应是 AND，实际 %T（%s）", e, String(e))
	}
	btw, ok := and.L.(*Between)
	if !ok {
		t.Fatalf("AND 左边应是 BETWEEN，实际 %T（%s）", and.L, String(and.L))
	}
	if String(btw.Hi) != "5" {
		t.Fatalf("BETWEEN 上界应是 5，实际 %s", String(btw.Hi))
	}
	if _, ok := and.R.(*Binary); !ok {
		t.Fatalf("AND 右边应是比较，实际 %T（%s）", and.R, String(and.R))
	}
}

// ---------- 错误用例 ----------

// 错误信息里带不带行号列号，决定前端能不能把光标挪到出错处。
func TestParseErrorHasPosition(t *testing.T) {
	_, err := Parse("SELECT 1\nFROM WHERE")
	if err == nil {
		t.Fatal("应报语法错误")
	}
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *sql.Error", err)
	}
	if pe.Line != 2 || pe.Col <= 0 {
		t.Fatalf("行列 = %d:%d，期望第 2 行且列号 > 0（前端靠它定位光标）", pe.Line, pe.Col)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		sql string
		sub string // 期望错误信息里出现这段
	}{
		{``, `empty query`},
		{`   ;  ;`, `empty query`},
		{`SELECT 'abc`, `unterminated`}, // 字符串没闭合
		{`SELEC 1`, `syntax error`},     // 拼错关键字
		{`SELECT * FROM a FULL JOIN b ON a.id = b.id`, `FULL JOIN`},
		{`SELECT * FROM a NATURAL JOIN b`, `NATURAL JOIN`},
		{`WITH RECURSIVE c AS (SELECT 1) SELECT * FROM c`, `RECURSIVE`},
		{`CREATE INDEX ON t (a)`, `requires a name`},
		{`CREATE TABLE t (a int PRIMARY KEY, b int PRIMARY KEY)`, `multiple primary keys`},
		{`CREATE TABLE t (a int GENERATED ALWAYS AS (1) STORED)`, `generated columns`},
		{`ALTER TABLE t ALTER COLUMN a TYPE serial`, `serial`},
		// 复合唯一约束目前不支持：单列才行。这条同时也把「已知限制」写进测试，
		// 将来实现复合唯一时这里会红，提醒一起改文档。
		{`CREATE TABLE t (a int, b int, UNIQUE (a, b))`, `composite unique`},
		{`SELECT * FROM a CROSS JOIN b ON a.id = b.id`, `CROSS JOIN cannot have an ON clause`},
		{`CREATE VIEW v AS SELECT 1`, `only CREATE TABLE is supported`},
		{`DROP VIEW v`, `only DROP TABLE is supported`},
		// 给已有表补约束：只有外键支持，其它要明说「不支持」，
		// 不能是「syntax error at or near CONSTRAINT」
		{`ALTER TABLE t ADD CONSTRAINT x UNIQUE (a)`, `ADD UNIQUE is not supported`},
		{`ALTER TABLE t ADD PRIMARY KEY (a)`, `ADD PRIMARY KEY is not supported`},
		{`SELECT 1 SELECT 2`, `syntax error`}, // 少分号
		{`SELECT (1`, `syntax error`},         // 括号没闭合
	}

	for _, c := range cases {
		_, err := Parse(c.sql)
		if err == nil {
			t.Errorf("%q\n  期望报错（含 %q），实际解析成功", c.sql, c.sub)
			continue
		}
		if !strings.Contains(err.Error(), c.sub) {
			t.Errorf("%q\n  错误 = %q\n  期望包含 %q", c.sql, err.Error(), c.sub)
		}
	}
}
