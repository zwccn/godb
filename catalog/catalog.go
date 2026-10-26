// Package catalog 保存库表结构（元数据）。
package catalog

import (
	"fmt"
	"strings"

	"godb/types"
)

// Column 是一列的定义（可 JSON 持久化）。
type Column struct {
	Name       string `json:"name"`
	TypeName   string `json:"type"`
	Kind       int    `json:"kind"`
	Len        int    `json:"len,omitempty"`
	Prec       int    `json:"prec,omitempty"`
	Scale      int    `json:"scale,omitempty"`
	NotNull    bool   `json:"not_null,omitempty"`
	Unique     bool   `json:"unique,omitempty"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
	Serial     bool   `json:"serial,omitempty"`
	Default    string `json:"default,omitempty"`
}

// NewColumn 由类型与约束构造列定义。
func NewColumn(name string, t types.Type) *Column {
	return &Column{
		Name:     name,
		TypeName: t.String(),
		Kind:     int(t.Kind),
		Len:      t.Len,
		Prec:     t.Prec,
		Scale:    t.Scale,
	}
}

// Type 返回运行时类型。
func (c *Column) Type() types.Type {
	return types.Type{Kind: types.Kind(c.Kind), Name: c.TypeName, Len: c.Len, Prec: c.Prec, Scale: c.Scale}
}

// Table 是一张表的定义。
type Table struct {
	Name    string    `json:"name"`
	Columns []*Column `json:"columns"`

	colIdx map[string]int
}

// Rebuild 重建内部索引，加载/变更后必须调用。
func (t *Table) Rebuild() {
	t.colIdx = make(map[string]int, len(t.Columns))
	for i, c := range t.Columns {
		t.colIdx[strings.ToLower(c.Name)] = i
	}
}

// ColumnIdx 查找列位置。
func (t *Table) ColumnIdx(name string) (int, bool) {
	if t.colIdx == nil {
		t.Rebuild()
	}
	i, ok := t.colIdx[strings.ToLower(name)]
	return i, ok
}

// Col 按名字取列。
func (t *Table) Col(name string) (*Column, int, bool) {
	i, ok := t.ColumnIdx(name)
	if !ok {
		return nil, -1, false
	}
	return t.Columns[i], i, true
}

// PKIdx 返回主键列位置，-1 表示无主键。
func (t *Table) PKIdx() int {
	for i, c := range t.Columns {
		if c.PrimaryKey {
			return i
		}
	}
	return -1
}

// SerialIdx 返回第一个自增列位置，-1 表示无。
func (t *Table) SerialIdx() int {
	for i, c := range t.Columns {
		if c.Serial {
			return i
		}
	}
	return -1
}

// UniqueCols 返回需要唯一性校验的列（含主键）。
func (t *Table) UniqueCols() []int {
	var out []int
	for i, c := range t.Columns {
		if c.Unique || c.PrimaryKey {
			out = append(out, i)
		}
	}
	return out
}

// Validate 校验表定义是否自洽。
func (t *Table) Validate() error {
	if len(t.Columns) == 0 {
		return fmt.Errorf("table %q must have at least one column", t.Name)
	}
	seen := map[string]bool{}
	pks := 0
	for _, c := range t.Columns {
		l := strings.ToLower(c.Name)
		if seen[l] {
			return fmt.Errorf("column %q specified more than once", c.Name)
		}
		seen[l] = true
		if c.PrimaryKey {
			pks++
		}
	}
	if pks > 1 {
		return fmt.Errorf("multiple primary keys for table %q are not allowed", t.Name)
	}
	t.Rebuild()
	return nil
}

// Catalog 是整个库的表集合。
type Catalog struct {
	Tables []*Table `json:"tables"`

	byName map[string]*Table
}

// New 创建空 Catalog。
func New() *Catalog {
	c := &Catalog{}
	c.Rebuild()
	return c
}

// Rebuild 重建表名索引。
func (c *Catalog) Rebuild() {
	c.byName = make(map[string]*Table, len(c.Tables))
	for _, t := range c.Tables {
		t.Rebuild()
		c.byName[strings.ToLower(t.Name)] = t
	}
}

// Get 按名字取表。
func (c *Catalog) Get(name string) (*Table, bool) {
	if c.byName == nil {
		c.Rebuild()
	}
	t, ok := c.byName[strings.ToLower(name)]
	return t, ok
}

// Names 返回所有表名（按创建顺序）。
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.Tables))
	for _, t := range c.Tables {
		out = append(out, t.Name)
	}
	return out
}

// Add 新增表。
func (c *Catalog) Add(t *Table) error {
	if _, ok := c.Get(t.Name); ok {
		return fmt.Errorf("relation %q already exists", t.Name)
	}
	if err := t.Validate(); err != nil {
		return err
	}
	c.Tables = append(c.Tables, t)
	c.Rebuild()
	return nil
}

// Drop 删除表。
func (c *Catalog) Drop(name string) error {
	t, ok := c.Get(name)
	if !ok {
		return fmt.Errorf("relation %q does not exist", name)
	}
	for i, x := range c.Tables {
		if x == t {
			c.Tables = append(c.Tables[:i], c.Tables[i+1:]...)
			break
		}
	}
	c.Rebuild()
	return nil
}

// Rename 重命名表。
func (c *Catalog) Rename(oldName, newName string) error {
	t, ok := c.Get(oldName)
	if !ok {
		return fmt.Errorf("relation %q does not exist", oldName)
	}
	if _, exists := c.Get(newName); exists {
		return fmt.Errorf("relation %q already exists", newName)
	}
	t.Name = newName
	c.Rebuild()
	return nil
}
