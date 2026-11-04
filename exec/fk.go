package exec

import (
	"fmt"
	"strings"

	"godb/catalog"
	"godb/sql"
	"godb/storage"
	"godb/types"
)

// ---------- 外键 ----------

// buildForeignKeys 把 AST 里的外键定义整理成 catalog 里的形式，并做基本校验。
//
// 列级 REFERENCES 与表级 FOREIGN KEY 最终是同一个结构；没写引用列时默认
// 引用父表主键（PostgreSQL 的默认行为），并要求引用列上有唯一约束。
func (e *Engine) buildForeignKeys(t *catalog.Table, s *sql.CreateTable) error {
	for _, d := range s.Cols {
		if d.RefTable == "" {
			continue
		}
		fk := &catalog.ForeignKey{
			Name:     fmt.Sprintf("%s_%s_fkey", t.Name, d.Name),
			Columns:  []string{d.Name},
			RefTable: d.RefTable,
			OnDelete: d.RefOnDel,
		}
		if d.RefCol != "" {
			fk.RefColumns = []string{d.RefCol}
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	}
	for i, f := range s.FKs {
		name := fmt.Sprintf("%s_fkey%d", t.Name, i+1)
		if len(s.FKs) == 1 {
			name = t.Name + "_fkey"
		}
		t.ForeignKeys = append(t.ForeignKeys, &catalog.ForeignKey{
			Name:       name,
			Columns:    f.Cols,
			RefTable:   f.RefTable,
			RefColumns: f.RefCols,
			OnDelete:   f.OnDelete,
			OnUpdate:   f.OnUpdate,
		})
	}

	for _, fk := range t.ForeignKeys {
		if err := e.validateFK(t, fk); err != nil {
			return err
		}
	}
	return nil
}

// validateFK 校验一条外键定义：引用表存在、两边列都在、引用列上有唯一约束，
// 而且两边的列类型能按同一个键比较。
//
// 建表与 ALTER TABLE ADD CONSTRAINT 都走这里 —— 约束的合法性只有一套标准，
// 两条路径各写一遍，迟早出现「建表能过、补加不能过」这种说不清的区别。
func (e *Engine) validateFK(t *catalog.Table, fk *catalog.ForeignKey) error {
	parent, ok := e.Cat.Get(fk.RefTable)
	if !ok {
		return fmt.Errorf("referenced relation %q does not exist", fk.RefTable)
	}
	if len(fk.RefColumns) == 0 {
		pk := parent.PKIdxes()
		if len(pk) == 0 {
			return fmt.Errorf("there is no primary key for referenced table %q", parent.Name)
		}
		for _, ci := range pk {
			fk.RefColumns = append(fk.RefColumns, parent.Columns[ci].Name)
		}
	}
	if len(fk.RefColumns) != len(fk.Columns) {
		return fmt.Errorf("number of referencing and referenced columns for foreign key %q disagree", fk.Name)
	}
	childIdx, err := fkColIndexes(t, fk.Columns)
	if err != nil {
		return err
	}
	parentIdx, err := fkColIndexes(parent, fk.RefColumns)
	if err != nil {
		return err
	}
	// 类型不同的话，两边值编码出来的键永远不相等 —— 约束建得成功，写数据时
	// 每行都报「没有父行」，用户完全看不出是类型不对。建约束时就说清楚。
	for i := range childIdx {
		c, p := t.Columns[childIdx[i]].Type(), parent.Columns[parentIdx[i]].Type()
		if !fkTypesMatch(c, p) {
			return fmt.Errorf("foreign key constraint %q cannot be implemented: column %q of %q is %s, "+
				"but referenced column %q of %q is %s",
				fk.Name, t.Columns[childIdx[i]].Name, t.Name, c.String(),
				parent.Columns[parentIdx[i]].Name, parent.Name, p.String())
		}
	}
	// PostgreSQL 要求被引用的列上有唯一约束，否则无法保证「至多一行」
	if !refColsUnique(parent, fk.RefColumns) {
		return fmt.Errorf("there is no unique constraint matching given keys for referenced table %q", parent.Name)
	}
	return nil
}

// fkFromDef 把 ALTER TABLE ADD CONSTRAINT 的定义整理成 catalog 里的形式。
//
// 没给约束名时按 PG 的习惯自动取（表_列_fkey），重名加序号：约束名在
// information_schema / pg_constraint 里是要唯一的那一列，两条同名约束会让
// 客户端的约束列表出现一模一样的两行，删哪条都说不清。
func (e *Engine) fkFromDef(t *catalog.Table, cname string, d *sql.FKDef) (*catalog.ForeignKey, error) {
	fk := &catalog.ForeignKey{
		Name:       cname,
		Columns:    d.Cols,
		RefTable:   d.RefTable,
		RefColumns: d.RefCols,
		OnDelete:   d.OnDelete,
		OnUpdate:   d.OnUpdate,
	}
	if fk.Name == "" {
		base := t.Name + "_" + strings.Join(d.Cols, "_") + "_fkey"
		fk.Name = base
		for n := 2; e.fkNameTaken(t, fk.Name); n++ {
			fk.Name = fmt.Sprintf("%s%d", base, n)
		}
	} else if e.fkNameTaken(t, fk.Name) {
		return nil, fmt.Errorf("constraint %q for relation %q already exists", fk.Name, t.Name)
	}
	if err := e.validateFK(t, fk); err != nil {
		return nil, err
	}
	return fk, nil
}

// fkNameTaken 判断表上是否已有同名外键（大小写不敏感，和列名规则一致）。
func (e *Engine) fkNameTaken(t *catalog.Table, name string) bool {
	for _, fk := range t.ForeignKeys {
		if strings.EqualFold(fk.Name, name) {
			return true
		}
	}
	return false
}

// checkFKExistingRows 用已有数据验证一条新加的外键：加约束前必须确认没有孤儿行。
//
// 做法是「父表键先扫进 map，再逐行查」，不是逐行扫父表 —— 后者在 10 万行上
// 就是平方级，加个约束能卡到人以为服务挂了。父表的 NULL 键按 SQL 语义跳过
// （NULL 不等于任何值，包括 NULL）。
func (e *Engine) checkFKExistingRows(t *catalog.Table, fk *catalog.ForeignKey) error {
	parent, ok := e.Cat.Get(fk.RefTable)
	if !ok {
		return fmt.Errorf("referenced relation %q does not exist", fk.RefTable)
	}
	childIdx, err := fkColIndexes(t, fk.Columns)
	if err != nil {
		return err
	}
	parentIdx, err := fkColIndexes(parent, fk.RefColumns)
	if err != nil {
		return err
	}
	keys := make(map[string]struct{})
	pt := e.Store.Table(parent.Name)
	for i := 0; i < pt.SlotCount(); i++ {
		row, ok := pt.RowAt(i)
		if !ok {
			continue
		}
		vals, hasNull := fkPick(row, parentIdx)
		if hasNull {
			continue
		}
		k, err := types.Key(vals)
		if err != nil {
			return err
		}
		keys[k] = struct{}{}
	}
	ct := e.Store.Table(t.Name)
	for i := 0; i < ct.SlotCount(); i++ {
		row, ok := ct.RowAt(i)
		if !ok {
			continue
		}
		vals, hasNull := fkPick(row, childIdx)
		if hasNull {
			continue
		}
		k, err := types.Key(vals)
		if err != nil {
			return err
		}
		if _, found := keys[k]; !found {
			return fmt.Errorf("existing row in %q violates foreign key constraint %q: no matching row in %q "+
				"(the column value may be 0 or an old id); fix or clear those rows first",
				t.Name, fk.Name, parent.Name)
		}
	}
	return nil
}

// fkPick 按列位置取一组值；越界或含 NULL 返回 hasNull=true。
func fkPick(row types.Row, cols []int) ([]types.Value, bool) {
	vals := make([]types.Value, len(cols))
	for i, ci := range cols {
		if ci >= len(row) {
			return nil, true
		}
		vals[i] = row[ci]
		if vals[i].IsNull() {
			return nil, true
		}
	}
	return vals, false
}

// fkTypesMatch 判断外键两边能不能按同一个键编码比较。
//
// 要求 Kind 相同：types.Key 会把 Kind 编进键里（int 写成 "a123"，文本写成
// "t3:abc"），Kind 不同就是「同一个值的两种写法」，键必然不相等。int2/4/8
// 的 Kind 都是 KInt，所以它们之间互相引用仍然允许（PG 也允许）。向量没有
// 可比较的键（存的只是槽位号），一律拒绝。
func fkTypesMatch(a, b types.Type) bool {
	if a.Kind == types.KVector || b.Kind == types.KVector {
		return false
	}
	return a.Kind == b.Kind
}

// refColsUnique 判断这组列上是否有唯一约束（复合主键算一组）。
func refColsUnique(parent *catalog.Table, cols []string) bool {
	idx, err := fkColIndexes(parent, cols)
	if err != nil {
		return false
	}
	for _, grp := range parent.UniqueGroups() {
		if sameIdx(idx, grp) {
			return true
		}
	}
	return false
}

// fkColIndexes 把列名翻译成列位置。
func fkColIndexes(t *catalog.Table, cols []string) ([]int, error) {
	out := make([]int, len(cols))
	for i, name := range cols {
		idx, ok := t.ColumnIdx(name)
		if !ok {
			return nil, fmt.Errorf("column %q of relation %q does not exist", name, t.Name)
		}
		out[i] = idx
	}
	return out, nil
}

// checkFKRow 校验一行数据在它的外键上都有父行（NULL 组合跳过，SQL 语义）。
func (e *Engine) checkFKRow(tx *storage.Tx, t *catalog.Table, row types.Row) error {
	for _, fk := range t.ForeignKeys {
		parent, ok := e.Cat.Get(fk.RefTable)
		if !ok {
			return fmt.Errorf("referenced relation %q does not exist", fk.RefTable)
		}
		childIdx, err := fkColIndexes(t, fk.Columns)
		if err != nil {
			return err
		}
		parentIdx, err := fkColIndexes(parent, fk.RefColumns)
		if err != nil {
			return err
		}
		vals := make([]types.Value, len(childIdx))
		hasNull := false
		for i, ci := range childIdx {
			if ci >= len(row) {
				hasNull = true
				break
			}
			vals[i] = row[ci]
			if vals[i].IsNull() {
				hasNull = true
			}
		}
		if hasNull {
			continue
		}
		found, err := fkParentHas(tx, parent, parentIdx, vals)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("insert or update on table %q violates foreign key constraint %q",
				t.Name, fk.Name)
		}
	}
	return nil
}

// fkParentHas 在父表里找一组列值都相等的行。
func fkParentHas(tx *storage.Tx, parent *catalog.Table, cols []int, vals []types.Value) (bool, error) {
	// 不用哈希索引：索引只在写入/加载后的重建点维护，
	// 外键检查必须看到「当前可见」的数据，扫描最稳。
	_ = parent.PKIdxes()
	found := false
	err := tx.Scan(parent.Name, func(ref storage.Ref, row types.Row) error {
		if fkRowMatches(row, cols, vals) {
			found = true
		}
		return nil
	})
	return found, err
}

// fkRowMatches 判断行在这组列上是否等于给定值。
func fkRowMatches(row types.Row, cols []int, vals []types.Value) bool {
	for i, ci := range cols {
		if ci >= len(row) || !types.Same(row[ci], vals[i]) {
			return false
		}
	}
	return true
}

// checkFKDelete 在删除父表某行前检查子表引用。
//
// cascade 的按级联删除处理；其余（含默认 NO ACTION）报错。
// 返回需要级联删除的子行，由调用方执行删除（保证在一个事务里）。
func (e *Engine) checkFKDelete(tx *storage.Tx, parent *catalog.Table, row types.Row) ([]fkCascade, error) {
	var cascades []fkCascade
	for _, child := range e.Cat.Tables {
		for _, fk := range child.ForeignKeys {
			if !strings.EqualFold(fk.RefTable, parent.Name) {
				continue
			}
			parentIdx, err := fkColIndexes(parent, fk.RefColumns)
			if err != nil {
				return nil, err
			}
			childIdx, err := fkColIndexes(child, fk.Columns)
			if err != nil {
				return nil, err
			}
			vals := make([]types.Value, len(parentIdx))
			hasNull := false
			for i, ci := range parentIdx {
				if ci >= len(row) {
					hasNull = true
					break
				}
				vals[i] = row[ci]
				if vals[i].IsNull() {
					hasNull = true
				}
			}
			if hasNull {
				continue
			}
			var hit []storage.Ref
			var hitRows []types.Row
			err = tx.Scan(child.Name, func(ref storage.Ref, r types.Row) error {
				if fkRowMatches(r, childIdx, vals) {
					cp := make(types.Row, len(r))
					copy(cp, r)
					hit = append(hit, ref)
					hitRows = append(hitRows, cp)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if len(hit) == 0 {
				continue
			}
			if fk.OnDelete == "cascade" {
				cascades = append(cascades, fkCascade{table: child, refs: hit, rows: hitRows})
				continue
			}
			return nil, fmt.Errorf("update or delete on table %q violates foreign key constraint %q on table %q",
				parent.Name, fk.Name, child.Name)
		}
	}
	return cascades, nil
}

// fkCascade 记录一批待级联删除的子行。
type fkCascade struct {
	table *catalog.Table
	refs  []storage.Ref
	rows  []types.Row
}

// applyCascades 执行级联删除（含孙表，递归）。
func (e *Engine) applyCascades(tx *storage.Tx, list []fkCascade) error {
	for _, c := range list {
		for i, ref := range c.refs {
			// 先处理孙表
			sub, err := e.checkFKDelete(tx, c.table, c.rows[i])
			if err != nil {
				return err
			}
			if err := e.applyCascades(tx, sub); err != nil {
				return err
			}
			if err := tx.Delete(c.table.Name, ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDropFK 检查是否有别的表引用了要删的表。
func (e *Engine) checkDropFK(name string) error {
	for _, t := range e.Cat.Tables {
		for _, fk := range t.ForeignKeys {
			if strings.EqualFold(fk.RefTable, name) {
				return fmt.Errorf("cannot drop table %q because other objects depend on it (constraint %q on table %q)",
					name, fk.Name, t.Name)
			}
		}
	}
	return nil
}

// sameIdx 判断两个列位置列表是否相同（顺序敏感）。
func sameIdx(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
