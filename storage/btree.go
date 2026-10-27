package storage

import "sort"

// 内存 B+Tree：节点就是一个「页」（固定容量的有序数组），叶子之间用 next 串成链表。
//
// 为什么不用跳表：这个库将来要支持超过内存的数据量，那时索引节点必须能按页换入
// 换出。B+Tree 的节点天然等于磁盘页（一次 IO 换来几百个键），范围扫描沿叶子链走
// 顺序 IO；跳表的链表指针在外存上就是随机 IO 制造机。所以哪怕现在全内存，
// 也按「节点 = 页」的形状来写，将来把节点的读写换成 buffer pool 即可，树逻辑不动。
//
// 删除采用**延迟合并**：只从叶子里摘键，不做兄弟借位/节点合并。欠载的叶子留着，
// 由调用方在空叶比例过高时整树重建（本库本来就有重建能力，20 万行约 40 ms）。
// 代价是长期「大批删 + 零散插」后索引里会有空叶，换来的是少 300 行最容易出 bug 的
// 合并/根收缩逻辑。
const (
	// btreeMaxKeys 是一个节点最多容纳的键数，超出就分裂。
	// 256 个键约 4–6 KB，接近一个磁盘页；20 万行只要 2 层。
	btreeMaxKeys = 256
	// btreeEmptyRatio 是「空叶占比达到多少就该重建」。延迟合并的兜底。
	btreeEmptyRatio = 0.3
)

// btree 是一棵 B+Tree。键是保序编码后的字符串（见 orderKey），
// 所以直接用 Go 的字符串比较就是键序。
type btree struct {
	root *bnode
	n    int // 条目总数（含重复键）
}

type bnode struct {
	leaf bool
	// keys 升序。内部节点的 keys[i] 是 kids[i+1] 子树里的最小键，
	// 所以下探用「第一个 > key」，不是「第一个 >= key」。
	keys []string
	// 叶子：与 keys 对齐的槽位号（同一个键可以有多条，彼此相邻）
	slots []int
	// 内部节点：len(kids) == len(keys)+1
	kids []*bnode
	// next 只对叶子有效：范围扫描沿它顺序走
	next *bnode
}

func newBTree() *btree {
	return &btree{root: &bnode{leaf: true}}
}

// Len 返回条目总数。
func (t *btree) Len() int { return t.n }

// Insert 插入一个 (key, slot)。允许重复键（非唯一索引）。
func (t *btree) Insert(key string, slot int) {
	up, right := t.insertInto(t.root, key, slot)
	if right != nil {
		t.root = &bnode{leaf: false, keys: []string{up}, kids: []*bnode{t.root, right}}
	}
	t.n++
}

// insertInto 递归插入，返回（上提键，新的右兄弟）；right 为 nil 表示没有分裂。
func (t *btree) insertInto(n *bnode, key string, slot int) (string, *bnode) {
	if n.leaf {
		i := sort.SearchStrings(n.keys, key) // 第一个 >= key，相同的键因此聚在一起
		n.keys = append(n.keys, "")
		copy(n.keys[i+1:], n.keys[i:])
		n.keys[i] = key
		n.slots = append(n.slots, 0)
		copy(n.slots[i+1:], n.slots[i:])
		n.slots[i] = slot
		if len(n.keys) > btreeMaxKeys {
			return n.splitLeaf()
		}
		return "", nil
	}
	i := sort.Search(len(n.keys), func(j int) bool { return n.keys[j] > key })
	up, right := t.insertInto(n.kids[i], key, slot)
	if right == nil {
		return "", nil
	}
	n.keys = append(n.keys, "")
	copy(n.keys[i+1:], n.keys[i:])
	n.keys[i] = up
	n.kids = append(n.kids, nil)
	copy(n.kids[i+2:], n.kids[i+1:])
	n.kids[i+1] = right
	if len(n.keys) > btreeMaxKeys {
		return n.splitInner()
	}
	return "", nil
}

// splitLeaf 把叶子分半，右半的第一个键上提到父节点。
func (n *bnode) splitLeaf() (string, *bnode) {
	mid := len(n.keys) / 2
	right := &bnode{leaf: true}
	right.keys = append([]string(nil), n.keys[mid:]...)
	right.slots = append([]int(nil), n.slots[mid:]...)
	n.keys = n.keys[:mid]
	n.slots = n.slots[:mid]
	// 维护叶子链：范围扫描完全依赖它
	right.next = n.next
	n.next = right
	return right.keys[0], right
}

// splitInner 把内部节点分半，中间键上提（内部键不复制到子节点）。
func (n *bnode) splitInner() (string, *bnode) {
	mid := len(n.keys) / 2
	up := n.keys[mid]
	right := &bnode{leaf: false}
	right.keys = append([]string(nil), n.keys[mid+1:]...)
	right.kids = append([]*bnode(nil), n.kids[mid+1:]...)
	n.keys = n.keys[:mid]
	n.kids = n.kids[:mid+1]
	return up, right
}

// seek 定位到「第一个 >= key 的条目」所在的叶子与下标。
//
// 下探用「第一个 >= key」，不是「第一个 > key」：键允许重复，分裂时同一个键
// 会被切到相邻两片叶子里，此时内部节点的分隔键等于该键。用 >= 会走到左边那片，
// 往右沿叶子链扫就能把散落的重复键全部捞到；用 > 会直接跳到最右，漏掉前面的。
func (t *btree) seek(key string) (*bnode, int) {
	n := t.root
	for !n.leaf {
		i := sort.Search(len(n.keys), func(j int) bool { return n.keys[j] >= key })
		n = n.kids[i]
	}
	return n, sort.SearchStrings(n.keys, key)
}

// Get 返回某个键对应的全部槽位（等值查找）。
func (t *btree) Get(key string) []int {
	n, i := t.seek(key)
	var out []int
	for ; n != nil; n, i = n.next, 0 {
		for ; i < len(n.keys); i++ {
			k := n.keys[i]
			if k > key {
				return out // 有序，越过就不必再找
			}
			if k == key {
				out = append(out, n.slots[i])
			}
		}
	}
	return out
}

// Delete 删掉精确匹配的 (key, slot)，返回是否删到。
//
// 延迟合并：只动叶子，内部节点和空叶都留着。
func (t *btree) Delete(key string, slot int) bool {
	n, i := t.seek(key)
	for ; n != nil; n, i = n.next, 0 {
		for ; i < len(n.keys); i++ {
			k := n.keys[i]
			if k > key {
				return false
			}
			if k != key || n.slots[i] != slot {
				continue
			}
			copy(n.keys[i:], n.keys[i+1:])
			n.keys = n.keys[:len(n.keys)-1]
			copy(n.slots[i:], n.slots[i+1:])
			n.slots = n.slots[:len(n.slots)-1]
			t.n--
			return true
		}
	}
	return false
}

// keyRange 描述一个键区间。hasLo/hasHi 为 false 表示该侧不设限。
type keyRange struct {
	lo, hi       string
	hasLo, hasHi bool
	loInc, hiInc bool
}

// Range 按键升序遍历区间内的条目，fn 返回 false 时提前结束。
func (t *btree) Range(r keyRange, fn func(key string, slot int) bool) {
	var n *bnode
	idx := 0
	if r.hasLo {
		n, idx = t.seek(r.lo)
	} else {
		n = t.leftmost() // 根可能是内部节点，必须先降到最左叶
	}
	for ; n != nil; n, idx = n.next, 0 {
		for ; idx < len(n.keys); idx++ {
			k := n.keys[idx]
			if r.hasLo && k < r.lo {
				continue // seek 可能落在含更小键的叶子上
			}
			if r.hasLo && !r.loInc && k == r.lo {
				continue // 开区间：跳过所有等于 lo 的条目
			}
			if r.hasHi {
				if k > r.hi || (!r.hiInc && k == r.hi) {
					return
				}
			}
			if !fn(k, n.slots[idx]) {
				return
			}
		}
	}
}

// Walk 按全局键序访问每个条目（测试与重建用）。
func (t *btree) Walk(fn func(key string, slot int)) {
	for n := t.leftmost(); n != nil; n = n.next {
		for i := range n.keys {
			fn(n.keys[i], n.slots[i])
		}
	}
}

func (t *btree) leftmost() *bnode {
	n := t.root
	for !n.leaf {
		n = n.kids[0]
	}
	return n
}

// Reset 清空（保留一个空叶作为根）。
func (t *btree) Reset() {
	t.root = &bnode{leaf: true}
	t.n = 0
}

// Stats 返回节点总数与空叶数，供「碎片太多就重建」判断用。
func (t *btree) Stats() (nodes, emptyLeaves int) {
	var walk func(*bnode)
	walk = func(n *bnode) {
		nodes++
		if n.leaf {
			if len(n.keys) == 0 {
				emptyLeaves++
			}
			return
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(t.root)
	return nodes, emptyLeaves
}

// Height 返回树高（根为 1），调试与 EXPLAIN 用。
func (t *btree) Height() int {
	h := 1
	for n := t.root; !n.leaf; n = n.kids[0] {
		h++
	}
	return h
}

// NeedRebuild 判断延迟合并攒下的碎片是否已经值得整树重建。
func (t *btree) NeedRebuild() bool {
	nodes, empty := t.Stats()
	if nodes <= 4 {
		return false
	}
	return float64(empty) > float64(nodes)*btreeEmptyRatio
}
