package db

import (
	"sort"
	"strings"
	"sync"
	"time"

	"godb/exec"
	"godb/hrtime"
)

// 本文件按「语句结构」聚合执行统计，替代原来「存 256 条原始样本、取最慢 10 条」
// 的做法。
//
// 为什么要换：环形缓冲只记得最近 256 次执行，跑得久一点就把早先的记录冲掉；
// 而且同一条语句执行一千次就占一千个槽位，把真正低频但慢的那条挤出去。
// 按结构聚合之后，一千次执行只占一个槽位，次数、累计、平均、最大都能算，
// 1000 个结构能覆盖一个应用几乎所有语句。
//
// 归一化：字符串字面量和独立的数字字面量换成 ?，标识符里的数字（t_user1）
// 原样保留。不做完整词法分析 —— 见 appendNorm 的注释。
//
// 两把尺子：累计耗时（谁最费这台机器）和单次最慢（谁偶发卡了一下）。
// 槽位只有 1000 个，淘汰只能按一个口径，所以按累计耗时淘汰，
// 但单次最慢的前 100 个结构有豁免权，不会被挤出去。
const (
	stmtStatCap   = 1000 // 同时跟踪的语句结构数
	stmtDetailCap = 100  // 每个结构保留的明细条数
	stmtKeepSlow  = 100  // 「单次最慢」名单容量（也是豁免名额）
	stmtKeyMax    = 4096 // 结构键长度上限

	txStatCap   = 500
	txDetailCap = 100
	txKeepSlow  = 100
)

// StmtSample 是一次执行明细。故意不存 SQL 原文：1000 个结构 × 100 条明细再乘
// 2KB 原文就是 200MB，原文只在结构上留一份样本。
type StmtSample struct {
	TookNs  int64  `json:"tookNs"`
	AtMs    int64  `json:"atMs"`
	Session string `json:"session"`
	Rows    int    `json:"rows"`
	Affect  int    `json:"affected"`
	Scanned int64  `json:"scanned"`
}

// StmtStat 是慢 SQL 页的一行（一个语句结构）。
type StmtStat struct {
	SQL      string   `json:"sql"`    // 归一化文本，字面量显示成 ?
	Sample   string   `json:"sample"` // 一条原文（详情里给人看）
	Tables   []string `json:"tables,omitempty"`
	Write    bool     `json:"write"`
	N        int64    `json:"n"`
	TotalNs  int64    `json:"totalNs"`
	AvgNs    int64    `json:"avgNs"`
	MaxNs    int64    `json:"maxNs"`
	MinNs    int64    `json:"minNs"`
	FirstMs  int64    `json:"firstMs"`
	LastMs   int64    `json:"lastMs"`
	LastSess string   `json:"lastSession,omitempty"`
	Slowest  bool     `json:"slowest,omitempty"` // 在「单次最慢」名单里
	Running  bool     `json:"running,omitempty"` // 正在跑的那条（不属于任何结构）
}

// TxSample 是一次事务明细。
type TxSample struct {
	DurMs   int64  `json:"durMs"`
	AtMs    int64  `json:"atMs"`
	Session string `json:"session"`
	Stmts   int    `json:"stmts"`
}

// TxStat 是慢事务页的一行（一种「表集合 + 读写」的事务结构）。
type TxStat struct {
	// Key 是 txKey(Tables, Write) 的结果，前端拿它去取明细 —— 让前端自己
	// 拼一遍排序/去重规则的话，两边迟早会不一致。
	Key     string   `json:"key"`
	Tables  []string `json:"tables"`
	Write   bool     `json:"write"`
	N       int64    `json:"n"`
	TotalMs int64    `json:"totalMs"`
	AvgMs   int64    `json:"avgMs"`
	MaxMs   int64    `json:"maxMs"`
	MinMs   int64    `json:"minMs"`
	FirstMs int64    `json:"firstMs"`
	LastMs  int64    `json:"lastMs"`
	Slowest bool     `json:"slowest,omitempty"`
}

// ---- 归一化 ----

// appendNorm 把一条 SQL 归一化成「语句结构」。
//
// 不用 sql.Lex 的理由：词法器要先分配 token 切片、再逐个判断类型，而这里
// 只需要一遍字节扫描把字面量换成 ?。热路径上每条语句都要跑一次，实测
// 54 字节 189ns / 209 字节 436ns，零分配（stmtstat_test.go 的 BenchmarkNormalize）。
//
// 三个容易搞错的地方：
//   - t_user1 里的 1 不能换，所以要看前一个字符是不是标识符的一部分；
//   - 双引号/反引号里是标识符，原样保留；
//   - 注释里的数字原样保留 —— 有的驱动会往注释里塞 trace id，
//     替换成 ? 反而会把结构拆散。
func appendNorm(dst []byte, s string) []byte {
	ident := false // 前一个字符属于标识符（字母/_/$/数字）
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			dst = append(dst, '?')
			i++
			for i < len(s) {
				if s[i] == '\'' {
					// '' 是转义的单引号，不是结束
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			ident = false

		case c == '"' || c == '`':
			q := c
			start := i
			i++
			for i < len(s) && s[i] != q {
				i++
			}
			// i 停在结束引号上，交给外层 for 的 i++ 跳过它
			if i < len(s) {
				dst = append(dst, s[start:i+1]...)
			} else {
				dst = append(dst, s[start:]...)
			}
			ident = false

		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			start := i
			for i < len(s) && s[i] != '\n' {
				i++
			}
			dst = append(dst, s[start:i]...)
			ident = false

		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			start := i
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i++
			}
			dst = append(dst, s[start:i+1]...)
			ident = false

		case c >= '0' && c <= '9':
			if ident {
				dst = append(dst, c)
				continue // 标识符的一部分，原样保留（ident 仍为 true）
			}
			dst = append(dst, '?')
			for i+1 < len(s) {
				d := s[i+1]
				if (d >= '0' && d <= '9') || d == '.' {
					i++
					continue
				}
				break
			}
			ident = false

		case isIdentByte(c):
			dst = append(dst, c)
			ident = true

		default:
			dst = append(dst, c)
			ident = false
		}
	}
	return dst
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$'
}

// normKey 把 text 归一化成一个 map 键。
//
// 短语句走栈缓冲，配合 stmtPool.add 里的 m[string(buf)] 查找可以做到
// 零分配；这里为了返回 string 才复制一次，只给测试和外部调用用。
func normKey(text string) string {
	var stack [1024]byte
	buf := stack[:0]
	if len(text) > len(stack) {
		buf = make([]byte, 0, len(text))
	}
	key := appendNorm(buf, text)
	if len(key) > stmtKeyMax {
		key = key[:stmtKeyMax]
	}
	return string(key)
}

// ---- 语句结构池 ----

type stmtStat struct {
	key     string
	sample  string
	tables  []string
	write   bool
	plan    []string
	n       int64
	totalNs int64
	maxNs   int64
	minNs   int64
	firstMs int64
	lastMs  int64
	lastSes string
	det     []StmtSample // 按 tookNs 降序，最多 stmtDetailCap 条
}

type stmtPool struct {
	mu      sync.Mutex
	m       map[string]*stmtStat
	slow    map[string]struct{} // 单次最慢名单（同时是淘汰豁免名单）
	slowCut int64               // 名单满员时的门槛（第 100 名的 maxNs）
	evicted int64
	zeroNs  int64 // 读数为 0 的语句在累计里按这个值计
}

func newStmtPool() *stmtPool {
	return &stmtPool{
		m:      make(map[string]*stmtStat, stmtStatCap),
		slow:   make(map[string]struct{}, stmtKeepSlow),
		zeroNs: zeroFloor(),
	}
}

// zeroFloor 是「计时读数为 0」的语句计入累计耗时的估计值：半个时钟步进。
//
// 读数为 0 只说明真实耗时落在 [0, 步进) 这个区间里，取中点是没有偏置的猜法。
// 不这么做的话，一条每秒跑几千次、单次只有几十微秒的语句累计会一直显示 0 ——
// 它明明吃掉了半个核，榜单却当它不存在。代价是这类极短语句的平均耗时偏高
// （0.1ms 是估计值，不是测量值），面板上它们本来就显示成「<0.2 ms」。
func zeroFloor() int64 {
	g := int64(hrtime.Granularity())
	if g <= 0 {
		return 1
	}
	if half := g / 2; half > 0 {
		return half
	}
	return 1
}

// add 记一次执行。
//
// took 可能是 0：hrtime 在本机只有 0.2ms 步进，跑得比它还快的语句测出来就是 0。
// 这种语句照样要计次 —— 「执行总次数」是这一页的第二个核心数字，漏记会让
// 高频短语句看起来只跑了几次。累计耗时按「半个时钟步进」估算计入（见 zeroFloor），
// 最慢/最快只用非零样本，明细也不收 0（「最慢的 100 次」里塞一堆 0 没意义）。
func (p *stmtPool) add(text string, tables []string, write bool, id string, res *exec.Result, now time.Time) {
	took := res.TookNs

	var stack [1024]byte
	buf := stack[:0]
	if len(text) > len(stack) {
		buf = make([]byte, 0, len(text))
	}
	key := appendNorm(buf, text)
	if len(key) > stmtKeyMax {
		key = key[:stmtKeyMax]
	}

	atMs := now.UnixMilli()

	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.m[string(key)] // []byte 查 map 不分配（编译器特化）
	if st == nil {
		if len(p.m) >= stmtStatCap && !p.evictLocked() {
			return // 全在豁免名单里，这个结构挤不进来
		}
		st = &stmtStat{
			key:     string(key),
			sample:  clampText(text, sqlFullLen),
			tables:  tables,
			write:   write,
			firstMs: atMs,
		}
		p.m[st.key] = st
	}

	st.n++
	if took > 0 {
		st.totalNs += took
	} else {
		st.totalNs += p.zeroNs // 测不出来的，按半个时钟步进估
	}
	if took > st.maxNs {
		st.maxNs = took
		p.touchSlowLocked(st)
	}
	if took > 0 && (st.minNs == 0 || took < st.minNs) {
		st.minNs = took
	}
	st.lastMs = atMs
	st.lastSes = id
	if took > 0 && took >= planKeepNs {
		st.plan = trimPlan(res.Plan)
	}
	if took > 0 {
		p.addDetailLocked(st, took, atMs, id, res)
	}
}

// addDetailLocked 把一次执行插进明细（保持降序）。
//
// 先和第 100 名的耗时比一次，比不过直接退出 —— 高频语句稳定之后，
// 绝大多数样本只付这一次比较的代价。
func (p *stmtPool) addDetailLocked(st *stmtStat, took, atMs int64, id string, res *exec.Result) {
	if len(st.det) >= stmtDetailCap && took <= st.det[len(st.det)-1].TookNs {
		return
	}
	s := StmtSample{
		TookNs:  took,
		AtMs:    atMs,
		Session: id,
		Rows:    res.RowCount,
		Affect:  res.Affected,
		Scanned: res.Scanned,
	}
	pos := len(st.det)
	for pos > 0 && st.det[pos-1].TookNs < took {
		pos--
	}
	if pos >= stmtDetailCap {
		return
	}
	if len(st.det) < stmtDetailCap {
		st.det = append(st.det, StmtSample{})
	}
	copy(st.det[pos+1:], st.det[pos:])
	st.det[pos] = s
}

// touchSlowLocked 维护「单次最慢」名单。
func (p *stmtPool) touchSlowLocked(st *stmtStat) {
	if _, ok := p.slow[st.key]; ok {
		if len(p.slow) >= stmtKeepSlow {
			p.slowCut = p.minSlowMaxLocked()
		}
		return
	}
	if len(p.slow) < stmtKeepSlow {
		p.slow[st.key] = struct{}{}
		if len(p.slow) >= stmtKeepSlow {
			p.slowCut = p.minSlowMaxLocked()
		}
		return
	}
	if st.maxNs <= p.slowCut {
		return
	}
	// 名单满了，踢掉里面「最慢值」最小的那个
	worst := ""
	var worstMax int64
	for k := range p.slow {
		o := p.m[k]
		if o == nil {
			worst = k
			break
		}
		if worst == "" || o.maxNs < worstMax {
			worst, worstMax = k, o.maxNs
		}
	}
	if worst != "" {
		delete(p.slow, worst)
	}
	p.slow[st.key] = struct{}{}
	p.slowCut = p.minSlowMaxLocked()
}

func (p *stmtPool) minSlowMaxLocked() int64 {
	var min int64
	first := true
	for k := range p.slow {
		if o := p.m[k]; o != nil {
			if first || o.maxNs < min {
				min, first = o.maxNs, false
			}
		}
	}
	return min
}

// evictLocked 腾一个位置：踢掉累计耗时最小、且不在豁免名单里的结构。
//
// 池满之后新结构出现才走到这里（罕见），线性扫 1000 项约 2µs，不再做
// 更聪明的结构 —— 这个函数可解释性比那 2µs 值钱。
func (p *stmtPool) evictLocked() bool {
	var victim *stmtStat
	for _, st := range p.m {
		if _, keep := p.slow[st.key]; keep {
			continue
		}
		if victim == nil || st.totalNs < victim.totalNs {
			victim = st
		}
	}
	if victim == nil {
		return false
	}
	delete(p.m, victim.key)
	p.evicted++
	return true
}

// snapshot 返回按 byTotal 排序的前 n 条（byTotal=false 时按单次最慢）。
//
// 统计字段在锁内拷出来再排序：如果锁外读 *stmtStat，add 会同时改它，
// 就是数据竞争。1000 条 × 一百来字节 = 120KB，2 秒一次可接受。
func (p *stmtPool) snapshot(n int, byTotal bool) []StmtStat {
	p.mu.Lock()
	out := make([]StmtStat, 0, len(p.m))
	for _, st := range p.m {
		_, keep := p.slow[st.key]
		out = append(out, StmtStat{
			SQL:      st.key,
			Sample:   st.sample,
			Tables:   st.tables,
			Write:    st.write,
			N:        st.n,
			TotalNs:  st.totalNs,
			MaxNs:    st.maxNs,
			MinNs:    st.minNs,
			FirstMs:  st.firstMs,
			LastMs:   st.lastMs,
			LastSess: st.lastSes,
			Slowest:  keep,
		})
	}
	p.mu.Unlock()

	for i := range out {
		if out[i].N > 0 {
			out[i].AvgNs = out[i].TotalNs / out[i].N
		}
	}
	sortStmt(out, byTotal)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func sortStmt(list []StmtStat, byTotal bool) {
	sort.Slice(list, func(i, j int) bool {
		if byTotal {
			if list[i].TotalNs != list[j].TotalNs {
				return list[i].TotalNs > list[j].TotalNs
			}
		} else if list[i].MaxNs != list[j].MaxNs {
			return list[i].MaxNs > list[j].MaxNs
		}
		return list[i].SQL < list[j].SQL
	})
}

// detail 取一个结构的明细和执行计划（详情弹窗用）。
func (p *stmtPool) detail(key string) ([]StmtSample, []string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.m[key]
	if st == nil {
		return nil, nil, false
	}
	det := append([]StmtSample(nil), st.det...)
	return det, append([]string(nil), st.plan...), true
}

// count 返回当前跟踪的结构数与被淘汰过的结构数。
func (p *stmtPool) count() (live, evicted int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(len(p.m)), p.evicted
}

// ---- 事务结构池 ----

type txStat struct {
	key     string
	tables  []string
	write   bool
	n       int64
	totalMs int64
	maxMs   int64
	minMs   int64
	firstMs int64
	lastMs  int64
	det     []TxSample
}

type txPool struct {
	mu      sync.Mutex
	m       map[string]*txStat
	slow    map[string]struct{}
	slowCut int64
}

func newTxPool() *txPool {
	return &txPool{
		m:    make(map[string]*txStat, txStatCap),
		slow: make(map[string]struct{}, txKeepSlow),
	}
}

// txKey 把「事务涉及的表集合 + 读写」变成一个结构键。
//
// 事务没有「语句结构」这种自然的分组维度，表集合是最接近的：读 t_orders
// 的短事务和读写 t_orders+t_items 的长事务，本该分在两个桶里。
func txKey(tables []string, write bool) string {
	var b strings.Builder
	if write {
		b.WriteString("w:")
	} else {
		b.WriteString("r:")
	}
	if len(tables) == 0 {
		b.WriteString("(无表)")
		return b.String()
	}
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for i, t := range sorted {
		if i > 0 && sorted[i-1] == t {
			continue
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t)
	}
	return b.String()
}

// txDone 是一个刚结束的事务。
type txDone struct {
	tables  []string
	write   bool
	dur     time.Duration
	stmts   int
	session string
}

func (p *txPool) add(d txDone) {
	// 只有「负数」才是真异常（时钟被回拨）。dur == 0 是**正常**的：极短的事务
	// 测出来就是 0 —— 事务开始时刻是从 Session 的 UnixNano 还原的
	// （`time.Unix(0, ns)`，没有 monotonic 时钟），Windows 上墙钟精度只有
	// 0.5–16ms，几百微秒的事务常常算出 0。下面那行 `ms <= 0 { ms = 1 }`
	// 本来就是为这种情况准备的，这里却先把 0 丢了，两边自相矛盾。
	if d.dur < 0 {
		return
	}
	key := txKey(d.tables, d.write)
	ms := d.dur.Milliseconds()
	if ms <= 0 {
		ms = 1
	}
	now := time.Now().UnixMilli()

	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.m[key]
	if st == nil {
		if len(p.m) >= txStatCap && !p.evictLocked() {
			return
		}
		st = &txStat{
			key:     key,
			tables:  d.tables,
			write:   d.write,
			minMs:   ms,
			firstMs: now,
		}
		p.m[key] = st
	}
	st.n++
	st.totalMs += ms
	if ms > st.maxMs {
		st.maxMs = ms
		p.touchSlowLocked(st)
	}
	if ms < st.minMs {
		st.minMs = ms
	}
	st.lastMs = now
	p.addDetailLocked(st, ms, now, d.session, d.stmts)
}

func (p *txPool) addDetailLocked(st *txStat, ms, atMs int64, id string, stmts int) {
	if len(st.det) >= txDetailCap && ms <= st.det[len(st.det)-1].DurMs {
		return
	}
	s := TxSample{DurMs: ms, AtMs: atMs, Session: id, Stmts: stmts}
	pos := len(st.det)
	for pos > 0 && st.det[pos-1].DurMs < ms {
		pos--
	}
	if pos >= txDetailCap {
		return
	}
	if len(st.det) < txDetailCap {
		st.det = append(st.det, TxSample{})
	}
	copy(st.det[pos+1:], st.det[pos:])
	st.det[pos] = s
}

func (p *txPool) touchSlowLocked(st *txStat) {
	if _, ok := p.slow[st.key]; ok {
		if len(p.slow) >= txKeepSlow {
			p.slowCut = p.minSlowMaxLocked()
		}
		return
	}
	if len(p.slow) < txKeepSlow {
		p.slow[st.key] = struct{}{}
		if len(p.slow) >= txKeepSlow {
			p.slowCut = p.minSlowMaxLocked()
		}
		return
	}
	if st.maxMs <= p.slowCut {
		return
	}
	worst := ""
	var worstMax int64
	for k := range p.slow {
		o := p.m[k]
		if o == nil {
			worst = k
			break
		}
		if worst == "" || o.maxMs < worstMax {
			worst, worstMax = k, o.maxMs
		}
	}
	if worst != "" {
		delete(p.slow, worst)
	}
	p.slow[st.key] = struct{}{}
	p.slowCut = p.minSlowMaxLocked()
}

func (p *txPool) minSlowMaxLocked() int64 {
	var min int64
	first := true
	for k := range p.slow {
		if o := p.m[k]; o != nil {
			if first || o.maxMs < min {
				min, first = o.maxMs, false
			}
		}
	}
	return min
}

func (p *txPool) evictLocked() bool {
	var victim *txStat
	for _, st := range p.m {
		if _, keep := p.slow[st.key]; keep {
			continue
		}
		if victim == nil || st.totalMs < victim.totalMs {
			victim = st
		}
	}
	if victim == nil {
		return false
	}
	delete(p.m, victim.key)
	return true
}

func (p *txPool) snapshot(n int, byTotal bool) []TxStat {
	p.mu.Lock()
	out := make([]TxStat, 0, len(p.m))
	for _, st := range p.m {
		_, keep := p.slow[st.key]
		out = append(out, TxStat{
			Key:     st.key,
			Tables:  st.tables,
			Write:   st.write,
			N:       st.n,
			TotalMs: st.totalMs,
			MaxMs:   st.maxMs,
			MinMs:   st.minMs,
			FirstMs: st.firstMs,
			LastMs:  st.lastMs,
			Slowest: keep,
		})
	}
	p.mu.Unlock()

	for i := range out {
		if out[i].N > 0 {
			out[i].AvgMs = out[i].TotalMs / out[i].N
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if byTotal {
			if out[i].TotalMs != out[j].TotalMs {
				return out[i].TotalMs > out[j].TotalMs
			}
		} else if out[i].MaxMs != out[j].MaxMs {
			return out[i].MaxMs > out[j].MaxMs
		}
		return txKey(out[i].Tables, out[i].Write) < txKey(out[j].Tables, out[j].Write)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (p *txPool) detail(key string) ([]TxSample, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.m[key]
	if st == nil {
		return nil, false
	}
	return append([]TxSample(nil), st.det...), true
}

func (p *txPool) count() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(len(p.m))
}
