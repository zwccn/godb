package db

import (
	"fmt"
	"strings"
	"testing"
)

// 整数字面量要按值域提升成 bigint。以前任何整数都是 integer（OID 23），
// 客户端按 4 字节解码，select 9223372036854775807 会拿到 -1。
func TestBigintLiteralTyping(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	cases := []struct{ sql, wantType, wantVal string }{
		{"SELECT 2147483647", "integer", "2147483647"},
		{"SELECT 2147483648", "bigint", "2147483648"},
		{"SELECT 10000000000", "bigint", "10000000000"},
		{"SELECT 9223372036854775807", "bigint", "9223372036854775807"},
		{"SELECT '9000000000'::bigint + 1", "bigint", "9000000001"},
		{"SELECT 1::bigint * 3000000000", "bigint", "3000000000"},
		{"SELECT '9223372036854775807'::bigint - 1", "bigint", "9223372036854775806"},
		// 两边都是 int4 时保持 integer，不要无脑升级
		{"SELECT 1 + 2", "integer", "3"},
	}
	for _, c := range cases {
		res := run(t, d, s, c.sql)
		if got := res[0].Columns[0].Type; got != c.wantType {
			t.Errorf("%s 类型 = %q，期望 %q", c.sql, got, c.wantType)
		}
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.wantVal {
			t.Errorf("%s 值 = %q，期望 %q", c.sql, got, c.wantVal)
		}
	}

	// 溢出必须报错，不能回绕成错的数字
	if err := runErr(t, d, s, "SELECT '9223372036854775807'::bigint + 1"); !strings.Contains(err.Error(), "out of range") {
		t.Errorf("bigint 溢出应报 out of range，实际: %v", err)
	}
}

// 连接池归还连接时 Npgsql 会发一串会话清理语句；任何一条报错都会让
// 客户端把这条连接标记为不可用（复测里表现为"偶发的语法错误"）。
func TestSessionResetSequence(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	for _, q := range []string{
		"SET SESSION AUTHORIZATION DEFAULT",
		"RESET ALL",
		"CLOSE ALL",
		"CLOSE some_cursor",
		"UNLISTEN *",
		"UNLISTEN some_channel",
		"LISTEN some_channel",
		"DISCARD SEQUENCES",
		"DISCARD TEMP",
		"DISCARD ALL",
		"DEALLOCATE ALL",
	} {
		run(t, d, s, q)
	}

	// Npgsql 真实发的那一整串（CLOSE 的位置必须是 no-op 而不是语法错）
	run(t, d, s,
		"SET SESSION AUTHORIZATION DEFAULT;RESET ALL;CLOSE ALL;UNLISTEN *;"+
			"SELECT pg_advisory_unlock_all();DISCARD SEQUENCES;DISCARD TEMP;")

	res := run(t, d, s, "SELECT pg_advisory_unlock_all()")
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "true" {
		t.Fatalf("pg_advisory_unlock_all() = %q，期望 true", got)
	}
	if got := res[0].Columns[0].Type; got != "boolean" {
		t.Fatalf("pg_advisory_unlock_all 返回类型 = %q，期望 boolean", got)
	}
}

// FreeSql / EFCore 的 CodeFirst 会问索引列的性质。
func TestIndexColumnHasProperty(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE idx_p (id int PRIMARY KEY, v text)`)

	res := run(t, d, s, `SELECT
		pg_index_column_has_property('idx_p_pkey', 1, 'asc'),
		pg_index_column_has_property('idx_p_pkey', 1, 'desc'),
		pg_index_column_has_property('idx_p_pkey', 1, 'orderable'),
		pg_index_column_has_property('idx_p_pkey', 1, 'returnable'),
		pg_index_column_has_property('idx_p_pkey', 1, 'not_a_property')`)
	row := res[0].Rows[0]
	want := []string{"true", "false", "true", "false", "<nil>"}
	for i, w := range want {
		if got := fmt.Sprint(row[i]); got != w {
			t.Errorf("属性第 %d 个 = %q，期望 %q", i+1, got, w)
		}
	}
}

// ORM 生成的建表语句：表级 CONSTRAINT 前缀 + 尾部 WITH (OIDS=FALSE)。
func TestORMTableSyntax(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	run(t, d, s, `CREATE TABLE orm_a (
		id   int NOT NULL,
		v    text,
		CONSTRAINT orm_a_pk PRIMARY KEY (id)
	) WITH (OIDS=FALSE)`)

	run(t, d, s, `CREATE TABLE orm_b (
		id  int PRIMARY KEY,
		aid int,
		CONSTRAINT orm_b_fk FOREIGN KEY (aid) REFERENCES orm_a(id)
	)`)

	run(t, d, s, `CREATE TABLE orm_c (id int) WITHOUT OIDS`)
	run(t, d, s, `CREATE TABLE orm_d (id int, CONSTRAINT orm_d_uq UNIQUE (id))`)

	// 表级主键真的生效（不是只把语法吃掉）
	run(t, d, s, `INSERT INTO orm_a (id, v) VALUES (1, 'x')`)
	if err := runErr(t, d, s, `INSERT INTO orm_a (id, v) VALUES (1, 'y')`); !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("表级主键没生效，期望 duplicate key，实际: %v", err)
	}

	// 表级外键也生效
	if err := runErr(t, d, s, `INSERT INTO orm_b (id, aid) VALUES (1, 999)`); err == nil {
		t.Error("表级外键没生效")
	}
	run(t, d, s, `INSERT INTO orm_b (id, aid) VALUES (1, 1)`)

	// 表级 UNIQUE
	run(t, d, s, `INSERT INTO orm_d (id) VALUES (7)`)
	if err := runErr(t, d, s, `INSERT INTO orm_d (id) VALUES (7)`); !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("表级 UNIQUE 没生效，实际: %v", err)
	}
}

// 数组内部一律按 PG 数组文本存；cast 不能把 "numeric[]" 当标量 numeric 处理。
func TestArrayCastKeepsText(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	cases := []struct{ sql, wantType, want string }{
		{"SELECT '{1,2,3}'::int[]", "int[]", "{1,2,3}"},
		{"SELECT '{1.5,2.5}'::numeric[]", "numeric[]", "{1.5,2.5}"},
		{"SELECT '{a,b}'::text[]", "text[]", "{a,b}"},
		{"SELECT '{}'::int[]", "int[]", "{}"},
	}
	for _, c := range cases {
		res := run(t, d, s, c.sql)
		if got := res[0].Columns[0].Type; got != c.wantType {
			t.Errorf("%s 类型 = %q，期望 %q", c.sql, got, c.wantType)
		}
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.want {
			t.Errorf("%s 值 = %q，期望 %q", c.sql, got, c.want)
		}
	}

	// 标量转数组是非法转换（PG 同样拒绝），别静默当成文本
	if err := runErr(t, d, s, "SELECT 1::int[]"); !strings.Contains(err.Error(), "cannot cast") {
		t.Errorf("标量转数组应报 cannot cast，实际: %v", err)
	}
}
