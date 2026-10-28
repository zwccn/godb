package types

import (
	"fmt"
	"math"
	"strings"
)

// Type 是列的数据类型。Kind 决定运行时语义，Name 是对外显示（PG 风格）的写法。
type Type struct {
	Kind  Kind
	Name  string
	Len   int // 字符类型的最大长度，0 表示不限
	Prec  int // numeric 精度
	Scale int // numeric 小数位
}

func (t Type) String() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Kind.String()
}

func (t Type) IsNumeric() bool { return t.Kind == KInt || t.Kind == KFloat }

// TypeOf 返回值的自然类型。
func TypeOf(v Value) Type { return Type{Kind: v.K, Name: v.K.String()} }

// LiteralType 返回「字面量」的类型。
//
// 和 TypeOf 的差别只在整数：TypeOf 对任何 KInt 都报 integer，但 PG 的整数字面量
// 是按值域提升的（int4 → int8 → numeric）。如果大整数也声明成 int4，RowDescription
// 给出的就是 OID 23，客户端按 4 字节解码，值会被静默截断（select 9223372036854775807
// 变成 -1）。这里按 int32 边界判断，超出就用 bigint。
func LiteralType(v Value) Type {
	if v.K == KInt {
		if v.I >= math.MinInt32 && v.I <= math.MaxInt32 {
			return Type{Kind: KInt, Name: "integer"}
		}
		return Type{Kind: KInt, Name: "bigint"}
	}
	return TypeOf(v)
}

// ParseType 把 SQL 里写的类型名（已小写）解析成 Type。
func ParseType(name string, args []int) (Type, error) {
	n := strings.Join(strings.Fields(strings.ToLower(name)), " ")
	t := Type{Kind: KText}
	switch n {
	case "smallint", "int2":
		t = Type{Kind: KInt, Name: "smallint"}
	case "int", "integer", "int4", "serial", "serial4":
		t = Type{Kind: KInt, Name: "integer"}
	case "bigint", "int8", "bigserial", "serial8":
		t = Type{Kind: KInt, Name: "bigint"}
	case "text", "string", "varchar", "character varying", "char varying":
		if n == "varchar" || n == "character varying" || n == "char varying" {
			t = Type{Kind: KText, Name: "varchar"}
		} else {
			t = Type{Kind: KText, Name: "text"}
		}
	case "char", "character", "bpchar":
		t = Type{Kind: KText, Name: "char"}
	case "boolean", "bool":
		t = Type{Kind: KBool, Name: "boolean"}
	case "real", "float4":
		t = Type{Kind: KFloat, Name: "real"}
	case "double precision", "float8", "float":
		t = Type{Kind: KFloat, Name: "double precision"}
	case "numeric", "decimal":
		t = Type{Kind: KDecimal, Name: "numeric"}
	case "date":
		t = Type{Kind: KTime, Name: "date"}
	case "timestamp", "timestamp without time zone", "timestamptz", "timestamp with time zone", "datetime", "time_stamp":
		t = Type{Kind: KTime, Name: "timestamp"}
	case "time", "timetz", "time without time zone", "time with time zone":
		t = Type{Kind: KText, Name: n}
	case "interval":
		// interval：I=纳秒、z=月数（见 types.Interval）
		t = Type{Kind: KInterval, Name: "interval"}
	case "oid", "xid", "cid", "tid", "regproc", "regprocedure", "regoper", "regoperator":
		t = Type{Kind: KInt, Name: n}
	case "regclass", "regtype", "regnamespace", "regrole", "regconfig", "regdictionary":
		// 归为文本：值就是对象名，比较时按字符串走
		t = Type{Kind: KText, Name: n}
	case "name", "bytea", "uuid", "json", "jsonb", "xml", "inet", "cidr", "macaddr",
		"macaddr8", "money", "point", "line", "lseg", "box", "path", "polygon", "circle",
		"tsvector", "tsquery", "pg_node_tree", "pg_lsn", "int2vector", "oidvector", "aclitem",
		"record", "anyarray", "anyelement", "anyenum", "void", "unknown", "cstring":
		t = Type{Kind: KText, Name: n}
	case "sql_identifier", "character_data", "yes_or_no", "cardinal_number":
		// information_schema 里的域（domain）：值是文本，但客户端会写
		// (n.nspname)::information_schema.sql_identifier 这样的显式转换。
		// 不认这些名字会导致整条查询类型错误。
		t = Type{Kind: KText, Name: n}
	default:
		// 数组类型（text[]、oid[]、_text 等）与其它未列出的类型统一按文本处理，
		// 这样客户端发来的 pg_catalog 查询不会因为类型名不认识而直接报错。
		if strings.HasSuffix(n, "[]") || strings.HasPrefix(n, "_") {
			t = Type{Kind: KText, Name: n}
			break
		}
		return Type{}, fmt.Errorf("type %q does not exist", name)
	}

	switch t.Kind {
	case KText:
		if len(args) > 0 && args[0] > 0 {
			t.Len = args[0]
			t.Name = fmt.Sprintf("%s(%d)", t.Name, args[0])
		}
	case KDecimal:
		if n == "numeric" || n == "decimal" {
			if len(args) >= 2 {
				t.Prec, t.Scale = args[0], args[1]
				t.Name = fmt.Sprintf("numeric(%d,%d)", args[0], args[1])
			} else if len(args) == 1 {
				t.Prec = args[0]
				t.Name = fmt.Sprintf("numeric(%d)", args[0])
			}
		}
	case KFloat:
		// 保留旧分支：历史数据里 Kind 可能还是 float
		if n == "numeric" || n == "decimal" {
			if len(args) >= 2 {
				t.Prec, t.Scale = args[0], args[1]
				t.Name = fmt.Sprintf("numeric(%d,%d)", args[0], args[1])
			} else if len(args) == 1 {
				t.Prec = args[0]
				t.Name = fmt.Sprintf("numeric(%d)", args[0])
			}
		}
	}
	return t, nil
}
