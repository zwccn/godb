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
		if _, err := fkColIndexes(t, fk.Columns); err != nil {
			return err
		}
		if _, err := fkColIndexes(parent, fk.RefColumns); err != nil {
			return err
		}
		// PostgreSQL 要求被引用的列上有唯一约束，否则无法保证「至多一行」
		if !refColsUnique(parent, fk.RefColumns) {
			return fmt.Errorf("there is no unique constraint matching given keys for referenced table %q", parent.Name)
		}
	}
	return nil
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
