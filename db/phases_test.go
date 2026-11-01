package db

import (
	"fmt"
	"strings"
	"testing"

	"godb/exec"
)

// 一条语句的耗时必须能对上账：装载 / 等锁 / 执行 / 收尾 加起来就是服务端耗时。
//
// 这是为了回答一桩具体的悬案：探测里每一步都不到 1ms，服务端却说 19ms ——
// 那 19ms 全在「首次访问表」的装载里，发生在任何算子开始之前。
func TestPhasesFirstLoad(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	run(t, d, "s", `CREATE TABLE p1 (id INTEGER PRIMARY KEY, v TEXT)`)
	var b strings.Builder
	b.WriteString(`INSERT INTO p1 VALUES `)
	for i := 1; i <= 2000; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,'row-%04d')", i, i)
	}
	run(t, d, "s", b.String())
	run(t, d, "s", `CHECKPOINT`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重开：数据在磁盘上，表还没进内存
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()

	first := run(t, d2, "s2", `SELECT * FROM p1 LIMIT 5`)[0]
	if first.Phases == nil {
		t.Fatal("语句应带时间分段")
	}
	if first.Phases.LoadNs <= 0 {
		t.Fatalf("首次访问应有装载耗时，实际 %+v", *first.Phases)
	}
	if first.Phases.LoadPages <= 0 {
		t.Fatalf("首次访问应从页文件读页，实际 %+v", *first.Phases)
	}

	// 第二次：表已经在内存里，装载段应当消失
	second := run(t, d2, "s2", `SELECT * FROM p1 LIMIT 5`)[0]
	if second.Phases == nil {
		t.Fatal("第二次查询也应带时间分段")
	}
	if second.Phases.LoadNs != 0 || second.Phases.LoadPages != 0 {
		t.Fatalf("表已加载，不该再有装载开销，实际 %+v", *second.Phases)
	}
}

// 执行期的读页要能归到具体算子上（独占口径，不含子节点）。
// 池子只有 2 页时，一张十几页的表必然不停换进换出 —— 这时扫描算子自己就在读盘。
func TestPlanTreeExecIO(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{
		BufferPoolPages: 2, // 16KB，逼着扫描时不停换页
		Lock:            testOptions().Lock,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	run(t, d, "s", `CREATE TABLE ev (id INTEGER PRIMARY KEY, v TEXT)`)
	pad := strings.Repeat("p", 120)
	var b strings.Builder
	b.WriteString(`INSERT INTO ev VALUES `)
	for i := 1; i <= 800; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,'%s-%03d')", i, pad, i)
	}
	run(t, d, "s", b.String())
	run(t, d, "s", `CHECKPOINT`)

	res := run(t, d, "s", `EXPLAIN ANALYZE SELECT * FROM ev`)
	tree := res[0].PlanTree
	if tree == nil {
		t.Fatal("应带结构化计划树")
	}

	var selfPages int64
	var bad bool
	walkTree(tree, func(n *exec.PlanNode) {
		selfPages += n.SelfIOPages
		if n.SelfIONs > n.IONs || n.SelfIONs < 0 {
			bad = true
		}
	})
	if bad {
		t.Fatalf("独占 I/O 超过了累计 I/O：%+v", tree)
	}
	if selfPages <= 0 {
		t.Fatalf("2 页的池子扫 800 行，应当有算子自己读了页；读页数 %d", selfPages)
	}
	// 总读页数不会少于扫描读到的页数
	if tree.IOPages < selfPages {
		t.Fatalf("根节点累计 %d 页 < 各节点独占之和 %d 页", tree.IOPages, selfPages)
	}
}

// 大字段不走页池，所以它不产生「页数」，但时间一样要算。
// 不然 `SELECT` 一列几 MB 的 TEXT 会显示 I/O 0，而它明明在 pread 上等了很久。
func TestPlanTreeLobIO(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := "s"
	run(t, d, s, `CREATE TABLE doc (id INTEGER PRIMARY KEY, body TEXT)`)

	// >2KB 才会走大字段文件
	big := strings.Repeat("x", 200*1024)
	run(t, d, s, fmt.Sprintf(`INSERT INTO doc VALUES (1,'%s')`, big))
	run(t, d, s, `CHECKPOINT`)
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 必须重开：插入后的行还在内存的槽位数组里，扫描根本不会去读大字段。
	d2, err := Open(dir, testOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()

	res := run(t, d2, "s2", `EXPLAIN ANALYZE SELECT body FROM doc`)
	tree := res[0].PlanTree
	if tree == nil {
		t.Fatal("应带结构化计划树")
	}

	var ioBytes, ioPages int64
	walkTree(tree, func(n *exec.PlanNode) {
		ioBytes += n.SelfIOBytes
		ioPages += n.SelfIOPages
	})
	if ioBytes < int64(len(big)) {
		t.Fatalf("大字段读取没有入账：字节 %d，期望 ≥ %d", ioBytes, len(big))
	}
	if ioPages != 0 {
		t.Fatalf("大字段不占页，不该记页数，实际 %d 页", ioPages)
	}
}
