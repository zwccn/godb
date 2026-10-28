package exec

import (
	"fmt"
	"sort"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/syscat"
	"godb/types"
)

// Column 是结果集的一列（给前端用）。
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// 列的来源表 OID 与列号（PG 协议 RowDescription 要回给客户端）。
	// 表达式/聚合列保持 0 —— 客户端据此判断结果集能不能回写。
	TableOID int32 `json:"-"`
	AttNum   int16 `json:"-"`
}

// Result 是一条语句的执行结果。
type Result struct {
	Command  string          `json:"command"`
	Tag      string          `json:"tag"`
	Columns  []Column        `json:"columns,omitempty"`
	Rows     [][]interface{} `json:"rows,omitempty"`
	RowCount int             `json:"rowCount"`
	Affected int             `json:"affected"`
	Plan     []string        `json:"plan,omitempty"`
	Message  string          `json:"message,omitempty"`
	// TookNs 是这条语句的耗时（纳秒，含等待锁，不含 SQL 解析），由 db 层填充。
	TookNs int64 `json:"tookNs"`
}

// Engine 执行 DDL/DML/DQL。DDL 会直接落盘（本库限制：DDL 不能在显式事务中执行）。
type Engine struct {
	Dir   string
	Cat   *catalog.Catalog
	Store *storage.Store

	// Info 描述「当前是哪个库、还有哪些库」。单库模式（测试里常见）留 nil，
	// 此时 pg_database / current_database() 只报一个默认库。
	Info *DBInfo
}

// DBInfo 是一个库的自我描述。
//
// All 用函数而不是切片：CREATE DATABASE / DROP DATABASE 之后不用回头去改
// 引擎里存的副本，查目录时现取即可。
type DBInfo struct {
	Name string
	All  func() []string
}

// planner 统一构造 Planner，把库信息带上。
// 之前有三处各自 `&Planner{Tx: tx, Cat: e.Cat}`，加字段容易漏。
func (e *Engine) planner(tx *storage.Tx) *Planner {
	p := &Planner{Tx: tx, Cat: e.Cat, Info: e.Info}
	return p
}

// compileDML 编译 DML 里的表达式（UPDATE 的 SET 值 / WHERE、DELETE 的 WHERE）。
//
// 以前这些地方用包级 Compile(expr, sc)，它构造的 compileCtx 没有 planner，
// 于是 `IN (子查询)` / 标量子查询 / `EXISTS` 一律报 "subqueries are not supported
// in this context" —— 而 PG 在这些位置是允许子查询的（EF Core 的 ExecuteDelete
// 就靠 `WHERE id IN (SELECT …)`，手写的数据清理也常用）。
//
// 安全性：UPDATE / DELETE 都是**先把目标行收集齐再写**，子查询在收集阶段逐行求值，
// 看不到本语句改了一半的数据（不存在 Halloween 问题）。
// 代价：相关子查询每行重开一次子计划，和 SELECT 里的标量子查询同一量级。
func (e *Engine) compileDML(tx *storage.Tx, expr sql.Expr, sc Schema) (Eval, error) {
	return compile(expr, &compileCtx{sc: sc, planner: e.planner(tx)})
}

// constDML 编译 INSERT ... VALUES 里的单值表达式：schema 为空（VALUES 不许引用列），
// 但同样要允许子查询（`VALUES ((SELECT max(id)+1 FROM t))`）。
func (e *Engine) constDML(tx *storage.Tx, expr sql.Expr) (types.Value, error) {
	if expr == nil {
		return types.Null, nil
	}
	ev, err := compile(expr, &compileCtx{planner: e.planner(tx)})
	if err != nil {
		return types.Null, err
	}
	return ev.Eval(nil)
}

// ExecStmt 执行一条非事务控制语句。
func (e *Engine) ExecStmt(tx *storage.Tx, stmt sql.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *sql.Select:
		return e.execSelect(tx, s, explainOff)
	case *sql.Explain:
		sel, ok := s.Stmt.(*sql.Select)
		if !ok {
			// 客户端「执行计划」按钮会对整段 SQL 加 EXPLAIN，多语句/事务演示就会被
			// 打到这里。错误里说清楚"只有查询能解释"，别让人以为是语法错。
			return nil, fmt.Errorf("EXPLAIN in godb only supports query statements (SELECT / WITH / VALUES); run other statements directly")
		}
		mode := explainPlan
		if s.Analyze {
			mode = explainAnalyze
		}
		return e.execSelect(tx, sel, mode)
	case *sql.Insert:
		return e.execInsert(tx, s)
	case *sql.CreateIndex:
		return e.execCreateIndex(s)
	case *sql.DropIndex:
		return e.execDropIndex(s)
	case *sql.Copy:
		// COPY 需要协议层的 CopyIn/CopyOut 数据流，SQL 层单独调不通
		return nil, fmt.Errorf("COPY must be executed through the PostgreSQL protocol")
	case *sql.CommentOn:
		return e.execCommentOn(s)
	case *sql.Update:
		return e.execUpdate(tx, s)
	case *sql.Delete:
		return e.execDelete(tx, s)
	case *sql.CreateTable:
		return e.execCreateTable(s)
	case *sql.DropTable:
		return e.execDropTable(s)
	case *sql.AlterTable:
		return e.execAlterTable(s)
	case *sql.Truncate:
		return e.execTruncate(tx, s)
	}
	return nil, fmt.Errorf("unsupported statement")
}

// ---------- SELECT ----------

// explainMode 决定 execSelect 是普通执行还是只出计划。
type explainMode int

const (
	explainOff explainMode = iota
	explainPlan
	explainAnalyze
)

func (e *Engine) execSelect(tx *storage.Tx, s *sql.Select, mode explainMode) (*Result, error) {
	plan, err := e.planner(tx).BuildSelect(s)
	if err != nil {
		return nil, err
	}

	if mode != explainOff {
		var planText []string
		if mode == explainAnalyze {
			// 真跑一遍：结果丢掉，只留每个算子的行数与耗时
			planText, err = ExplainAnalyze(plan)
		} else {
			planText = ExplainPlan(plan)
		}
		if err != nil {
			return nil, err
		}
		rows := make([][]interface{}, 0, len(planText))
		for _, line := range planText {
			rows = append(rows, []interface{}{line})
		}
		tag := "EXPLAIN"
		if mode == explainAnalyze {
			tag = "EXPLAIN ANALYZE"
		}
		return &Result{
			Command:  tag,
			Tag:      tag,
			Columns:  []Column{{Name: "QUERY PLAN", Type: "text"}},
			Rows:     rows,
			RowCount: len(rows),
			Plan:     planText,
		}, nil
	}

	if err := plan.Open(); err != nil {
		return nil, err
	}
	defer plan.Close()

	sc := plan.Schema()
	cols := make([]Column, len(sc))
	for i, c := range sc {
		cols[i] = Column{Name: c.Name, Type: c.Type.String(), TableOID: c.TableOID, AttNum: c.AttNum}
	}
	var rows [][]interface{}
	for {
		row, ok, err := plan.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		out := make([]interface{}, len(row))
		for i, v := range row {
			ct := ""
			if i < len(cols) {
				ct = cols[i].Type
			}
			out[i] = cellValue(v, ct)
		}
		rows = append(rows, out)
	}
	return &Result{
		Command:  "SELECT",
		Tag:      fmt.Sprintf("SELECT %d", len(rows)),
		Columns:  cols,
		Rows:     rows,
		RowCount: len(rows),
		Plan:     ExplainPlan(plan),
	}, nil
}

// ---------- DDL ----------

func (e *Engine) buildColumns(defs []*sql.ColumnDef) ([]*catalog.Column, error) {
	cols := make([]*catalog.Column, 0, len(defs))
	for _, d := range defs {
		t, err := types.ParseType(d.TypeName, d.TypeArgs)
		if err != nil {
			return nil, err
		}
		c := catalog.NewColumn(d.Name, t)
		c.NotNull = d.NotNull
		c.PrimaryKey = d.PrimaryKey
		c.Unique = d.Unique
		c.Serial = d.Serial
		if d.Serial {
			c.NotNull = true
			// 老式 SERIAL 在 godb 里一直是隐含唯一的（历史行为，保持兼容）。
			// IDENTITY 按 SQL 标准只保证 NOT NULL，唯一性由列级 PRIMARY KEY /
			// UNIQUE 显式给，这样和 PostgreSQL 的语义一致。
			if !d.Identity {
				c.Unique = true
			}
		}
		if d.Default != nil {
			c.Default = sql.String(d.Default)
		}
		cols = append(cols, c)
	}
	return cols, nil
}

// persistSchema 保存表结构到 catalog.json。
//
// 只有"磁盘上的旧行无法按新结构解释"的 DDL 才需要立刻重写全量快照：
//
//	CREATE / DROP TABLE  安全——快照里多出来的表在加载时会被跳过
//	ADD COLUMN           安全——旧行少一列，加载时补 NULL，正好等于"新列默认 NULL"
//	RENAME COLUMN        安全——行是按下标存的，改名不影响
//	DROP COLUMN          不安全——列从中间被挖掉，旧行按下标解释会错位
//	RENAME TABLE         不安全——快照按表名存
//	ALTER TYPE           不安全——快照里是旧类型的值
//
// 不安全的动作仍然做一次全量 checkpoint；安全的动作只写 catalog.json，
// 数据交给后续的自动 checkpoint（每 128 次提交）或显式 CHECKPOINT。
func (e *Engine) persistSchema(needsSnapshot bool) error {
	if err := e.Cat.Save(e.Dir); err != nil {
		return err
	}
	if !needsSnapshot {
		return nil
	}
	return e.Store.Checkpoint()
}

func (e *Engine) execCreateTable(s *sql.CreateTable) (*Result, error) {
	if s.As != nil {
		return e.execCreateTableAs(s)
	}
	if _, exists := e.Cat.Get(s.Table); exists {
		if s.IfNotExists {
			return &Result{Command: "CREATE TABLE", Tag: "CREATE TABLE", Message: "表已存在，跳过"}, nil
		}
		return nil, fmt.Errorf("relation %q already exists", s.Table)
	}
	cols, err := e.buildColumns(s.Cols)
	if err != nil {
		return nil, err
	}
	t := &catalog.Table{Name: s.Table, Columns: cols}
	// 表级 CHECK 在前、列级在后（和 PostgreSQL 的命名顺序一致）
	t.Checks = append(t.Checks, s.Checks...)
	for _, c := range s.Cols {
		if c.Check != "" {
			t.Checks = append(t.Checks, c.Check)
		}
	}
	if err := e.buildForeignKeys(t, s); err != nil {
		return nil, err
	}
	if err := e.Cat.Add(t); err != nil {
		return nil, err
	}
	e.Store.AddTable(t.Name)
	if err := e.persistSchema(false); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE TABLE", Tag: "CREATE TABLE"}, nil
}

func (e *Engine) execDropTable(s *sql.DropTable) (*Result, error) {
	for _, name := range s.Tables {
		if _, ok := e.Cat.Get(name); !ok {
			if s.IfExists {
				continue
			}
			return nil, fmt.Errorf("table %q does not exist", name)
		}
		if err := e.checkDropFK(name); err != nil {
			return nil, err
		}
		if err := e.Cat.Drop(name); err != nil {
			return nil, err
		}
		e.Store.DropTable(name)
	}
	if err := e.persistSchema(false); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP TABLE", Tag: "DROP TABLE"}, nil
}

func (e *Engine) execTruncate(tx *storage.Tx, s *sql.Truncate) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	var refs []storage.Ref
	if err := tx.Scan(t.Name, func(ref storage.Ref, _ types.Row) error {
		refs = append(refs, ref)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if err := tx.Delete(t.Name, ref); err != nil {
			return nil, err
		}
	}
	// 重置自增序列
	tx.ResetSeq(t.Name)
	return &Result{Command: "TRUNCATE TABLE", Tag: "TRUNCATE TABLE", Affected: len(refs)}, nil
}

func (e *Engine) execAlterTable(s *sql.AlterTable) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	td := e.Store.Table(t.Name)
	// ALTER 会按下标重写每一行，先压实空槽：否则空槽会被当成
	// 「全默认值的一行」写回去，行数凭空多出来。
	td.CompactRows()
	act := s.Action
	note := ""
	switch act.Kind {
	case "add_column":
		if _, _, exists := t.Col(act.Col.Name); exists {
			return nil, fmt.Errorf("column %q of relation %q already exists", act.Col.Name, t.Name)
		}
		cols, err := e.buildColumns([]*sql.ColumnDef{act.Col})
		if err != nil {
			return nil, err
		}
		newCol := cols[0]
		if newCol.NotNull && newCol.Default == "" && !newCol.Serial {
			return nil, fmt.Errorf("column %q contains null values", newCol.Name)
		}
		defVal, err := e.defaultValueOf(newCol)
		if err != nil {
			return nil, err
		}
		t.Columns = append(t.Columns, newCol)
		if err := t.Validate(); err != nil {
			return nil, err
		}
		for i, row := range td.Rows {
			v := defVal
			if newCol.Serial {
				td.Seq++
				v = types.Int(td.Seq)
			}
			newRow := make(types.Row, 0, len(row)+1)
			newRow = append(newRow, row...)
			newRow = append(newRow, v)
			td.Rows[i] = newRow
		}
		td.RebuildIndex(t)
	case "drop_column":
		idx, ok := t.ColumnIdx(act.Name)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", act.Name, t.Name)
		}
		if len(t.Columns) == 1 {
			return nil, fmt.Errorf("cannot drop the only column of table %q", t.Name)
		}
		t.Columns = append(t.Columns[:idx], t.Columns[idx+1:]...)
		if err := t.Validate(); err != nil {
			return nil, err
		}
		// 引用了被删列的索引必须一起删：留着它，列号会指向错位的列，
		// 查询可能走索引拿到完全不相干的行。PostgreSQL 也是连带删除。
		var kept []*catalog.Index
		var dropped []string
		for _, ix := range t.Indexes {
			uses := false
			for _, cn := range ix.Columns {
				if strings.EqualFold(cn, act.Name) {
					uses = true
					break
				}
			}
			if uses {
				dropped = append(dropped, ix.Name)
				continue
			}
			kept = append(kept, ix)
		}
		if len(dropped) > 0 {
			t.Indexes = kept
			note = fmt.Sprintf("同时删除了依赖该列的索引：%s", strings.Join(dropped, ", "))
		}
		for i, row := range td.Rows {
			newRow := make(types.Row, 0, len(row)-1)
			newRow = append(newRow, row[:idx]...)
			newRow = append(newRow, row[idx+1:]...)
			td.Rows[i] = newRow
		}
		td.RebuildIndex(t)
	case "rename_column":
		idx, ok := t.ColumnIdx(act.Name)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", act.Name, t.Name)
		}
		if _, _, exists := t.Col(act.NewName); exists {
			return nil, fmt.Errorf("column %q of relation %q already exists", act.NewName, t.Name)
		}
		t.Columns[idx].Name = act.NewName
		// 索引按列名引用列，改名要同步，否则索引变成「引用了不存在的列」而被跳过
		for _, ix := range t.Indexes {
			for j, cn := range ix.Columns {
				if strings.EqualFold(cn, act.Name) {
					ix.Columns[j] = act.NewName
				}
			}
		}
		t.Rebuild()
	case "rename_table":
		oldName := t.Name
		if err := e.Cat.Rename(oldName, act.NewName); err != nil {
			return nil, err
		}
		e.Store.RenameTable(oldName, act.NewName)
	case "alter_type":
		idx, ok := t.ColumnIdx(act.Name)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", act.Name, t.Name)
		}
		newType, err := types.ParseType(act.TypeName, act.TypeArgs)
		if err != nil {
			return nil, err
		}
		for i, row := range td.Rows {
			if idx >= len(row) {
				continue
			}
			nv, err := types.Cast(row[idx], newType)
			if err != nil {
				return nil, err
			}
			row[idx] = nv
			td.Rows[i] = row
		}
		// 只换类型，别把约束丢了。之前这里用 NewColumn 重建列定义，
		// NOT NULL / UNIQUE / DEFAULT / SERIAL 全被清掉，主键和索引因为
		// 存在 catalog.Table 的索引里反而留了下来 —— 表设计器改个类型就会
		// 悄悄丢掉 NOT NULL，属于静默的数据完整性损失。
		col := t.Columns[idx]
		col.TypeName = newType.String()
		col.Kind = int(newType.Kind)
		col.Len, col.Prec, col.Scale = newType.Len, newType.Prec, newType.Scale
		t.Columns[idx] = col
		t.Rebuild()
	default:
		return nil, fmt.Errorf("unsupported ALTER TABLE action")
	}
	// 任何一种 ALTER 都可能让现有的哈希索引失效（列位置、表名、列类型都变了），
	// 统一重建一次。DDL 本来就少，不值得为省这一次扫描去区分。
	e.Store.RebuildIndex(t.Name)
	// DROP COLUMN / RENAME TABLE / ALTER TYPE 必须重写快照，见 persistSchema 的说明
	needsSnapshot := act.Kind == "drop_column" || act.Kind == "rename_table" || act.Kind == "alter_type"
	if err := e.persistSchema(needsSnapshot); err != nil {
		return nil, err
	}
	return &Result{Command: "ALTER TABLE", Tag: "ALTER TABLE", Message: note}, nil
}

// ---------- INSERT ----------

func (e *Engine) defaultValueOf(col *catalog.Column) (types.Value, error) {
	if col.Default == "" {
		return types.Null, nil
	}
	expr, err := sql.ParseExpr(col.Default)
	if err != nil {
		return types.Null, err
	}
	v, err := CompileConst(expr)
	if err != nil {
		return types.Null, err
	}
	return types.Cast(v, col.Type())
}

func (e *Engine) execInsert(tx *storage.Tx, s *sql.Insert) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	targets := make([]int, 0, len(t.Columns))
	if len(s.Columns) == 0 {
		for i := range t.Columns {
			targets = append(targets, i)
		}
	} else {
		seen := map[int]bool{}
		for _, name := range s.Columns {
			idx, ok := t.ColumnIdx(name)
			if !ok {
				return nil, fmt.Errorf("column %q of relation %q does not exist", name, t.Name)
			}
			if seen[idx] {
				return nil, fmt.Errorf("column %q specified more than once", name)
			}
			seen[idx] = true
			targets = append(targets, idx)
		}
	}

	groups := t.UniqueGroups()
	uixs := uniqueIndexGroups(t)
	checks, err := e.compileChecks(t)
	if err != nil {
		return nil, err
	}

	// INSERT ... SELECT：先把查询跑出全部行，再折成字面量走同一条插入路径。
	// 这样默认值、自增、NOT NULL 与唯一性校验只有一份实现，不会两边走偏。
	// 注意不要改 s.Rows —— 语句对象可能被预编译语句反复执行。
	rows := s.Rows
	if s.Select != nil {
		src, _, err := e.selectRows(tx, s.Select)
		if err != nil {
			return nil, err
		}
		rows = make([][]sql.Expr, 0, len(src))
		for _, r := range src {
			if len(r) != len(targets) {
				return nil, fmt.Errorf("INSERT has %d expressions but %d target columns", len(r), len(targets))
			}
			exprs := make([]sql.Expr, len(r))
			for i, v := range r {
				exprs[i] = &sql.Literal{Val: v}
			}
			rows = append(rows, exprs)
		}
	}

	// RETURNING 的输出项按「目标表的一行」编译，插完再逐行求值。
	var retEvals []Eval
	var retCols []Column
	if len(s.Returning) > 0 {
		ev, cols, cerr := e.compileReturning(e.tableScope(t, ""), s.Returning, tx)
		if cerr != nil {
			return nil, cerr
		}
		retEvals, retCols = ev, cols
	}
	var inserted []types.Row

	// 语句内已出现过的唯一键（避免 O(n²) 的 overlay 扫描）
	seen := map[string]map[string]bool{}
	baseIns := tx.OverlayInsLen(t.Name)
	// skipped 是 ON CONFLICT DO NOTHING 跳过的行数，updated 是 DO UPDATE 改的行数。
	var skipped, updated int

	for _, exprs := range rows {
		if len(exprs) != len(targets) {
			return nil, fmt.Errorf("INSERT has %d expressions but %d target columns", len(exprs), len(targets))
		}
		row := make(types.Row, len(t.Columns))
		provided := make([]bool, len(t.Columns))
		for i, expr := range exprs {
			col := t.Columns[targets[i]]
			provided[targets[i]] = true
			if _, isDefault := expr.(*sql.DefaultVal); isDefault {
				provided[targets[i]] = false
				continue
			}
			v, err := e.constDML(tx, expr)
			if err != nil {
				return nil, err
			}
			cv, err := types.Cast(v, col.Type())
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", col.Name, err)
			}
			row[targets[i]] = cv
		}
		for i, col := range t.Columns {
			if provided[i] {
				continue
			}
			if col.Serial {
				row[i] = types.Int(tx.NextSeq(t.Name))
				continue
			}
			if col.NotNull && col.Default == "" {
				return nil, fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
			}
			v, err := e.defaultValueOf(col)
			if err != nil {
				return nil, err
			}
			row[i] = v
		}
		// 约束校验
		for i, col := range t.Columns {
			if col.NotNull && row[i].IsNull() {
				return nil, fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
			}
		}
		// CHECK 约束（INSERT 按行校验）
		if err := evalChecks(checks, row); err != nil {
			return nil, err
		}
		if err := e.checkFKRow(tx, t, row); err != nil {
			return nil, err
		}
		conflict, hasConflict, err := e.insertConflict(tx, t, row, groups, uixs, seen, baseIns, nil, s.OnConflict)
		if err != nil {
			return nil, err
		}
		if hasConflict {
			if s.OnConflict == nil {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", conflict.name)
			}
			if s.OnConflict.DoNothing {
				skipped++
				continue
			}
			// DO UPDATE：把冲突行按 SET 列表改掉，而不是插入新行。
			// 同一条语句里出现两次相同键时 PostgreSQL 会拒绝（否则「改两次」
			// 的结果取决于顺序），这里照做。
			if conflict.fromStmt {
				return nil, fmt.Errorf("ON CONFLICT DO UPDATE command cannot affect row a second time")
			}
			oldRow, ok := tx.Get(t.Name, conflict.ref)
			if !ok {
				return nil, fmt.Errorf("failed to fetch the conflicting row")
			}
			nr, doUpdate, err := e.buildConflictUpdate(tx, t, oldRow, row, s.OnConflict)
			if err != nil {
				return nil, err
			}
			if !doUpdate {
				skipped++
				continue
			}
			if err := evalChecks(checks, nr); err != nil {
				return nil, err
			}
			if err := e.checkFKRow(tx, t, nr); err != nil {
				return nil, err
			}
			// 改完可能撞上别的唯一键（比如把 name 改成了别人已有的值）。
			// 用全新的 seen 复检，并排除被改的这一行自身。
			if again, dup, err := e.insertConflict(tx, t, nr, groups, uixs,
				map[string]map[string]bool{}, baseIns, &conflict.ref, s.OnConflict); err != nil {
				return nil, err
			} else if dup {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", again.name)
			}
			if err := tx.Update(t.Name, conflict.ref, nr); err != nil {
				return nil, err
			}
			updated++
			if len(retEvals) > 0 {
				// DO UPDATE 的 RETURNING 看到的是更新后的行
				inserted = append(inserted, nr)
			}
			continue
		}
		if _, err := tx.Insert(t.Name, row); err != nil {
			return nil, err
		}
		if len(retEvals) > 0 {
			inserted = append(inserted, row)
		}
	}
	// PostgreSQL 的 INSERT 标签报的是真正写进去的行数：ON CONFLICT 跳过的
	// 那部分不算，DO UPDATE 的那部分算（它确实改了行）。
	res := &Result{
		Command:  "INSERT",
		Tag:      fmt.Sprintf("INSERT 0 %d", len(rows)-skipped),
		Affected: len(rows) - skipped,
	}
	if len(retEvals) > 0 {
		res.Columns = retCols
		for _, r := range inserted {
			out := make([]interface{}, len(retEvals))
			for i, ev := range retEvals {
				v, err := ev.Eval(r)
				if err != nil {
					return nil, err
				}
				ct := ""
				if i < len(retCols) {
					ct = retCols[i].Type
				}
				out[i] = cellValue(v, ct)
			}
			res.Rows = append(res.Rows, out)
		}
		res.RowCount = len(res.Rows)
	}
	return res, nil
}

// relatedRows 把 UPDATE ... FROM / DELETE ... USING 的关联表整表读进内存。
// 返回它的作用域（要拼在目标表后面）和行数据。
//
// 关联表通常很小（字典表、临时表），整表读进来比逐行回表简单得多，
// 也避免在事务扫描过程里再嵌套一次扫描。
// relatedRows 把 UPDATE ... FROM / DELETE ... USING 的数据源跑成「列布局 + 全部行」。
//
// 以前它只会读一张表（*sql.BaseTable），派生表和多表 JOIN 在解析阶段就被拒了。
// 现在复用 SELECT 的 FROM 构造（planner.buildFrom），所以 `FROM b, c`、
// `FROM b JOIN c ON …`、`FROM (SELECT …) x`、`FROM (VALUES …) x(a,b)` 全都能用 ——
// 而且和 SELECT 走同一份算子代码，语义不会两边走偏。
//
// 注意这里**不做谓词下推**：p.pushdown 只有 BuildSelect 才会填，WHERE 里同时引用
// 目标表和关联表，推到关联表上会改语义。所以整份数据源会被读完再配对。
func (e *Engine) relatedRows(tx *storage.Tx, ref sql.TableRef) (Schema, [][]types.Value, error) {
	if ref == nil {
		return nil, nil, nil
	}
	p := e.planner(tx)
	op, sc, err := p.buildFrom([]sql.TableRef{ref})
	if err != nil {
		return nil, nil, err
	}
	if op == nil {
		return nil, nil, nil
	}
	if err := op.Open(); err != nil {
		return nil, nil, err
	}
	defer op.Close()

	var rows [][]types.Value
	for {
		row, ok, err := op.Next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			break
		}
		// 算子可能复用缓冲：留下之前拷一份
		cp := make(types.Row, len(row))
		copy(cp, row)
		rows = append(rows, cp)
	}
	return sc, rows, nil
}

// conflictInfo 描述一次唯一约束冲突。
type conflictInfo struct {
	name string // 约束名，报错和 ON CONFLICT 都用它
	ref  storage.Ref
	// fromStmt 为 true 表示冲突来自同一条 INSERT 里前面的行（ref 无效）。
	fromStmt bool
}

// insertConflict 检查一行是否与已有数据、或本语句前面插入的行冲突。
// self 非 nil 时排除这一行自身（ON CONFLICT DO UPDATE 复检用）。
func (e *Engine) insertConflict(tx *storage.Tx, t *catalog.Table, row types.Row,
	groups [][]int, uixs []uxGroup, seen map[string]map[string]bool,
	baseIns int, self *storage.Ref, oc *sql.OnConflict) (conflictInfo, bool, error) {

	for gi, g := range groups {
		vals, ok := groupValues(row, g)
		if !ok {
			continue // 含 NULL 的组合不参与唯一性判断
		}
		name := groupName(t, g)
		handled := conflictHandled(t, oc, g, name)
		k := types.Key(vals)
		cm := seenKeys(seen, fmt.Sprintf("g%d", gi))
		if cm[k] {
			if !handled {
				return conflictInfo{}, false, fmt.Errorf("duplicate key value violates unique constraint %q", name)
			}
			return conflictInfo{name: name, fromStmt: true}, true, nil
		}
		cm[k] = true
		// 只扫本语句之前插入的行；本语句内重复已由 seen 拦住
		ref, dup, err := tx.FindUniqueGroupRef(t, g, vals, self, baseIns)
		if err != nil {
			return conflictInfo{}, false, err
		}
		if dup {
			if !handled {
				return conflictInfo{}, false, fmt.Errorf("duplicate key value violates unique constraint %q", name)
			}
			return conflictInfo{name: name, ref: ref}, true, nil
		}
	}
	for _, ux := range uixs {
		vals, ok := groupValues(row, ux.cols)
		if !ok {
			continue
		}
		k := storage.OrderKey(vals) // 和树里的键同一套编码
		handled := conflictHandled(t, oc, ux.cols, ux.name)
		cm := seenKeys(seen, "i"+ux.name)
		if cm[k] {
			if !handled {
				return conflictInfo{}, false, fmt.Errorf("duplicate key value violates unique constraint %q", ux.name)
			}
			return conflictInfo{name: ux.name, fromStmt: true}, true, nil
		}
		cm[k] = true
		// 唯一索引自带哈希表，判重是 O(1)：命中任何一行都算重复。
		for _, h := range tx.IndexLookup(t, ux.name, ux.cols, k) {
			if self != nil && h.Ref.New == self.New && h.Ref.Slot == self.Slot {
				continue
			}
			if !handled {
				return conflictInfo{}, false, fmt.Errorf("duplicate key value violates unique constraint %q", ux.name)
			}
			return conflictInfo{name: ux.name, ref: h.Ref}, true, nil
		}
	}
	return conflictInfo{}, false, nil
}

// conflictHandled 判断某个唯一约束上的冲突是否归 ON CONFLICT 处理。
//
// 不带目标列的 ON CONFLICT 管所有唯一冲突；带了 (col, ...) 或 ON CONSTRAINT
// 名字时只管对应的那一个，别的约束冲突照常报错 —— PostgreSQL 就是这个语义。
func conflictHandled(t *catalog.Table, oc *sql.OnConflict, cols []int, name string) bool {
	if oc == nil {
		return false
	}
	if oc.Constraint != "" {
		return strings.EqualFold(oc.Constraint, name)
	}
	if len(oc.Target) == 0 {
		return true
	}
	if len(oc.Target) != len(cols) {
		return false
	}
	want := make([]int, 0, len(oc.Target))
	for _, cn := range oc.Target {
		idx, ok := t.ColumnIdx(cn)
		if !ok {
			return false
		}
		want = append(want, idx)
	}
	sort.Ints(want)
	got := append([]int(nil), cols...)
	sort.Ints(got)
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// buildConflictUpdate 按 ON CONFLICT DO UPDATE SET 生成更新后的行。
// 第二个返回值是 false 表示 DO UPDATE 的 WHERE 不成立，等价于 DO NOTHING。
//
// excluded 不是真表：把目标表的列复制一份、限定符换成 "excluded"，挂在
// scope 后面，求值时行也拼成「旧行 + 新行」。这样 excluded.col 就是待插入
// 行的值，裸写 col 则因为两列同名而报 ambiguous —— 和 PostgreSQL 一致。
func (e *Engine) buildConflictUpdate(tx *storage.Tx, t *catalog.Table,
	oldRow, newRow types.Row, oc *sql.OnConflict) (types.Row, bool, error) {

	sc := e.tableScope(t, "")
	excluded := make(Schema, len(sc))
	for i, ci := range sc {
		excluded[i] = ci
		excluded[i].Qualifier = "excluded"
	}
	combined := append(append(Schema{}, sc...), excluded...)
	row := make(types.Row, 0, len(oldRow)+len(newRow))
	row = append(row, oldRow...)
	row = append(row, newRow...)

	ctx := &compileCtx{sc: combined, planner: e.planner(tx)}

	if oc.UpdateWhere != nil {
		ev, err := compile(oc.UpdateWhere, ctx)
		if err != nil {
			return nil, false, err
		}
		v, err := ev.Eval(row)
		if err != nil {
			return nil, false, err
		}
		if v.IsNull() || !v.Bool() {
			return nil, false, nil
		}
	}

	out := make(types.Row, len(oldRow))
	copy(out, oldRow)
	assigned := map[int]bool{}
	for _, a := range oc.Updates {
		idx, ok := t.ColumnIdx(a.Col)
		if !ok {
			return nil, false, fmt.Errorf("column %q of relation %q does not exist", a.Col, t.Name)
		}
		if assigned[idx] {
			return nil, false, fmt.Errorf("column %q specified more than once", a.Col)
		}
		assigned[idx] = true
		ev, err := compile(a.Value, ctx)
		if err != nil {
			return nil, false, err
		}
		v, err := ev.Eval(row)
		if err != nil {
			return nil, false, err
		}
		cv, err := types.Cast(v, t.Columns[idx].Type())
		if err != nil {
			return nil, false, fmt.Errorf("column %q: %w", a.Col, err)
		}
		out[idx] = cv
	}
	for i, col := range t.Columns {
		if col.NotNull && out[i].IsNull() {
			return nil, false, fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
		}
	}
	return out, true, nil
}

// selectRows 跑一条查询，返回原始值行（不受客户端格式影响）。
// INSERT ... SELECT 用它取数据源。
func (e *Engine) selectRows(tx *storage.Tx, s *sql.Select) ([][]types.Value, Schema, error) {
	plan, err := e.planner(tx).BuildSelect(s)
	if err != nil {
		return nil, nil, err
	}
	if err := plan.Open(); err != nil {
		return nil, nil, err
	}
	defer plan.Close()

	var out [][]types.Value
	for {
		r, ok, err := plan.Next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			break
		}
		// 算子可能复用内部缓冲，必须拷贝一份
		cp := make([]types.Value, len(r))
		copy(cp, r)
		out = append(out, cp)
	}
	return out, plan.Schema(), nil
}

// compileReturning 把 RETURNING 的输出项编译成「针对目标表一行」的求值器。
func (e *Engine) compileReturning(sc Schema, items []*sql.SelectItem, tx *storage.Tx) ([]Eval, []Column, error) {
	items, err := expandStars(items, sc)
	if err != nil {
		return nil, nil, err
	}
	ctx := &compileCtx{sc: sc, planner: e.planner(tx)}
	evals := make([]Eval, 0, len(items))
	cols := make([]Column, 0, len(items))
	for _, it := range items {
		ev, err := compile(it.Expr, ctx)
		if err != nil {
			return nil, nil, err
		}
		name := it.Alias
		if name == "" {
			if cr, ok := it.Expr.(*sql.ColumnRef); ok {
				name = cr.Name
			} else {
				name = "?column?"
			}
		}
		evals = append(evals, ev)
		cols = append(cols, Column{Name: name, Type: ev.Type().String()})
	}
	return evals, cols, nil
}

// groupValues 取一组列的值；任一列为 NULL 时返回 ok=false（不参与唯一性判断）。
func groupValues(row types.Row, cols []int) ([]types.Value, bool) {
	vals := make([]types.Value, len(cols))
	for i, ci := range cols {
		if ci >= len(row) || row[ci].IsNull() {
			return nil, false
		}
		vals[i] = row[ci]
	}
	return vals, true
}

// groupUnchanged 判断这一组列在更新前后是否完全相同。
func groupUnchanged(before, after types.Row, cols []int) bool {
	for _, ci := range cols {
		if ci >= len(before) || ci >= len(after) || !types.Same(before[ci], after[ci]) {
			return false
		}
	}
	return true
}

// groupName 给唯一组造约束名：主键用 <表>_pkey，单个 UNIQUE 列用 <表>_<列>_key。
func groupName(t *catalog.Table, cols []int) string {
	if len(cols) == 1 && !t.Columns[cols[0]].PrimaryKey {
		return fmt.Sprintf("%s_%s_key", t.Name, t.Columns[cols[0]].Name)
	}
	return t.Name + "_pkey"
}

// uxGroup 是一条需要判重的用户唯一索引（列位置已经解析好）。
type uxGroup struct {
	name string
	cols []int
}

// uniqueIndexGroups 返回表上所有 UNIQUE 的用户索引。
//
// 在此之前 CREATE UNIQUE INDEX 只写进 catalog 和系统目录：既不加速查询，
// 也不参与判重，用户建完以为有了唯一约束、实际还能插重复值。既然索引表
// 已经真建起来了，判重顺带就能做，而且是 O(1)（列级 UNIQUE 只能全表扫）。
func uniqueIndexGroups(t *catalog.Table) []uxGroup {
	var out []uxGroup
	for _, ix := range t.Indexes {
		if !ix.Unique || len(ix.Columns) == 0 {
			continue
		}
		cols := make([]int, 0, len(ix.Columns))
		ok := true
		for _, cn := range ix.Columns {
			ci, exists := t.ColumnIdx(cn)
			if !exists {
				ok = false
				break
			}
			cols = append(cols, ci)
		}
		if !ok {
			continue
		}
		out = append(out, uxGroup{name: ix.Name, cols: cols})
	}
	return out
}

// seenKeys 取出（必要时新建）某个唯一组的「本语句已见过键」集合。
func seenKeys(m map[string]map[string]bool, group string) map[string]bool {
	s := m[group]
	if s == nil {
		s = map[string]bool{}
		m[group] = s
	}
	return s
}

// collectByIndex 用索引取出 WHERE 能完整匹配的候选行。
//
// 返回的 ok 表示「用上了索引」；false 时调用方退回全表扫描。命中索引但
// 一行都没有时返回 ok=true 加空结果，这时不能回退 —— 回退会把整表捞回来。
func collectByIndex(tx *storage.Tx, t *catalog.Table, where sql.Expr) ([]storage.Ref, []types.Row, bool) {
	if where == nil {
		return nil, nil, false
	}
	plan, found := pickIndex(t, where, t.Name)
	if !found {
		return nil, nil, false
	}
	if plan.isRange {
		hits := tx.IndexRanges(t, plan.ixName, plan.cols, plan.ixCols, plan.rs)
		refs := make([]storage.Ref, 0, len(hits))
		rows := make([]types.Row, 0, len(hits))
		for _, h := range hits {
			refs = append(refs, h.Ref)
			rows = append(rows, h.Row)
		}
		return refs, rows, true
	}
	for _, v := range plan.eq {
		if v.IsNull() {
			return nil, nil, true // 键里有 NULL：恒不匹配
		}
	}
	key := storage.OrderKey(plan.eq) // 索引键用保序编码，和树里的键一致
	if plan.ixName == "" {
		if hit, ok := tx.PKLookup(t, key); ok {
			return []storage.Ref{hit.Ref}, []types.Row{hit.Row}, true
		}
		return nil, nil, true
	}
	hits := tx.IndexLookup(t, plan.ixName, plan.cols, key)
	refs := make([]storage.Ref, 0, len(hits))
	rows := make([]types.Row, 0, len(hits))
	for _, h := range hits {
		refs = append(refs, h.Ref)
		rows = append(rows, h.Row)
	}
	return refs, rows, true
}

// ---------- UPDATE / DELETE ----------

func (e *Engine) tableScope(t *catalog.Table, alias string) Schema {
	return tableScopeOf(e.Cat, t, alias)
}

func (e *Engine) execUpdate(tx *storage.Tx, s *sql.Update) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	ncol := len(t.Columns)
	sc := e.tableScope(t, s.Alias)
	// UPDATE ... FROM：把关联表整表读进来，scope 拼在目标表后面。
	var fromRows [][]types.Value
	if len(s.From) > 0 {
		fs, fr, err := e.relatedRows(tx, s.From[0])
		if err != nil {
			return nil, err
		}
		fromRows = fr
		sc = append(append(Schema{}, sc...), fs...)
	}

	type setItem struct {
		idx int
		ev  Eval
		col *catalog.Column
	}
	sets := make([]setItem, 0, len(s.Sets))
	seen := map[int]bool{}
	for _, a := range s.Sets {
		idx, ok := t.ColumnIdx(a.Col)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", a.Col, t.Name)
		}
		if seen[idx] {
			return nil, fmt.Errorf("multiple assignments to same column %q", a.Col)
		}
		seen[idx] = true
		ev, err := e.compileDML(tx, a.Value, sc)
		if err != nil {
			return nil, err
		}
		sets = append(sets, setItem{idx: idx, ev: ev, col: t.Columns[idx]})
	}

	var pred Eval
	if s.Where != nil {
		p, err := e.compileDML(tx, s.Where, sc)
		if err != nil {
			return nil, err
		}
		pred = p
	}

	// RETURNING 按表的一行求值；UPDATE 语义上返回的是「改之后」的行。
	var retEvals []Eval
	var retCols []Column
	if len(s.Returning) > 0 {
		ev, cols, cerr := e.compileReturning(sc, s.Returning, tx)
		if cerr != nil {
			return nil, cerr
		}
		retEvals, retCols = ev, cols
	}
	var retRows []types.Row
	checks, err := e.compileChecks(t)
	if err != nil {
		return nil, err
	}

	groups := t.UniqueGroups()
	uixs := uniqueIndexGroups(t)
	affected := 0
	var refs []storage.Ref
	var rows []types.Row
	// matchRow 判断一行（可能已拼上关联表的值）是否满足 WHERE。
	matchRow := func(row types.Row) (bool, error) {
		if pred == nil {
			return true, nil
		}
		v, err := pred.Eval(row)
		if err != nil {
			return false, err
		}
		b, isNull, err := asBool(v)
		if err != nil {
			return false, err
		}
		return !isNull && b, nil
	}
	joinRow := func(row types.Row, fr types.Row) types.Row {
		out := make(types.Row, 0, len(row)+len(fr))
		out = append(out, row...)
		return append(out, fr...)
	}
	collect := func(ref storage.Ref, row types.Row) error {
		if len(s.From) == 0 {
			ok, err := matchRow(row)
			if err != nil || !ok {
				return err
			}
			refs = append(refs, ref)
			rows = append(rows, row)
			return nil
		}
		// 有关联表时逐行配对；命中第一条匹配的关联行就够（和 PG 的非确定语义
		// 一致：只保证用的是一条匹配行）。
		for _, fr := range fromRows {
			combined := joinRow(row, fr)
			ok, err := matchRow(combined)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			refs = append(refs, ref)
			rows = append(rows, combined)
			return nil
		}
		return nil
	}
	// WHERE 能完整匹配某个索引时就不全表扫（多数 UPDATE 都是按主键改一行）。
	// 带 FROM 时不能走索引路径：WHERE 还引用了关联表的列。
	if len(s.From) == 0 {
		if irefs, irows, indexed := collectByIndex(tx, t, s.Where); indexed {
			for i := range irefs {
				if err := collect(irefs[i], irows[i]); err != nil {
					return nil, err
				}
			}
		} else if err := tx.Scan(t.Name, collect); err != nil {
			return nil, err
		}
	} else if err := tx.Scan(t.Name, collect); err != nil {
		return nil, err
	}

	for i, row := range rows {
		// row 可能是「目标行 + 关联表行」拼起来的，写回去的只有前 ncol 列。
		base := row[:ncol]
		newRow := make(types.Row, ncol)
		copy(newRow, base)
		for _, st := range sets {
			v, err := st.ev.Eval(row)
			if err != nil {
				return nil, err
			}
			cv, err := types.Cast(v, st.col.Type())
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", st.col.Name, err)
			}
			newRow[st.idx] = cv
		}
		for ci, col := range t.Columns {
			if col.NotNull && newRow[ci].IsNull() {
				return nil, fmt.Errorf("null value in column %q violates not-null constraint", col.Name)
			}
		}
		if err := evalChecks(checks, newRow); err != nil {
			return nil, err
		}
		if err := e.checkFKRow(tx, t, newRow); err != nil {
			return nil, err
		}
		self := refs[i]
		for _, g := range groups {
			vals, ok := groupValues(newRow, g)
			if !ok {
				continue
			}
			if groupUnchanged(base, newRow, g) {
				continue // 没动这一组，不必查重
			}
			dup, err := tx.FindUniqueGroup(t, g, vals, &self, -1)
			if err != nil {
				return nil, err
			}
			if dup {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", groupName(t, g))
			}
		}
		for _, ux := range uixs {
			vals, ok := groupValues(newRow, ux.cols)
			if !ok {
				continue
			}
			if groupUnchanged(base, newRow, ux.cols) {
				continue // 没动这一组，不必查重
			}
			dup := false
			for _, hit := range tx.IndexLookup(t, ux.name, ux.cols, storage.OrderKey(vals)) {
				if hit.Ref == self {
					continue
				}
				dup = true
				break
			}
			if dup {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", ux.name)
			}
		}
		if err := tx.Update(t.Name, refs[i], newRow); err != nil {
			return nil, err
		}
		if len(retEvals) > 0 {
			retRows = append(retRows, newRow)
		}
		affected++
	}
	res := &Result{Command: "UPDATE", Tag: fmt.Sprintf("UPDATE %d", affected), Affected: affected}
	if len(retEvals) > 0 {
		res.Columns = retCols
		if err := fillReturning(res, retEvals, retRows); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// fillReturning 按行求值 RETURNING 的每个输出项，填进 res 的结果集。
// cellValue 把一个值转成结果集里的单元格。
//
// date 列只留年月日：内部时间统一是 KTime，Interface() 会给出
// "2024-03-05 00:00:00"，而 PG 的 date 文本就是 "2024-03-05"。
// 协议层（pgwire.textValueAs）和 exec 的 ::text 转换都各挡了一次，
// 这里是控制台走的 JSON 那条路 —— 三条都盖住才不会从某个入口漏出来。
func cellValue(v types.Value, colType string) interface{} {
	if v.K == types.KTime && strings.HasPrefix(strings.ToLower(colType), "date") {
		s := v.Interface().(string)
		if len(s) >= 10 {
			return s[:10]
		}
	}
	return v.Interface()
}

func fillReturning(res *Result, evals []Eval, rows []types.Row) error {
	for _, r := range rows {
		out := make([]interface{}, len(evals))
		for i, ev := range evals {
			v, err := ev.Eval(r)
			if err != nil {
				return err
			}
			ct := ""
			if i < len(res.Columns) {
				ct = res.Columns[i].Type
			}
			out[i] = cellValue(v, ct)
		}
		res.Rows = append(res.Rows, out)
	}
	res.RowCount = len(res.Rows)
	return nil
}

func (e *Engine) execDelete(tx *storage.Tx, s *sql.Delete) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	sc := e.tableScope(t, s.Alias)
	// DELETE ... USING：把关联表整表读进来，scope 拼在目标表后面。
	var usingRows [][]types.Value
	if len(s.Using) > 0 {
		us, ur, err := e.relatedRows(tx, s.Using[0])
		if err != nil {
			return nil, err
		}
		usingRows = ur
		sc = append(append(Schema{}, sc...), us...)
	}
	var pred Eval
	if s.Where != nil {
		p, err := e.compileDML(tx, s.Where, sc)
		if err != nil {
			return nil, err
		}
		pred = p
	}

	// RETURNING 对 DELETE 返回的是被删掉的那一行。
	var retEvals []Eval
	var retCols []Column
	if len(s.Returning) > 0 {
		ev, cols, cerr := e.compileReturning(sc, s.Returning, tx)
		if cerr != nil {
			return nil, cerr
		}
		retEvals, retCols = ev, cols
	}

	var refs []storage.Ref
	var delRows []types.Row
	// matchUsing 判断目标行是否与 USING 表里的某一行共同满足 WHERE。
	// 命中任意一行即可（删不删是布尔结论，不需要挑具体是哪一行）。
	matchUsing := func(row types.Row) (bool, error) {
		if len(s.Using) == 0 {
			if pred == nil {
				return true, nil
			}
			v, err := pred.Eval(row)
			if err != nil {
				return false, err
			}
			b, isNull, err := asBool(v)
			if err != nil {
				return false, err
			}
			return !isNull && b, nil
		}
		for _, ur := range usingRows {
			combined := make(types.Row, 0, len(row)+len(ur))
			combined = append(combined, row...)
			combined = append(combined, ur...)
			if pred == nil {
				return true, nil
			}
			v, err := pred.Eval(combined)
			if err != nil {
				return false, err
			}
			b, isNull, err := asBool(v)
			if err != nil {
				return false, err
			}
			if !isNull && b {
				return true, nil
			}
		}
		return false, nil
	}
	collect := func(ref storage.Ref, row types.Row) error {
		ok, err := matchUsing(row)
		if err != nil || !ok {
			return err
		}
		refs = append(refs, ref)
		// Scan 可能复用缓冲，拷一份：外键检查与 RETURNING 都要用
		cp := make(types.Row, len(row))
		copy(cp, row)
		delRows = append(delRows, cp)
		return nil
	}
	// 带 USING 时不能走索引路径：WHERE 还引用了 USING 表的列。
	if s.Using == nil {
		if irefs, irows, indexed := collectByIndex(tx, t, s.Where); indexed {
			for i := range irefs {
				if err := collect(irefs[i], irows[i]); err != nil {
					return nil, err
				}
			}
			goto deleteRows
		}
	}
	if err := tx.Scan(t.Name, collect); err != nil {
		return nil, err
	}
deleteRows:
	for i, ref := range refs {
		// 先看有没有子表引用这行；cascade 的在这里一并删掉（同一事务）
		casc, err := e.checkFKDelete(tx, t, delRows[i])
		if err != nil {
			return nil, err
		}
		if err := e.applyCascades(tx, casc); err != nil {
			return nil, err
		}
		if err := tx.Delete(t.Name, ref); err != nil {
			return nil, err
		}
	}
	res := &Result{Command: "DELETE", Tag: fmt.Sprintf("DELETE %d", len(refs)), Affected: len(refs)}
	if len(retEvals) > 0 {
		res.Columns = retCols
		if err := fillReturning(res, retEvals, delRows); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ---------- CHECK 约束 ----------

// checkEval 是一条编译好的 CHECK 约束。
type checkEval struct {
	name  string
	table string
	ev    Eval
}

// compileChecks 把表上的 CHECK 约束编译成求值器。
// 每条语句编译一次，不要放进逐行循环里（要重新解析 SQL 文本）。
func (e *Engine) compileChecks(t *catalog.Table) ([]checkEval, error) {
	if len(t.Checks) == 0 {
		return nil, nil
	}
	sc := e.tableScope(t, "")
	out := make([]checkEval, 0, len(t.Checks))
	for i, src := range t.Checks {
		ex, err := sql.ParseExpr(src)
		if err != nil {
			return nil, fmt.Errorf("check constraint on %q is invalid: %w", t.Name, err)
		}
		ev, err := Compile(ex, sc)
		if err != nil {
			return nil, fmt.Errorf("check constraint on %q: %w", t.Name, err)
		}
		out = append(out, checkEval{name: checkName(t.Name, i, len(t.Checks)), table: t.Name, ev: ev})
	}
	return out, nil
}

// checkName 造一个和 PostgreSQL 风格接近的约束名。
func checkName(table string, i, n int) string {
	if n == 1 {
		return table + "_check"
	}
	return fmt.Sprintf("%s_check%d", table, i+1)
}

// evalChecks 按行求值所有 CHECK 约束，任一为 false 就报错。
// 结果为 NULL 视为通过（SQL 里 CHECK 只拦 false）。
func evalChecks(checks []checkEval, row types.Row) error {
	for _, ck := range checks {
		v, err := ck.ev.Eval(row)
		if err != nil {
			return err
		}
		b, isNull, err := asBool(v)
		if err != nil {
			return err
		}
		if isNull || b {
			continue
		}
		return fmt.Errorf("new row for relation %q violates check constraint %q", ck.table, ck.name)
	}
	return nil
}

// ---------- CREATE TABLE AS SELECT ----------

// execCreateTableAs 建表并灌数据。
//
// 列名/类型全部来自查询的输出 schema。建表是 DDL（不需要事务），灌数据自己
// 开一个事务：这样客户端拿到的语义是「表和数据一起出现」。
func (e *Engine) execCreateTableAs(s *sql.CreateTable) (*Result, error) {
	if _, exists := e.Cat.Get(s.Table); exists {
		if s.IfNotExists {
			return &Result{Command: "CREATE TABLE AS", Tag: "CREATE TABLE AS", Message: "表已存在，跳过"}, nil
		}
		return nil, fmt.Errorf("relation %q already exists", s.Table)
	}

	src := e.Store.Begin()
	defer src.Rollback()
	rows, sc, err := e.selectRows(src, s.As)
	if err != nil {
		return nil, err
	}

	used := map[string]int{}
	cols := make([]*catalog.Column, len(sc))
	for i, c := range sc {
		name := strings.ToLower(c.Name)
		if name == "" || name == "?column?" {
			name = fmt.Sprintf("column%d", i+1)
		}
		// SELECT 允许重名列，表不行：自动加序号
		if n, dup := used[name]; dup {
			used[name] = n + 1
			name = fmt.Sprintf("%s%d", name, n+1)
		} else {
			used[name] = 1
		}
		cols[i] = catalog.NewColumn(name, c.Type)
	}

	t := &catalog.Table{Name: s.Table, Columns: cols}
	if err := e.Cat.Add(t); err != nil {
		return nil, err
	}
	e.Store.AddTable(t.Name)
	if err := e.persistSchema(false); err != nil {
		return nil, err
	}

	if len(rows) > 0 {
		tx := e.Store.Begin()
		for _, r := range rows {
			if _, err := tx.Insert(t.Name, r); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &Result{
		Command:  "SELECT",
		Tag:      fmt.Sprintf("SELECT %d", len(rows)),
		Affected: len(rows),
	}, nil
}

// ---------- 二级索引（登记式） ----------

// execCreateIndex 建一条索引。
//
// 只登记元数据：客户端能在索引页看到、能建能删，系统目录里也会出现对应行；
// 但查询规划还没接入它（只有主键走哈希索引）。结果里会明确说明这一点，
// 避免让人以为加了索引就变快了。
// execCommentOn 实现 COMMENT ON：表/列注释写进 catalog（并落到 pg_description），
// 其它对象（索引、schema、库、视图、约束）接受即忽略。
//
// ORM 的 CodeFirst 建完表会连着发一串 COMMENT ON，任何一条报错都会被当成
// 「建表失败」；而注释本身对查询没有任何影响，所以这里宁宽不严。
func (e *Engine) execCommentOn(s *sql.CommentOn) (*Result, error) {
	text := s.Comment
	if s.IsNull {
		text = ""
	}
	switch s.Object {
	case "table":
		t, ok := e.Cat.Get(s.Table)
		if !ok {
			return nil, fmt.Errorf("relation %q does not exist", s.Table)
		}
		t.Comment = text
	case "column":
		t, ok := e.Cat.Get(s.Table)
		if !ok {
			return nil, fmt.Errorf("relation %q does not exist", s.Table)
		}
		col, _, ok := t.Col(s.Column)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", s.Column, t.Name)
		}
		col.Comment = text
	default:
		// 索引 / schema / 库 / 视图 / 约束：没有对应行为，直接确认。
		return &Result{Command: "COMMENT", Tag: "COMMENT"}, nil
	}
	if err := e.persistSchema(false); err != nil {
		return nil, err
	}
	return &Result{Command: "COMMENT", Tag: "COMMENT"}, nil
}

func (e *Engine) execCreateIndex(s *sql.CreateIndex) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	for _, idx := range e.Cat.Tables {
		for _, ix := range idx.Indexes {
			if strings.EqualFold(ix.Name, s.Name) {
				return nil, fmt.Errorf("relation %q already exists", s.Name)
			}
		}
	}
	if len(s.Cols) == 0 {
		return nil, fmt.Errorf("CREATE INDEX requires at least one column")
	}
	cols := make([]string, len(s.Cols))
	for i, name := range s.Cols {
		if _, ok := t.ColumnIdx(name); !ok {
			return nil, fmt.Errorf("column %q does not exist", name)
		}
		cols[i] = strings.ToLower(name)
	}
	// 和约束隐式建的索引重名会让客户端的索引页出现两条同名索引，提前拦掉。
	for _, ix := range syscat.ConstraintIndexNames(t) {
		if strings.EqualFold(ix, s.Name) {
			return nil, fmt.Errorf("relation %q already exists", s.Name)
		}
	}
	if s.Unique {
		if err := e.checkUniqueIndexData(t, cols, s.Name); err != nil {
			return nil, err
		}
	}
	t.Indexes = append(t.Indexes, &catalog.Index{Name: s.Name, Columns: cols, Unique: s.Unique})
	if err := e.persistSchema(false); err != nil {
		return nil, err
	}
	// 已有数据得先灌进新索引，否则索引建完只能查到以后写入的行。
	if e.Store != nil {
		e.Store.RebuildIndex(t.Name)
	}
	msg := "索引已建立：等值查询会走它"
	if s.Unique {
		msg += "，并参与唯一性校验"
	}
	return &Result{Command: "CREATE INDEX", Tag: "CREATE INDEX", Message: msg}, nil
}

// checkUniqueIndexData 在建立唯一索引前检查存量数据。
//
// PostgreSQL 在这种情况下直接拒绝建索引；放过去的话索引里某一列会有多行，
// 之后每次查询都要多一步去重，而且和「唯一」的语义自相矛盾。
func (e *Engine) checkUniqueIndexData(t *catalog.Table, cols []string, name string) error {
	if e.Store == nil {
		return nil
	}
	td := e.Store.Table(t.Name)
	if td == nil {
		return nil
	}
	pos := make([]int, 0, len(cols))
	for _, cn := range cols {
		ci, ok := t.ColumnIdx(cn)
		if !ok {
			return fmt.Errorf("column %q does not exist", cn)
		}
		pos = append(pos, ci)
	}
	seen := make(map[string]bool, len(td.Rows))
	for _, row := range td.Rows {
		vals, ok := groupValues(row, pos)
		if !ok {
			continue // 含 NULL 的行不参与唯一性判断
		}
		k := types.Key(vals)
		if seen[k] {
			return fmt.Errorf("could not create unique index %q: duplicate key value in (%s)", name, strings.Join(cols, ", "))
		}
		seen[k] = true
	}
	return nil
}

// execDropIndex 删除一条索引（按名字找，索引名全库唯一）。
func (e *Engine) execDropIndex(s *sql.DropIndex) (*Result, error) {
	for _, t := range e.Cat.Tables {
		for i, ix := range t.Indexes {
			if strings.EqualFold(ix.Name, s.Name) {
				t.Indexes = append(t.Indexes[:i], t.Indexes[i+1:]...)
				if err := e.persistSchema(false); err != nil {
					return nil, err
				}
				// 重建一次，把这张索引在内存里的哈希表一起清掉
				if e.Store != nil {
					e.Store.RebuildIndex(t.Name)
				}
				return &Result{Command: "DROP INDEX", Tag: "DROP INDEX"}, nil
			}
		}
	}
	if s.IfExists {
		return &Result{Command: "DROP INDEX", Tag: "DROP INDEX", Message: "索引不存在，跳过"}, nil
	}
	return nil, fmt.Errorf("index %q does not exist", s.Name)
}
