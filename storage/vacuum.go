package storage

import (
	"fmt"
	"os"

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
		td.unlockPages()
		if err != nil {
			return out, err
		}
		rep.Reclaimed = int(rec)

		// 6) 整理后的统计（文件大小要重新 stat，写盘之后才准）
		rep.Pages = int(td.pages.pf.pageCount())
		if st, err := os.Stat(td.pages.path); err == nil {
			rep.Bytes = st.Size()
		}
		s.loadMu.Lock()
		s.hasSnap[cleanName(t.Name)] = true
		s.tableGen[cleanName(t.Name)] = gen
		s.loadMu.Unlock()
		out = append(out, rep)
	}

	if err := s.writeMeta(gen, s.metaTablesLocked()); err != nil {
		return out, err
	}
	return out, nil
}
