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

	// Checks 是表级 CHECK 约束的表达式原文；列级 CHECK 在引擎里并进来。
	Checks []string

	// FKs 是外键定义（列级 REFERENCES 与表级 FOREIGN KEY 都收在这里）。
	FKs []*FKDef

	// As 非空表示 CREATE TABLE ... AS SELECT：列名与类型都来自这条查询。
	As *Select
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
	// Identity 表示该列是 GENERATED ... AS IDENTITY（SQL 标准的写法）。
	// 自增行为和 Serial 一样，区别只在语义：IDENTITY 只保证 NOT NULL，
	// 不像老式 SERIAL 那样顺手加个唯一约束。
	Identity bool

	// Check 是列级 CHECK 约束的表达式原文（只在建表解析阶段用，
	// 之后统一挪到 CreateTable.Checks / catalog.Table.Checks）。
	Check string

	// 列级 REFERENCES t(c)：解析后挪到 CreateTable.FKs。
	RefTable string
	RefCol   string
	RefOnDel string
}

// FKDef 是一条外键定义。
type FKDef struct {
	Cols     []string
	RefTable string
	RefCols  []string
	OnDelete string // "" / cascade / restrict / set null / no action
	OnUpdate string
}

// CreateIndex 对应 CREATE [UNIQUE] INDEX name ON table (cols)。
type CreateIndex struct {
	Name   string
	Table  string
	Cols   []string
	Unique bool
}

// DropIndex 对应 DROP INDEX [IF EXISTS] name。
type DropIndex struct {
	Name     string
	IfExists bool
}

// CommentOn 对应 COMMENT ON <对象> ... IS '文本' | NULL。
//
// 只有 TABLE / COLUMN 的注释会真的存下来（catalog 里带注释字段，
// pg_description 和 obj_description/col_description 能查到）；INDEX /
// SCHEMA / DATABASE / VIEW 的注释接受即忽略——ORM 的 CodeFirst 会连着
// 发一堆，报错会让它误判整次建表失败。
type CommentOn struct {
	Object  string // table / column / index / schema / database / view / constraint
	Table   string // 表名；object=column 时是列所在的表
	Column  string // object=column 时有效
	Name    string // index / schema / database 的名字
	Comment string // 文本；IsNull 为 true 时表示删除注释
	IsNull  bool
}

// Copy 对应 COPY ... FROM STDIN / TO STDOUT（只支持标准输入输出，不支持文件名）。
type Copy struct {
	Table string
	Cols  []string
	To    bool // true = TO STDOUT（导出），false = FROM STDIN（导入）
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

// Checkpoint 对应 CHECKPOINT / FLUSH：把内存数据与日志强制刷到磁盘。
// 它不改数据，只影响持久化时机。
type Checkpoint struct{}

// Show 对应 SHOW name：查会话参数。参数名原样保留（可能带空格，如
// "time zone"），由执行层去查表。
type Show struct{ Name string }

// SetStmt 对应 SET ...：会话参数设置。godb 不保存任何会话状态，
// 整条语句识别出来直接确认（客户端连上来时几乎都会先发 SET）。
type SetStmt struct{ Raw string }

// SessionCleanup 对应 DISCARD / RESET / DEALLOCATE 这三类「清理会话」的语句。
// godb 没有计划缓存、临时表、sequence 和会话参数，所以识别出来直接确认（no-op）——
// 关键是**别报语法错**：Npgsql 把连接还回池之前会发 DISCARD ALL，一报错整条
// 连接池就用不了（只能 Pooling=false 或 No Reset On Close=true 绕）。
type SessionCleanup struct {
	Command string // DISCARD / RESET / DEALLOCATE
	Tag     string // CommandComplete 里回给客户端的标签，如 "DISCARD ALL"
}

// ---------- DML ----------

// Insert 对应 INSERT INTO ... VALUES。
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr

	// Select 非空表示数据来自一条查询（INSERT ... SELECT），此时 Rows 为空。
	Select *Select

	// Returning 是 RETURNING 后面那串输出项。
	Returning []*SelectItem
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

	Returning []*SelectItem
}

// Delete 对应 DELETE。
type Delete struct {
	Table string
	Alias string
	Where Expr

	Returning []*SelectItem
}

// ---------- DQL ----------

// CTE 是 WITH 里的一个公用表表达式。
type CTE struct {
	Name   string
	Cols   []string // 可选的列名列表
	Select *Select
}

// Select 对应 SELECT。
//
// SetOp 非空表示这是一个集合运算（UNION / INTERSECT / EXCEPT）的左边，
// 右边放在 SetRight 里。ORDER BY / LIMIT 只属于最外层。
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

	// CTEs 是本查询前面 WITH 定义的公用表表达式（按声明顺序）。
	CTEs []*CTE

	SetOp    string // "union" / "intersect" / "except"
	SetAll   bool
	SetLeft  *Select
	SetRight *Select
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

// BaseTable 是普通表引用。Schema 非空表示写了限定名（pg_catalog.pg_class）。
type BaseTable struct {
	Schema string
	Table  string
	Alias  string
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

// ParenJoin 是括号包裹的联接组：LEFT JOIN (a JOIN b ON ...) x ON ...
//
// 这是标准 SQL 的 "joined table"，不是子查询。括号里可以继续是联接链，
// 括号外的别名会覆盖内部各表的限定符（所以外面用 x.col 引用内部列，
// 而 a、b 这些内部别名在括号外不可见 —— 与 PostgreSQL 行为一致）。
type ParenJoin struct {
	Inner TableRef
	Alias string
}

// SubTable 是 FROM 里的括号子查询：(SELECT ...) [AS] alias [(col, ...)]。
type SubTable struct {
	Select *Select
	Alias  string
	Cols   []string
}

// FuncTable 是 FROM 里的集合函数调用：func(args) [AS] alias [(col, ...)]。
type FuncTable struct {
	Name  string
	Args  []Expr
	Alias string
	Cols  []string
}

func (*BaseTable) tableRef() {}
func (*Join) tableRef()      {}
func (*ParenJoin) tableRef() {}
func (*SubTable) tableRef()  {}
func (*FuncTable) tableRef() {}

// ---------- 事务 ----------

// TxStmt 对应 BEGIN / COMMIT / ROLLBACK。
//
// Isolation / ReadOnly 只是把客户端请求的事务模式记下来：godb 的隔离级别
// 恒为 SERIALIZABLE（表级 S/X 两阶段封锁，锁持到提交），请求更弱的级别
// 不会真的降级——更强的保证总是安全的，客户端也不会因此出错。
type TxStmt struct {
	Kind      string
	Isolation string // serializable / repeatable read / read committed / read uncommitted
	ReadOnly  bool
}

// Explain 对应 EXPLAIN。
type Explain struct {
	Stmt Statement
	// Analyze 对应 EXPLAIN ANALYZE：真的执行一遍，并带上每个算子的实际
	// 行数与耗时 —— 判断「为什么慢」靠的就是它。
	Analyze bool
}

func (*CreateTable) stmtNode()    {}
func (*DropTable) stmtNode()      {}
func (*AlterTable) stmtNode()     {}
func (*Truncate) stmtNode()       {}
func (*Checkpoint) stmtNode()     {}
func (*Show) stmtNode()           {}
func (*SetStmt) stmtNode()        {}
func (*SessionCleanup) stmtNode() {}
func (*Insert) stmtNode()         {}
func (*Copy) stmtNode()           {}
func (*CreateIndex) stmtNode()    {}
func (*DropIndex) stmtNode()      {}
func (*CommentOn) stmtNode()      {}
func (*Update) stmtNode()         {}
func (*Delete) stmtNode()         {}
func (*Select) stmtNode()         {}
func (*TxStmt) stmtNode()         {}
func (*Explain) stmtNode()        {}

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

	// Sub 非空表示 IN (SELECT ...)，此时 List 为空。
	Sub *Subquery
}

// Exists 是 EXISTS (SELECT ...)。只关心子查询有没有行，不看具体列。
type Exists struct {
	Sub *Subquery
	Not bool
}

// Subquery 是括号里的 SELECT，只能出现在表达式位置（标量子查询）。
//
// 只支持「返回单列」的标量子查询，可以引用外层查询的列（相关子查询），
// 例如客户端常见的：
//
//	(SELECT count(*) FROM pg_inherits WHERE inhparent = c.oid) > 0
//
// 不支持 IN/EXISTS 形式与多列子查询。
type Subquery struct {
	Select *Select
}

// AnyOp 表示 x <op> ANY (arr) / x <op> ALL (arr)。
// 系统目录查询里非常常见（如 relkind = ANY (ARRAY['r','p'])）。
type AnyOp struct {
	Op  string // 比较运算符，= <> < <= > >=
	All bool   // true 表示 ALL，false 表示 ANY
	X   Expr
	Arr Expr
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

// Subscript 是数组下标 e[i]。客户端查触发器时会写 rm.m[1]，
// 我们从数组文本（'{a,b}'）里取第 i 个元素（下标从 1 开始）。
type Subscript struct {
	X     Expr
	Index Expr
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
func (*Exists) exprNode()     {}
func (*AnyOp) exprNode()      {}
func (*Subquery) exprNode()   {}
func (*Between) exprNode()    {}
func (*Like) exprNode()       {}
func (*Subscript) exprNode()  {}
func (*DefaultVal) exprNode() {}

// CreateDatabase 是 CREATE DATABASE name。库之间的数据完全隔离，
// 不支持跨库查询（PostgreSQL 也要 dblink/FDW 才能做到）。
type CreateDatabase struct {
	Name string
	// Options 是 WITH 后面的选项（OWNER / TEMPLATE / ENCODING / LC_COLLATE …）。
	// godb 每个库都是同一套 UTF-8 存储和权限模型，所以除了 ENCODING 会校验
	// 一下，其余只做语法兼容。
	Options map[string]string
}

// DropDatabase 是 DROP DATABASE [IF EXISTS] name。
type DropDatabase struct {
	Name     string
	IfExists bool
}

func (*CreateDatabase) stmtNode() {}
func (*DropDatabase) stmtNode()   {}
