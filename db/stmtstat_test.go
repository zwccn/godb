package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"godb/exec"
)

func TestNormalizeSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		// 参数不同、结构相同
		{"SELECT * FROM t_user WHERE id=1", "SELECT * FROM t_user WHERE id=?"},
		{"SELECT * FROM t_user WHERE id=9999", "SELECT * FROM t_user WHERE id=?"},
		{`SELECT * FROM t WHERE name='ok'`, "SELECT * FROM t WHERE name=?"},
		{`SELECT * FROM t WHERE name='bad'`, "SELECT * FROM t WHERE name=?"},
		{`SELECT * FROM t WHERE d > '2026-01-01'`, "SELECT * FROM t WHERE d > ?"},
		{"SELECT * FROM t LIMIT 10 OFFSET 20", "SELECT * FROM t LIMIT ? OFFSET ?"},
		{"INSERT INTO t (a,b) VALUES (1,'x')", "INSERT INTO t (a,b) VALUES (?,?)"},

		// 标识符里的数字要保留：把 t_user1 和 t_user2 归成一个结构是错的
		{"SELECT * FROM t_user1 WHERE id=1", "SELECT * FROM t_user1 WHERE id=?"},
		{"SELECT * FROM t_user2 WHERE id=1", "SELECT * FROM t_user2 WHERE id=?"},
		{"SELECT col1, col2 FROM t9", "SELECT col1, col2 FROM t9"},

		// 双引号/反引号里是标识符，原样保留
		{`SELECT "col1" FROM t`, `SELECT "col1" FROM t`},
		{"SELECT `col1` FROM t", "SELECT `col1` FROM t"},

		// 字符串里的数字和引号
		{`SELECT * FROM t WHERE c='a''b'`, "SELECT * FROM t WHERE c=?"},
		{`SELECT * FROM t WHERE c='it 123'`, "SELECT * FROM t WHERE c=?"},

		// 注释原样保留（有的驱动往注释里塞 trace id，替换成 ? 会把结构拆散）
		{"SELECT 1 -- trace 12345", "SELECT ? -- trace 12345"},
		{"SELECT 1 /* batch 7 */ FROM t", "SELECT ? /* batch 7 */ FROM t"},

		// 负号跟着数字走
		{"SELECT * FROM t WHERE x = -5", "SELECT * FROM t WHERE x = -?"},
		// 小数
		{"SELECT * FROM t WHERE x = 3.14", "SELECT * FROM t WHERE x = ?"},

		// 空串
		{"", ""},
	}
	for _, c := range cases {
		if got := normKey(c.in); got != c.want {
			t.Errorf("normKey(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// 同结构不同参数必须落到同一个计数器上：这是整个聚合的前提。
func TestStmtPoolGroupsByStructure(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	for i := 1; i <= 5; i++ {
		p.add(fmt.Sprintf("SELECT * FROM t WHERE id=%d", i), nil, false, "s1",
			&exec.Result{TookNs: int64(i) * 1000, RowCount: 1}, now)
	}
	got := p.snapshot(10, true)
	if len(got) != 1 {
		t.Fatalf("5 次执行应该归并成 1 个结构，实际 %d 个", len(got))
	}
	st := got[0]
	if st.N != 5 {
		t.Errorf("N = %d，want 5", st.N)
	}
	if st.TotalNs != 15000 {
		t.Errorf("TotalNs = %d，want 15000", st.TotalNs)
	}
	if st.AvgNs != 3000 {
		t.Errorf("AvgNs = %d，want 3000", st.AvgNs)
	}
	if st.MaxNs != 5000 || st.MinNs != 1000 {
		t.Errorf("Max/Min = %d/%d，want 5000/1000", st.MaxNs, st.MinNs)
	}
}

// 明细按耗时降序，满了以后只进更快的那条。
func TestStmtPoolDetailTopN(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	for i := 1; i <= stmtDetailCap+50; i++ {
		p.add("SELECT * FROM t WHERE id=?", nil, false, "s1",
			&exec.Result{TookNs: int64(i) * 1000}, now)
	}
	det, _, ok := p.detail(normKey("SELECT * FROM t WHERE id=?"))
	if !ok {
		t.Fatal("结构没了")
	}
	if len(det) != stmtDetailCap {
		t.Fatalf("明细 %d 条，want %d", len(det), stmtDetailCap)
	}
	if det[0].TookNs != int64(stmtDetailCap+50)*1000 {
		t.Errorf("最慢那条 = %d，want 最大值", det[0].TookNs)
	}
	// 严格降序
	for i := 1; i < len(det); i++ {
		if det[i-1].TookNs < det[i].TookNs {
			t.Fatalf("明细没按耗时降序：%d 位 %d < %d 位 %d", i-1, det[i-1].TookNs, i, det[i].TookNs)
		}
	}
	// 保留的应该是「最慢的 100 次」，不是「最近 100 次」
	if det[len(det)-1].TookNs != int64(51)*1000 {
		t.Errorf("明细第 100 名 = %d，want 51000（前 50 条最慢的被淘汰了）", det[len(det)-1].TookNs)
	}
}

// 池满之后淘汰累计耗时最小的那个。
func TestStmtPoolEvictsLowestTotal(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	for i := 0; i < stmtStatCap; i++ {
		p.add(fmt.Sprintf("SELECT * FROM t%d WHERE id=?", i), nil, false, "s1",
			&exec.Result{TookNs: 1000}, now)
	}
	live, evicted := p.count()
	if live != stmtStatCap || evicted != 0 {
		t.Fatalf("填满后 live=%d evicted=%d，want %d/0", live, evicted, stmtStatCap)
	}
	// 新结构进来，应该挤掉某个累计最小的
	p.add("SELECT * FROM brand_new WHERE id=?", nil, false, "s1",
		&exec.Result{TookNs: 1000}, now)
	live, evicted = p.count()
	if live != stmtStatCap || evicted != 1 {
		t.Fatalf("溢出后 live=%d evicted=%d，want %d/1", live, evicted, stmtStatCap)
	}
	if _, _, ok := p.detail(normKey("SELECT * FROM brand_new WHERE id=?")); !ok {
		t.Error("新结构没进来")
	}
}

// 单次最慢的结构有豁免权：即使累计耗时最小也不能被挤出去。
//
// 这是两个口径共存的全部理由 —— 一条每分钟跑一次、每次 2 秒的报表，
// 累计耗时远不如每秒 1000 次的短语句，纯按累计淘汰就永远看不到它。
func TestStmtPoolSlowestIsExemptFromEviction(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	slowKey := "SELECT * FROM report WHERE id=?"

	// 先把这条慢语句放进去（累计 100ms，但单次就是 100ms）
	p.add(slowKey, nil, false, "s1", &exec.Result{TookNs: 100 * int64(time.Millisecond)}, now)

	// 再用 999 条「累计更大、单次很小」的语句填满
	for i := 0; i < stmtStatCap-1; i++ {
		for k := 0; k < 200; k++ {
			p.add(fmt.Sprintf("SELECT * FROM hot%d WHERE id=?", i), nil, false, "s1",
				&exec.Result{TookNs: 1000}, now)
		}
	}
	// 慢语句累计只有 100ms，而每条 hot 累计 200ms，它是最小的一个
	for i := 0; i < 50; i++ {
		p.add(fmt.Sprintf("SELECT * FROM extra%d WHERE id=?", i), nil, false, "s1",
			&exec.Result{TookNs: 1000}, now)
	}
	if _, _, ok := p.detail(normKey(slowKey)); !ok {
		t.Fatal("单次最慢的结构被挤掉了 —— 豁免没生效")
	}
	// 榜单上要能看到它排在「单次最慢」的第一名
	list := p.snapshot(100, false)
	if len(list) == 0 || list[0].SQL != normKey(slowKey) {
		t.Errorf("按单次最慢排的第一名不是那条报表语句，而是 %q", list[0].SQL)
	}
}

// 耗时测成 0 的语句（比本机时钟步进 0.2ms 还快）也要计次：
// 「执行总次数」漏掉这一半，高频短语句看起来就只跑了几次。
func TestStmtPoolCountsZeroTook(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	for i := 0; i < 10; i++ {
		p.add("SELECT 1", nil, false, "s1", &exec.Result{TookNs: 0}, now)
	}
	p.add("SELECT 1", nil, false, "s1", &exec.Result{TookNs: 1000}, now)

	got := p.snapshot(10, true)[0]
	if got.N != 11 {
		t.Errorf("N = %d，want 11（10 次 0 耗时 + 1 次 1µs）", got.N)
	}
	if got.MaxNs != 1000 || got.MinNs != 1000 {
		t.Errorf("Max/Min = %d/%d，want 1000/1000（0 不该被当成最快）", got.MaxNs, got.MinNs)
	}
	// 累计里 0 耗时的那 10 次要按半个时钟步进估算计入，否则一条每秒几千次的
	// 短语句累计一直是 0，等于在榜单上隐身。
	if want := 10*p.zeroNs + 1000; got.TotalNs != want {
		t.Errorf("TotalNs = %d，want %d（10 × 半个步进 + 1000）", got.TotalNs, want)
	}
	det, _, _ := p.detail(normKey("SELECT 1"))
	if len(det) != 1 {
		t.Errorf("明细里不该收 0 耗时的样本，实际 %d 条", len(det))
	}
}

func TestTxKey(t *testing.T) {
	if got := txKey([]string{"t_b", "t_a"}, true); got != "w:t_a,t_b" {
		t.Errorf("txKey = %q，want w:t_a,t_b", got)
	}
	if got := txKey([]string{"t_a", "t_a"}, false); got != "r:t_a" {
		t.Errorf("重复表名没去掉：%q", got)
	}
	if got := txKey(nil, false); got != "r:(无表)" {
		t.Errorf("空表集合 = %q", got)
	}
}

func TestTxPoolRecordsDuration(t *testing.T) {
	p := newTxPool()
	p.add(txDone{tables: []string{"t_a"}, write: true, dur: 1500 * time.Millisecond, stmts: 3, session: "s1"})
	p.add(txDone{tables: []string{"t_a"}, write: true, dur: 500 * time.Millisecond, stmts: 1, session: "s2"})

	list := p.snapshot(10, true)
	if len(list) != 1 {
		t.Fatalf("同表集合应该归并成 1 条，实际 %d", len(list))
	}
	st := list[0]
	if st.N != 2 || st.TotalMs != 2000 || st.MaxMs != 1500 || st.MinMs != 500 || st.AvgMs != 1000 {
		t.Errorf("统计不对：N=%d total=%d max=%d min=%d avg=%d", st.N, st.TotalMs, st.MaxMs, st.MinMs, st.AvgMs)
	}
	det, ok := p.detail("w:t_a")
	if !ok || len(det) != 2 || det[0].DurMs != 1500 {
		t.Errorf("事务明细不对：%v", det)
	}
}

// 基准：稳态下（结构已存在、明细已满）add 应该零分配，这是热路径的前提。
func BenchmarkStmtAddExisting(b *testing.B) {
	p := newStmtPool()
	now := time.Now()
	res := &exec.Result{TookNs: 42000, RowCount: 1}
	// 预热到「明细已满」的稳态
	for i := 0; i < stmtDetailCap+10; i++ {
		p.add("SELECT id, name FROM t_user WHERE id=?", nil, false, "s1", res, now)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.add("SELECT id, name FROM t_user WHERE id=?", nil, false, "s1", res, now)
	}
}

func BenchmarkNormalize(b *testing.B) {
	cases := []struct{ name, sql string }{
		{"短语句", "SELECT id, name FROM t_user WHERE id=1 AND status='ok'"},
		{"中语句", "SELECT o.id, o.amount FROM t_orders o JOIN t_customer c ON c.id=o.cid WHERE o.status='paid' AND o.amount > 100"},
		{"长语句", "INSERT INTO t_log (a,b) VALUES (1,'x')" + strings.Repeat(" /* padding */", 60)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = normKey(c.sql)
			}
		})
	}
}

// 快照要按两个口径各自排好，且慢的那个在两个榜单里的位置能对上。
func TestStmtPoolTwoOrders(t *testing.T) {
	p := newStmtPool()
	now := time.Now()
	// A：跑 100 次、每次 1ms → 累计 100ms，单次 1ms
	for i := 0; i < 100; i++ {
		p.add("SELECT * FROM a WHERE id=?", nil, false, "s1", &exec.Result{TookNs: 1e6}, now)
	}
	// B：跑 1 次、每次 50ms → 累计 50ms，单次 50ms
	p.add("SELECT * FROM b WHERE id=?", nil, false, "s1", &exec.Result{TookNs: 5e7}, now)

	byTotal := p.snapshot(10, true)
	if byTotal[0].SQL != normKey("SELECT * FROM a WHERE id=?") {
		t.Errorf("按累计排第一应该是 A，实际 %q", byTotal[0].SQL)
	}
	byMax := p.snapshot(10, false)
	if byMax[0].SQL != normKey("SELECT * FROM b WHERE id=?") {
		t.Errorf("按单次最慢排第一应该是 B，实际 %q", byMax[0].SQL)
	}
}
