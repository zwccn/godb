package db

import (
	"strings"
	"testing"

	"godb/txn"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, warns, err := OpenManager(t.TempDir(), Options{Isolation: txn.TableLock})
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	for _, w := range warns {
		t.Fatalf("打开库时报警告: %v", w)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// 每个库是独立的一套 catalog：在 A 建的表 B 看不到。
func TestManagerIsolatesDatabases(t *testing.T) {
	m := newTestManager(t)
	def := m.Default()
	run(t, def, "s", `CREATE TABLE only_default (id int4 PRIMARY KEY)`)

	sales, err := m.Create("sales")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	run(t, sales, "s", `CREATE TABLE orders (id int4 PRIMARY KEY, amount int4)`)
	run(t, sales, "s", `INSERT INTO orders VALUES (1, 100)`)

	// 默认库没有 orders
	if err := runErr(t, def, "s", `SELECT * FROM orders`); err == nil {
		t.Fatal("默认库不该看到 sales 的表")
	}
	// sales 没有 only_default
	if err := runErr(t, sales, "s", `SELECT * FROM only_default`); err == nil {
		t.Fatal("sales 不该看到默认库的表")
	}
	// 各看各的
	assertRows(t, run(t, sales, "s", `SELECT count(*) FROM orders`), []string{"1"})
	assertRows(t, run(t, sales, "s", `SELECT current_database()`), []string{"sales"})
	assertRows(t, run(t, def, "s", `SELECT current_database()`), []string{"godb"})
}

// pg_database 列出所有库，默认库排第一；oid 稳定且不重复。
func TestManagerPgDatabase(t *testing.T) {
	m := newTestManager(t)
	def := m.Default()
	if _, err := m.Create("sales"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Create("archive"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	assertRows(t, run(t, def, "s", `SELECT count(*) FROM pg_database`), []string{"3"})
	rows := rowsOf(t, run(t, def, "s", `SELECT datname FROM pg_database ORDER BY datname`))
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	if strings.Join(got, ",") != "archive,godb,sales" {
		t.Fatalf("库列表 = %v", got)
	}
	// oid 不能重复，且默认库固定 16384
	assertRows(t, run(t, def, "s", `SELECT count(DISTINCT oid) FROM pg_database`), []string{"3"})
	assertRows(t, run(t, def, "s", `SELECT oid FROM pg_database WHERE datname = 'godb'`), []string{"16384"})

	// 从任意库看到的列表都一样（pg_database 是全局的）
	sales, _ := m.Get("sales")
	assertRows(t, run(t, sales, "s", `SELECT count(*) FROM pg_database`), []string{"3"})
}

// CREATE / DROP DATABASE 走 SQL；默认库和当前库都不能删。
func TestCreateDropDatabaseSQL(t *testing.T) {
	m := newTestManager(t)
	def := m.Default()

	res := run(t, def, "s", `CREATE DATABASE shop`)
	if res[0].Tag != "CREATE DATABASE" {
		t.Fatalf("tag = %q", res[0].Tag)
	}
	assertRows(t, run(t, def, "s", `SELECT count(*) FROM pg_database WHERE datname = 'shop'`), []string{"1"})

	// 重名
	if err := runErr(t, def, "s", `CREATE DATABASE shop`); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("重名应报 already exists，实际 %v", err)
	}
	// 默认库不能删
	if err := runErr(t, def, "s", `DROP DATABASE godb`); err == nil {
		t.Fatal("默认库不该能删")
	}
	// 当前连着的库不能删
	shop, err := m.Get("shop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := runErr(t, shop, "s", `DROP DATABASE shop`); err == nil ||
		!strings.Contains(err.Error(), "currently open") {
		t.Fatalf("当前库不该能删，实际 %v", err)
	}
	// 从别的库删就行
	run(t, def, "s", `DROP DATABASE shop`)
	assertRows(t, run(t, def, "s", `SELECT count(*) FROM pg_database WHERE datname = 'shop'`), []string{"0"})
	if _, err := m.Get("shop"); err == nil {
		t.Fatal("删掉的库不该还能取到")
	}
	// IF EXISTS
	run(t, def, "s", `DROP DATABASE IF EXISTS shop`)
	if err := runErr(t, def, "s", `DROP DATABASE shop`); err == nil {
		t.Fatal("不带 IF EXISTS 时删不存在的库应报错")
	}
}

// 库名要能直接当目录名用，非法名字一律拒绝（挡住路径穿越）。
func TestDatabaseNameValidation(t *testing.T) {
	m := newTestManager(t)
	bad := []string{"", "1abc", "a-b", "../etc", "a/b", "a b", "template0", strings.Repeat("x", 64)}
	for _, name := range bad {
		if _, err := m.Create(name); err == nil {
			t.Errorf("库名 %q 应被拒绝", name)
		}
	}
	if _, err := m.Create("ok_1"); err != nil {
		t.Fatalf("正常库名应能建: %v", err)
	}
	// 名字统一转小写
	if _, err := m.Create("MyDB"); err != nil {
		t.Fatalf("大写应被归一成小写: %v", err)
	}
	if !m.Exists("mydb") {
		t.Fatal("mydb 应该存在")
	}
}

// 有**进行中的事务**时不能删库；只有空闲会话不挡。
func TestDropRefusesOpenTransactions(t *testing.T) {
	m := newTestManager(t)
	shop, err := m.Create("shop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 空闲会话不挡删除（Web 控制台的会话关浏览器也不会消失，挡了就没法用了）
	shop.Session("someone")
	run(t, shop, "s", `BEGIN`)
	if err := m.Drop("shop"); err == nil || !strings.Contains(err.Error(), "being accessed") {
		t.Fatalf("有进行中的事务时不该删，实际 %v", err)
	}
	// 库还在，事务也还能继续
	assertRows(t, run(t, shop, "s", `SELECT current_database()`), []string{"shop"})
	run(t, shop, "s", `ROLLBACK`)

	// 事务结束后就能删了
	if err := m.Drop("shop"); err != nil {
		t.Fatalf("ROLLBACK 之后应能删: %v", err)
	}
	if _, err := m.Get("shop"); err == nil {
		t.Fatal("删掉的库不该还能取到")
	}
	// 还挂在旧对象上的连接会拿到明确错误，而不是撞上已关闭的 Store
	if err := runErr(t, shop, "s", `SELECT 1`); err == nil ||
		!strings.Contains(err.Error(), "has been dropped") {
		t.Fatalf("删库后旧连接应报 has been dropped，实际 %v", err)
	}
}

// 库要能跨重启活下来，数据也在。
func TestManagerPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	m1, _, err := OpenManager(dir, Options{Isolation: txn.TableLock})
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	shop, err := m1.Create("shop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	run(t, shop, "s", `CREATE TABLE t (id int4 PRIMARY KEY, v text)`)
	run(t, shop, "s", `INSERT INTO t VALUES (1, 'a'), (2, 'b')`)
	if err := m1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m2, warns, err := OpenManager(dir, Options{Isolation: txn.TableLock})
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer m2.Close()
	if len(warns) > 0 {
		t.Fatalf("重开时报警告: %v", warns)
	}
	d, err := m2.Get("shop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM t`), []string{"2"})
	assertRows(t, run(t, d, "s", `SELECT current_database()`), []string{"shop"})
	assertRows(t, run(t, m2.Default(), "s", `SELECT count(*) FROM pg_database`), []string{"2"})
}

// 空库名 / postgres 都落到默认库（客户端工具默认填的就是 postgres）。
func TestGetDefaultAliases(t *testing.T) {
	m := newTestManager(t)
	if d, err := m.Get(""); err != nil || d.Name != "godb" {
		t.Fatalf(`Get("") = %v, %v`, d, err)
	}
	if d, err := m.Get("postgres"); err != nil || d.Name != "godb" {
		t.Fatalf(`Get("postgres") = %v, %v`, d, err)
	}
	if _, err := m.Get("nope"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("不存在的库应报 does not exist，实际 %v", err)
	}
}

// 事务里不能建库删库（PostgreSQL 也不允许）。
func TestCreateDropDatabaseInTx(t *testing.T) {
	m := newTestManager(t)
	def := m.Default()
	run(t, def, "s", `BEGIN`)
	if err := runErr(t, def, "s", `CREATE DATABASE txdb`); err == nil ||
		!strings.Contains(err.Error(), "transaction block") {
		t.Fatalf("事务里建库应报错，实际 %v", err)
	}
	run(t, def, "s", `ROLLBACK`)
}
