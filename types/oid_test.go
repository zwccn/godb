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
