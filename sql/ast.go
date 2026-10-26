package sql

import "godb/types"

// Statement 是所有语句的接口。
type Statement interface{ stmtNode() }

// Expr 是所有表达式的接口。
type Expr interface{ exprNode() }

// ---------- DDL ----------

// CreateTable 对应 CREATE TABLE。
type CreateTable struct {
	Table       string
	IfNotExists bool
	Cols        []*ColumnDef
}

// ColumnDef 是一列的定义。
type ColumnDef struct {
	Name       string
	TypeName   string
	TypeArgs   []int
	NotNull    bool
	PrimaryKey bool
	Unique     bool
	Default    Expr
	Serial     bool
}

// DropTable 对应 DROP TABLE。
type DropTable struct {
	Tables   []string
	IfExists bool
}

// AlterTable 对应 ALTER TABLE。
type AlterTable struct {
	Table  string
	Action *AlterAction
}

// AlterAction 是 ALTER TABLE 的具体动作。
type AlterAction struct {
	Kind     string // add_column | drop_column | rename_column | rename_table | alter_type
	Col      *ColumnDef
	Name     string // drop/rename_column: 旧列名
	NewName  string // rename_column: 新列名；rename_table: 新表名
	TypeName string
	TypeArgs []int
}

// Truncate 对应 TRUNCATE TABLE。
type Truncate struct{ Table string }

// ---------- DML ----------

// Insert 对应 INSERT INTO ... VALUES。
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}

// Assignment 是 UPDATE 的一个 SET 项。
type Assignment struct {
	Col   string
	Value Expr
}

// Update 对应 UPDATE。
type Update struct {
	Table string
	Alias string
	Sets  []*Assignment
	Where Expr
}

// Delete 对应 DELETE。
type Delete struct {
	Table string
	Alias string
	Where Expr
}

// ---------- DQL ----------

// Select 对应 SELECT。
type Select struct {
	Distinct bool
	Items    []*SelectItem
	From     []TableRef
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []*OrderItem
	Limit    Expr
	Offset   Expr
}

// SelectItem 是选择列表中的一项（可能是 * 或 table.*）。
type SelectItem struct {
	Expr      Expr
	Alias     string
	Star      bool
	StarTable string
}

// OrderItem 是 ORDER BY 的一项。
type OrderItem struct {
	Expr       Expr
	Desc       bool
	NullsFirst *bool // nil 表示按 PG 默认（ASC: NULLS LAST，DESC: NULLS FIRST）
}

// TableRef 是 FROM 子句中的表引用。
type TableRef interface{ tableRef() }

// BaseTable 是普通表引用。
type BaseTable struct {
	Table string
	Alias string
}

// JoinType 是连接类型。
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeft
	JoinRight
	JoinCross
)

// Join 是一个连接节点。
type Join struct {
	Left  TableRef
	Right TableRef
	Type  JoinType
	On    Expr
}

func (*BaseTable) tableRef() {}
func (*Join) tableRef()      {}

// ---------- 事务 ----------

// TxStmt 对应 BEGIN / COMMIT / ROLLBACK。
type TxStmt struct{ Kind string }

// Explain 对应 EXPLAIN。
type Explain struct{ Stmt Statement }

func (*CreateTable) stmtNode() {}
func (*DropTable) stmtNode()   {}
func (*AlterTable) stmtNode()  {}
func (*Truncate) stmtNode()    {}
func (*Insert) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*Select) stmtNode()      {}
func (*TxStmt) stmtNode()      {}
func (*Explain) stmtNode()     {}

// ---------- 表达式 ----------

// Literal 是常量。
type Literal struct{ Val types.Value }

// ColumnRef 是列引用（Table 为空表示未限定）。
type ColumnRef struct{ Table, Name string }

// Binary 是二元运算：算术、比较、and/or、||。
type Binary struct {
	Op   string
	L, R Expr
}

// Unary 是一元运算：- + not。
type Unary struct {
	Op string
	X  Expr
}

// FuncCall 是函数调用。Star 为 true 表示 count(*)。
type FuncCall struct {
	Name     string
	Args     []Expr
	Star     bool
	Distinct bool
}

// Case 是 CASE 表达式。
type Case struct {
	Operand Expr
	Whens   []*When
	Else    Expr
}

// When 是 CASE 的一个分支。
type When struct{ Cond, Result Expr }

// Cast 是 CAST(x AS type) 或 x::type。
type Cast struct {
	X        Expr
	TypeName string
	TypeArgs []int
}

// IsNull 是 IS [NOT] NULL。
type IsNull struct {
	X   Expr
	Not bool
}

// In 是 [NOT] IN (list)。
type In struct {
	X    Expr
	List []Expr
	Not  bool
}

// Between 是 [NOT] BETWEEN。
type Between struct {
	X, Lo, Hi Expr
	Not       bool
}

// Like 是 [NOT] LIKE / ILIKE。
type Like struct {
	X, Pattern Expr
	Not        bool
	IgnoreCase bool
}

// DefaultVal 是 INSERT 里的 DEFAULT 关键字。
type DefaultVal struct{}

func (*Literal) exprNode()    {}
func (*ColumnRef) exprNode()  {}
func (*Binary) exprNode()     {}
func (*Unary) exprNode()      {}
func (*FuncCall) exprNode()   {}
func (*Case) exprNode()       {}
func (*Cast) exprNode()       {}
func (*IsNull) exprNode()     {}
func (*In) exprNode()         {}
func (*Between) exprNode()    {}
func (*Like) exprNode()       {}
func (*DefaultVal) exprNode() {}
