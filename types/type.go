package types

import (
	"fmt"
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
		t = Type{Kind: KFloat, Name: "numeric"}
	case "date":
		t = Type{Kind: KTime, Name: "date"}
	case "timestamp", "timestamp without time zone", "timestamptz", "timestamp with time zone", "datetime", "time_stamp":
		t = Type{Kind: KTime, Name: "timestamp"}
	case "time", "timetz", "time without time zone", "time with time zone":
		t = Type{Kind: KText, Name: n}
	case "interval":
		// interval 以「纳秒数」存进 KInt，配合时间加减（month/year 按 30/365 天近似）。
		t = Type{Kind: KInt, Name: "interval"}
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
	case KFloat:
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
