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

func (e *Engine) persistSchema() error {
	if err := e.Cat.Save(e.Dir); err != nil {
		return err
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
	if err := e.Cat.Add(t); err != nil {
		return nil, err
	}
	e.Store.AddTable(t.Name)
	if err := e.persistSchema(); err != nil {
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
	if err := e.persistSchema(); err != nil {
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
		t.Columns[idx] = catalog.NewColumn(t.Columns[idx].Name, newType)
		t.Rebuild()
	default:
		return nil, fmt.Errorf("unsupported ALTER TABLE action")
	}
	if err := e.persistSchema(); err != nil {
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

	// 语句内已出现过的唯一键（避免 O(n²) 的 overlay 扫描）
	seen := map[int]map[string]bool{}
	baseIns := tx.OverlayInsLen(t.Name)

	for _, exprs := range s.Rows {
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
	}
	return &Result{
		Command:  "INSERT",
		Tag:      fmt.Sprintf("INSERT 0 %d", len(s.Rows)),
		Affected: len(s.Rows),
	}, nil
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
		affected++
	}
	return &Result{Command: "UPDATE", Tag: fmt.Sprintf("UPDATE %d", affected), Affected: affected}, nil
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

	var refs []storage.Ref
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
		return nil
	}); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if err := tx.Delete(t.Name, ref); err != nil {
			return nil, err
		}
	}
	return &Result{Command: "DELETE", Tag: fmt.Sprintf("DELETE %d", len(refs)), Affected: len(refs)}, nil
}
