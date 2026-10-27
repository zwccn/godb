package types

import "strings"

// PostgreSQL 内置类型的 OID。系统目录（pg_type / pg_attribute）和
// 前端协议（RowDescription）都用这套编号，客户端据此决定如何渲染值。
const (
	OIDBool        = 16
	OIDBytea       = 17
	OIDChar        = 18 // "char"（单字节内部类型）
	OIDName        = 19
	OIDInt8        = 20
	OIDInt2        = 21
	OIDInt2Vector  = 22
	OIDInt4        = 23
	OIDRegproc     = 24
	OIDText        = 25
	OIDOid         = 26
	OIDTid         = 27
	OIDXid         = 28
	OIDCid         = 29
	OIDOidVector   = 30
	OIDJson        = 114
	OIDXml         = 142
	OIDNodeTree    = 194
	OIDPoint       = 600
	OIDLseg        = 601
	OIDPath        = 602
	OIDBox         = 603
	OIDPolygon     = 604
	OIDLine        = 628
	OIDCidr        = 650
	OIDFloat4      = 700
	OIDFloat8      = 701
	OIDUnknown     = 705
	OIDCircle      = 718
	OIDMacaddr8    = 774
	OIDMoney       = 790
	OIDMacaddr     = 829
	OIDInet        = 869
	OIDAclitem     = 1033
	OIDBpchar      = 1042
	OIDVarchar     = 1043
	OIDDate        = 1082
	OIDTime        = 1083
	OIDTimestamp   = 1114
	OIDTimestamptz = 1184
	OIDInterval    = 1186
	OIDTimetz      = 1266
	OIDBit         = 1560
	OIDVarbit      = 1562
	OIDNumeric     = 1700
	OIDRefcursor   = 1790
	OIDRegclass    = 2205
	OIDRegtype     = 2206
	OIDRecord      = 2249
	OIDAnyArray    = 2277
	OIDAnyElement  = 2283
	OIDUuid        = 2950
	OIDTsvector    = 3614
	OIDTsquery     = 3615
	OIDJsonb       = 3802
)

// OIDByName 把类型名（可能带长度/精度，如 varchar(32)）映射成 PG 类型 OID。
func OIDByName(name string) int32 {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(n, '('); i >= 0 {
		n = strings.TrimSpace(n[:i])
	}
	switch n {
	case "smallint", "int2":
		return OIDInt2
	case "integer", "int", "int4", "serial", "serial4":
		return OIDInt4
	case "bigint", "int8", "bigserial", "serial8":
		return OIDInt8
	case "text":
		return OIDText
	case "varchar", "character varying", "char varying":
		return OIDVarchar
	case "char", "character", "bpchar":
		return OIDBpchar
	case "name":
		return OIDName
	case "boolean", "bool":
		return OIDBool
	case "real", "float4":
		return OIDFloat4
	case "double precision", "float8", "float":
		return OIDFloat8
	case "numeric", "decimal":
		return OIDNumeric
	case "date":
		return OIDDate
	case "time", "time without time zone":
		return OIDTime
	case "timetz", "time with time zone":
		return OIDTimetz
	case "timestamp", "timestamp without time zone", "datetime":
		return OIDTimestamp
	case "timestamptz", "timestamp with time zone":
		return OIDTimestamptz
	case "interval":
		return OIDInterval
	case "oid":
		return OIDOid
	case "xid":
		return OIDXid
	case "cid":
		return OIDCid
	case "tid":
		return OIDTid
	case "regclass":
		return OIDRegclass
	case "regtype":
		return OIDRegtype
	case "regproc":
		return OIDRegproc
	case "uuid":
		return OIDUuid
	case "json":
		return OIDJson
	case "jsonb":
		return OIDJsonb
	case "bytea":
		return OIDBytea
	case "inet":
		return OIDInet
	case "cidr":
		return OIDCidr
	case "macaddr":
		return OIDMacaddr
	case "money":
		return OIDMoney
	case "xml":
		return OIDXml
	case "tsvector":
		return OIDTsvector
	case "tsquery":
		return OIDTsquery
	case "aclitem":
		return OIDAclitem
	case "int2vector":
		return OIDInt2Vector
	case "oidvector":
		return OIDOidVector
	case "pg_node_tree":
		return OIDNodeTree
	case "record":
		return OIDRecord
	case "\"char\"":
		return OIDChar
	// information_schema 里的域（domain）名。客户端是按 OID 解析结果集的：
	// 把 cardinal_number 当 text 发，Navicat 的长度/精度列就解析不出来，
	// 表设计器里整列类型显示成 "(Type)"。这里给出与真实 PostgreSQL 一致的
	// 底层类型 OID（实测：cardinal_number=int4，其余三个域=varchar）。
	case "cardinal_number":
		return OIDInt4
	case "sql_identifier", "character_data", "yes_or_no":
		return OIDVarchar
	case "time_stamp":
		return OIDTimestamptz
	}
	if strings.HasSuffix(n, "[]") {
		if strings.HasPrefix(n, "aclitem") {
			return 1034 // aclitem[]
		}
		// 其它数组类型的 OID 不重要，统一报 text[]，避免客户端误判成标量
		return 1009
	}
	return OIDText
}

// OIDOf 返回一个具体类型的 OID。
func OIDOf(t Type) int32 { return OIDByName(t.String()) }

// TypeLenByName 返回该类型的 pg_type.typlen（-1 表示变长）。
func TypeLenByName(name string) int32 {
	switch OIDByName(name) {
	case OIDBool, OIDChar:
		return 1
	case OIDInt2, OIDInt2Vector:
		return 2
	case OIDInt4, OIDDate, OIDFloat4, OIDOid, OIDXid, OIDCid, OIDRegproc,
		OIDRegclass, OIDRegtype, OIDTid, OIDAclitem:
		return 4
	case OIDInt8, OIDFloat8, OIDMoney, OIDTimestamp, OIDTimestamptz, OIDTime,
		OIDTimetz, OIDInterval:
		return 8
	case OIDUuid:
		return 16
	case OIDPoint:
		return 16
	case OIDInet, OIDCidr:
		return -1
	}
	return -1
}

// oidNames 是 OID 到标准类型名的反查表（format_type 用）。
var oidNames = map[int32]string{
	OIDBool: "boolean", OIDBytea: "bytea", OIDChar: `"char"`, OIDName: "name",
	OIDInt8: "bigint", OIDInt2: "smallint", OIDInt2Vector: "int2vector",
	OIDInt4: "integer", OIDRegproc: "regproc", OIDText: "text", OIDOid: "oid",
	OIDTid: "tid", OIDXid: "xid", OIDCid: "cid", OIDOidVector: "oidvector",
	OIDJson: "json", OIDXml: "xml", OIDNodeTree: "pg_node_tree",
	OIDPoint: "point", OIDCidr: "cidr", OIDFloat4: "real",
	OIDFloat8: "double precision", OIDUnknown: "unknown", OIDMoney: "money",
	OIDMacaddr: "macaddr", OIDInet: "inet", OIDAclitem: "aclitem",
	OIDBpchar: "character", OIDVarchar: "character varying",
	OIDDate: "date", OIDTime: "time without time zone",
	OIDTimestamp:   "timestamp without time zone",
	OIDTimestamptz: "timestamp with time zone",
	OIDInterval:    "interval", OIDTimetz: "time with time zone",
	OIDBit: "bit", OIDVarbit: "bit varying", OIDNumeric: "numeric",
	OIDRegclass: "regclass", OIDRegtype: "regtype", OIDRecord: "record",
	OIDUuid: "uuid", OIDJsonb: "jsonb", OIDTsvector: "tsvector",
	OIDTsquery: "tsquery", OIDAnyArray: "anyarray", OIDAnyElement: "anyelement",
	1000: "boolean[]", 1009: "text[]", 1015: "character varying[]",
	1016: "bigint[]", 1007: "integer[]", 1005: "smallint[]",
	1021: "real[]", 1022: "double precision[]", 1231: "numeric[]",
	1182: "date[]", 1115: "timestamp without time zone[]",
	2951: "uuid[]", 199: "json[]", 3807: "jsonb[]", 1028: "oid[]",
}

// NameByOID 把类型 OID 换成 PG 的显示名；未知 OID 返回 "text"。
func NameByOID(oid int32) string {
	if n, ok := oidNames[oid]; ok {
		return n
	}
	return "text"
}
