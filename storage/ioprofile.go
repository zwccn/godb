package storage

import (
	"os"
	"sort"
)

// 本文件收集「存储层自己的 IO 画像」：WAL fsync 延迟、checkpoint 耗时与触发原因、
// 各表数据文件大小。
//
// 为什么不报系统的磁盘 %util：共用一块盘时那个数字无法归因（别的进程在写也算你头上），
// 而 fsync 延迟是「我这一次 COMMIT 实际等了多久」，对数据库才有决策价值。
//
// 全部只依赖标准库，三平台一致，零 cgo。

// latRing 是定长环形耗时样本（纳秒）。
//
// storage 不能反向依赖 db 包，那边也有一份同类实现 —— 各自二十来行，
// 比为了复用抽一个公共包划算。
type latRing struct {
	buf []int64
	pos int
	n   int
}

func newLatRing(capacity int) *latRing {
	return &latRing{buf: make([]int64, capacity)}
}

// add 记录一个样本。负值忽略（表示这次没测到），0 记成 1——
// 真存 0 会让百分位算出来是 0，前端显示「0.00ms」看着像测量坏了。
func (r *latRing) add(v int64) {
	if v < 0 {
		return
	}
	if v == 0 {
		v = 1
	}
	r.buf[r.pos] = v
	r.pos = (r.pos + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// stats 返回 p50 / p95 / max。样本为 0 时返回全 0（前端据此显示「—」）。
func (r *latRing) stats() (p50, p95, max int64) {
	if r.n == 0 {
		return 0, 0, 0
	}
	cp := make([]int64, r.n)
	copy(cp, r.buf[:r.n])
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	pick := func(q float64) int64 {
		idx := int(q * float64(len(cp)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return pick(0.50), pick(0.95), cp[len(cp)-1]
}

// FsyncProfile 是 WAL 落盘延迟的画像。
//
// Runs 和提交次数不是一回事：组提交会把多个事务合并成一次 fsync，
// 两者一比就能看出合并效果（例如 340 次/s 的 fsync 服务了 1200 次/s 的提交）。
type FsyncProfile struct {
	Runs        int64 `json:"runs"`        // 累计实际 fsync 次数
	P50Ns       int64 `json:"p50Ns"`       // 样本 p50
	P95Ns       int64 `json:"p95Ns"`       // 样本 p95
	MaxNs       int64 `json:"maxNs"`       // 样本最大值
	LastNs      int64 `json:"lastNs"`      // 上一次 fsync 耗时
	Bytes       int64 `json:"bytes"`       // 累计写入 WAL 的字节数
	LiveBytes   int64 `json:"liveBytes"`   // 当前 WAL 文件大小
	Frames      int64 `json:"frames"`      // 累计写出的日志帧数
	SyncedSeq   int64 `json:"syncedSeq"`   // 已 fsync 的最高序号
	WrittenSeq  int64 `json:"writtenSeq"`  // 已写入的最高序号
	RingSamples int   `json:"ringSamples"` // 样本数（判断 p95 可不可信）
}

// CheckpointProfile 是快照落盘的画像。
//
// LastReason 回答「这一次为什么写盘」：commits / seconds / walMB / manual / flush / ddl。
// 它和 fsync 变慢强相关——正好撞上快照在写盘，fsync 自然慢。
type CheckpointProfile struct {
	Runs         int64            `json:"runs"`
	P50Ns        int64            `json:"p50Ns"`
	P95Ns        int64            `json:"p95Ns"`
	MaxNs        int64            `json:"maxNs"`
	LastNs       int64            `json:"lastNs"`
	LastBytes    int64            `json:"lastBytes"`  // 上一次写了多少字节
	LastReason   string           `json:"lastReason"` // 上一次的触发原因
	LastAtMs     int64            `json:"lastAtMs"`   // 上一次落盘的时刻（Unix 毫秒）
	Reasons      map[string]int64 `json:"reasons"`    // 各原因的累计次数
	CommitsSince int64            `json:"commitsSince"`
}

// TableSize 是一张表在磁盘上的占用。
type TableSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Rows  int    `json:"rows"`
}

// PoolProfile 是缓冲池的汇总（全库所有表加在一起）。
//
// 为什么值得单独报：页存储之后，「内存够不够」这件事全看这几个数 ——
// 命中率低说明预算太小（或者访问太随机），Evicts 涨得快说明表比池子大得多，
// Overflow > 0 说明已经出现过「预算满了还得硬塞」的窘境。以前这些只能靠猜。
type PoolProfile struct {
	Budget   int   `json:"budget"`   // 全库预算页数
	Resident int   `json:"resident"` // 当前驻留的页框数
	Dirty    int   `json:"dirty"`    // 其中还没落盘的
	Tables   int   `json:"tables"`   // 打开了几个页文件
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Evicts   int64 `json:"evicts"`
	Flushes  int64 `json:"flushes"`
	Prefetch int64 `json:"prefetched"`
	Overflow int64 `json:"overflow"` // 预算满时硬塞进来的格数（越多越该调大预算）
	Blocked  int64 `json:"blocked"`  // 想换页但一页都换不出去（全被钉住）的次数
}

// PoolStats 汇总全库缓冲池的状态。面板每秒调一次，所以只做累加、不排序。
func (s *Store) PoolStats() PoolProfile {
	p := PoolProfile{}
	if s.Budget != nil {
		p.Budget = s.Budget.totalPages()
		p.Resident = s.Budget.usedPages()
	}
	s.loadMu.RLock()
	tables := make([]*TableData, 0, len(s.Data))
	for _, td := range s.Data {
		tables = append(tables, td)
	}
	s.loadMu.RUnlock()

	for _, td := range tables {
		if td == nil {
			continue
		}
		// td.pages 会被 DDL / VACUUM FULL 整体换掉，读它要跟着 pageMu 走
		td.pageMu.RLock()
		tp := td.pages
		if tp != nil {
			st := tp.pool.stats()
			p.Tables++
			p.Hits += st.Hits
			p.Misses += st.Misses
			p.Evicts += st.Evicts
			p.Flushes += st.Flushes
			p.Prefetch += st.Prefetched
			p.Overflow += st.Overflow
			p.Blocked += st.Blocked
			p.Dirty += tp.pool.dirtyCount()
		}
		td.pageMu.RUnlock()
	}
	return p
}

// IndexProfile 是索引在磁盘上的占用（「存储与 IO」卡片用）。
//
// 为什么不并进 PoolProfile：池报的是内存页框（运行时计数），这里是磁盘文件
// （一次 stat）。两者一起看才解释得清「索引占了多少地方、为什么重启后慢了一下」。
//
// Rebuilds 是一个不该涨的数值：索引不写 WAL，靠 meta.json 里的代次对齐；
// 正常重启代次相等、叶子链校验通过 → 重建 0 次。真涨了说明索引文件与堆不同步
// （代次对不上 / CRC 坏页 / 链断了），那一次启动就白重建了一整棵树。
type IndexProfile struct {
	Indexes  int   `json:"indexes"`  // catalog 里的索引定义数
	Pages    int   `json:"pages"`    // 已落盘的索引页数
	Bytes    int64 `json:"bytes"`    // 索引文件总字节
	Rebuilds int64 `json:"rebuilds"` // 启动以来整棵重建的次数
}

// IndexStats 汇总索引文件大小与重建次数。
//
// 只 stat 不打开：未加载的表也要能报，而面板每秒调一次，不能为了个数字
// 把索引页读进缓冲池。行数按 PageSize 折算，和文件真实布局一致。
func (s *Store) IndexStats() IndexProfile {
	p := IndexProfile{Rebuilds: s.idxRebuilds.Load()}
	if s.Cat == nil {
		return p
	}
	s.loadMu.RLock()
	defer s.loadMu.RUnlock()
	for _, t := range s.Cat.Tables {
		for _, ix := range t.Indexes {
			p.Indexes++
			st, err := os.Stat(indexFilePath(s.Dir, t.ID, "ix:"+cleanName(ix.Name)))
			if err != nil {
				continue // 还没落过盘的索引（只有 WAL 里有）
			}
			p.Bytes += st.Size()
			p.Pages += int(st.Size() / PageSize)
		}
	}
	return p
}

// checkpoint 的触发原因。空串表示「还没落过盘」。
const (
	CPReasonCommits = "commits" // 攒够了提交次数
	CPReasonSeconds = "seconds" // 后台定时器到点
	CPReasonWALMB   = "walMB"   // WAL 涨到阈值
	CPReasonManual  = "manual"  // 显式 CHECKPOINT
	CPReasonFlush   = "flush"   // 手动 Flush / 进程退出
	CPReasonDDL     = "ddl"     // DDL 引起的强制落盘
)

const (
	// fsyncSampleCap 是 fsync 延迟样本容量。512 条按每秒几百次算，
	// 大约覆盖最近一两秒的高频写入，足以算稳 p95。
	fsyncSampleCap = 512
	// cpSampleCap 是 checkpoint 耗时样本容量。checkpoint 几十秒才一次，
	// 128 条够覆盖最近一两个小时。
	cpSampleCap = 128
)

// CheckpointProfile 返回快照落盘的画像。
func (s *Store) CheckpointProfile() CheckpointProfile {
	p := CheckpointProfile{CommitsSince: s.cpSince.Load(), Runs: s.cpRuns.Load()}
	if s.cpRing != nil {
		p.P50Ns, p.P95Ns, p.MaxNs = s.cpRing.stats()
	}
	p.LastNs = s.cpDurNs.Load()
	p.LastBytes = s.cpBytes.Load()
	if ns := s.cpLastNs.Load(); ns > 0 {
		p.LastAtMs = ns / 1e6
	}
	s.cpStatMu.Lock()
	p.LastReason = s.cpLastReason
	if len(s.cpReasons) > 0 {
		p.Reasons = make(map[string]int64, len(s.cpReasons))
		for k, v := range s.cpReasons {
			p.Reasons[k] = v
		}
	}
	s.cpStatMu.Unlock()
	return p
}

// FsyncProfile 返回 WAL 落盘延迟的画像。没有 WAL（纯内存模式）时返回零值。
func (s *Store) FsyncProfile() FsyncProfile {
	if s.wal == nil {
		return FsyncProfile{}
	}
	return s.wal.SyncProfile()
}

// TableSizes 返回各表数据文件的大小与行数。
//
// 只读文件头附近的信息（os.Stat），不读内容——未加载的表也不需要把行读进内存。
// 表还没落过盘（只有 WAL 里有）时 Bytes 为 0。
func (s *Store) TableSizes() []TableSize {
	if s.Cat == nil {
		return nil
	}
	out := make([]TableSize, 0, len(s.Cat.Tables))
	s.loadMu.RLock()
	for _, t := range s.Cat.Tables {
		key := cleanName(t.Name)
		ts := TableSize{Name: t.Name}
		if st, err := os.Stat(tablePagePath(s.Dir, t.ID)); err == nil {
			ts.Bytes = st.Size()
		}
		if td := s.Data[key]; td != nil && s.loaded[key] {
			ts.Rows = td.Live
		} else {
			ts.Rows = s.tableRows[key]
		}
		out = append(out, ts)
	}
	s.loadMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// TotalCommits 返回进程启动以来的累计提交数，给面板算每秒提交数。
func (s *Store) TotalCommits() int64 { return s.totalCommits.Load() }
