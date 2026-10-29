package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"godb/catalog"
)

// RewriteReport 是 VACUUM FULL 对一张表的结果。
type RewriteReport struct {
	Table string
	// Rows 是重写时搬过去的行数。
	Rows int
	// PagesBefore/After 是重写前后的数据页数。
	PagesBefore, PagesAfter int
	// BytesBefore/After 是重写前后的页文件大小。
	BytesBefore, BytesAfter int64
	// LobBefore/After 是大字段文件在重写前后的大小。
	LobBefore, LobAfter int64
}

// swapMarkerMagic 是「文件交换进行中」的标记内容。
//
// 这个标记是整个重写唯一的原子点：两个 .new 文件都写完整并 fsync 之后才写它，
// 恢复时看到它就把两次 rename 补做完。rename 本身是原子的，所以不存在
// 「一半新一半旧」的中间态。
const swapMarkerMagic = "GDBKSWAP"

// rewriteTableLocked 把一张表的数据整份重写（VACUUM FULL）。
//
// 为什么需要它：大字段是**追加写**的（UPDATE 不原地改，否则会破坏仍在读的旧版本），
// 所以文件里会攒下大量没人引用的旧正文；页里的空洞同理 —— 普通 VACUUM 只能把洞
// 收进页里、收不到文件外。重写一次 = 全新的堆页文件 + 全新的大字段文件，只装活数据。
//
// 为什么不用 WAL：这是一次「文件级」的操作，逻辑重放帮不上忙。改用
// 「两个 .new 写完整 → 标记 → 两次 rename」这套：
//
//	崩在标记之前：旧文件完好，.new 是垃圾（下次启动顺手删掉）
//	崩在标记之后：两个 .new 都已经 fsync 过，恢复时把 rename 补做完即可
//
// RID 一个都不动（槽位号原样写进新页的 gid），所以索引完全不用重建。
//
// 调用方必须持 CommitMu（VACUUM 就是这样），并且事先做过 checkpoint，
// 保证 WAL 里没有「针对旧页的全页镜像」——那些镜像会被重放到新页上，直接写坏。
func (s *Store) rewriteTableLocked(t *catalog.Table) (RewriteReport, error) {
	rep := RewriteReport{Table: t.Name}
	td := s.Table(t.Name)
	if td == nil || td.pages == nil || td.id <= 0 {
		return rep, nil
	}
	datPath := tablePagePath(s.Dir, td.id)
	lobPath := lobPathOf(datPath)
	newDat := datPath + ".new"
	newLob := lobPath + ".new"

	rep.BytesBefore = statSize(datPath)
	rep.LobBefore = statSize(lobPath)
	rep.PagesBefore = int(rep.BytesBefore / PageSize)

	_ = os.Remove(newDat)
	_ = os.Remove(newLob)
	// 新文件不参与全局预算：重写是离线操作，给它一个固定的小池子就够了
	np, err := openTablePagesAt(newDat, newLob, nil, defaultPoolPages)
	if err != nil {
		return rep, err
	}
	cleanup := func() {
		_ = np.close()
		_ = os.Remove(newDat)
		_ = os.Remove(newLob)
	}

	// 分批搬：一次编 256 行，内存只占一页批的量（整表塞进内存会把大表打死）
	batch := make([]pendingRow, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := np.putBatch(batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	slots := td.SlotCount()
	for i := 0; i < slots; i++ {
		r, ok := td.rowAt(i) // 被换出的行会从旧页换进来
		if !ok {
			continue
		}
		tuple, err := np.encodeRowTuple(r)
		if err != nil {
			cleanup()
			return rep, err
		}
		batch = append(batch, pendingRow{slot: i, tuple: tuple})
		rep.Rows++
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				cleanup()
				return rep, err
			}
		}
	}
	if err := flush(); err != nil {
		cleanup()
		return rep, err
	}

	// 刷盘顺序：大字段 → 页 → 两个都 fsync。都落到盘上才有资格写标记。
	if err := np.lob.sync(); err != nil {
		cleanup()
		return rep, err
	}
	if _, err := np.checkpoint(); err != nil {
		cleanup()
		return rep, err
	}
	if err := np.pf.sync(); err != nil {
		cleanup()
		return rep, err
	}
	if err := np.close(); err != nil {
		_ = os.Remove(newDat)
		_ = os.Remove(newLob)
		return rep, err
	}

	// 旧页文件必须先关掉：Windows 上「目标文件还开着」时 rename 直接 Access denied。
	// 关之前所有行已经搬完（rowAt 把换出的行都取回来了），所以不再需要它。
	td.lockPages()
	td.closePages()
	restore := func() {
		// 失败时把表挂回原来的文件（内存里的行还在，重建目录即可）
		if err := td.attachPages(s.Dir, td.id, t, s.Budget, s.idxGen[cleanName(t.Name)]); err == nil {
			_ = td.pages.loadAll(func(int, []byte) error { return nil })
		}
	}

	// ---- 从这里开始是「不可回头」的一段 ----
	marker := datPath + ".swap"
	if err := writeFileAtomic(marker, []byte(swapMarkerMagic)); err != nil {
		restore()
		td.unlockPages()
		return rep, err
	}
	if err := os.Rename(newDat, datPath); err != nil {
		_ = os.Remove(marker)
		restore()
		td.unlockPages()
		return rep, err
	}
	if _, err := os.Stat(newLob); err == nil {
		if err := os.Rename(newLob, lobPath); err != nil {
			restore()
			td.unlockPages()
			return rep, err
		}
	} else {
		// 表里已经没有大字段了：旧文件直接删掉（否则永远占着地方）
		_ = os.Remove(lobPath)
	}
	syncDir(filepath.Dir(datPath))
	_ = os.Remove(marker)
	syncDir(filepath.Dir(datPath))

	rep.PagesAfter = int(np.pageCount())
	rep.BytesAfter = statSize(datPath)
	rep.LobAfter = statSize(lobPath)

	// 把内存里的表挂到新文件上：重开 → 只重建 RID 目录（不解码行）。
	// 页号可能整页不同，所以 loadAll 必须走一遍；行本身（td.rows）不用动 ——
	// RID 没变，内容也没变。
	err = td.attachPages(s.Dir, td.id, t, s.Budget, s.idxGen[cleanName(t.Name)])
	if err == nil && td.pages != nil {
		err = td.pages.loadAll(func(int, []byte) error { return nil })
	}
	td.unlockPages()
	if err != nil {
		return rep, err
	}
	return rep, nil
}

// finishPendingSwaps 在启动时收尾「重写到一半」的表：见 rewriteTableLocked 的注释。
//
// 没有标记的 .new 文件是垃圾（崩在写标记之前），直接删掉。
func finishPendingSwaps(dir string) {
	tables := filepath.Join(dir, "tables")
	entries, err := os.ReadDir(tables)
	if err != nil {
		return
	}
	// 两轮：**先**把标记处理完，**再**清理没人认领的 .new。
	//
	// 合成一轮会踩到一个很隐蔽的坑：目录项是按名字排的，`t000001.dat.new`
	// 排在 `t000001.dat.swap` 前面 —— 先看到 .new 就当成垃圾删掉，
	// 等到标记想 rename 时源文件已经没了（实测就是这么翻车的）。
	for _, de := range entries {
		name := de.Name()
		if !strings.HasSuffix(name, ".swap") {
			continue
		}
		// de.Name() 只是文件名，必须拼回 tables/ 才是真实路径
		base := filepath.Join(tables, strings.TrimSuffix(name, ".swap")) // …/tXXXXXX.dat
		if _, err := os.Stat(base + ".new"); err == nil {
			_ = os.Rename(base+".new", base)
		}
		lob := lobPathOf(base)
		if _, err := os.Stat(lob + ".new"); err == nil {
			_ = os.Rename(lob+".new", lob)
		}
		_ = os.Remove(filepath.Join(tables, name))
		syncDir(tables)
	}
	entries, err = os.ReadDir(tables)
	if err != nil {
		return
	}
	for _, de := range entries {
		if strings.HasSuffix(de.Name(), ".new") {
			_ = os.Remove(filepath.Join(tables, de.Name()))
		}
	}
}

// statSize 返回文件大小（不存在就是 0）。
func statSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// syncDir 把目录项刷到盘上（rename / remove 之后要它才真正「落定」）。
// Windows 上目录不是句柄可 fsync 的对象，失败就直接忽略。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

// FullVacuum 是给「VACUUM FULL」用的入口：先做一次 checkpoint（把 WAL 清空、
// 保证没有悬着的全页镜像），再把表整份重写。
func (s *Store) FullVacuum(t *catalog.Table) (RewriteReport, error) {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()
	// 关键：重写期间不能有「指向旧页的镜像」等在 WAL 里。先落一轮盘把 WAL 轮转掉。
	if err := s.forceCheckpointLocked(CPReasonFlush); err != nil {
		return RewriteReport{Table: t.Name}, err
	}
	rep, err := s.rewriteTableLocked(t)
	if err != nil {
		return rep, fmt.Errorf("重写表 %s 失败: %w", t.Name, err)
	}
	// 新文件是「整份写完后原子换上去」的，没有任何 WAL 记录描述它；
	// 这里补一条 meta，把代次和行数对齐，重启时就不会以为磁盘落后于 WAL。
	gen := s.appliedGen
	if s.wal != nil {
		gen = s.wal.Gen()
	}
	key := cleanName(t.Name)
	s.loadMu.Lock()
	s.hasSnap[key] = true
	s.tableGen[key] = gen
	if td := s.Data[key]; td != nil {
		s.tableRows[key] = td.Live
	}
	s.loadMu.Unlock()
	if err := s.writeMeta(gen, s.metaTablesLocked()); err != nil {
		return rep, err
	}
	return rep, nil
}
