package db

import (
	"fmt"
	"strings"
	"testing"
)

// 列声明的宽度必须**写入时**就校验。
//
// 这批用例来自一次实测：int2 列能存 40000、int4 列能存 3000000000、
// numeric(5,2) 能存 1234.5 —— 插入成功、提交成功，网页控制台也正常显示
// （它按内存值格式化，不看 OID）。但 PG 客户端按声明类型解码：二进制格式
// 直接报 22003，文本格式拿到超界字符串。结果就是「写得进、读不出」的毒行，
// 而且报错发生在之后某次查询里，完全指不到根因。
func TestColumnRangeEnforcedOnWrite(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, "CREATE TABLE rng (id int2, v int4, n numeric(5,2))")

	bad := []string{
		"INSERT INTO rng (id) VALUES (40000)",
		"INSERT INTO rng (id) VALUES (-32769)",
		"INSERT INTO rng (v) VALUES (3000000000)",
		"INSERT INTO rng (v) VALUES (2147483647 + 1)",
		"INSERT INTO rng (n) VALUES (1234.5)",
		"INSERT INTO rng (n) VALUES (-1000)",
	}
	for _, sql := range bad {
		if err := runErr(t, d, s, sql); !strings.Contains(err.Error(), "out of range") {
			t.Errorf("%s\n应报 out of range，实际: %v", sql, err)
		}
	}

	// 边界内的值必须照常写入 —— 校验别做过火
	run(t, d, s, "INSERT INTO rng (id, v, n) VALUES (32767, 2147483647, 999.99)")
	run(t, d, s, "INSERT INTO rng (id, v, n) VALUES (-32768, -2147483648, -999.99)")
	res := run(t, d, s, "SELECT count(*) FROM rng")
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Errorf("边界内的值应当写入成功，实际行数 %s", got)
	}

	// UPDATE 也走同一条校验：不能绕过 INSERT 的检查
	if err := runErr(t, d, s, "UPDATE rng SET id = 40000"); !strings.Contains(err.Error(), "out of range") {
		t.Errorf("UPDATE 越界应报 out of range，实际: %v", err)
	}
}

// int4 + int4 的溢出要在**计算时**报错。
//
// Go 的 int64 装得下 2147483648，所以这里一直是「值算出来了、列类型还是
// integer」—— 客户端按 4 字节解码拿到错值。PG 在计算时就报 22003。
func TestInt4ArithmeticOverflow(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	over := []string{
		"SELECT 2147483647 + 1",
		"SELECT -2147483647 - 2",
		"SELECT 2147483647 * 2",
		"SELECT (-2147483647 - 1) / -1",
	}
	for _, sql := range over {
		if err := runErr(t, d, s, sql); !strings.Contains(err.Error(), "out of range") {
			t.Errorf("%s\n应报 out of range，实际: %v", sql, err)
		}
	}

	// int8 与边界内不该被误伤
	ok := []struct{ sql, want string }{
		{"SELECT 2147483647", "2147483647"},
		{"SELECT 9000000000 + 1", "9000000001"},
		{"SELECT 2147483647 - 1", "2147483646"},
	}
	for _, c := range ok {
		res := run(t, d, s, c.sql)
		if got := fmt.Sprint(res[0].Rows[0][0]); got != c.want {
			t.Errorf("%s = %s，期望 %s", c.sql, got, c.want)
		}
	}
}
