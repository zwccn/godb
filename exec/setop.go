package exec

import (
	"fmt"
	"regexp"
	"strings"

	"godb/sql"
	"godb/types"
)

// ---------- 集合运算 ----------

// setOp 是两个查询的 UNION / INTERSECT / EXCEPT。
//
// 左操作数的列名与类型决定结果的 schema（和 PostgreSQL 一致）。
// 两侧行数必须一致，否则报错 —— 和 PG 一样，不做隐式补齐。
type setOp struct {
	left, right Operator
	op          string
	all         bool
	sc          Schema

	phase int // 0=左边 1=右边 2=结束
	seen  map[string]bool
	buf   []types.Row
	i     int
}

func (o *setOp) Schema() Schema { return o.sc }

func (o *setOp) Open() error {
	if len(o.sc) == 0 {
		o.sc = o.left.Schema()
	}
	if len(o.left.Schema()) != len(o.right.Schema()) {
		return fmt.Errorf("%s: each query must have the same number of columns", strings.ToUpper(o.op))
	}
	o.i = 0
	o.buf = o.buf[:0]
	o.seen = map[string]bool{}
	if err := o.materialize(); err != nil {
		return err
	}
	return nil
}

// materialize 把两个操作数的行读出来算好，放进 buf。
//
// 行数不大（这类集合运算几乎只出现在常量表、系统目录查询里），
// 物化比流式拼装简单得多，也便于去重。
func (o *setOp) materialize() error {
	leftRows, err := drain(o.left)
	if err != nil {
		return err
	}
	rightRows, err := drain(o.right)
	if err != nil {
		return err
	}
	switch o.op {
	case "union":
		for _, r := range append(leftRows, rightRows...) {
			if !o.all {
				k := types.Key(r)
				if o.seen[k] {
					continue
				}
				o.seen[k] = true
			}
			o.buf = append(o.buf, r)
		}
	case "intersect":
		right := map[string]bool{}
		for _, r := range rightRows {
			right[types.Key(r)] = true
		}
		for _, r := range leftRows {
			k := types.Key(r)
			if !right[k] || o.seen[k] {
				continue
			}
			o.seen[k] = true
			o.buf = append(o.buf, r)
		}
	case "except":
		right := map[string]bool{}
		for _, r := range rightRows {
			right[types.Key(r)] = true
		}
		for _, r := range leftRows {
			k := types.Key(r)
			if right[k] || o.seen[k] {
				continue
			}
			o.seen[k] = true
			o.buf = append(o.buf, r)
		}
	default:
		return fmt.Errorf("unsupported set operator %q", o.op)
	}
	return nil
}

func (o *setOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.buf) {
		return nil, false, nil
	}
	r := o.buf[o.i]
	o.i++
	return r, true, nil
}

func (o *setOp) Close() error {
	o.buf = nil
	o.seen = nil
	o.i = 0
	return nil
}

func (o *setOp) Describe() string {
	name := strings.ToUpper(o.op)
	if o.all {
		name += " ALL"
	}
	return name
}

func (o *setOp) Children() []Operator { return []Operator{o.left, o.right} }

func drain(op Operator) ([]types.Row, error) {
	if err := op.Open(); err != nil {
		return nil, err
	}
	defer op.Close()
	var out []types.Row
	for {
		r, ok, err := op.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, r)
	}
}

// ---------- FROM 里的子查询 / 集合函数 ----------

// rowsOp 是一张内存行表：FROM 子查询与集合函数的结果都走它。
type rowsOp struct {
	name string
	sc   Schema
	rows []types.Row
	i    int

	// child 非空表示行要现算（FROM 子查询），Open 时把它跑完。
	child Operator
}

func (o *rowsOp) Schema() Schema { return o.sc }

func (o *rowsOp) Open() error {
	o.i = 0
	if o.child == nil {
		return nil
	}
	rows, err := drain(o.child)
	if err != nil {
		return err
	}
	o.rows = rows
	return nil
}

func (o *rowsOp) Next() (types.Row, bool, error) {
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	r := o.rows[o.i]
	o.i++
	return r, true, nil
}

// Close 复位的规矩和 virtualOp 一样：rows 与游标一起清，child 的行留给下次 Open 重算。
func (o *rowsOp) Close() error {
	if o.child != nil {
		o.rows = nil
	}
	o.i = 0
	return nil
}

func (o *rowsOp) Describe() string {
	return fmt.Sprintf("%s(%d rows)", o.name, len(o.rows))
}

func (o *rowsOp) Children() []Operator {
	if o.child != nil {
		return []Operator{o.child}
	}
	return nil
}

// buildSubTable 构建 FROM 里的括号子查询。
func (p *Planner) buildSubTable(n *sql.SubTable) (Operator, Schema, error) {
	child, err := p.BuildSelect(n.Select)
	if err != nil {
		return nil, nil, err
	}
	sc := make(Schema, len(child.Schema()))
	copy(sc, child.Schema())
	for i := range sc {
		// 给了列名就按位置改名（PG 的 (col, ...) 语义）
		if i < len(n.Cols) {
			sc[i].Name = n.Cols[i]
		}
		// 别名覆盖限定符，这样外面写 alias.col 能取到
		sc[i].Qualifier = strings.ToLower(n.Alias)
	}
	if len(n.Cols) > len(sc) {
		return nil, nil, fmt.Errorf("table %q has %d columns available but %d columns specified",
			n.Alias, len(sc), len(n.Cols))
	}
	op := &rowsOp{name: "Subquery", sc: sc, child: child}
	return op, sc, nil
}

// buildFuncTable 构建 FROM 里的集合函数。
//
// 只实现真正需要的两个（generate_series / regexp_matches）；参数里如果
// 引用了外层列（横向连接），我们没法逐行求值，按 0 行处理 —— Navicat 的
// 触发器查询就是这种写法，而它的参数本身恒为 NULL，结果一致。
func (p *Planner) buildFuncTable(n *sql.FuncTable) (Operator, Schema, error) {
	name := strings.ToLower(n.Name)
	args := make([]types.Value, 0, len(n.Args))
	constArgs := true
	for _, a := range n.Args {
		v, err := CompileConst(a)
		if err != nil {
			constArgs = false
			break
		}
		args = append(args, v)
	}
	var rows []types.Row
	if constArgs {
		var err error
		rows, err = tableFuncRows(name, args)
		if err != nil {
			return nil, nil, err
		}
	}

	colName := name
	if len(n.Cols) > 0 {
		colName = n.Cols[0]
	} else if n.Alias != "" {
		// 单列集合函数带别名时，PostgreSQL 允许直接写 SELECT g。（我们只有
		// 单列，所以用别名当列名不会和别的列冲突。）
		colName = strings.ToLower(n.Alias)
	}
	kind := types.KText
	if name == "generate_series" {
		kind = types.KInt
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		kind = rows[0][0].K
	}
	colType := types.Type{Kind: kind, Name: "text"}
	if kind == types.KInt {
		colType = types.Type{Kind: types.KInt, Name: "bigint"}
	}
	sc := Schema{{Qualifier: strings.ToLower(n.Alias), Name: colName, Type: colType}}
	return &rowsOp{name: "FunctionScan(" + name + ")", sc: sc, rows: rows}, sc, nil
}

// tableFuncRows 计算集合函数的行。未知函数返回 0 行。
func tableFuncRows(name string, args []types.Value) ([]types.Row, error) {
	switch name {
	case "unnest":
		// unnest(数组)：数组在我们这里就是 {a,b} 这样的文本。
		// psql 的 \d 用它把列名数组摊成行。
		if len(args) < 1 || args[0].IsNull() {
			return nil, nil
		}
		txt := args[0].TextForm()
		body := strings.TrimSuffix(strings.TrimPrefix(txt, "{"), "}")
		var rows []types.Row
		for _, el := range splitArrayElems(body) {
			rows = append(rows, types.Row{types.Text(el)})
		}
		return rows, nil
	case "generate_series":
		if len(args) < 2 {
			return nil, nil
		}
		lo, err := toInt(args[0])
		if err != nil {
			return nil, err
		}
		hi, err := toInt(args[1])
		if err != nil {
			return nil, err
		}
		step := int64(1)
		if len(args) > 2 {
			if step, err = toInt(args[2]); err != nil {
				return nil, err
			}
		}
		if step == 0 {
			return nil, fmt.Errorf("step size cannot equal zero")
		}
		var rows []types.Row
		if step > 0 {
			for v := lo; v <= hi; v += step {
				rows = append(rows, types.Row{types.Int(v)})
			}
		} else {
			for v := lo; v >= hi; v += step {
				rows = append(rows, types.Row{types.Int(v)})
			}
		}
		return rows, nil
	case "regexp_matches":
		if len(args) < 2 || args[0].IsNull() || args[1].IsNull() {
			return nil, nil
		}
		re, err := regexp.Compile(args[1].TextForm())
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %v", err)
		}
		var rows []types.Row
		for _, m := range re.FindAllStringSubmatch(args[0].TextForm(), -1) {
			groups := m[1:]
			if len(groups) == 0 {
				groups = m
			}
			rows = append(rows, types.Row{types.Text(arrayLiteral(groups))})
		}
		return rows, nil
	}
	return nil, nil
}

// arrayLiteral 把若干文本拼成 PostgreSQL 的数组字面量 {a,b}。
func arrayLiteral(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		if strings.ContainsAny(s, ",{}\" ") {
			parts[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		} else {
			parts[i] = s
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}
