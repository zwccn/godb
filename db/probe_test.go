package db

import (
	"fmt"
	"strings"
	"testing"

	"godb/exec"
)

// setupProbeTable 造一张够大的表：本机时钟步进约 0.2–0.6ms，几千行的查询
// 每算子的耗时会被读成 0，测不出「谁最贵」—— 两万行才稳。
func setupProbeTable(t *testing.T) *DB {
	t.Helper()
	d := newTestDB(t)
	s := "s"
	run(t, d, s, `CREATE TABLE big (id int4 PRIMARY KEY, g int4, v int4)`)
	var b strings.Builder
	b.WriteString(`INSERT INTO big VALUES `)
	for i := 1; i <= 20000; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d,%d,%d)", i, i%7, i*3)
	}
	run(t, d, s, b.String())
	return d
}

func walkTree(n *exec.PlanNode, fn func(*exec.PlanNode)) {
	fn(n)
	for _, c := range n.Children {
		walkTree(c, fn)
	}
}

// 探测执行的前提是「每一步都有数」：算子树里除了扫描节点，中间的过滤、
// 聚合、排序、LIMIT 也得报行数，否则前端画出来只有最底下一行有数字。
func TestPlanTreePerOperatorStats(t *testing.T) {
	d := setupProbeTable(t)
	s := "s"

	res := run(t, d, s, `EXPLAIN ANALYZE SELECT g, count(*) AS n, sum(v) AS s FROM big
		GROUP BY g ORDER BY n DESC LIMIT 3`)
	tree := res[0].PlanTree
	if tree == nil {
		t.Fatal("EXPLAIN ANALYZE 应带结构化计划树")
	}

	var nodes []*exec.PlanNode
	walkTree(tree, func(n *exec.PlanNode) { nodes = append(nodes, n) })
	if len(nodes) < 4 {
		t.Fatalf("算子树太浅，只看到 %d 个节点", len(nodes))
	}

	// 非根节点里必须有报行数的（以前只有扫描节点有，其余全是 0）
	withRows := 0
	for _, n := range nodes[1:] {
		if n.Rows > 0 {
			withRows++
		}
	}
	if withRows == 0 {
		t.Fatalf("除根节点外没有任何节点报行数：%+v", nodes)
	}

	// 根节点有耗时；每个节点的独占时间不能超过总时间
	if tree.TotalNs <= 0 {
		t.Fatalf("根节点总耗时 = %d（两万行聚合应当超过时钟步进）", tree.TotalNs)
	}
	var sumSelf int64
	for _, n := range nodes {
		if n.SelfNs > n.TotalNs {
			t.Fatalf("%s 独占 %d ns 超过总时间 %d ns", n.Name, n.SelfNs, n.TotalNs)
		}
		sumSelf += n.SelfNs
	}
	// 父 = 自己 + 子，所以全树的独占时间之和应当约等于根节点的总时间。
	// 计时本身有开销，留 20% 余量。
	if sumSelf > tree.TotalNs*12/10 || sumSelf < tree.TotalNs/2 {
		t.Fatalf("独占时间之和 %d 与根节点总时间 %d 对不上", sumSelf, tree.TotalNs)
	}
	// 聚合这一步自己的活（建哈希表）必须被算在独占时间里，
	// 而不是全被扫描节点吃掉
	var agg *exec.PlanNode
	walkTree(tree, func(n *exec.PlanNode) {
		if strings.HasPrefix(n.Name, "HashAggregate") {
			agg = n
		}
	})
	if agg == nil {
		t.Fatal("计划里应有 HashAggregate")
	}
	if agg.SelfNs <= 0 {
		t.Fatalf("聚合节点独占耗时应大于 0，实际 %d", agg.SelfNs)
	}
}

// 「读了 N 行」「循环了几轮」是两个独立的口子：前者只有全表扫描有，
// 后者只有嵌套循环/索引连接有。
func TestPlanTreeScanAndLoops(t *testing.T) {
	d := setupProbeTable(t)
	s := "s"

	// 全表扫描 3000 行，过滤后只剩 5 行 —— scanned 必须大于 rows
	res := run(t, d, s, `EXPLAIN ANALYZE SELECT * FROM big WHERE v > 59985`)
	var scan *exec.PlanNode
	walkTree(res[0].PlanTree, func(n *exec.PlanNode) {
		if n.Scanned > 0 {
			scan = n
		}
	})
	if scan == nil {
		t.Fatal("计划里应有节点报「从堆表读了多少行」")
	}
	if scan.Scanned != 20000 {
		t.Fatalf("扫描节点读了 %d 行，期望 20000", scan.Scanned)
	}
	// 过滤在 scan 之上：上层只剩 5 行，这才有「读了 3000 行只留 5 行」的可比性
	var filtered bool
	walkTree(res[0].PlanTree, func(n *exec.PlanNode) {
		if n.Rows == 5 && n != scan {
			filtered = true
		}
	})
	if !filtered {
		t.Fatalf("计划里应有节点输出 5 行（过滤后）")
	}

	// 索引连接：外表每来一行点查一次内表，轮次要等于驱动行数
	d2 := setupJoinTable(t)
	res = run(t, d2, s, `EXPLAIN ANALYZE SELECT * FROM dept JOIN emp ON emp.dept_id = dept.id`)
	var withLoops *exec.PlanNode
	walkTree(res[0].PlanTree, func(n *exec.PlanNode) {
		if n.Loops > 0 {
			withLoops = n
		}
	})
	if withLoops == nil {
		t.Fatal("连接节点应报「跑了几轮」")
	}
	if withLoops.Loops != 4 {
		// dept 有 4 行，每行点查一次 emp 索引
		t.Fatalf("loops = %d，期望 4（dept 4 行各点查一次）", withLoops.Loops)
	}
}

// 普通 EXPLAIN 不执行，树里的数字必须全是 0 —— 否则前端会当成真实耗时去画柱状图。
func TestPlanTreeWithoutAnalyzeHasNoStats(t *testing.T) {
	d := setupProbeTable(t)
	s := "s"
	res := run(t, d, s, `EXPLAIN SELECT * FROM big WHERE v > 59985`)
	tree := res[0].PlanTree
	if tree == nil {
		t.Fatal("普通 EXPLAIN 也应带树（只有形状，没有数字）")
	}
	walkTree(tree, func(n *exec.PlanNode) {
		if n.TotalNs != 0 || n.SelfNs != 0 || n.Rows != 0 {
			t.Fatalf("普通 EXPLAIN 不该有运行统计：%s rows=%d total=%d", n.Name, n.Rows, n.TotalNs)
		}
	})
}
