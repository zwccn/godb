package storage

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
//
// 三个文件分开报，因为它们是三种性质不同的空间：堆页可以靠 VACUUM 整理回收、
// 索引页只能靠重建回收、大字段只有 VACUUM FULL 才回收。合成一个数字就看不见
// 「该整理哪一块」了。
type TableSize struct {
	Name       string  `json:"name"`
	Bytes      int64   `json:"bytes"`      // 堆页文件 t<ID>.dat
	IndexBytes int64   `json:"indexBytes"` // 该表所有索引文件合计
	LobBytes   int64   `json:"lobBytes"`   // 大字段文件 t<ID>.lob
	// VecBytes 是该表所有向量列文件（t<ID>.c<列>.vec）合计。
	// 向量不进堆页，所以这块钱既不体现在 Bytes 里、也不占页池预算 ——
	// 不单列出来，一张纯向量表会显示成 0 字节。
	VecBytes int64 `json:"vecBytes"`
	VecCols  int   `json:"vecCols,omitempty"`
	Rows     int   `json:"rows"` // 存活行数
	Pages      int     `json:"pages"`
	IndexPages int     `json:"indexPages"`
	FreeBytes  int64   `json:"freeBytes"` // 页里还能用的字节（按 64B 档位估算）
	Fill       float64 `json:"fill"`      // 页密度 = 1 - FreeBytes/(Pages×8KB)，0~1
	// FillKnown 为 false 表示「空闲字节还没记录过」（表一直没被加载/落盘过），
	// 这时 Fill 没有意义，前端应该显示「—」而不是 100%。
	FillKnown bool `json:"fillKnown"`
	// VacuumAt / RewriteAt 是「上次整理 / 上次整份重写」的时刻（Unix 毫秒，0 = 从没）。
	// 密度只说「现在有多空」，配上时间才是「该不该现在整理」。
	VacuumAt  int64 `json:"vacuumAt"`
	RewriteAt int64 `json:"rewriteAt"`
	// Indexes 是这张表的每棵索引（名字 + 占用），页地图的下拉要靠它列文件。
	Indexes []IndexInfo `json:"indexes,omitempty"`
}

// IndexInfo 是单棵索引的占用（逐表明细用）。
type IndexInfo struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Pages int    `json:"pages"`
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
//
// 加锁分三段，**不嵌套**（顺序：loadMu → 放掉 → CommitMu → 放掉 → 没有任何锁）：
//
//	1. loadMu.RLock：读 s.Data / loaded / tableRows / tableFree / tableVac 这些 map，
//	   顺手把已加载表的 *TableData 指针抄出来（map 读必须持锁，否则并发写会 panic）。
//	2. CommitMu.Lock：只读第 1 步抄出来的那些 td 的 Live / pages —— 它们由提交、
//	   VACUUM、重写在 CommitMu 下改，和 loadMu 不是一个锁。
//	3. 无锁：os.Stat 各文件、算页数与密度。文件 IO 绝不能放在锁里（这套诊断的
//	   基本原则），也不需要在锁里 —— 文件大小本来就和内存状态无关。
//
// 为什么第 2 步必须存在：以前这里只持 loadMu 就读 td.Live / td.pages.freeBytes()，
// 而提交路径是「schema 读锁 + CommitMu」下 `td.Live++` 的。两个读锁互不排斥，于是
// 一边在提交、一边在轮询监控，就是一个数据竞争：轻则 -race 报红（本地没装 gcc
// 跑不了 -race，所以一直没被发现），重则 freeBytes() 读到正在被改的空闲空间图，
// 监控接口 panic 一回 500 —— 前端只显示「加载失败」，没有任何线索。
func (s *Store) TableSizes() []TableSize {
	if s.Cat == nil {
		return nil
	}
	tables := s.Cat.Tables

	// 1) map 侧（loadMu）
	type memSnap struct {
		td    *TableData // 已加载：第 2 步再去读它的字段；未加载为 nil
		rows  int
		free  int64
		vac   vacuumTimes
		vacOK bool
	}
	snaps := make([]memSnap, len(tables))
	s.loadMu.RLock()
	for i, t := range tables {
		key := cleanName(t.Name)
		// 整理时间是「这张表上次被 VACUUM 的时刻」，和加载状态无关 —— 必须每张都读。
		snaps[i].vac, snaps[i].vacOK = s.tableVac[key]
		if td := s.Data[key]; td != nil && s.loaded[key] {
			snaps[i].td = td
			continue
		}
		snaps[i].rows = s.tableRows[key]
		snaps[i].free = s.tableFree[key]
	}
	s.loadMu.RUnlock()

	// 2) 内存态（CommitMu）。注意这里只碰第 1 步抄下来的指针，不再碰 map，
	//    所以不需要也不能再取 loadMu（避免形成 loadMu ↔ CommitMu 的嵌套）。
	has := false
	for i := range snaps {
		if snaps[i].td != nil {
			has = true
			break
		}
	}
	if has {
		s.CommitMu.Lock()
		for i := range snaps {
			td := snaps[i].td
			if td == nil {
				continue
			}
			snaps[i].rows = td.Live // 真实行数（槽位数组可能带空槽，不能数长度）
			if td.pages != nil {
				snaps[i].free = td.pages.freeBytes()
			}
		}
		s.CommitMu.Unlock()
	}

	// 3) 文件（无锁）
	vecByTable := vecBytesByTable(s.Dir)
	out := make([]TableSize, 0, len(tables))
	for i, t := range tables {
		ts := TableSize{Name: t.Name}
		if st, err := os.Stat(tablePagePath(s.Dir, t.ID)); err == nil {
			ts.Bytes = st.Size()
		}
		if st, err := os.Stat(lobPathOf(tablePagePath(s.Dir, t.ID))); err == nil {
			ts.LobBytes = st.Size()
		}
		if v, ok := vecByTable[t.ID]; ok {
			ts.VecBytes, ts.VecCols = v.bytes, v.cols
		}
		// 索引文件按「该表的所有索引定义」去 stat：DROP 过的索引文件已经不在了，
		// 这样也不会把孤儿文件算进来。
		for _, ix := range t.Indexes {
			if st, err := os.Stat(indexFilePath(s.Dir, t.ID, "ix:"+cleanName(ix.Name))); err == nil {
				ts.IndexBytes += st.Size()
				ts.IndexPages += int(st.Size() / PageSize)
				ts.Indexes = append(ts.Indexes, IndexInfo{
					Name: ix.Name, Bytes: st.Size(), Pages: int(st.Size() / PageSize),
				})
			}
		}
		loadedNow := snaps[i].td != nil
		ts.Rows = snaps[i].rows
		ts.FreeBytes = snaps[i].free
		if loadedNow && snaps[i].td.pages != nil {
			ts.Pages = int(snaps[i].td.pages.pageCount())
		}
		// 未加载的表按文件大小算页数：meta 里的 pages 只在「这张表被写过」的那轮
		// 落盘更新，一直干净的表会永远报旧值（VACUUM FULL 换过文件尤其明显）。
		if !loadedNow && ts.Bytes > 0 {
			ts.Pages = int(ts.Bytes / PageSize)
		}
		if snaps[i].vacOK {
			ts.VacuumAt, ts.RewriteAt = snaps[i].vac.At, snaps[i].vac.Full
		}
		// 空闲字节从哪来：加载过的表实时算，没加载的只能信 meta 里上次记的值。
		// meta 里没有（=0）说明这张表还没被这一版落过盘，密度算不出来 —— 不能当成
		// 「一点空闲都没有」，否则一张从没打开过的表会报 100% 满。
		ts.FillKnown = (loadedNow && ts.Pages > 0) || (!loadedNow && snaps[i].free > 0)
		if ts.FillKnown && ts.Pages > 0 {
			capacity := float64(ts.Pages) * PageSize
			ts.Fill = 1 - float64(ts.FreeBytes)/capacity
			// 空闲字节是估算值，理论上不会超过容量；夹一下免得浮点抖动报出 -0.1%
			if ts.Fill < 0 {
				ts.Fill = 0
			}
			if ts.Fill > 1 {
				ts.Fill = 1
			}
		}
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// minFillPages 是「参与全库页密度统计」的最小页数：小表的最后一页天然不满，
// 算进去只会让百分比看起来比实际稀疏。前端挑「最稀疏的表」用同一档阈值。
const minFillPages = 8

// SpaceProfile 是全库的空间构成（「数据 / 索引 / 大字段 / WAL」各占多少）。
//
// 这是给「空间监控」用的地基：每一块都能往下钻到表级（见 TableSizes），
// 而四者的回收方式完全不同 —— 堆页靠 VACUUM、索引靠重建、大字段只能 VACUUM FULL、
// WAL 靠 checkpoint 轮转。混成一个「占用 120MB」看不出该做什么。
type SpaceProfile struct {
	DataBytes  int64 `json:"dataBytes"`
	IndexBytes int64 `json:"indexBytes"`
	LobBytes   int64 `json:"lobBytes"`
	// VecBytes 是向量列文件（tables/t<ID>.c<列>.vec）合计。
	// 必须单独算：向量不进堆页，一张 2 万行 × 128 维的表 .dat 是 0 字节、
	// .vec 是 9.8 MB —— 不加这一项，存储页会把它显示成一张空表。
	VecBytes int64 `json:"vecBytes"`
	WalBytes int64 `json:"walBytes"`
	Pages    int   `json:"pages"`
	// VecCols / VecPages 是向量列数与折算页数（按 8KB/页 折算，好和页数放一起看）。
	VecCols    int `json:"vecCols"`
	VecPages   int `json:"vecPages"`
	IndexPages int `json:"indexPages"`
	FreeBytes  int64 `json:"freeBytes"` // 已知表里可复用的空闲字节
	// FillPages 是「算得出密度的那些表的页数」：刚升级、还没落过盘的表没记空闲，
	// 把它们按 0 空闲算进分母会把全库密度虚报高。
	FillPages int     `json:"fillPages"`
	Fill      float64 `json:"fill"` // 全库页密度：1 - Σfree/(Σpages×8KB)
}

// vecBytesByTable 扫一次 tables/ 目录，按文件名里的表 ID 汇总向量文件占用。
//
// 用一次目录扫描而不是「逐表 os.Stat/Glob」：一个向量表可能有多个向量列，
// 逐表 glob 是 N 次目录遍历，而监控每 2 秒就会问一次；扫一次按名字分组只要一次。
//
// 只看磁盘上的真实文件，不看 Store.vecFiles（那里面只有**已打开**的列：
// 从没被查过的向量表根本不在里面，空间统计会漏掉它 —— 而这张表恰恰是最
// 需要被看见的那类：占着几百 MB 却没人查过）。
func vecBytesByTable(dir string) map[int64]vecTableBytes {
	ents, err := os.ReadDir(filepath.Join(dir, "tables"))
	if err != nil {
		return nil
	}
	out := make(map[int64]vecTableBytes)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 命名规则见 vecPath：t%06d.c%03d.vec
		if !strings.HasSuffix(name, ".vec") || !strings.HasPrefix(name, "t") {
			continue
		}
		dot := strings.IndexByte(name, '.')
		if dot <= 1 {
			continue
		}
		id, err := strconv.ParseInt(name[1:dot], 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		v := out[id]
		v.bytes += info.Size()
		v.cols++
		out[id] = v
	}
	return out
}

// vecTableBytes 是一张表的向量文件汇总。
type vecTableBytes struct {
	bytes int64
	cols  int
}

// Space 汇总全库空间构成（会重新 stat 一遍表文件）。
func (s *Store) Space() SpaceProfile { return SumSpace(s.TableSizes(), s.WalBytes()) }

// WalBytes 是当前 WAL 文件大小（纯内存模式返回 0）。
func (s *Store) WalBytes() int64 {
	if s.wal == nil {
		return 0
	}
	return s.wal.LiveBytes()
}

// SumSpace 把逐表明细加成全局构成。已经在手上有 TableSizes 的调用方用它，
// 免得为了一个汇总再 stat 一遍所有文件。
func SumSpace(tables []TableSize, walBytes int64) SpaceProfile {
	var p SpaceProfile
	for _, t := range tables {
		p.DataBytes += t.Bytes
		p.IndexBytes += t.IndexBytes
		p.LobBytes += t.LobBytes
		p.VecBytes += t.VecBytes
		if t.VecBytes > 0 {
			// 按 8KB/页 折算，好和堆页数放一起比（向量不是页，只是换算口径）
			p.VecPages += int((t.VecBytes + PageSize - 1) / PageSize)
			p.VecCols += t.VecCols
		}
		p.Pages += t.Pages
		p.IndexPages += t.IndexPages
		// 太小的表不参与全库密度：每张表的最后一页本来就装不满，页数只有一两页时
		// 这一页的「半空」会把整个百分比拖下去（4 行 1 页 = 12% 密度，但那是正常的）。
		if t.FillKnown && t.Pages >= minFillPages {
			p.FreeBytes += t.FreeBytes
			p.FillPages += t.Pages
		}
	}
	p.WalBytes = walBytes
	if p.FillPages > 0 {
		p.Fill = 1 - float64(p.FreeBytes)/(float64(p.FillPages)*PageSize)
		if p.Fill < 0 {
			p.Fill = 0
		}
		if p.Fill > 1 {
			p.Fill = 1
		}
	}
	return p
}

// TotalCommits 返回进程启动以来的累计提交数，给面板算每秒提交数。
func (s *Store) TotalCommits() int64 { return s.totalCommits.Load() }
