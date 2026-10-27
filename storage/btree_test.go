package storage

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// checkBTree 校验树的三个不变量：全局有序、叶子链完整、条目数与 Len 一致。
func checkBTree(t *testing.T, tr *btree) []string {
	t.Helper()
	var got []string
	seen := 0
	prev := ""
	for n := tr.leftmost(); n != nil; n = n.next {
		if !n.leaf {
			t.Fatal("叶子链里出现了内部节点")
		}
		for i, k := range n.keys {
			if i > 0 && n.keys[i-1] > k {
				t.Fatalf("叶子内键不有序: %q > %q", n.keys[i-1], k)
			}
			if seen > 0 && k < prev {
				t.Fatalf("跨叶子键不有序: %q 之后出现 %q", prev, k)
			}
			prev = k
			got = append(got, k)
			seen++
			if n.slots[i] < 0 {
				t.Fatal("槽位号不该是负数")
			}
		}
	}
	if seen != tr.Len() {
		t.Fatalf("扫描到 %d 条，Len() = %d", seen, tr.Len())
	}
	return got
}

func TestBTreeInsertGet(t *testing.T) {
	tr := newBTree()
	for i := 0; i < 1000; i++ {
		tr.Insert(fmt.Sprintf("k%04d", i), i)
	}
	if tr.Len() != 1000 {
		t.Fatalf("Len = %d", tr.Len())
	}
	checkBTree(t, tr)
	for i := 0; i < 1000; i++ {
		slots := tr.Get(fmt.Sprintf("k%04d", i))
		if len(slots) != 1 || slots[0] != i {
			t.Fatalf("Get k%04d = %v", i, slots)
		}
	}
	if got := tr.Get("nope"); len(got) != 0 {
		t.Fatalf("不存在的键应返回空: %v", got)
	}
	// 树高应该有上界：256 路，1000 条最多 2 层
	if h := tr.Height(); h > 3 {
		t.Fatalf("1000 条的高度不该超过 3，实际 %d", h)
	}
}

// 重复键会被分裂切到相邻叶子里，Get 必须跨叶子取全。
func TestBTreeDuplicateKeys(t *testing.T) {
	tr := newBTree()
	for i := 0; i < 5000; i++ {
		tr.Insert("same", i)
	}
	got := tr.Get("same")
	if len(got) != 5000 {
		t.Fatalf("重复键应全部取到，实际 %d", len(got))
	}
	seen := map[int]bool{}
	for _, s := range got {
		if seen[s] {
			t.Fatalf("槽位 %d 重复出现", s)
		}
		seen[s] = true
	}
	checkBTree(t, tr)
	// 逐个删掉，最后应该清空
	for i := 0; i < 5000; i++ {
		if !tr.Delete("same", i) {
			t.Fatalf("删不到 slot=%d", i)
		}
	}
	if tr.Len() != 0 {
		t.Fatalf("全删后 Len = %d", tr.Len())
	}
	checkBTree(t, tr)
}

func TestBTreeRange(t *testing.T) {
	tr := newBTree()
	for i := 0; i < 1000; i++ {
		tr.Insert(fmt.Sprintf("%04d", i), i)
	}
	collect := func(r keyRange) []string {
		var out []string
		tr.Range(r, func(k string, _ int) bool {
			out = append(out, k)
			return true
		})
		return out
	}
	// [0100, 0103]
	got := collect(keyRange{lo: "0100", hi: "0103", hasLo: true, hasHi: true, loInc: true, hiInc: true})
	if len(got) != 4 || got[0] != "0100" || got[3] != "0103" {
		t.Fatalf("闭区间 [0100,0103] = %v", got)
	}
	// (0100, 0103)
	got = collect(keyRange{lo: "0100", hi: "0103", hasLo: true, hasHi: true})
	if len(got) != 2 || got[0] != "0101" || got[1] != "0102" {
		t.Fatalf("开区间 (0100,0103) = %v", got)
	}
	// >= 0997
	got = collect(keyRange{lo: "0997", hasLo: true, loInc: true})
	if len(got) != 3 || got[0] != "0997" || got[2] != "0999" {
		t.Fatalf(">= 0997 = %v", got)
	}
	// < 0003
	got = collect(keyRange{hi: "0003", hasHi: true})
	if len(got) != 3 || got[0] != "0000" || got[2] != "0002" {
		t.Fatalf("< 0003 = %v", got)
	}
	// 无边界 = 全量
	got = collect(keyRange{})
	if len(got) != 1000 {
		t.Fatalf("全量扫 = %d", len(got))
	}
	// 提前结束
	n := 0
	tr.Range(keyRange{}, func(string, int) bool { n++; return n < 5 })
	if n != 5 {
		t.Fatalf("提前结束应访问 5 条，实际 %d", n)
	}
}

// 随机增删对拍：每一步都和 map 的期望集合比对（等值 + 全序 + 不变量）。
func TestBTreeRandomOps(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tr := newBTree()
	want := map[string][]int{} // key → slots
	inserted := map[[2]interface{}]bool{}

	for step := 0; step < 4000; step++ {
		key := fmt.Sprintf("k%d", rng.Intn(300))
		if rng.Intn(100) < 60 {
			slot := rng.Intn(5000)
			tr.Insert(key, slot)
			want[key] = append(want[key], slot)
			inserted[[2]interface{}{key, slot}] = true
		} else {
			// 删一个已知存在的条目
			var keys []string
			for k := range want {
				keys = append(keys, k)
			}
			if len(keys) == 0 {
				continue
			}
			k := keys[rng.Intn(len(keys))]
			list := want[k]
			if len(list) == 0 {
				continue
			}
			slot := list[0]
			want[k] = list[1:]
			if len(want[k]) == 0 {
				delete(want, k)
			}
			if !tr.Delete(k, slot) {
				t.Fatalf("step %d: 删不到 %s/%d", step, k, slot)
			}
			delete(inserted, [2]interface{}{k, slot})
		}
		if step%500 == 0 {
			checkAgainstWant(t, tr, want)
		}
	}
	checkAgainstWant(t, tr, want)
}

func checkAgainstWant(t *testing.T, tr *btree, want map[string][]int) {
	t.Helper()
	// 全序扫描应与「排序后的期望集合」逐项一致
	var exp []string
	total := 0
	for k, slots := range want {
		total += len(slots)
		for range slots {
			exp = append(exp, k)
		}
	}
	sort.Strings(exp)
	got := checkBTree(t, tr)
	if len(got) != len(exp) {
		t.Fatalf("条目数 %d，期望 %d", len(got), len(exp))
	}
	for i := range exp {
		if got[i] != exp[i] {
			t.Fatalf("第 %d 条 = %q，期望 %q", i, got[i], exp[i])
		}
	}
	// 等值查找
	for k, slots := range want {
		gotSlots := tr.Get(k)
		if len(gotSlots) != len(slots) {
			t.Fatalf("Get(%s) 返回 %d 条，期望 %d", k, len(gotSlots), len(slots))
		}
		a := append([]int(nil), gotSlots...)
		b := append([]int(nil), slots...)
		sort.Ints(a)
		sort.Ints(b)
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("Get(%s) = %v，期望 %v", k, a, b)
			}
		}
	}
	if tr.Len() != total {
		t.Fatalf("Len = %d，期望 %d", tr.Len(), total)
	}
}

// 顺序 / 逆序插入是分裂路径的最坏情况，也顺手验证树高不会失控。
func TestBTreeSequentialInsert(t *testing.T) {
	for _, name := range []string{"asc", "desc"} {
		t.Run(name, func(t *testing.T) {
			tr := newBTree()
			const n = 20000
			for i := 0; i < n; i++ {
				k := i
				if name == "desc" {
					k = n - i
				}
				tr.Insert(fmt.Sprintf("%06d", k), k)
			}
			checkBTree(t, tr)
			// 2 万条 / 256 路 → 2 层足够
			if h := tr.Height(); h > 3 {
				t.Fatalf("高度 %d 太大", h)
			}
			if tr.Len() != n {
				t.Fatalf("Len = %d", tr.Len())
			}
		})
	}
}

// 全删之后还能继续用（延迟合并下结构性节点会留着）。
func TestBTreeDeleteAllAndReuse(t *testing.T) {
	tr := newBTree()
	for i := 0; i < 3000; i++ {
		tr.Insert(fmt.Sprintf("%05d", i), i)
	}
	for i := 0; i < 3000; i++ {
		if !tr.Delete(fmt.Sprintf("%05d", i), i) {
			t.Fatalf("删不到 %d", i)
		}
	}
	if tr.Len() != 0 {
		t.Fatalf("Len = %d", tr.Len())
	}
	checkBTree(t, tr)
	// 空叶占比应该很高，需要重建
	if !tr.NeedRebuild() {
		t.Fatal("全删之后应该判定需要重建")
	}
	tr.Reset()
	if tr.Len() != 0 || tr.Height() != 1 {
		t.Fatal("Reset 后应该是空树")
	}
	tr.Insert("a", 1)
	if got := tr.Get("a"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Reset 后插入取不到: %v", got)
	}
}
