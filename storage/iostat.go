package storage

import (
	"sync/atomic"
	"time"
)

// 读页阻塞的库级累计。
//
// 为什么值得单独记：一条语句的「服务端耗时」里，有一大块既不在算子里、也不是 CPU ——
// 冷启动后第一次访问一张表，要把整个页文件读进缓冲池，5 万行的表能花掉二十毫秒。
// 这段时间按算子统计永远统计不到（它发生在计划执行之前），于是界面上就成了
// 「服务端 19ms，每一步却只有 0.5ms」的悬案。
//
// 口径：
//   - ns    = 真正 pread 的墙钟累计（含预取），不含命中、不含解码
//   - pages = 从页文件读了多少页
//   - bytes = 读了多少字节（大字段文件也走这里）
//
// 全部是库级而非会话级：bufPool 是共享的，读写页时不知道"当前是谁在做"。
// 所以取的是「前后快照的差值」——单会话（桌面版）完全准确；多人同时查时
// 会把别人的读页算进来，这一点在界面上要讲明。
var (
	ioReadNs    atomic.Int64
	ioReadPages atomic.Int64
	ioReadBytes atomic.Int64
)

// IOSnapshot 返回读数页累计（纳秒 / 页数 / 字节数）。
//
// 调用方取两次快照相减，就得到自己这段时间里等页文件等了多久。
func IOSnapshot() (ns, pages, bytes int64) {
	return ioReadNs.Load(), ioReadPages.Load(), ioReadBytes.Load()
}

// IOSnapshot 让 *Store 满足 exec 的计时口（exec/profile.go 里的 ioStat）。
// 计的是库级累计，与是哪个 Store 实例无关 —— 一个进程一个数据目录。
func (s *Store) IOSnapshot() (ns, pages, bytes int64) { return IOSnapshot() }

func noteReadPage(start time.Time, pages int, bytes int) {
	if pages <= 0 {
		return
	}
	// 只在真正读盘之后调用。计时本身（两次取时钟）相对一次 pread 可以忽略。
	ioReadNs.Add(int64(time.Since(start)))
	ioReadPages.Add(int64(pages))
	ioReadBytes.Add(int64(bytes))
}
