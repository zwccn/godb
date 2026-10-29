package storage

import (
	"log"
	"time"
)

// 自动整理（autovacuum）。
//
// 为什么需要它：删一行只把槽位置空，占的字节还留在页里。删得多了，一页可能
// 只剩两行却仍占着 8KB —— 文件只涨不缩，除非有人记得手动 VACUUM。在这个库里
// 「记得手动跑」约等于「永远不会跑」。
//
// 什么时候动手：某张表删出来的洞累计超过文件大小的 1/4（小表不足 8 页不折腾）。
//
// 为什么不拿表锁：整理只搬页内的字节、摘掉文件尾的空页，RID 和行的可见性都
// 不变，读能照读（页内互斥 pageMu 已经够）。所以这条路径**不需要停写**，也不会
// 跟谁的事务抢 X 锁 —— 它只用 CommitMu 把「刷页 + 改 meta」串起来。
//
// 一句话：删出来的垃圾攒够四分之一，后台就顺手扫掉。

// autoVacuumInterval 是两轮检查的最小间隔（秒）。整理不便宜，别一有机会就上。
const autoVacuumInterval = 60

// tickAutoVacuum 由后台定时器每秒调一次，自己按间隔节流。
func (s *Store) tickAutoVacuum() {
	now := time.Now().Unix()
	last := s.avLast.Load()
	if now-last < autoVacuumInterval {
		return
	}
	// CAS 抢这一轮：抢不到说明别的 goroutine 已经在整理了
	if !s.avLast.CompareAndSwap(last, now) {
		return
	}
	for _, name := range s.tablesNeedingVacuum() {
		reps, err := s.Vacuum(name)
		if err != nil {
			log.Printf("自动整理 %s 失败: %v", name, err)
			continue
		}
		for _, r := range reps {
			if r.Compacted == 0 && r.Reclaimed == 0 {
				continue // 洞都散在没被整理到的页里，下一轮再说
			}
			log.Printf("自动整理 %s：收了 %d 页空洞、摘掉 %d 个尾部空页，现在 %d 页 %d 字节",
				r.Table, r.Compacted, r.Reclaimed, r.Pages, r.Bytes)
		}
	}
}

// tablesNeedingVacuum 挑出「值得整理一张」的表。只看已经在内存里的表：
// 没加载的表说明一直没人读它，不着急。
func (s *Store) tablesNeedingVacuum() []string {
	s.loadMu.RLock()
	defer s.loadMu.RUnlock()
	var out []string
	for key, td := range s.Data {
		if !s.loaded[key] || td == nil || td.pages == nil {
			continue
		}
		if td.pages.needsVacuum() {
			out = append(out, td.Name)
		}
	}
	return out
}
