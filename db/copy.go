package db

import (
	"godb/lock"
	"godb/sql"
	"godb/txn"
)

// COPY 要自己开语句与事务，所以放在 db 层（和 execOneInner 一样的加锁流程），
// 而不是让 exec 包去碰锁。

// CopyIn 把 COPY 文本数据导入表里，返回导入行数。
func (d *DB) CopyIn(sess *txn.Session, c *sql.Copy, data []byte) (int64, error) {
	if sess.InTx() && sess.Aborted() {
		return 0, ErrAborted
	}
	// 写这张表 → X 锁
	intents := map[string]lock.Mode{c.Table: lock.Exclusive}
	d.Store.LoadTables([]string{c.Table})

	stmt, err := sess.BeginStatement(intents)
	if err != nil {
		return 0, err
	}
	d.Store.SchemaMu.RLock()
	n, execErr := d.Engine.CopyFromText(stmt.Tx(), c, data)
	d.Store.SchemaMu.RUnlock()

	if err := stmt.End(execErr); err != nil {
		return 0, err
	}
	return n, nil
}

// CopyOut 导出整表（或指定列）为 COPY 文本行，返回行数。
func (d *DB) CopyOut(sess *txn.Session, c *sql.Copy) ([]string, int64, error) {
	if sess.InTx() && sess.Aborted() {
		return nil, 0, ErrAborted
	}
	intents := map[string]lock.Mode{c.Table: lock.Shared}
	d.Store.LoadTables([]string{c.Table})

	stmt, err := sess.BeginStatement(intents)
	if err != nil {
		return nil, 0, err
	}
	d.Store.SchemaMu.RLock()
	lines, n, execErr := d.Engine.CopyOutText(stmt.Tx(), c)
	d.Store.SchemaMu.RUnlock()

	if err := stmt.End(execErr); err != nil {
		return nil, 0, err
	}
	return lines, n, nil
}
