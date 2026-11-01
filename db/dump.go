package db

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"godb/catalog"
)

// 这个文件只做一件事：把整个库导成一份可以直接执行的 SQL 文本。
//
// 为什么是 SQL 而不是「打包数据目录」：
//   - 数据目录里是页文件 + WAL，运行中直接复制会拿到撕裂的页；
//   - SQL 能看、能改、能只恢复一张表，出问题也好排查；
//   - 教学库的量级（几万行）导一遍是秒级的，不值得上二进制快照。
//
// 已知取舍，都写在导出的文件头里：
//   - **只含表结构 / 数据 / 索引**，不含视图以外的扩展对象（本库也没有）；
//   - 自增列用 SERIAL 重建，导完再用 setval 把序列对齐到当前最大值，
//     否则恢复后插入新行会撞主键；
//   - 外键按 catalog 里的顺序排在建表之后（表之间互相引用也能建起来）。

// dumpCol 是一列的结构信息快照。
type dumpCol struct {
	name    string
	typ     string
	notNull bool
	serial  bool
	def     string
	unique  bool
}

// dumpTable 是一张表的结构快照。先整体拷一份再读数据 —— 不要拿着 SchemaMu
// 去跑 SELECT（会阻塞 DDL，也可能在锁序上出问题）。
type dumpTable struct {
	name    string
	cols    []dumpCol
	pk      []string
	indexes []*catalog.Index
}

// DumpSQL 导出整个库。dbName 只用于文件头注释。
func (d *DB) DumpSQL(dbName string) (string, error) {
	tables := d.snapshotForDump()
	var b strings.Builder
	fmt.Fprintf(&b, "-- godb 备份 · 库 %s · %s\n", dbName, time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString("-- 恢复：设置 → 数据 → 从备份恢复（选这个文件），或直接把整段当 SQL 执行。\n")
	b.WriteString("-- 含表结构、数据、普通索引；自增列会重建为 SERIAL，并用 setval 对齐到当前最大值。\n")
	b.WriteString("-- 恢复到**空库**里最稳：同名表已存在时会报「已存在」，那一条之后的语句不会执行。\n\n")

	// 1) 建表
	for _, t := range tables {
		b.WriteString("CREATE TABLE " + quoteIdent(t.name) + " (\n")
		lines := make([]string, 0, len(t.cols)+1)
		inPK := map[string]bool{}
		for _, c := range t.pk {
			inPK[c] = true
		}
		for _, c := range t.cols {
			line := "  " + quoteIdent(c.name) + " "
			if c.serial {
				line += "SERIAL"
			} else {
				line += c.typ
			}
			if c.notNull {
				line += " NOT NULL"
			}
			// 主键列上的 UNIQUE 是冗余的（PK 本身就是唯一索引），
			// 原样写回去会多建一个索引，恢复出来的库比原来多占一块空间。
			if c.unique && !inPK[c.name] {
				line += " UNIQUE"
			}
			if c.def != "" && !strings.Contains(strings.ToLower(c.def), "nextval") {
				line += " DEFAULT " + c.def
			}
			lines = append(lines, line)
		}
		if len(t.pk) > 0 {
			q := make([]string, len(t.pk))
			for i, c := range t.pk {
				q[i] = quoteIdent(c)
			}
			lines = append(lines, "  PRIMARY KEY ("+strings.Join(q, ", ")+")")
		}
		b.WriteString(strings.Join(lines, ",\n"))
		b.WriteString("\n);\n\n")
	}

	// 2) 数据
	seqs := d.dumpRows(&b, tables)

	// 3) 索引（放在数据之后：先灌数据再建索引，比反过来快）
	for _, t := range tables {
		for _, ix := range t.indexes {
			kw := "CREATE INDEX "
			if ix.Unique {
				kw = "CREATE UNIQUE INDEX "
			}
			q := make([]string, len(ix.Columns))
			for i, c := range ix.Columns {
				q[i] = quoteIdent(c)
			}
			b.WriteString(kw + quoteIdent(ix.Name) + " ON " + quoteIdent(t.name) +
				" (" + strings.Join(q, ", ") + ");\n")
		}
	}
	if len(tables) > 0 {
		b.WriteString("\n")
	}

	// 4) 自增序列对齐到最后一行（不然后面 INSERT 会从 1 开始撞主键）
	for _, s := range seqs {
		fmt.Fprintf(&b, "SELECT setval(%s, %d, true);\n", quoteSQL(s.name), s.max)
	}
	return b.String(), nil
}

// snapshotForDump 在 SchemaMu 读锁里拷一份表结构，锁外再读数据。
func (d *DB) snapshotForDump() []dumpTable {
	d.Store.SchemaMu.RLock()
	defer d.Store.SchemaMu.RUnlock()

	out := make([]dumpTable, 0, len(d.Cat.Tables))
	for _, t := range d.Cat.Tables {
		dt := dumpTable{name: t.Name}
		for _, c := range t.Columns {
			dt.cols = append(dt.cols, dumpCol{
				name:    c.Name,
				typ:     c.Type().String(),
				notNull: c.NotNull,
				serial:  c.Serial,
				def:     c.Default,
				unique:  c.Unique,
			})
			if c.PrimaryKey {
				dt.pk = append(dt.pk, c.Name)
			}
		}
		for _, ix := range t.Indexes {
			dt.indexes = append(dt.indexes, ix)
		}
		out = append(out, dt)
	}
	return out
}

// seqMax 记录某个自增序列应该被推到多少。
type seqMax struct {
	name string
	max  int64
}

// dumpRows 把每张表的数据写成批量 INSERT，顺便算出每个自增列的最大值。
func (d *DB) dumpRows(b *strings.Builder, tables []dumpTable) []seqMax {
	sess := d.Session("__dump__")
	var out []seqMax

	for _, t := range tables {
		res, err := d.Exec(sess, "SELECT * FROM "+quoteIdent(t.name))
		if err != nil || len(res) == 0 {
			// 读不出来就明确写一行注释，别让恢复出来的库「少一张表却没人知道」
			fmt.Fprintf(b, "-- 跳过 %s：读取失败（%v）\n\n", t.name, err)
			continue
		}
		rows := res[0].Rows
		if len(rows) == 0 {
			continue
		}

		// 自增列的最大值：按列位置取
		serialIdx := map[int]int{}
		seqPos := []int{}
		for i, c := range t.cols {
			if c.serial {
				serialIdx[i] = len(seqPos)
				seqPos = append(seqPos, i)
			}
		}
		maxes := make([]int64, len(seqPos))

		cols := make([]string, len(t.cols))
		for i, c := range t.cols {
			cols[i] = quoteIdent(c.name)
		}
		head := "INSERT INTO " + quoteIdent(t.name) + " (" + strings.Join(cols, ", ") + ") VALUES\n"

		b.WriteString(head)
		for r, row := range rows {
			vals := make([]string, len(row))
			for i, v := range row {
				vals[i] = sqlLiteral(v)
				if p, ok := serialIdx[i]; ok {
					if n, ok2 := v.(int64); ok2 && n > maxes[p] {
						maxes[p] = n
					}
				}
			}
			b.WriteString("  (" + strings.Join(vals, ", ") + ")")
			if r == len(rows)-1 {
				b.WriteString(";\n")
			} else {
				b.WriteString(",\n")
			}
		}
		b.WriteString("\n")

		for p, idx := range seqPos {
			if maxes[p] <= 0 {
				continue
			}
			out = append(out, seqMax{
				name: strings.ToLower(t.name) + "_" + strings.ToLower(t.cols[idx].name) + "_seq",
				max:  maxes[p],
			})
		}
	}
	return out
}

// quoteIdent 一律加双引号并转义内部引号：大小写、中文、保留字全都不用特判。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteSQL 生成字符串字面量。
func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqlLiteral 把一行的单元格转成 SQL 字面量。
//
// Result.Rows 里已经是 interface{}（执行器用 Value.Interface() 转过的）：
// 整数是 int64、浮点是 float64、其余（时间 / decimal / interval / 文本）都是 string。
// 所以这里只按 Go 类型分支就够了，不需要再拿列类型做二次判断 ——
// 字符串进到 timestamp/numeric 列时由引擎做隐式转换。
func sqlLiteral(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		// NaN / ±Inf 不是合法的数字字面量，得写成字符串再让引擎转
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return quoteSQL(strconv.FormatFloat(x, 'g', -1, 64))
		}
		// 'g' + -1 是最短往返表示，不会丢精度也不会出 1.2e+06 这种意外
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return quoteSQL(x)
	case []byte:
		return quoteSQL(string(x))
	case time.Time:
		return quoteSQL(x.Format("2006-01-02 15:04:05.999999"))
	}
	return quoteSQL(fmt.Sprint(v))
}
