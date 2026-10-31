package storage

import "fmt"

// TableSpaceDetail 是一张表的「页级构成」：把 页数×8KB 拆成三块。
//
// 和 TableSize 的区别是**它要读页**：TableSize 只 stat 文件，回答「占了多少」；
// 这里要把那 8KB×N 拆成「真的放着行」「整理一下就能用」「删出来的洞」。
// 洞只有 VACUUM 能收（洞不算可复用空闲），所以这三块的回收手段各不相同：
//
//	LiveBytes 收不回来（那是数据本身）
//	FreeBytes 插入可以直接复用
//	HoleBytes 要 VACUUM 整理之后才变成 FreeBytes
//
// 代价是 O(页数) 的读，所以它是「点开某张表才算」的按需接口，不进每秒轮询。
type TableSpaceDetail struct {
	Table     string `json:"table"`
	Pages     int    `json:"pages"`
	Capacity  int64  `json:"capacity"`  // Pages × PageSize
	LiveBytes int64  `json:"liveBytes"` // 存活元组占的字节
	FreeBytes int64  `json:"freeBytes"` // 整理后能用的空闲（不含洞）
	HoleBytes int64  `json:"holeBytes"` // 删出来的洞：只有 VACUUM 能收回
	LiveRows  int    `json:"liveRows"`  // 页里的存活元组数（与 td.Live 交叉验证）
	DeadSlots int    `json:"deadSlots"` // 空槽数：被删掉、还没被复用的槽位
	BadPages  int    `json:"badPages"`  // 解码失败的页（计数，不中断整表统计）
}

// SpaceDetail 逐页扫一遍，把这张表的页空间拆成「已用 / 空闲 / 洞」。
//
// 只读：不动槽位目录、不改页、不碰空闲空间图。为了让 VACUUM FULL 中途不会把
// pages 换掉，扫描期间持 pageMu 的读锁 —— 换页文件那条路径要写锁。
func (s *Store) SpaceDetail(name string) (TableSpaceDetail, error) {
	td := s.Table(name) // 没加载就现加载（加载只建目录，不解码行）
	if td == nil {
		return TableSpaceDetail{Table: name}, fmt.Errorf("relation %q does not exist", name)
	}
	td.pageMu.RLock()
	defer td.pageMu.RUnlock()

	rep := TableSpaceDetail{Table: td.Name, LiveRows: 0}
	tp := td.pages
	if tp == nil {
		return rep, nil
	}
	n := tp.pf.pageCount()
	for page := uint32(0); page < n; page++ {
		f, err := tp.fetch(page)
		if err != nil {
			return rep, err
		}
		f.latch.RLock()
		pg, err := decodeOrInitPage(f.data, page)
		f.latch.RUnlock()
		tp.pool.unpin(f, false)
		if err != nil {
			// 单页坏了不该让整张表的统计失败：记一笔，继续扫。
			rep.BadPages++
			rep.Pages++
			continue
		}
		rep.Pages++
		rep.LiveBytes += int64(pg.liveBytes())
		rep.FreeBytes += int64(pg.freeBytes())
		rep.HoleBytes += int64(pg.holeBytes())
		rep.LiveRows += pg.liveCount()
		if dead := len(pg.slots) - pg.liveCount(); dead > 0 {
			rep.DeadSlots += dead
		}
	}
	rep.Capacity = int64(rep.Pages) * PageSize
	return rep, nil
}
