package types

import "testing"

// information_schema 的列用的是 SQL 标准的域（domain）名，不是基础类型名。
// 客户端是**按 RowDescription 里的类型 OID 解析结果集**的：把
// cardinal_number 当 text 发出去，Navicat 的长度/精度列就会解析成 0，
// 表设计器里整列类型显示成 "(Type)"；而值的文本完全相同，光看查询结果
// 根本发现不了。这里钉住与真实 PostgreSQL 一致的 OID。
//
// 期望值是在本机 PostgreSQL 10.23 上实测出来的（同一条
// information_schema.columns 查询，对比两边的 RowDescription）。
func TestInformationSchemaDomainOIDs(t *testing.T) {
	cases := []struct {
		name string
		want int32
	}{
		{"cardinal_number", OIDInt4},   // 23：长度/精度/序号这些列
		{"sql_identifier", OIDVarchar}, // 1043：schema/table/column 名字
		{"character_data", OIDVarchar}, // 1043
		{"yes_or_no", OIDVarchar},      // 1043：is_nullable 之类
		{"time_stamp", OIDTimestamptz}, // 1184
		{"int2[]", 1005},               // pg_constraint.conkey
		{"oid[]", 1028},                // pg_constraint.conexclop
		{"int2vector", 22},             // pg_index.indkey / indoption
		{"oidvector", 30},              // pg_index.indclass / indcollation
		{"aclitem[]", 1034},            // 权限数组
	}
	for _, c := range cases {
		if got := OIDByName(c.name); got != c.want {
			t.Fatalf("OIDByName(%q) = %d，期望 %d", c.name, got, c.want)
		}
	}

	// 别把普通类型带坏
	if OIDByName("integer") != OIDInt4 || OIDByName("text") != OIDText {
		t.Fatal("基础类型的 OID 映射被改动了")
	}
}

// typeFast 是为了省掉 OIDOf 里的三次字符串分配而加的直查表。
// 它必须与慢路径（OIDByName / TypeLenByName）逐个对齐 —— 表里的值是启动时
// 由这两个函数算出来的，所以这条断言其实是「别有人把 map 改成手抄常量」。
func TestTypeFastMatchesSlowPath(t *testing.T) {
	if len(typeFast) == 0 {
		t.Fatal("快查表是空的")
	}
	for name, f := range typeFast {
		if got := OIDByName(name); got != f.oid {
			t.Fatalf("%s: OID 快路径 %d ≠ OIDByName %d", name, f.oid, got)
		}
		if got := TypeLenByName(name); got != f.length {
			t.Fatalf("%s: typlen 快路径 %d ≠ TypeLenByName %d", name, f.length, got)
		}
		// 走 OIDOf/TypeLenOf 也必须一致（Type.Name 可能就是这个名字）
		ty := Type{Name: name}
		if OIDOf(ty) != f.oid || TypeLenOf(ty) != f.length {
			t.Fatalf("%s: OIDOf/TypeLenOf 与快查表不一致", name)
		}
	}
}

// 带参数的类型名命中不了快查表，必须落回慢路径且结果正确。
// （varchar(32) 这种名字在 ParseType 里是拼出来的，库里到处都有。）
func TestOIDOfParameterizedType(t *testing.T) {
	cases := []struct {
		typ  Type
		oid  int32
		tlen int32
	}{
		{Type{Kind: KText, Name: "varchar(32)"}, OIDVarchar, -1},
		{Type{Kind: KDecimal, Name: "numeric(10,2)"}, OIDNumeric, -1},
	}
	for _, c := range cases {
		if got := OIDOf(c.typ); got != c.oid {
			t.Fatalf("OIDOf(%s) = %d，期望 %d", c.typ.Name, got, c.oid)
		}
		if got := TypeLenOf(c.typ); got != c.tlen {
			t.Fatalf("TypeLenOf(%s) = %d，期望 %d", c.typ.Name, got, c.tlen)
		}
	}
}
