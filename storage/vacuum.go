package storage

import (
	"fmt"
	"log"
	"os"
	"time"

	"godb/catalog"
)

// VacuumReport 是 VACUUM 对一张表的处理结果。
type VacuumReport struct {
	Table string
	// Compacted 是整理过（把删掉的行留下的洞收回来）的页数。
	Compacted int
	// Reclaimed 是从文件尾部摘掉的空页数。中间的整页空页留着（页号不能重排）。
	Reclaimed int
	// Pages 是整理之后的页数，Bytes 是页文件大小。
	Pages int
	Bytes int64
	// IndexesRebuilt 是顺手重建的索引个数（空叶占比过阈值时才会做）。
	IndexesRebuilt int
	// LobBytes 是大字段文件的字节数，LobLive 是其中**还被行引用**的部分。
	// 两者差得越多，越该跑 VACUUM FULL（追加写留下的旧正文只能靠重写回收）。
	LobBytes int64
	LobLive  int64
}

// Vacuum 整理表数据：页内空洞回收 + 尾部空页回收。
//
// 为什么需要它：删一行只把槽位置空，占的字节还留在页里；反复「大批删 + 零散插」
// 之后，一页可能只剩几行却仍占 8KB。VACUUM 把这些洞收回来，并把文件尾部
// 整页空的页摘掉。
//
// 不做的事：**不重排 RID**。索引里存的就是槽位号，重排等于把索引全废掉 ——
// 那是「搬迁式 compact」才做的事，不在这套设计里。
//
// 顺序和 checkpoint 一样（日志先于页）：待落盘行写进页 → 整理页（取全页镜像）
// → 镜像写 WAL 并 fsync → 刷脏页 → 回收尾部空页 → 写 meta。
func (s *Store) Vacuum(name string) ([]VacuumReport, error) {
	s.CommitMu.Lock()
	defer s.CommitMu.Unlock()

	gen := s.appliedGen
	if s.wal != nil {
		gen = s.wal.Gen()
		if err := s.wal.SyncTo(s.wal.Written()); err != nil {
			return nil, err
		}
	}

	var targets []*catalog.Table
	if name == "" {
		targets = append(targets, s.Cat.Tables...)
	} else {
		t, ok := s.Cat.Get(name)
		if !ok {
			return nil, fmt.Errorf("relation %q does not exist", name)
		}
		targets = append(targets, t)
	}

	var out []VacuumReport
	for _, t := range targets {
		td := s.Table(t.Name) // 没加载就现加载：整理前必须知道它有多少行
		if td == nil || td.pages == nil {
			continue
		}
		rep := VacuumReport{Table: t.Name}
		// 整理期间不许读路径换页（整理要改页内容，镜像还没落盘）
		td.lockPages()

		// 1) 先把待落盘行写进页，否则整理的是旧内容
		fpis, err := td.pageStage()
		if err != nil {
			td.unlockPages()
			return out, err
		}
		// 2) 逐页整理（只搬元组字节，不动槽位目录）
		n, more, err := td.pages.vacuumPages()
		if err != nil {
			td.unlockPages()
			return out, err
		}
		rep.Compacted = n
		fpis = append(fpis, more...)

		// 3) 全页镜像先写进 WAL：改页之前必须让镜像先落盘
		if len(fpis) > 0 && s.wal != nil {
			recs := make([]walRec, 0, len(fpis))
			for _, img := range fpis {
				recs = append(recs, walRec{Gen: gen, Op: opFPI, Table: t.Name, Slot: -1, Page: img.page, Img: img.data})
			}
			if _, err := s.wal.Append(recs); err != nil {
				td.unlockPages()
				return out, err
			}
			if err := s.wal.SyncTo(s.wal.Written()); err != nil {
				td.unlockPages()
				return out, err
			}
		}

		// 4) 刷脏页
		if _, err := td.pageFlush(); err != nil {
			td.unlockPages()
			return out, err
		}
		// 5) 回收尾部空页
		rec, err := td.pages.reclaim()
		// 洞已经收干净（compact 把所有活元组挪紧了），计数清零。
		// 中途出错就不清：下次还能凭这个数再试一次。
		if err == nil {
			td.pages.holes.Store(0)
		}
		td.unlockPages()
		if err != nil {
			return out, err
		}
		// 6) 索引碎片：删除只摘键不合并页，空叶攒多了就顺手整棵重建
		//    （必须在放开 pageMu 之后做：重建要走 rowAt，那是读锁）
		if td.IndexFragmented() {
			td.RebuildIndex(t)
			rep.IndexesRebuilt = len(td.indexTrees)
			log.Printf("表 %s：索引空叶过多，已顺手重建（%d 个）", t.Name, rep.IndexesRebuilt)
		}
		rep.Reclaimed = int(rec)
		// 大字段：追加写会留下大量没人引用的旧正文，这里只统计（不动文件）——
		// 回收要走 VACUUM FULL（整份重写，见 rewrite.go）。
		if lp := td.pages.lobPath(); lp != "" {
			if rep.LobBytes = statSize(lp); rep.LobBytes > 0 {
				rep.LobLive, _ = td.pages.lobUsage()
			}
		}

		// 6) 整理后的统计（文件大小要重新 stat，写盘之后才准）
		rep.Pages = int(td.pages.pf.pageCount())
		if st, err := os.Stat(td.pages.path); err == nil {
			rep.Bytes = st.Size()
		}
		key := cleanName(t.Name)
		s.loadMu.Lock()
		s.hasSnap[key] = true
		s.tableGen[key] = gen
		// 记下「整理过了」的时刻：页密度低 + 这个时间很旧，才是「该整理了」的信号
		prev := s.tableVac[key]
		s.tableVac[key] = vacuumTimes{At: time.Now().UnixMilli(), Full: prev.Full}
		s.loadMu.Unlock()
		out = append(out, rep)
	}

	if err := s.writeMeta(gen, s.metaTablesLocked()); err != nil {
		return out, err
	}
	return out, nil
}

// vacuumTimes 是某张表的整理历史（Unix 毫秒，0 = 从没整理过）。
//
// At 是「最后一次 VACUUM 或 VACUUM FULL」；Full 只有整份重写才更新。
// 两者分开是因为它们的代价差一个量级：普通 VACUUM 只整理页内（页数不变），
// VACUUM FULL 要整份重写、期间占着表锁。
type vacuumTimes struct {
	At   int64
	Full int64
}
