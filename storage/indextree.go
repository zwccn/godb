package storage

import (
	"sort"
)

// 页式 B+Tree：节点就是索引文件里的一页（8 KB），叶子用 next/prev 串成双向链表。
//
// 和改造前的内存树相比，形状不变（键升序、内部节点存「子树最小键」、删除延迟合并），
// 变的是三件事：
//  1. 节点的读写走缓冲池（和堆页共享同一个全局预算），页可以换出；
//  2. 插入/删除是**页内原地改**（槽位目录 8 字节 × 几百项的小 memmove），
//     不是「解整页 → 改 → 重新编码」；CRC 只在落盘前算一次（pool.pack）；
//  3. 游标不攥节点指针，只记 (页号, 下标)，每走一步重新取页 —— 页随时可能被换出，
//     指针一存下来就会悬空。
//
// 内部节点的孩子布局：slots[i].val = children[i]，最右孩子放在页头的 next 字段里。
// 所以 n 个键配 n+1 个孩子，正好落在「槽位目录 + next」里。
//
// 删除沿用**延迟合并**：只摘键，不借位不合并。空叶靠整棵重建回收
// （调用方在空叶占比过高时重建，20 万行几百毫秒）。

// btree 是一棵挂在索引文件上的 B+Tree。ip 为 nil 表示「这张表没有这个索引」
// （所有方法都按空树处理）—— 这样调用方不必到处判空。
type btree struct {
	f *indexFile
}

func newBTree(f *indexFile) *btree { return &btree{f: f} }

func (t *btree) valid() bool { return t != nil && t.f != nil }

// Len 返回条目总数（含重复键）。
func (t *btree) Len() int {
	if !t.valid() {
		return 0
	}
	return int(t.f.count)
}

// Height 返回树高（根为 1）。
func (t *btree) Height() int {
	if !t.valid() {
		return 0
	}
	return int(t.f.height)
}

// PageStats 返回（索引页数、树高、条目数）。
func (t *btree) PageStats() (pages, height int, entries int64) {
	if !t.valid() {
		return 0, 0, 0
	}
	return t.f.pageStats()
}

// ---- 插入 ----

// Insert 插入一个 (key, slot)。允许重复键（非唯一索引）。
func (t *btree) Insert(key string, slot int) {
	if !t.valid() {
		return
	}
	up, right, err := t.insert(t.f.root, []byte(key), uint32(slot))
	if err != nil {
		t.f.setErr(err)
		return
	}
	if right != idxNoPage {
		// 根分裂：新根只放一个分隔键，两个孩子是「老根」和「新的右兄弟」
		newRoot := t.f.allocPage()
		if err := t.f.writeIdx(newRoot, func(p idxPage) error {
			p.initAs(newRoot, false)
			p.insertAt(0, up, t.f.root)
			p.setNext(right)
			return nil
		}); err != nil {
			t.f.setErr(err)
			return
		}
		t.f.root = newRoot
		t.f.height++
	}
	t.f.count++
	t.f.headerDirty = true
}

// insert 递归插入，返回（要上提的分隔键，新的右兄弟页号）；right == idxNoPage 表示没分裂。
func (t *btree) insert(page uint32, key []byte, val uint32) ([]byte, uint32, error) {
	fr, err := t.lockPage(page)
	if err != nil {
		return nil, idxNoPage, err
	}
	p := idxPage{data: fr.data}

	if p.leaf() {
		pos := p.searchFirstGE(key) // 相同键聚在一起
		if !p.insertAt(pos, key, val) {
			p.compact()
			if !p.insertAt(pos, key, val) {
				// pos == 页尾：顺序插入（自增主键最常见）。这种情况把右半留薄一点
				// （90/10），否则每次分裂都在左边留一个半空的叶子，文件白大一倍、
				// 扫描多读一倍页 —— 这是 B+Tree 顺序插入的经典优化。
				up, right, err := t.splitLeaf(fr, p, pos >= p.count())
				if err != nil {
					t.unlockPage(fr, false)
					return nil, idxNoPage, err
				}
				// 分裂只均分了「原来那些条目」，新条目还没进去 —— 现在两半都有空间了，
				// 按分隔键决定放哪一边（这一步漏掉就是「插进去的行凭空少掉几条」）。
				if bytesCompare(key, up) >= 0 {
					err = t.f.writeIdx(right, func(q idxPage) error {
						q.insertAt(q.searchFirstGE(key), key, val)
						return nil
					})
				} else {
					p.insertAt(p.searchFirstGE(key), key, val)
				}
				t.unlockPage(fr, err == nil)
				if err != nil {
					return nil, idxNoPage, err
				}
				return up, right, nil
			}
		}
		t.unlockPage(fr, true)
		return nil, idxNoPage, nil
	}

	n := p.count()
	i := p.searchFirstGT(key)
	child := p.next()
	if i < n {
		child = p.val(i)
	}
	up, right, err := t.insert(child, key, val)
	if err != nil {
		t.unlockPage(fr, false)
		return nil, idxNoPage, err
	}
	if right == idxNoPage {
		t.unlockPage(fr, false)
		return nil, idxNoPage, nil
	}

	// 把 (up, right) 塞进本层：键插在第 i 位，右孩子插在第 i+1 个孩子的位置
	if !p.insertInnerAt(i, up, right) {
		p.compact()
		if !p.insertInnerAt(i, up, right) {
			up2, right2, err := t.splitInner(fr, p)
			if err != nil {
				t.unlockPage(fr, false)
				return nil, idxNoPage, err
			}
			// 分裂之后同样要把这次要插的分隔键补进去（落到两半之一）
			if bytesCompare(up, up2) >= 0 {
				err = t.f.writeIdx(right2, func(q idxPage) error {
					q.insertInnerAt(q.searchFirstGT(up), up, right)
					return nil
				})
			} else {
				p.insertInnerAt(p.searchFirstGT(up), up, right)
			}
			t.unlockPage(fr, err == nil)
			if err != nil {
				return nil, idxNoPage, err
			}
			return up2, right2, nil
		}
	}
	t.unlockPage(fr, true)
	return nil, idxNoPage, nil
}

// insertInnerAt 在内部节点的第 i 个键位置插入分隔键，并把新孩子放到第 i+1 个位置。
func (p idxPage) insertInnerAt(i int, up []byte, right uint32) bool {
	n := p.count()
	if i >= n { // 追加在末尾：新孩子就是最右孩子
		last := p.next()
		if !p.insertAt(i, up, last) {
			return false
		}
		p.setNext(right)
		return true
	}
	oldChild := p.val(i)
	if !p.insertAt(i, up, oldChild) {
		return false
	}
	// 插入后 slot[i+1] 是原来的 slot[i]（键对、孩子不对）：把它的孩子换成新的右兄弟
	p.setSlotVal(i+1, right)
	return true
}

func (p idxPage) setSlotVal(i int, val uint32) {
	_, ln, _ := p.slotAt(i)
	off, _, _ := p.slotAt(i)
	p.setSlot(i, off, ln, val)
}

// splitLeaf 把叶子对半分，返回（右半的第一个键，右页号）。
func (t *btree) splitLeaf(left *bufFrame, p idxPage, atEnd bool) ([]byte, uint32, error) {
	rightPage := t.f.allocPage()
	n := p.count()
	mid := n / 2
	if atEnd {
		mid = n - n/10
		if mid < 1 {
			mid = 1
		}
		if mid >= n {
			mid = n - 1
		}
	}
	var firstKey []byte
	err := t.f.writeIdx(rightPage, func(r idxPage) error {
		r.initAs(rightPage, true)
		for i := mid; i < n; i++ {
			k := append([]byte(nil), p.key(i)...)
			if i == mid {
				firstKey = append([]byte(nil), k...)
			}
			r.insertAt(r.count(), k, p.val(i))
		}
		// 维护叶子链（双向）
		r.setNext(p.next())
		r.setPrev(left.id)
		if nx := p.next(); nx != idxNoPage {
			_ = t.f.writeIdx(nx, func(q idxPage) error {
				q.setPrev(rightPage)
				return nil
			})
		}
		p.setNext(rightPage)
		// 左半摘掉后半
		for p.count() > mid {
			p.removeAt(p.count() - 1)
		}
		p.compact()
		return nil
	})
	if err != nil {
		return nil, idxNoPage, err
	}
	if len(firstKey) == 0 {
		return nil, idxNoPage, errIdxMagic
	}
	return firstKey, rightPage, nil
}

// splitInner 把内部节点对半分，中间的键上提，返回（上提键，右页号）。
func (t *btree) splitInner(left *bufFrame, p idxPage) ([]byte, uint32, error) {
	rightPage := t.f.allocPage()
	n := p.count()
	mid := n / 2
	up := append([]byte(nil), p.key(mid)...)
	oldNext := p.next()
	moveChild := p.val(mid) // 左半的最右孩子

	err := t.f.writeIdx(rightPage, func(r idxPage) error {
		r.initAs(rightPage, false)
		for i := mid + 1; i < n; i++ {
			k := append([]byte(nil), p.key(i)...)
			r.insertAt(r.count(), k, p.val(i))
		}
		r.setNext(oldNext)
		for p.count() > mid {
			p.removeAt(p.count() - 1)
		}
		p.setNext(moveChild)
		p.compact()
		return nil
	})
	if err != nil {
		return nil, idxNoPage, err
	}
	return up, rightPage, nil
}

// ---- 删除 ----

// Delete 删掉精确匹配的 (key, slot)，返回是否删到。
func (t *btree) Delete(key string, slot int) bool {
	if !t.valid() {
		return false
	}
	page, i := t.seek([]byte(key))
	for page != idxNoPage {
		next, removed, err := t.deleteInPage(page, []byte(key), uint32(slot), i)
		if err != nil {
			t.f.setErr(err)
			return false
		}
		if removed {
			t.f.count--
			t.f.headerDirty = true
			return true
		}
		if next == idxNoPage {
			return false
		}
		page, i = next, 0
	}
	return false
}

// deleteInPage 在一页里找 (key, slot) 并删掉；返回（下一个要看的叶子页号，是否删到）。
func (t *btree) deleteInPage(page uint32, key []byte, slot uint32, start int) (uint32, bool, error) {
	fr, err := t.lockPage(page)
	if err != nil {
		return idxNoPage, false, err
	}
	p := idxPage{data: fr.data}
	n := p.count()
	for i := start; i < n; i++ {
		c := bytesCompare(p.key(i), key)
		if c > 0 {
			t.unlockPage(fr, false)
			return idxNoPage, false, nil
		}
		if c == 0 && p.val(i) == slot {
			p.removeAt(i)
			if p.count() == 0 {
				t.f.emptyLeaves++
			}
			t.unlockPage(fr, true)
			return idxNoPage, true, nil
		}
	}
	nx := p.next()
	t.unlockPage(fr, false)
	return nx, false, nil
}

// ---- 查找 ----

// Get 返回某个键对应的全部槽位（等值查找）。
func (t *btree) Get(key string) []int {
	if !t.valid() {
		return nil
	}
	kb := []byte(key)
	page, i := t.seek(kb)
	var out []int
	for page != idxNoPage {
		next, done := t.scanPage(page, i, func(k []byte, v uint32) int {
			c := bytesCompare(k, kb)
			if c > 0 {
				return -1
			}
			if c == 0 {
				out = append(out, int(v))
			}
			return 0
		})
		if done {
			return out
		}
		page, i = next, 0
	}
	return out
}

// scanPage 在一页里从 start 开始逐个回调；cb 返回 -1 表示提前结束（已经越过目标）。
func (t *btree) scanPage(page uint32, start int, cb func(k []byte, v uint32) int) (uint32, bool) {
	fr, err := t.f.pool.fetch(page)
	if err != nil {
		t.f.setErr(err)
		return idxNoPage, true
	}
	defer t.f.pool.unpin(fr, false)
	fr.latch.RLock()
	defer fr.latch.RUnlock()
	p := idxPage{data: fr.data}
	for i := start; i < p.count(); i++ {
		if cb(p.key(i), p.val(i)) < 0 {
			return idxNoPage, true
		}
	}
	return p.next(), false
}

// seek 定位到「第一个 >= key 的条目」所在的叶子与下标。
//
// 内部节点下探用「第一个 >= key」（注意不是 >）：键允许重复，分裂会把同一个键
// 切到相邻两片叶子里，此时内部节点的分隔键就等于这个键。用 >= 落到最左那片，
// 再沿叶子链往右扫就能把散落的重复键全部捞到；用 > 会直接跳到最右，漏掉前面的。
//
// （插入下探才用「第一个 > key」—— 那是要插在所有等于 key 的条目之后。）
func (t *btree) seek(key []byte) (uint32, int) {
	if !t.valid() {
		return idxNoPage, 0
	}
	page := t.f.root
	for {
		var i int
		var next uint32
		err := t.f.readIdx(page, func(p idxPage) error {
			if p.leaf() {
				i = p.searchFirstGE(key)
				next = idxNoPage
				return nil
			}
			n := p.count()
			// 下探用「第一个 >= key」：重复键会被切到相邻叶子，分隔键就等于该键，
			// 用 >= 才能落到最左那片（用 > 会直接跳到最右，漏掉前面的重复键）
			j := p.searchFirstGE(key)
			if j < n {
				next = p.val(j)
			} else {
				next = p.next()
			}
			return nil
		})
		if err != nil {
			t.f.setErr(err)
			return idxNoPage, 0
		}
		if next == idxNoPage {
			return page, i
		}
		page = next
	}
}

// ---- 游标 ----

// btreeCursor 是位置游标：只记页号和下标，每走一步重新取页。
//
// 不能存节点指针：页随时可能被缓冲池换出，指针立刻悬空。
type btreeCursor struct {
	t    *btree
	page uint32
	idx  int
	done bool
}

// cursor 定位到第一个 >= key 的条目。
func (t *btree) cursor(key string) *btreeCursor {
	if !t.valid() {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	page, i := t.seek([]byte(key))
	return &btreeCursor{t: t, page: page, idx: i}
}

// first 定位到最小的键。
func (t *btree) first() *btreeCursor {
	if !t.valid() {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	page := t.f.root
	for {
		var leaf bool
		var left uint32
		if err := t.f.readIdx(page, func(p idxPage) error {
			leaf = p.leaf()
			if !leaf {
				if p.count() > 0 {
					left = p.val(0)
				} else {
					left = p.next()
				}
			}
			return nil
		}); err != nil {
			t.f.setErr(err)
			return &btreeCursor{t: t, page: idxNoPage, done: true}
		}
		if leaf {
			return &btreeCursor{t: t, page: page, idx: 0}
		}
		if left == idxNoPage {
			return &btreeCursor{t: t, page: idxNoPage, done: true}
		}
		page = left
	}
}

// last 定位到最大的键。
func (t *btree) last() *btreeCursor {
	if !t.valid() {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	page := t.f.root
	for {
		var leaf bool
		var right uint32
		if err := t.f.readIdx(page, func(p idxPage) error {
			leaf, right = p.leaf(), p.next()
			return nil
		}); err != nil {
			t.f.setErr(err)
			return &btreeCursor{t: t, page: idxNoPage, done: true}
		}
		if leaf {
			n := 0
			_ = t.f.readIdx(page, func(p idxPage) error { n = p.count(); return nil })
			return &btreeCursor{t: t, page: page, idx: n - 1}
		}
		if right == idxNoPage {
			return &btreeCursor{t: t, page: idxNoPage, done: true}
		}
		page = right
	}
}

// revCursorLe 定位到最后一个 `<= key` 的条目（含等于）。
func (t *btree) revCursorLe(key string) *btreeCursor {
	if !t.valid() {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	page, i := t.seekUpper([]byte(key)) // 第一个 > key
	if i < 0 {
		return t.prevLeafCursor(page)
	}
	return &btreeCursor{t: t, page: page, idx: i - 1}
}

// revCursorLt 定位到最后一个 `< key` 的条目（不含等于）。
func (t *btree) revCursorLt(key string) *btreeCursor {
	if !t.valid() {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	page, i := t.seek([]byte(key)) // 第一个 >= key
	if i < 0 {
		return t.prevLeafCursor(page)
	}
	return &btreeCursor{t: t, page: page, idx: i - 1}
}

// prevLeafCursor 当定位落在叶子左端之外时，退到上一片叶子的末尾。
func (t *btree) prevLeafCursor(page uint32) *btreeCursor {
	if page == idxNoPage {
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	var prev uint32
	if err := t.f.readIdx(page, func(p idxPage) error { prev = p.prev(); return nil }); err != nil {
		t.f.setErr(err)
		return &btreeCursor{t: t, page: idxNoPage, done: true}
	}
	if prev == idxNoPage {
		return &btreeCursor{t: t, page: page, idx: -1}
	}
	n := 0
	_ = t.f.readIdx(prev, func(p idxPage) error { n = p.count(); return nil })
	return &btreeCursor{t: t, page: prev, idx: n - 1}
}

// seekUpper 定位到第一个 > key 的条目所在的叶子与下标。
func (t *btree) seekUpper(key []byte) (uint32, int) {
	if !t.valid() {
		return idxNoPage, 0
	}
	page := t.f.root
	for {
		var i int
		var next uint32
		if err := t.f.readIdx(page, func(p idxPage) error {
			if p.leaf() {
				i, next = p.searchFirstGT(key), idxNoPage
				return nil
			}
			n := p.count()
			j := p.searchFirstGT(key)
			if j < n {
				next = p.val(j)
			} else {
				next = p.next()
			}
			return nil
		}); err != nil {
			t.f.setErr(err)
			return idxNoPage, 0
		}
		if next == idxNoPage {
			return page, i
		}
		page = next
	}
}

// next 前进一格。走到末尾后返回 ok=false，之后再调也一直是 false。
func (c *btreeCursor) next() (string, int, bool) {
	if c == nil || c.done || !c.t.valid() {
		return "", 0, false
	}
	for c.page != idxNoPage {
		var key []byte
		var val uint32
		var cnt int
		var nx uint32
		err := c.t.f.readIdx(c.page, func(p idxPage) error {
			cnt, nx = p.count(), p.next()
			if c.idx >= 0 && c.idx < cnt {
				key = append(key[:0], p.key(c.idx)...)
				val = p.val(c.idx)
			}
			return nil
		})
		if err != nil {
			c.t.f.setErr(err)
			c.done = true
			return "", 0, false
		}
		if c.idx >= 0 && c.idx < cnt {
			c.idx++
			return string(key), int(val), true
		}
		c.page, c.idx = nx, 0
	}
	c.done = true
	return "", 0, false
}

// prev 反向走一格。走到开头后返回 ok=false。
func (c *btreeCursor) prev() (string, int, bool) {
	if c == nil || c.done || !c.t.valid() {
		return "", 0, false
	}
	for c.page != idxNoPage {
		var key []byte
		var val uint32
		var cnt int
		var pv uint32
		err := c.t.f.readIdx(c.page, func(p idxPage) error {
			cnt, pv = p.count(), p.prev()
			if c.idx >= 0 && c.idx < cnt {
				key = append(key[:0], p.key(c.idx)...)
				val = p.val(c.idx)
			}
			return nil
		})
		if err != nil {
			c.t.f.setErr(err)
			c.done = true
			return "", 0, false
		}
		if c.idx >= 0 && c.idx < cnt {
			c.idx--
			return string(key), int(val), true
		}
		c.page, c.idx = pv, -1
		if c.page != idxNoPage {
			n := 0
			if err := c.t.f.readIdx(c.page, func(p idxPage) error { n = p.count(); return nil }); err != nil {
				c.t.f.setErr(err)
				c.done = true
				return "", 0, false
			}
			c.idx = n - 1
		}
	}
	c.done = true
	return "", 0, false
}

// ---- 区间与遍历 ----

// KeyRange 描述一个键区间。HasLo/HasHi 为 false 表示该侧不设限。
type KeyRange struct {
	Lo, Hi       string
	HasLo, HasHi bool
	LoInc, HiInc bool
}

// Range 按键升序遍历区间内的条目，fn 返回 false 时提前结束。
func (t *btree) Range(r KeyRange, fn func(key string, slot int) bool) {
	if !t.valid() {
		return
	}
	var c *btreeCursor
	if r.HasLo {
		c = t.cursor(r.Lo)
	} else {
		c = t.first()
	}
	for {
		k, s, ok := c.next()
		if !ok {
			return
		}
		if r.HasLo && k < r.Lo {
			continue // seek 可能落在含更小键的叶子上
		}
		if r.HasLo && !r.LoInc && k == r.Lo {
			continue
		}
		if r.HasHi && (k > r.Hi || (!r.HiInc && k == r.Hi)) {
			return
		}
		if !fn(k, s) {
			return
		}
	}
}

// Walk 按全局键序遍历每个条目（测试与重建用）。
func (t *btree) Walk(fn func(key string, slot int)) {
	if !t.valid() {
		return
	}
	c := t.first()
	for {
		k, s, ok := c.next()
		if !ok {
			return
		}
		fn(k, s)
	}
}

// Reset 清空整棵树（TRUNCATE 用）。
func (t *btree) Reset() {
	if !t.valid() {
		return
	}
	if err := t.f.reset(); err != nil {
		t.f.setErr(err)
	}
	t.f.emptyLeaves = 0
	t.f.nodes = 1
}

// Stats 返回节点总数与空叶数。这里是**增量计数**（建页/清空页时累加），
// 不再遍历整棵树 —— 这个判断在每次提交后都可能被调用。
func (t *btree) Stats() (nodes, emptyLeaves int) {
	if !t.valid() {
		return 0, 0
	}
	return int(t.f.nodes), int(t.f.emptyLeaves)
}

// NeedRebuild 判断延迟合并攒下的碎片是否已经值得整树重建。
func (t *btree) NeedRebuild() bool {
	nodes, empty := t.Stats()
	if nodes <= 4 {
		return false
	}
	return float64(empty) > float64(nodes)*btreeEmptyRatio
}

const btreeEmptyRatio = 0.3

// ---- 批量构建 ----

// idxPair 是一条待建索引的（键，槽位）。
type idxPair struct {
	Key  string
	Slot int
}

// Build 用一批条目重建整棵树：排好序后顺序填叶、再自底向上造内部节点。
//
// 逐条 Insert 建 20 万行的索引要几百毫秒（每次都要下探 + 页内搬移），
// 顺序填页是纯追加写，快一个量级；而且叶子的填充率天然接近 100%。
func (t *btree) Build(pairs []idxPair) error {
	if !t.valid() {
		return nil
	}
	if err := t.f.reset(); err != nil {
		return err
	}
	t.f.nodes, t.f.emptyLeaves = 1, 0
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].Key < pairs[j].Key })

	// 叶层：一页一页填满
	type nodeRef struct {
		page uint32
		min  []byte
	}
	var level []nodeRef
	var cur *bufFrame
	var curPage uint32
	var curMin []byte

	flushLeaf := func() error {
		if cur == nil {
			return nil
		}
		t.unlockPage(cur, true)
		level = append(level, nodeRef{page: curPage, min: curMin})
		cur = nil
		return nil
	}
	for _, pr := range pairs {
		kb := []byte(pr.Key)
		for {
			if cur == nil {
				curPage = t.f.allocPage()
				fr, err := t.lockPage(curPage)
				if err != nil {
					return err
				}
				idxPage{data: fr.data}.initAs(curPage, true)
				cur, curMin = fr, append([]byte(nil), kb...)
			}
			p := idxPage{data: cur.data}
			if p.insertAt(p.count(), kb, uint32(pr.Slot)) {
				break
			}
			// 这页满了：收尾、开新叶
			if err := flushLeaf(); err != nil {
				return err
			}
		}
	}
	if err := flushLeaf(); err != nil {
		return err
	}
	if len(level) == 0 {
		// 空索引：一个空叶当根
		root := t.f.allocPage()
		if err := t.f.writeIdx(root, func(p idxPage) error {
			p.initAs(root, true)
			return nil
		}); err != nil {
			return err
		}
		t.f.root, t.f.height, t.f.count = root, 1, 0
		t.f.headerDirty = true
		return nil
	}

	// 叶子链：按生成顺序串起来
	for i := 0; i < len(level); i++ {
		next := idxNoPage
		if i+1 < len(level) {
			next = level[i+1].page
		}
		prev := idxNoPage
		if i > 0 {
			prev = level[i-1].page
		}
		pg := level[i].page
		nx, pv := next, prev
		if err := t.f.writeIdx(pg, func(p idxPage) error {
			p.setNext(nx)
			p.setPrev(pv)
			return nil
		}); err != nil {
			return err
		}
	}

	// 内部层：自底向上
	leafCount := uint32(len(level))
	height := 1
	for len(level) > 1 {
		var up []nodeRef
		for i := 0; i < len(level); {
			page := t.f.allocPage()
			fr, err := t.lockPage(page)
			if err != nil {
				return err
			}
			p := idxPage{data: fr.data}
			p.initAs(page, false)
			minKey := append([]byte(nil), level[i].min...)
			// 第一个孩子放 slot 0 的孩子位，往后的孩子各自带自己的分隔键
			p.setNext(level[i].page)
			i++
			for i < len(level) {
				if !p.insertInnerAt(p.count(), level[i].min, level[i].page) {
					break
				}
				i++
			}
			t.unlockPage(fr, true)
			up = append(up, nodeRef{page: page, min: minKey})
		}
		level = up
		height++
	}
	t.f.root = level[0].page
	t.f.height = uint16(height)
	t.f.count = int64(len(pairs))
	// 重建判断只看叶子页：内部页数量级小得多，混进来只会让阈值失真
	t.f.nodes, t.f.emptyLeaves = leafCount, 0
	t.f.headerDirty = true
	return nil
}

// ---- 页操作小工具 ----

// initAs 把一个页初始化成「空叶 / 空内部节点」。
func (p idxPage) initAs(page uint32, leaf bool) {
	initIdxPage(p.data, page, leaf)
}

// lockPage 取页并加写锁；用完必须 unlockPage。
func (t *btree) lockPage(page uint32) (*bufFrame, error) {
	fr, err := t.f.pool.fetch(page)
	if err != nil {
		return nil, err
	}
	fr.latch.Lock()
	return fr, nil
}

func (t *btree) unlockPage(fr *bufFrame, dirty bool) {
	fr.latch.Unlock()
	t.f.pool.unpin(fr, dirty)
}
