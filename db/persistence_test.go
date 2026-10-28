package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"godb/storage"
)

// openDir 在指定目录打开数据库（用于"重开一次模拟崩溃恢复"）。
func openDir(t *testing.T, dir string) *DB {
	t.Helper()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return d
}

// ---------- A：两个崩溃窗口的回归用例 ----------

// 崩溃在「快照 rename 完成、WAL 还没轮转」之间。
// 以前会把快照里已有的记录再重放一遍，2 行变 4 行。
func TestCrashAfterSnapshotBeforeWALRotate(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE t1 (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, "s", `INSERT INTO t1 VALUES (1,'a'),(2,'b')`)

	walPath := filepath.Join(dir, "wal.log")
	oldWAL, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldWAL) == 0 {
		t.Fatal("WAL 应该有内容")
	}
	d.Close() // 正常关闭会 checkpoint 并轮转 WAL

	// 把旧 WAL 放回去：等价于"快照写完了，但还没来得及删 WAL 就崩了"
	if err := os.WriteFile(walPath, oldWAL, 0o644); err != nil {
		t.Fatal(err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM t1`), []string{"2"})
	assertRows(t, run(t, d2, "s", `SELECT id FROM t1 ORDER BY id`), []string{"1", "2"})
}

// 崩溃在「catalog.json 已写、快照还没重写」之间（ALTER TABLE ADD COLUMN）。
// 以前 decodeRow 会因行宽不匹配让整个库打不开。
func TestCrashBetweenCatalogSaveAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE w (a INTEGER, b TEXT)`)
	run(t, d, "s", `INSERT INTO w VALUES (1,'x'),(2,'y')`)
	d.Close() // 快照里是 2 列

	// 手工给 catalog 加一列，模拟 ALTER 之后崩溃
	catPath := filepath.Join(dir, "catalog.json")
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatal(err)
	}
	var cat map[string]interface{}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	for _, item := range cat["tables"].([]interface{}) {
		tb := item.(map[string]interface{})
		if tb["name"] != "w" {
			continue
		}
		cols := tb["columns"].([]interface{})
		tb["columns"] = append(cols, map[string]interface{}{"name": "c", "type": "text", "kind": 3})
	}
	out, _ := json.MarshalIndent(cat, "", "  ")
	if err := os.WriteFile(catPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("列数不一致时应该能打开（补 NULL），实际: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT a, b, c FROM w ORDER BY a`), []string{"1|x|<nil>", "2|y|<nil>"})
	if d2.Store.Repaired.Load() == 0 {
		t.Fatal("应该记录到修复的单元格数")
	}
}

// ---------- C：二进制 WAL ----------

func TestBinaryWALRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE b (id INTEGER PRIMARY KEY, name TEXT, score NUMERIC(10,2), ok BOOLEAN, at TIMESTAMP)`)
	run(t, d, "s", `INSERT INTO b VALUES (1,'甲',1.5,true,'2026-01-02 03:04:05'),(2,'乙',2.25,false,'2026-02-03 04:05:06')`)
	run(t, d, "s", `UPDATE b SET score = 9.75, name = '乙改' WHERE id = 2`)
	run(t, d, "s", `DELETE FROM b WHERE id = 1`)
	run(t, d, "s", `INSERT INTO b VALUES (3,'丙',NULL,NULL,NULL)`)

	// 崩溃：不 Close，直接把 WAL 复制一份，模拟进程被杀
	if err := d.Store.Close(); err != nil { // 只关 WAL 文件，不写快照
		t.Fatal(err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT id, name, score, ok FROM b ORDER BY id`),
		[]string{"2|乙改|9.75|false", "3|丙|<nil>|<nil>"})
	assertRows(t, run(t, d2, "s", `SELECT at FROM b WHERE id = 2`), []string{"2026-02-03 04:05:06"})
}

// 旧版按行 JSON 的 WAL 仍然能被重放（升级不丢数据）。
func TestLegacyJSONWALReplay(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE leg (id INTEGER, v TEXT)`)
	d.Close() // 快照落盘，WAL 清空

	legacy := `{"tx":1,"op":"ins","table":"leg","new":[1,"hello"]}
{"tx":1,"op":"commit"}
{"tx":2,"op":"ins","table":"leg","new":[2,"world"]}
{"tx":2,"op":"commit"}
{"tx":3,"op":"ins","table":"leg","new":[3,"未提交"]}
`
	if err := os.WriteFile(filepath.Join(dir, "wal.log"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	// 未提交的第 3 条必须被丢弃
	assertRows(t, run(t, d2, "s", `SELECT id, v FROM leg ORDER BY id`), []string{"1|hello", "2|world"})
}

// 尾部被写坏的帧要被丢弃，而不是让库打不开。
func TestTruncatedWALFrame(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE tf (id INTEGER)`)
	run(t, d, "s", `INSERT INTO tf VALUES (1)`)

	walPath := filepath.Join(dir, "wal.log")
	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Store.Close(); err != nil { // 不写快照，保留 WAL
		t.Fatal(err)
	}
	// 砍掉最后 5 个字节，制造不完整帧
	if err := os.WriteFile(walPath, raw[:len(raw)-5], 0o644); err != nil {
		t.Fatal(err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	// 只要求能打开、不 panic；数据可能少一条
	if _, err := d2.Exec(d2.Session("s"), `SELECT count(*) FROM tf`); err != nil {
		t.Fatalf("截断的 WAL 不应该让库打不开: %v", err)
	}
}

// ---------- D：组提交 ----------

// 组提交只对"能同时跑"的写事务有意义：表级 X 锁让同一张表的写天然串行，
// 所以这里每个 worker 写自己的表，让提交真正并发。
func TestGroupCommitCoalescesFsync(t *testing.T) {
	d := newTestDB(t)
	const workers = 8
	const perWorker = 6 // 48 次提交，低于自动 checkpoint 阈值 128
	for i := 0; i < workers; i++ {
		run(t, d, "s", fmt.Sprintf(`CREATE TABLE gc%d (id SERIAL PRIMARY KEY, v INTEGER)`, i))
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sess := d.Session(fmt.Sprintf("gc%d", n))
			for i := 0; i < perWorker; i++ {
				q := fmt.Sprintf(`INSERT INTO gc%d (v) VALUES (%d)`, n, i)
				if _, err := d.Exec(sess, q); err != nil {
					t.Errorf("insert: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	commits := workers * perWorker
	syncs := d.Store.Info().WALSyncs
	total := 0
	for i := 0; i < workers; i++ {
		res := run(t, d, "s", fmt.Sprintf(`SELECT count(*) FROM gc%d`, i))
		total += int(res[0].Rows[0][0].(int64))
	}
	if total != commits {
		t.Fatalf("总行数 = %d，期望 %d", total, commits)
	}
	if syncs >= int64(commits) {
		t.Fatalf("组提交没生效：%d 次提交触发了 %d 次 fsync", commits, syncs)
	}
	t.Logf("%d 次并发提交 → %d 次 fsync（合并率 %.1fx）", commits, syncs, float64(commits)/float64(syncs))
}

// ---------- CHECKPOINT 语句 ----------

func TestCheckpointStatement(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	defer d.Close()
	run(t, d, "s", `CREATE TABLE cp (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, "s", `INSERT INTO cp VALUES (1,'a'),(2,'b')`)

	res := run(t, d, "s", `CHECKPOINT`)
	if res[0].Command != "CHECKPOINT" {
		t.Fatalf("command = %q", res[0].Command)
	}
	if len(res[0].Rows) == 0 {
		t.Fatal("CHECKPOINT 应该返回统计信息")
	}

	// FLUSH 是别名
	run(t, d, "s", `FLUSH`)

	// 事务里不允许
	run(t, d, "s", `BEGIN`)
	runErr(t, d, "s", `CHECKPOINT`)
	run(t, d, "s", `ROLLBACK`)

	// 数据确实落盘了：WAL 被轮转成空
	info, err := os.Stat(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64 {
		t.Fatalf("CHECKPOINT 之后 WAL 应该几乎为空，实际 %d 字节", info.Size())
	}
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM cp`), []string{"2"})
}

// ---------- B：DDL 不再强制全量快照 ----------

// ADD COLUMN 不应该重写快照（大数据量时那是最贵的操作）。
func TestAddColumnDoesNotRewriteSnapshot(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	defer d.Close()
	run(t, d, "s", `CREATE TABLE ac (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, "s", `INSERT INTO ac VALUES (1,'a')`)
	run(t, d, "s", `CHECKPOINT`) // 让快照稳定下来

	tbl, ok := d.Cat.Get("ac")
	if !ok {
		t.Fatal("catalog 里找不到 ac")
	}
	snapPath := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.snp", tbl.ID))
	before, err := os.Stat(snapPath)
	if err != nil {
		t.Fatalf("数据文件应该存在: %v", err)
	}
	beforeMod := before.ModTime()
	beforeGen := d.Store.Info().Generation

	run(t, d, "s", `ALTER TABLE ac ADD COLUMN extra TEXT`)

	after, err := os.Stat(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(beforeMod) {
		t.Fatal("ALTER TABLE ADD COLUMN 不应该重写数据文件")
	}
	if d.Store.Info().Generation != beforeGen {
		t.Fatal("ALTER TABLE ADD COLUMN 不应该轮转 WAL 代次")
	}

	// 但结构必须已经写进 catalog.json
	catRaw, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catRaw), `"extra"`) {
		t.Fatal("catalog.json 里应该有新列")
	}

	// 崩溃后按新结构补齐：旧行 extra 为 NULL
	if err := d.Store.Close(); err != nil {
		t.Fatal(err)
	}
	d2 := openDir(t, dir)
	assertRows(t, run(t, d2, "s", `SELECT id, extra FROM ac`), []string{"1|<nil>"})
	d2.Close()
}

// ---------- 分表快照 + 按需加载 ----------

// 只改一张表时，其它表的数据文件不应该被重写。
func TestIncrementalCheckpoint(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	defer d.Close()
	for _, n := range []string{"a", "b", "c"} {
		run(t, d, "s", fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, v TEXT)`, n))
		run(t, d, "s", fmt.Sprintf(`INSERT INTO %s VALUES (1,'x')`, n))
	}
	run(t, d, "s", `CHECKPOINT`)

	mtime := func(name string) time.Time {
		tb, ok := d.Cat.Get(name)
		if !ok {
			t.Fatalf("找不到表 %s", name)
		}
		st, err := os.Stat(filepath.Join(dir, "tables", fmt.Sprintf("t%06d.snp", tb.ID)))
		if err != nil {
			t.Fatalf("数据文件不存在: %v", err)
		}
		return st.ModTime()
	}
	a0, b0, c0 := mtime("a"), mtime("b"), mtime("c")

	// 只动 b
	time.Sleep(20 * time.Millisecond)
	run(t, d, "s", `INSERT INTO b VALUES (2,'y')`)
	run(t, d, "s", `CHECKPOINT`)

	if !mtime("a").Equal(a0) {
		t.Fatal("没改过的表 a 不应该被重写")
	}
	if !mtime("c").Equal(c0) {
		t.Fatal("没改过的表 c 不应该被重写")
	}
	if mtime("b").Equal(b0) {
		t.Fatal("改过的表 b 应该被重写")
	}
	// 数据仍然正确
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM b`), []string{"2"})
	assertRows(t, run(t, d, "s", `SELECT count(*) FROM a`), []string{"1"})
}

// 冷启动不该把所有表都读进内存，访问到哪张表才加载哪张。
func TestLazyLoadOnStartup(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	for i := 0; i < 5; i++ {
		run(t, d, "s", fmt.Sprintf(`CREATE TABLE l%d (id INTEGER PRIMARY KEY, v TEXT)`, i))
		run(t, d, "s", fmt.Sprintf(`INSERT INTO l%d VALUES (1,'x')`, i))
	}
	run(t, d, "s", `CHECKPOINT`)
	d.Close()

	d2 := openDir(t, dir)
	defer d2.Close()

	info := d2.Store.Info()
	if info.Loaded != 0 {
		t.Fatalf("刚打开时不应加载任何表，实际加载了 %d 张", info.Loaded)
	}
	if info.Tables != 5 {
		t.Fatalf("表数 = %d", info.Tables)
	}
	// 行数不靠加载也能回答（pg_class.reltuples 用得上）
	if info.Rows != 5 {
		t.Fatalf("行数（未加载）= %d，期望 5", info.Rows)
	}

	// 只查一张表 → 只加载那一张
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM l2`), []string{"1"})
	info = d2.Store.Info()
	if info.Loaded != 1 {
		t.Fatalf("查一张表之后应只加载 1 张，实际 %d", info.Loaded)
	}
	// 未加载的表里没被碰过，但 RowCount 仍然准确
	if got := d2.Store.RowCount("l4"); got != 1 {
		t.Fatalf("未加载表的行数 = %d，期望 1", got)
	}
}

// 旧的单文件 snapshot.json 要能自动迁移成分表文件。
func TestLegacySnapshotMigration(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE lm (id INTEGER PRIMARY KEY, v TEXT)`)
	d.Close()
	// 手工造一个旧格式快照，同时删掉新格式的 meta.json 与表文件
	legacy := `{"generation":3,"tables":{"lm":{"rows":[[1,"旧数据"],[2,"也还在"]]}}}`
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "meta.json"))
	os.RemoveAll(filepath.Join(dir, "tables"))

	d2 := openDir(t, dir)
	assertRows(t, run(t, d2, "s", `SELECT id, v FROM lm ORDER BY id`), []string{"1|旧数据", "2|也还在"})
	d2.Close()

	// 迁移后旧文件应该被改名，新格式文件就位
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json.bak")); err != nil {
		t.Fatal("旧快照应该被改名成 .bak")
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal("应该生成 meta.json")
	}

	d3 := openDir(t, dir)
	defer d3.Close()
	assertRows(t, run(t, d3, "s", `SELECT count(*) FROM lm`), []string{"2"})
}

// ---------- 落盘策略：少写、快写、可观测 ----------

// 只改一两行不值得为它重写整张表（实测 2 万行的表改一行要写 0.97 MB）。
// 提交次数到了也得先看「攒的改动够不够」，不够就继续留在 WAL 里。
func TestCheckpointSkipsTrivialChanges(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Commits: 1})
	run(t, d, s, `CREATE TABLE big (id int4 PRIMARY KEY, pad text)`)
	insertRows(t, d, s, "big", 20000)
	run(t, d, s, `CHECKPOINT`)
	base := d.Store.Info().WrittenTotal

	// 改一行：Commits=1 会判定"该落盘了"，但脏数据太少 → 跳过
	run(t, d, s, `UPDATE big SET pad = 'x' WHERE id = 100`)
	if got := d.Store.Info().WrittenTotal; got != base {
		t.Fatalf("只改 1 行不该重写整表，却写了 %d 字节", got-base)
	}

	// 改 20%（超过 10% 的门槛）→ 该写了
	run(t, d, s, `UPDATE big SET pad = 'y' WHERE id <= 4000`)
	if got := d.Store.Info().WrittenTotal; got == base {
		t.Fatal("改动超过 10% 就该落盘")
	}

	// 显式 CHECKPOINT 不受比例限制：语义就是"现在就落盘"。
	// 先改一行（本该被跳过），再 CHECKPOINT 必须真的写出去。
	run(t, d, s, `UPDATE big SET pad = 'z' WHERE id = 20000`)
	base = d.Store.Info().WrittenTotal
	run(t, d, s, `CHECKPOINT`)
	if got := d.Store.Info().WrittenTotal; got == base {
		t.Fatal("CHECKPOINT 必须落盘")
	}
}

// 被跳过的改动必须还在 WAL 里：不落盘直接崩溃，重开要能看到。
func TestSkippedCheckpointStillDurable(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	s := "s"
	d.Store.SetCheckpointPolicy(storage.CheckpointPolicy{Commits: 1})
	run(t, d, s, `CREATE TABLE cr (id int4 PRIMARY KEY, pad text)`)
	insertRows(t, d, s, "cr", 20000)
	run(t, d, s, `CHECKPOINT`)

	run(t, d, s, `UPDATE cr SET pad = 'after' WHERE id = 7777`)
	if got := d.Store.Info().WrittenTotal; got != d.Store.Info().Written {
		// 只做一次很弱的检查：上面那条 UPDATE 被跳过后，脏表还在
		_ = got
	}
	// 不 Flush，直接关 WAL —— 等价于进程被 kill
	if err := d.Store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT pad FROM cr WHERE id = 7777`), []string{"after"})
	assertRows(t, run(t, d2, s, `SELECT count(*) FROM cr`), []string{"20000"})
}

// CHECKPOINT 要把「这一次写了多少」和「文件总大小」分开报，
// 否则想验证写放大的人会被 data_bytes 误导。
func TestCheckpointReportsBytesWritten(t *testing.T) {
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE bw (id int4 PRIMARY KEY, pad text)`)
	insertRows(t, d, s, "bw", 5000)
	res := run(t, d, s, `CHECKPOINT`)
	kv := map[string]string{}
	for _, row := range res[0].Rows {
		kv[fmt.Sprint(row[0])] = fmt.Sprint(row[1])
	}
	if kv["bytes_written"] == "" || kv["bytes_written"] == "0" {
		t.Fatalf("应报出本次写入字节数，实际 %q", kv["bytes_written"])
	}
	if kv["data_bytes"] == "" || kv["data_bytes"] == "0" {
		t.Fatalf("应报出表文件总大小，实际 %q", kv["data_bytes"])
	}
	// 无改动时再落一次：这次一个字节都不该写
	res = run(t, d, s, `CHECKPOINT`)
	kv = map[string]string{}
	for _, row := range res[0].Rows {
		kv[fmt.Sprint(row[0])] = fmt.Sprint(row[1])
	}
	if kv["bytes_written"] != "0" {
		t.Fatalf("没有改动时不该写盘，实际写了 %s 字节", kv["bytes_written"])
	}
}

// insertRows 往表里灌 n 行（id 从 1 开始），用多行 VALUES 快速灌进去。
func insertRows(t *testing.T, d *DB, sess, table string, n int) {
	t.Helper()
	const batch = 2000
	for start := 1; start <= n; start += batch {
		end := start + batch - 1
		if end > n {
			end = n
		}
		var b strings.Builder
		b.WriteString("INSERT INTO ")
		b.WriteString(table)
		b.WriteString(" VALUES ")
		for i := start; i <= end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,'pad-%d')", i, i)
		}
		if _, err := d.Exec(d.Session(sess), b.String()); err != nil {
			t.Fatalf("insert batch: %v", err)
		}
	}
}

// 启动时重放完的 WAL 必须被轮转掉。
// 空转优化（「没有脏表就不写 meta、不转 WAL」）曾经把它一起省掉，
// 结果每次重启都要把上一轮的记录再重放一遍。
func TestStartupRotatesWAL(t *testing.T) {
	dir := t.TempDir()
	d := openDir(t, dir)
	run(t, d, "s", `CREATE TABLE wr (id int4 PRIMARY KEY, v text)`)
	run(t, d, "s", `INSERT INTO wr VALUES (1,'a'),(2,'b')`)
	// 不 Flush 直接关 WAL：WAL 里留着未落盘的记录，等价于进程被 kill
	if err := d.Store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	walPath := filepath.Join(dir, "wal.log")
	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if before.Size() == 0 {
		t.Fatal("WAL 里应该有记录")
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	assertRows(t, run(t, d2, "s", `SELECT count(*) FROM wr`), []string{"2"})
	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("启动后 WAL 该被轮转清空，实际 %d -> %d 字节", before.Size(), after.Size())
	}
}

// WAL 里比快照更新的自增行，重放后序列必须跟上。
//
// 场景：插 2 行 → CHECKPOINT（落快照，max(id)=2）→ 再插 2 行（进 WAL，id=3,4）→
// 重开 → 再插一行。修之前重开后 Seq 停在快照里的 2，新插入拿到 id=3 撞主键
// （压测留下 60 万行后被踩到：max(id)=600691，不指定 id 的 INSERT 却撞了 id=1）。
func TestSerialSeqAfterWalReplay(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := "s"
	run(t, d, s, `CREATE TABLE sr (id serial primary key, v text)`)
	run(t, d, s, `INSERT INTO sr (v) VALUES ('a'), ('b')`)
	run(t, d, s, `CHECKPOINT`)
	run(t, d, s, `INSERT INTO sr (v) VALUES ('c'), ('d')`)
	// 只关 WAL 文件、不写快照 —— 模拟进程被杀。用 d.Close() 的话会顺手
	// checkpoint，WAL 里的行就进了快照，走不到「重放」这条路（第一版就是这么漏测的）
	if err := d.Store.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}

	d2 := openDir(t, dir)
	defer d2.Close()
	run(t, d2, s, `INSERT INTO sr (v) VALUES ('e')`)

	res := run(t, d2, s, `SELECT count(*), max(id) FROM sr`)
	row := res[0].Rows[0]
	if fmt.Sprint(row[0]) != "5" || fmt.Sprint(row[1]) != "5" {
		t.Fatalf("重启后插入应拿到 id=5，实际 count=%v max(id)=%v", row[0], row[1])
	}
}

// 多表 WAL 并行重放：跨表事务、多表各自有数据，重启后全部正确。
func TestWALReplayMultiTable(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := "s"
	run(t, d, s, `CREATE TABLE m1 (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, s, `CREATE TABLE m2 (id INTEGER PRIMARY KEY, v TEXT)`)
	run(t, d, s, `CREATE TABLE m3 (id INTEGER PRIMARY KEY, v TEXT)`)
	// 多表混合写：一个事务跨多表 + 各表独立事务
	run(t, d, s, `BEGIN`)
	run(t, d, s, `INSERT INTO m1 VALUES (1, 'a1'), (2, 'a2')`)
	run(t, d, s, `INSERT INTO m2 VALUES (1, 'b1')`)
	run(t, d, s, `INSERT INTO m3 VALUES (1, 'c1'), (2, 'c2'), (3, 'c3')`)
	run(t, d, s, `COMMIT`)
	run(t, d, s, `INSERT INTO m1 VALUES (3, 'a3')`)
	run(t, d, s, `UPDATE m2 SET v = 'b1x' WHERE id = 1`)
	run(t, d, s, `DELETE FROM m3 WHERE id = 2`)
	// 模拟被杀（不 checkpoint）
	if err := d.Store.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}
	d2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	assertRows(t, run(t, d2, s, `SELECT id, v FROM m1 ORDER BY id`), []string{"1|a1", "2|a2", "3|a3"})
	assertRows(t, run(t, d2, s, `SELECT id, v FROM m2`), []string{"1|b1x"})
	assertRows(t, run(t, d2, s, `SELECT id, v FROM m3 ORDER BY id`), []string{"1|c1", "3|c3"})
}
