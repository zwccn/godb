package db

import (
	"fmt"
	"strings"
	"testing"
)

// EXTRACT(field FROM x) / date_part / date_trunc —— 报表和 ORM 的分组统计
// 都绕不开这几个，之前报 42601。
func TestExtractAndDateTrunc(t *testing.T) {
	d := newTestDB(t)
	s := "s"

	res := run(t, d, s, `
		SELECT
			extract(year  from '2026-10-27 13:45:30'::timestamp),
			extract(month from '2026-10-27 13:45:30'::timestamp),
			extract(day   from '2026-10-27 13:45:30'::timestamp),
			extract(hour  from '2026-10-27 13:45:30'::timestamp),
			extract(minute from '2026-10-27 13:45:30'::timestamp),
			extract(second from '2026-10-27 13:45:30'::timestamp),
			extract(quarter from '2026-10-27 13:45:30'::timestamp),
			extract(doy   from '2026-10-27 13:45:30'::timestamp),
			extract(isodow from '2026-10-27 13:45:30'::timestamp)`)

	want := []string{"2026", "10", "27", "13", "45", "30", "4", "300", "2"}
	row := res[0].Rows[0]
	for i, w := range want {
		if got := fmt.Sprint(row[i]); got != w {
			t.Fatalf("第 %d 个 extract = %q，期望 %q（整行 %v）", i+1, got, w, row)
		}
	}

	// date_part 是 extract 的函数写法
	res = run(t, d, s, `SELECT date_part('year', '2026-10-27'::timestamp)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2026" {
		t.Fatalf("date_part = %q", got)
	}

	// date_trunc：天/月/周/年
	res = run(t, d, s, `
		SELECT
			date_trunc('day',   '2026-10-27 13:45:30'::timestamp),
			date_trunc('month', '2026-10-27 13:45:30'::timestamp),
			date_trunc('year',  '2026-10-27 13:45:30'::timestamp),
			date_trunc('week',  '2026-10-27 13:45:30'::timestamp)`)
	row = res[0].Rows[0]
	for i, w := range []string{
		"2026-10-27 00:00:00", "2026-10-01 00:00:00", "2026-01-01 00:00:00", "2026-10-26 00:00:00",
	} {
		got := fmt.Sprint(row[i])
		if !strings.HasPrefix(got, w[:10]) {
			t.Fatalf("date_trunc 第 %d 个 = %q，期望以 %q 开头", i+1, got, w)
		}
	}

	// 不认识的字段要报错，别静默返回 0
	if err := runErr(t, d, s, `SELECT extract(nosuchfield from now())`); err == nil {
		t.Fatal("未知 extract 字段应当报错")
	}
	if err := runErr(t, d, s, `SELECT date_trunc('nosuchunit', now())`); err == nil {
		t.Fatal("未知 date_trunc 单位应当报错")
	}
}

// currval / lastval / nextval / pg_get_serial_sequence
func TestSerialFunctions(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE seq_t (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text)`)
	run(t, d, s, `INSERT INTO seq_t (v) VALUES ('a'), ('b')`)

	res := run(t, d, s, `SELECT currval('seq_t_id_seq')`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("currval = %q，期望 2", got)
	}

	res = run(t, d, s, `SELECT pg_get_serial_sequence('seq_t', 'id')`)
	want := "public.seq_t_id_seq"
	if got := fmt.Sprint(res[0].Rows[0][0]); got != want {
		t.Fatalf("pg_get_serial_sequence = %q，期望 %q", got, want)
	}

	// 客户端常见写法：currval(pg_get_serial_sequence(...))
	res = run(t, d, s, `SELECT currval(pg_get_serial_sequence('seq_t', 'id'))`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("嵌套 pg_get_serial_sequence 的 currval = %q", got)
	}

	// nextval 分配下一个值；lastval 返回最后一次分配的值
	res = run(t, d, s, `SELECT nextval('seq_t_id_seq'), lastval()`)
	row := res[0].Rows[0]
	if fmt.Sprint(row[0]) != "3" || fmt.Sprint(row[1]) != "3" {
		t.Fatalf("nextval/lastval = %v，期望 3/3", row)
	}

	// 不存在的序列要报错
	if err := runErr(t, d, s, `SELECT currval('no_such_seq')`); err == nil {
		t.Fatal("不存在的序列应当报错")
	}
}

// SAVEPOINT：ORM 的嵌套事务（Npgsql 的 TransactionScope）靠它实现部分回滚。
func TestSavepoint(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sp_t (id int PRIMARY KEY)`)

	run(t, d, s, `BEGIN`)
	run(t, d, s, `INSERT INTO sp_t VALUES (1)`)
	run(t, d, s, `SAVEPOINT sp1`)
	run(t, d, s, `INSERT INTO sp_t VALUES (2)`)
	run(t, d, s, `ROLLBACK TO SAVEPOINT sp1`)
	run(t, d, s, `INSERT INTO sp_t VALUES (3)`)
	run(t, d, s, `RELEASE SAVEPOINT sp1`)
	run(t, d, s, `COMMIT`)

	res := run(t, d, s, `SELECT id FROM sp_t ORDER BY id`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应有 2 行（1 和 3），实际 %v", res[0].Rows)
	}
	if fmt.Sprint(res[0].Rows[0][0]) != "1" || fmt.Sprint(res[0].Rows[1][0]) != "3" {
		t.Fatalf("行内容不对: %v", res[0].Rows)
	}

	// 多层保存点：回滚到中间那一层，它之上的都丢弃
	run(t, d, s, `BEGIN`)
	run(t, d, s, `SAVEPOINT a`)
	run(t, d, s, `INSERT INTO sp_t VALUES (10)`)
	run(t, d, s, `SAVEPOINT b`)
	run(t, d, s, `INSERT INTO sp_t VALUES (11)`)
	run(t, d, s, `ROLLBACK TO a`)
	run(t, d, s, `COMMIT`)
	res = run(t, d, s, `SELECT count(*) FROM sp_t WHERE id >= 10`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("回滚到 a 之后 10/11 都应消失，实际 count=%s", got)
	}

	// 事务中毒后能靠 ROLLBACK TO 救回来（这是 TransactionScope 的关键）
	run(t, d, s, `BEGIN`)
	run(t, d, s, `INSERT INTO sp_t VALUES (20)`)
	run(t, d, s, `SAVEPOINT s2`)
	if err := runErr(t, d, s, `INSERT INTO sp_t VALUES (20)`); err == nil {
		t.Fatal("重复主键应当报错")
	}
	run(t, d, s, `ROLLBACK TO SAVEPOINT s2`) // 中毒状态解除
	run(t, d, s, `INSERT INTO sp_t VALUES (21)`)
	run(t, d, s, `COMMIT`)

	res = run(t, d, s, `SELECT count(*) FROM sp_t WHERE id IN (20, 21)`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("20 和 21 都应在，实际 count=%s", got)
	}

	// 不存在的保存点要报错
	run(t, d, s, `BEGIN`)
	if err := runErr(t, d, s, `ROLLBACK TO SAVEPOINT nope`); err == nil {
		t.Fatal("回滚到不存在的保存点应当报错")
	}
	run(t, d, s, `ROLLBACK`)
}

// 窗口函数没实现，但要给出「不支持」而不是含糊的 syntax error。
func TestWindowFunctionMessage(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE win_t (id int)`)

	err := runErr(t, d, s, `SELECT row_number() OVER (ORDER BY id) FROM win_t`)
	if err == nil {
		t.Fatal("窗口函数应当报错")
	}
	if !strings.Contains(err.Error(), "window functions") {
		t.Fatalf("错误消息应说明是窗口函数不支持，实际: %v", err)
	}
}
