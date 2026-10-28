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

// ON CONFLICT（upsert）：FreeSql / EF Core 的插入或更新全靠它。
func TestOnConflict(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE oc_t (id int PRIMARY KEY, name text, n int DEFAULT 0)`)
	run(t, d, s, `INSERT INTO oc_t VALUES (1, 'a', 1)`)

	// DO NOTHING：冲突行跳过，标签里的行数是真正插入的行数
	res := run(t, d, s, `INSERT INTO oc_t VALUES (1, 'zzz', 9) ON CONFLICT DO NOTHING`)
	if got := res[0].Tag; got != "INSERT 0 0" {
		t.Fatalf("DO NOTHING 的标签应为 INSERT 0 0，实际 %q", got)
	}
	res = run(t, d, s, `SELECT name, n FROM oc_t WHERE id = 1`)
	if fmt.Sprint(res[0].Rows[0][0]) != "a" || fmt.Sprint(res[0].Rows[0][1]) != "1" {
		t.Fatalf("DO NOTHING 不应改数据，实际 %v", res[0].Rows[0])
	}

	// 指定冲突列
	run(t, d, s, `INSERT INTO oc_t VALUES (1, 'yyy', 8) ON CONFLICT (id) DO NOTHING`)

	// DO UPDATE + excluded + RETURNING
	res = run(t, d, s, `
		INSERT INTO oc_t VALUES (1, 'b', 2)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, n = excluded.n
		RETURNING id, name, n`)
	row := res[0].Rows[0]
	if fmt.Sprint(row[0]) != "1" || fmt.Sprint(row[1]) != "b" || fmt.Sprint(row[2]) != "2" {
		t.Fatalf("DO UPDATE 的 RETURNING = %v，期望 1/b/2", row)
	}
	res = run(t, d, s, `SELECT name, n FROM oc_t WHERE id = 1`)
	if fmt.Sprint(res[0].Rows[0][0]) != "b" || fmt.Sprint(res[0].Rows[0][1]) != "2" {
		t.Fatalf("DO UPDATE 之后数据 = %v", res[0].Rows[0])
	}

	// 用旧值参与运算（oc_t.n + excluded.n）
	run(t, d, s, `INSERT INTO oc_t VALUES (1, 'c', 5) ON CONFLICT (id) DO UPDATE SET n = oc_t.n + excluded.n`)
	res = run(t, d, s, `SELECT n FROM oc_t WHERE id = 1`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "7" {
		t.Fatalf("n 应为 2+5=7，实际 %s", got)
	}

	// DO UPDATE ... WHERE 不成立时等于 DO NOTHING
	run(t, d, s, `INSERT INTO oc_t VALUES (1, 'd', 0) ON CONFLICT (id) DO UPDATE SET name = 'd' WHERE oc_t.n > 100`)
	res = run(t, d, s, `SELECT name, n FROM oc_t WHERE id = 1`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "b" {
		t.Fatalf("WHERE 不成立时不应更新，实际 name=%s", got)
	}
	if got := fmt.Sprint(res[0].Rows[0][1]); got != "7" {
		t.Fatalf("WHERE 不成立时 n 应保持 7，实际 %s", got)
	}

	// 没有冲突时正常插入
	res = run(t, d, s, `INSERT INTO oc_t VALUES (2, 'e', 1) ON CONFLICT (id) DO NOTHING`)
	if got := res[0].Tag; got != "INSERT 0 1" {
		t.Fatalf("无冲突时应插入 1 行，实际 %q", got)
	}

	// 唯一索引上的冲突
	run(t, d, s, `CREATE UNIQUE INDEX oc_t_name_uq ON oc_t (name)`)
	if err := runErr(t, d, s, `INSERT INTO oc_t VALUES (3, 'e', 1) ON CONFLICT (id) DO NOTHING`); err == nil {
		t.Fatal("ON CONFLICT (id) 不该吞掉 name 唯一索引的冲突")
	}
	run(t, d, s, `INSERT INTO oc_t VALUES (3, 'e', 1) ON CONFLICT (name) DO NOTHING`)
	res = run(t, d, s, `SELECT count(*) FROM oc_t`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "2" {
		t.Fatalf("应有 2 行，实际 %s", got)
	}

	// 同一条语句里两次相同键 + DO UPDATE：PostgreSQL 会拒绝
	if err := runErr(t, d, s,
		`INSERT INTO oc_t VALUES (9,'x',1),(9,'y',2) ON CONFLICT (id) DO UPDATE SET name = excluded.name`); err == nil {
		t.Fatal("同一语句两次命中同一行时 DO UPDATE 应当报错")
	}
}

// UPDATE ... FROM / DELETE ... USING：批量同步与按关联条件清理。
func TestUpdateFromDeleteUsing(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE uf_t (id int PRIMARY KEY, v text, n int DEFAULT 0)`)
	run(t, d, s, `CREATE TABLE uf_s (id int PRIMARY KEY, v text, n int)`)
	run(t, d, s, `INSERT INTO uf_t VALUES (1, 'a', 0), (2, 'b', 0), (3, 'c', 0)`)
	run(t, d, s, `INSERT INTO uf_s VALUES (1, 'A', 10), (3, 'C', 30)`)

	// UPDATE ... FROM：注意 uf_t 的 v 和 uf_s 的 v 同名，必须带限定符
	run(t, d, s, `UPDATE uf_t SET v = s.v, n = s.n FROM uf_s s WHERE uf_t.id = s.id`)
	res := run(t, d, s, `SELECT id, v, n FROM uf_t ORDER BY id`)
	want := [][3]string{{"1", "A", "10"}, {"2", "b", "0"}, {"3", "C", "30"}}
	if len(res[0].Rows) != 3 {
		t.Fatalf("应有 3 行，实际 %d", len(res[0].Rows))
	}
	for i, w := range want {
		row := res[0].Rows[i]
		got := [3]string{fmt.Sprint(row[0]), fmt.Sprint(row[1]), fmt.Sprint(row[2])}
		if got != w {
			t.Fatalf("第 %d 行 = %v，期望 %v", i+1, got, w)
		}
	}

	// DELETE ... USING：只删与 uf_s 对得上的行
	run(t, d, s, `DELETE FROM uf_t USING uf_s s WHERE uf_t.id = s.id AND s.n > 20`)
	res = run(t, d, s, `SELECT id FROM uf_t ORDER BY id`)
	if len(res[0].Rows) != 2 {
		t.Fatalf("应只剩 2 行，实际 %v", res[0].Rows)
	}
	if fmt.Sprint(res[0].Rows[0][0]) != "1" || fmt.Sprint(res[0].Rows[1][0]) != "2" {
		t.Fatalf("剩余行不对: %v", res[0].Rows)
	}

	// 别名要能用在 SET 右侧，也能用在 WHERE 左侧
	run(t, d, s, `UPDATE uf_t AS t SET n = src.n FROM uf_s AS src WHERE t.id = src.id`)
	res = run(t, d, s, `SELECT n FROM uf_t WHERE id = 1`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "10" {
		t.Fatalf("带别名的 UPDATE FROM 后 n = %s，期望 10", got)
	}

	// 关联表为空时不该更新任何行
	run(t, d, s, `CREATE TABLE uf_empty (id int PRIMARY KEY, n int)`)
	run(t, d, s, `UPDATE uf_t SET n = 999 FROM uf_empty e WHERE uf_t.id = e.id`)
	res = run(t, d, s, `SELECT count(*) FROM uf_t WHERE n = 999`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "0" {
		t.Fatalf("关联表为空时不应更新任何行，实际 %s", got)
	}
}

// nextval 必须在**只读语句**里也落盘。
//
// 之前 Commit 是遍历「有行变更的表」来构造变更集的，纯 SELECT 没有行变更，
// 计数器既不更新也不进日志 —— 每条语句都返回同一个值，多个客户端会拿到
// 重复的自增 ID。这里同时覆盖 setval 的两种语义。
func TestSequencePersistsAcrossStatements(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE sq_t (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, v text)`)
	run(t, d, s, `INSERT INTO sq_t (v) VALUES ('a'), ('b')`)

	// 1) 连续两条独立语句，必须递增（修复前都是 3）
	for want := 3; want <= 4; want++ {
		res := run(t, d, s, `SELECT nextval('sq_t_id_seq')`)
		if got := fmt.Sprint(res[0].Rows[0][0]); got != fmt.Sprint(want) {
			t.Fatalf("第 %d 次 nextval = %s，期望 %d", want-2, got, want)
		}
	}

	// 2) 显式事务里只有 SELECT，提交后同样生效
	run(t, d, s, `BEGIN`)
	if got := fmt.Sprint(run(t, d, s, `SELECT nextval('sq_t_id_seq')`)[0].Rows[0][0]); got != "5" {
		t.Fatalf("事务内 nextval = %s，期望 5", got)
	}
	run(t, d, s, `COMMIT`)
	if got := fmt.Sprint(run(t, d, s, `SELECT nextval('sq_t_id_seq')`)[0].Rows[0][0]); got != "6" {
		t.Fatalf("提交后 nextval = %s，期望 6", got)
	}

	// 3) setval(seq, n)：下一次 nextval 是 n+1
	if got := fmt.Sprint(run(t, d, s, `SELECT setval('sq_t_id_seq', 100)`)[0].Rows[0][0]); got != "100" {
		t.Fatalf("setval 返回值 = %s，期望 100", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT nextval('sq_t_id_seq')`)[0].Rows[0][0]); got != "101" {
		t.Fatalf("setval(100) 之后 nextval = %s，期望 101", got)
	}

	// 4) setval(seq, n, false)：n 本身还没被用掉，下一次就是 n；返回值仍是 n
	if got := fmt.Sprint(run(t, d, s, `SELECT setval('sq_t_id_seq', 200, false)`)[0].Rows[0][0]); got != "200" {
		t.Fatalf("setval 返回值 = %s，期望 200", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT nextval('sq_t_id_seq')`)[0].Rows[0][0]); got != "200" {
		t.Fatalf("setval(200,false) 之后 nextval = %s，期望 200", got)
	}

	// 5) 计数器被推上去之后，自动 INSERT 不会撞主键
	res := run(t, d, s, `INSERT INTO sq_t (v) VALUES ('c') RETURNING id`)
	if got := fmt.Sprint(res[0].Rows[0][0]); got != "201" {
		t.Fatalf("INSERT 拿到 id = %s，期望 201", got)
	}

	// 6) setval 允许把计数器往下调（灌完数据后拨回 max(id) 是常见用法）；
	//    nextval 则只能单调递增，两者在提交时必须分开处理。
	if got := fmt.Sprint(run(t, d, s, `SELECT setval('sq_t_id_seq', 2)`)[0].Rows[0][0]); got != "2" {
		t.Fatalf("setval(2) 返回值 = %s，期望 2", got)
	}
	if got := fmt.Sprint(run(t, d, s, `SELECT nextval('sq_t_id_seq')`)[0].Rows[0][0]); got != "3" {
		t.Fatalf("调低到 2 之后 nextval = %s，期望 3", got)
	}

	// 7) 非法值要拒绝，不能悄悄改坏计数器
	if err := runErr(t, d, s, `SELECT setval('sq_t_id_seq', 0)`); err == nil {
		t.Fatal("setval(0) 应当报错")
	}
	if err := runErr(t, d, s, `SELECT setval('no_such_seq', 1)`); err == nil {
		t.Fatal("不存在的序列应当报错")
	}
}
