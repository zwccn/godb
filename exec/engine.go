package exec

import (
	"fmt"
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
}

// ExecStmt 执行一条非事务控制语句。
func (e *Engine) ExecStmt(tx *storage.Tx, stmt sql.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *sql.Select:
		return e.execSelect(tx, s, nil)
	case *sql.Explain:
		sel, ok := s.Stmt.(*sql.Select)
		if !ok {
			return nil, fmt.Errorf("EXPLAIN only supports SELECT")
		}
		return e.execSelect(tx, sel, []string{})
	case *sql.Insert:
		return e.execInsert(tx, s)
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

func (e *Engine) execSelect(tx *storage.Tx, s *sql.Select, explainTag []string) (*Result, error) {
	plan, err := (&Planner{Tx: tx, Cat: e.Cat}).BuildSelect(s)
	if err != nil {
		return nil, err
	}
	planText := ExplainPlan(plan)

	if explainTag != nil {
		rows := make([][]interface{}, 0, len(planText))
		for _, line := range planText {
			rows = append(rows, []interface{}{line})
		}
		return &Result{
			Command:  "EXPLAIN",
			Tag:      "EXPLAIN",
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
		cols[i] = Column{Name: c.Name, Type: c.Type.String()}
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
			out[i] = v.Interface()
		}
		rows = append(rows, out)
	}
	return &Result{
		Command:  "SELECT",
		Tag:      fmt.Sprintf("SELECT %d", len(rows)),
		Columns:  cols,
		Rows:     rows,
		RowCount: len(rows),
		Plan:     planText,
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
			c.Unique = true
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
	act := s.Action
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
	// DROP COLUMN / RENAME TABLE / ALTER TYPE 必须重写快照，见 persistSchema 的说明
	needsSnapshot := act.Kind == "drop_column" || act.Kind == "rename_table" || act.Kind == "alter_type"
	if err := e.persistSchema(needsSnapshot); err != nil {
		return nil, err
	}
	return &Result{Command: "ALTER TABLE", Tag: "ALTER TABLE"}, nil
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

	uniq := t.UniqueCols()
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
	seen := map[int]map[string]bool{}
	baseIns := tx.OverlayInsLen(t.Name)

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
			v, err := CompileConst(expr)
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
		for _, ci := range uniq {
			if row[ci].IsNull() {
				continue
			}
			k := types.Key([]types.Value{row[ci]})
			cm := seen[ci]
			if cm == nil {
				cm = map[string]bool{}
				seen[ci] = cm
			}
			if cm[k] {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", constraintName(t.Name, ci))
			}
			cm[k] = true
			// 只扫本语句之前插入的行；本语句内重复已由 seen 拦住
			dup, err := tx.FindUnique(t, ci, row[ci], nil, baseIns)
			if err != nil {
				return nil, err
			}
			if dup {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", constraintName(t.Name, ci))
			}
		}
		if _, err := tx.Insert(t.Name, row); err != nil {
			return nil, err
		}
		if len(retEvals) > 0 {
			inserted = append(inserted, row)
		}
	}
	res := &Result{
		Command:  "INSERT",
		Tag:      fmt.Sprintf("INSERT 0 %d", len(rows)),
		Affected: len(rows),
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
				out[i] = v.Interface()
			}
			res.Rows = append(res.Rows, out)
		}
		res.RowCount = len(res.Rows)
	}
	return res, nil
}

// selectRows 跑一条查询，返回原始值行（不受客户端格式影响）。
// INSERT ... SELECT 用它取数据源。
func (e *Engine) selectRows(tx *storage.Tx, s *sql.Select) ([][]types.Value, Schema, error) {
	plan, err := (&Planner{Tx: tx, Cat: e.Cat}).BuildSelect(s)
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
	ctx := &compileCtx{sc: sc, planner: &Planner{Tx: tx, Cat: e.Cat}}
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

func constraintName(table string, colIdx int) string {
	return fmt.Sprintf("%s_col%d_key", table, colIdx)
}

// ---------- UPDATE / DELETE ----------

func (e *Engine) tableScope(t *catalog.Table, alias string) Schema {
	qual := alias
	if qual == "" {
		qual = t.Name
	}
	sc := make(Schema, len(t.Columns))
	for i, c := range t.Columns {
		sc[i] = ColInfo{Qualifier: strings.ToLower(qual), Name: strings.ToLower(c.Name), Type: c.Type()}
	}
	return sc
}

func (e *Engine) execUpdate(tx *storage.Tx, s *sql.Update) (*Result, error) {
	t, ok := e.Cat.Get(s.Table)
	if !ok {
		if _, sys := syscat.Lookup("", s.Table); sys {
			return nil, fmt.Errorf("permission denied: %q is a system catalog", s.Table)
		}
		return nil, fmt.Errorf("relation %q does not exist", s.Table)
	}
	sc := e.tableScope(t, s.Alias)

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
		ev, err := Compile(a.Value, sc)
		if err != nil {
			return nil, err
		}
		sets = append(sets, setItem{idx: idx, ev: ev, col: t.Columns[idx]})
	}

	var pred Eval
	if s.Where != nil {
		p, err := Compile(s.Where, sc)
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

	uniq := t.UniqueCols()
	affected := 0
	var refs []storage.Ref
	var rows []types.Row
	if err := tx.Scan(t.Name, func(ref storage.Ref, row types.Row) error {
		if pred != nil {
			v, err := pred.Eval(row)
			if err != nil {
				return err
			}
			b, isNull, err := asBool(v)
			if err != nil {
				return err
			}
			if isNull || !b {
				return nil
			}
		}
		refs = append(refs, ref)
		rows = append(rows, row)
		return nil
	}); err != nil {
		return nil, err
	}

	for i, row := range rows {
		newRow := make(types.Row, len(row))
		copy(newRow, row)
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
		self := refs[i]
		for _, ci := range uniq {
			if types.Same(newRow[ci], row[ci]) {
				continue
			}
			dup, err := tx.FindUnique(t, ci, newRow[ci], &self, -1)
			if err != nil {
				return nil, err
			}
			if dup {
				return nil, fmt.Errorf("duplicate key value violates unique constraint %q", constraintName(t.Name, ci))
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
func fillReturning(res *Result, evals []Eval, rows []types.Row) error {
	for _, r := range rows {
		out := make([]interface{}, len(evals))
		for i, ev := range evals {
			v, err := ev.Eval(r)
			if err != nil {
				return err
			}
			out[i] = v.Interface()
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
	var pred Eval
	if s.Where != nil {
		p, err := Compile(s.Where, sc)
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
	if err := tx.Scan(t.Name, func(ref storage.Ref, row types.Row) error {
		if pred != nil {
			v, err := pred.Eval(row)
			if err != nil {
				return err
			}
			b, isNull, err := asBool(v)
			if err != nil {
				return err
			}
			if isNull || !b {
				return nil
			}
		}
		refs = append(refs, ref)
		if len(retEvals) > 0 {
			// Scan 可能复用缓冲，拷一份留给 RETURNING
			cp := make(types.Row, len(row))
			copy(cp, row)
			delRows = append(delRows, cp)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, ref := range refs {
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
