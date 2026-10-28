package storage

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"godb/hrtime"
	"godb/types"
)

const walFile = "wal.log"

// walMagic 标记二进制 WAL 文件。没有这个前缀的文件按旧版按行 JSON 解析。
var walMagic = []byte("GDBWAL\x01")

// WAL 记录的操作码。
const (
	opIns    byte = 1
	opUpd    byte = 2
	opDel    byte = 3
	opCommit byte = 4
)

// walRec 是一条 WAL 记录。行操作带旧/新行镜像，提交记录作为事务边界。
//
// Gen 是"代次号"：每次 checkpoint 之后 WAL 会轮转到新一代。
// 重放时只应用 Gen 大于快照代次的记录，从根上避免"快照已包含、WAL 又重放一遍"
// 造成的数据重复（崩溃在 snapshot rename 与 WAL 截断之间时必然会遇到）。
type walRec struct {
	Tx    int64
	Gen   int64
	Op    byte
	Table string
	Old   types.Row
	New   types.Row
}

// Wal 是追加写的日志文件。
//
// 并发约定：
//   - Append 由提交线程在持有 Store.CommitMu 时调用，因此文件写入天然串行。
//   - SyncTo 在释放 CommitMu 之后调用，多个事务的 fsync 会被合并成一次
//     （组提交 / group commit）：第一个到达的成为 leader 做 fsync，
//     其余等待它完成，leader 的 fsync 覆盖到"当时已写入的最高序号"。
type Wal struct {
	path string
	f    *os.File
	gen  int64

	mu        sync.Mutex
	cond      *sync.Cond
	written   int64 // 已写入文件的最高序号
	liveBytes int64 // 当前 WAL 文件的字节数（轮转后重置）
	synced    int64 // 已 fsync 的最高序号
	syncing   bool  // 是否已有 leader 在做 fsync
	syncErr   error
	closed    bool

	// 统计
	Frames   int64
	Bytes    int64
	SyncRuns int64 // 实际 fsync 次数（组提交效果看这个）

	// syncRing 存每次 fsync 的耗时（纳秒），给监控面板算 p50/p95/max。
	// lastSyncNs 是最近一次的耗时，单独存一份方便前端显示「刚才等了多久」。
	// 访问都在 w.mu 内（SyncTo 的 leader 阶段 fsync 回到锁内才记录）。
	syncRing   *latRing
	lastSyncNs atomic.Int64
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

func walPath(dir string) string { return filepath.Join(dir, walFile) }

// openWAL 打开（不存在则创建）WAL 文件，追加写。
// gen 是"下一次写入使用的代次号"。已有内容不会被清空——清空由 Rotate 负责，
// 这样即使 Open 之后立刻崩溃，旧记录仍在，可以配合快照的代次号正确重放。
func openWAL(dir string, gen int64) (*Wal, error) {
	path := walPath(dir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		if _, err := f.Write(walMagic); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	w := &Wal{path: path, f: f, gen: gen, liveBytes: st.Size(), syncRing: newLatRing(fsyncSampleCap)}
	w.cond = sync.NewCond(&w.mu)
	return w, nil
}

// Gen 返回当前代次号。
func (w *Wal) Gen() int64 { return w.gen }

// Append 追加一批记录（只写不 fsync），返回本批的序号。
func (w *Wal) Append(recs []walRec) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("WAL 已关闭")
	}
	if w.syncErr != nil {
		return 0, w.syncErr
	}
	var buf []byte
	for i := range recs {
		recs[i].Gen = w.gen
		buf = appendFrame(buf, &recs[i])
	}
	if _, err := w.f.Write(buf); err != nil {
		w.syncErr = fmt.Errorf("写 WAL 失败: %w", err)
		return 0, w.syncErr
	}
	w.written++
	w.Frames += int64(len(recs))
	w.Bytes += int64(len(buf))
	w.liveBytes += int64(len(buf))
	return w.written, nil
}

// Written 返回已写入的最高序号。
func (w *Wal) Written() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// SyncTo 保证序号 <= seq 的记录已经 fsync 落盘。
//
// 组提交：多个事务同时等待时只有一个 leader 真正调用 fsync，其余在 cond 上等。
// leader 刷完之后如果发现等待期间又有人写了日志，会接着再刷一次（仍然由它一个人刷），
// 这样 N 个并发事务的 fsync 次数接近 1 而不是 N。
//
// 注意：leader 必须保证退出时一定清掉 syncing 并 broadcast，否则其他等待者会永久挂住。
func (w *Wal) SyncTo(seq int64) error {
	w.mu.Lock()
	// 等待阶段：要么已经达标，要么抢到 leader 身份
	for {
		if w.syncErr != nil {
			err := w.syncErr
			w.mu.Unlock()
			return err
		}
		if w.synced >= seq {
			w.mu.Unlock()
			return nil
		}
		if !w.syncing {
			break
		}
		w.cond.Wait()
	}

	// leader 阶段：一直刷到"没有更新的写入"为止
	w.syncing = true
	for {
		target := w.written
		w.mu.Unlock()
		t0 := hrtime.Now()
		err := w.f.Sync()
		took := hrtime.Since(t0)
		w.mu.Lock()
		w.SyncRuns++
		// fsync 是 COMMIT 的关键路径，它的抖动直接变成客户端能感知的卡顿。
		// 失败的那次也记：一次 200ms 的失败重试同样值得看见。
		if w.syncRing != nil {
			w.syncRing.add(took)
		}
		w.lastSyncNs.Store(took)
		if err != nil {
			w.syncErr = fmt.Errorf("fsync WAL 失败: %w", err)
			break
		}
		if target > w.synced {
			w.synced = target
		}
		if w.written <= target {
			break
		}
	}
	w.syncing = false
	w.cond.Broadcast()
	err := w.syncErr
	w.mu.Unlock()
	return err
}

// SyncProfile 返回 fsync 延迟画像（给监控面板）。
func (w *Wal) SyncProfile() FsyncProfile {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := FsyncProfile{
		Runs:       w.SyncRuns,
		LastNs:     w.lastSyncNs.Load(),
		Bytes:      w.Bytes,
		LiveBytes:  w.liveBytes,
		Frames:     w.Frames,
		SyncedSeq:  w.synced,
		WrittenSeq: w.written,
	}
	if w.syncRing != nil {
		p.P50Ns, p.P95Ns, p.MaxNs = w.syncRing.stats()
		p.RingSamples = w.syncRing.n
	}
	return p
}

// Rotate 轮转到指定代次：删掉旧文件、建新文件。
// 调用者必须保证旧记录对应的数据已经写进快照（也就是在 checkpoint 之后调用）。
// 轮转后所有等待 fsync 的事务直接视为已持久化——数据已经在快照里了。
func (w *Wal) Rotate(nextGen int64) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("WAL 已关闭")
	}
	// 等正在进行的 fsync 结束，避免把它脚下的文件关掉
	for w.syncing {
		w.cond.Wait()
	}
	if err := w.f.Close(); err != nil {
		w.mu.Unlock()
		return err
	}
	if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		w.mu.Unlock()
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	if _, err := f.Write(walMagic); err != nil {
		f.Close()
		w.mu.Unlock()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		w.mu.Unlock()
		return err
	}
	w.f = f
	w.gen = nextGen
	w.liveBytes = int64(len(walMagic))
	w.synced = w.written // 旧记录已被快照覆盖，不需要再等 fsync
	w.cond.Broadcast()
	w.mu.Unlock()
	return nil
}

// Close 关闭文件。
func (w *Wal) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.syncing {
		w.cond.Wait()
	}
	if w.closed || w.f == nil {
		return nil
	}
	w.closed = true
	err := w.f.Close()
	w.f = nil
	w.cond.Broadcast()
	return err
}

// Stats 返回 WAL 统计。
// LiveBytes 返回当前 WAL 文件的大小（轮转后归零），
// 用于「WAL 涨到多少 MB 就落盘」这条策略。
func (w *Wal) LiveBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.liveBytes
}

func (w *Wal) Stats() (frames, bytes, syncs int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Frames, w.Bytes, w.SyncRuns
}

// ---------- 帧编码 ----------

// appendFrame 编码一帧：uint32 长度 + uint32 CRC32 + 负载。
func appendFrame(b []byte, rec *walRec) []byte {
	start := len(b)
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0) // 占位：长度 + CRC
	b = append(b, rec.Op)
	b = binary.LittleEndian.AppendUint64(b, uint64(rec.Gen))
	b = binary.LittleEndian.AppendUint64(b, uint64(rec.Tx))
	tbl := []byte(rec.Table)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(tbl)))
	b = append(b, tbl...)
	if rec.Op == opDel || rec.Op == opUpd {
		b = appendRow(b, rec.Old)
	}
	if rec.Op == opIns || rec.Op == opUpd {
		b = appendRow(b, rec.New)
	}
	payload := b[start+8:]
	binary.LittleEndian.PutUint32(b[start:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[start+4:], crc32.Checksum(payload, crcTable))
	return b
}

// decodeFrame 解一帧，返回 (记录, 新偏移, 错误)。
func decodeFrame(b []byte, off int) (*walRec, int, error) {
	if off+8 > len(b) {
		return nil, off, io.ErrUnexpectedEOF
	}
	n := int(binary.LittleEndian.Uint32(b[off:]))
	sum := binary.LittleEndian.Uint32(b[off+4:])
	off += 8
	if n < 0 || off+n > len(b) {
		return nil, off, io.ErrUnexpectedEOF
	}
	payload := b[off : off+n]
	if crc32.Checksum(payload, crcTable) != sum {
		return nil, off, fmt.Errorf("WAL 帧校验和不匹配")
	}
	rec := &walRec{}
	p := 0
	if p >= len(payload) {
		return nil, off, fmt.Errorf("WAL 帧为空")
	}
	rec.Op = payload[p]
	p++
	if p+16 > len(payload) {
		return nil, off, fmt.Errorf("WAL 帧头不完整")
	}
	rec.Gen = int64(binary.LittleEndian.Uint64(payload[p:]))
	p += 8
	rec.Tx = int64(binary.LittleEndian.Uint64(payload[p:]))
	p += 8
	if p+2 > len(payload) {
		return nil, off, fmt.Errorf("WAL 表名长度缺失")
	}
	tl := int(binary.LittleEndian.Uint16(payload[p:]))
	p += 2
	if p+tl > len(payload) {
		return nil, off, fmt.Errorf("WAL 表名越界")
	}
	rec.Table = string(payload[p : p+tl])
	p += tl
	if rec.Op == opDel || rec.Op == opUpd {
		row, next, err := readRowValues(payload, p)
		if err != nil {
			return nil, off, err
		}
		rec.Old = row
		p = next
	}
	if rec.Op == opIns || rec.Op == opUpd {
		row, next, err := readRowValues(payload, p)
		if err != nil {
			return nil, off, err
		}
		rec.New = row
		p = next
	}
	return rec, off + n, nil
}

// ---------- 重放 ----------

// readWALFile 读出文件里的全部记录（兼容旧版按行 JSON 的文件）。
// 末尾不完整的帧（崩溃时写了一半）直接丢弃。
func readWALFile(path string) ([]walRec, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	head := make([]byte, len(walMagic))
	n, _ := io.ReadFull(f, head)
	if n < len(walMagic) || string(head) != string(walMagic) {
		// 旧格式：按行 JSON
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return readLegacyJSON(f)
	}

	rest, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var out []walRec
	off := 0
	for off < len(rest) {
		rec, next, derr := decodeFrame(rest, off)
		if derr != nil {
			// 尾部写坏：保留已经读出来的部分
			break
		}
		out = append(out, *rec)
		off = next
	}
	return out, nil
}

type legacyRec struct {
	Tx    int64         `json:"tx"`
	Op    string        `json:"op"`
	Table string        `json:"table,omitempty"`
	Old   []interface{} `json:"old,omitempty"`
	New   []interface{} `json:"new,omitempty"`
}

func readLegacyJSON(r io.Reader) ([]walRec, error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	dec.UseNumber()
	var out []walRec
	for {
		var lr legacyRec
		if err := dec.Decode(&lr); err != nil {
			break
		}
		rec := walRec{Tx: lr.Tx, Table: lr.Table}
		switch lr.Op {
		case "ins":
			rec.Op = opIns
		case "upd":
			rec.Op = opUpd
		case "del":
			rec.Op = opDel
		case "commit":
			rec.Op = opCommit
		default:
			continue
		}
		if lr.Old != nil {
			rec.Old = legacyRow(lr.Old)
		}
		if lr.New != nil {
			rec.New = legacyRow(lr.New)
		}
		out = append(out, rec)
	}
	return out, nil
}

// legacyRow 把 JSON 行转成值列表（类型在此不关心，后面按列类型 cast）。
func legacyRow(raw []interface{}) []types.Value {
	out := make([]types.Value, len(raw))
	for i, v := range raw {
		out[i] = legacyValue(v)
	}
	return out
}

func legacyValue(v interface{}) types.Value {
	switch x := v.(type) {
	case nil:
		return types.Null
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return types.Int(i)
		}
		if f, err := x.Float64(); err == nil {
			return types.Float(f)
		}
		return types.Null
	case float64:
		return types.Float(x)
	case bool:
		return types.Bool(x)
	case string:
		return types.Text(x)
	case map[string]interface{}:
		if s, ok := x["$ts"].(string); ok {
			if t, err := parseRFC3339(s); err == nil {
				return types.Time(t)
			}
		}
	}
	return types.Null
}

// replayWAL 重放日志：只应用「代次高于快照」且带 commit 标记的事务。
// replayWAL 重放 WAL。多表并行：先按事务提交状态筛出需要重放的记录，
// 再按表分组（表内保序，表间无依赖），多表同时 apply。
// 单表场景与旧实现等价（顺序不变）。
func (s *Store) replayWAL() error {
	recs, err := readWALFile(walPath(s.Dir))
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	legacy := true
	maxGen := int64(0)
	for _, r := range recs {
		if r.Gen > 0 {
			legacy = false
		}
		if r.Gen > maxGen {
			maxGen = r.Gen
		}
	}
	// 旧格式记录没有代次号，视为"比当前快照更新"
	if legacy {
		maxGen = s.snapshotGen + 1
		for i := range recs {
			recs[i].Gen = maxGen
		}
	}
	if maxGen > s.appliedGen {
		s.appliedGen = maxGen
	}

	// 1) 哪些事务提交了
	committed := map[int64]bool{}
	for _, rec := range recs {
		if rec.Op == opCommit {
			committed[rec.Tx] = true
		}
	}
	// 2) 按表分组（只收已提交事务、且代次比表文件新的记录）
	byTable := map[string][]walRec{}
	for _, rec := range recs {
		if rec.Op == opCommit || !committed[rec.Tx] {
			continue
		}
		key := cleanName(rec.Table)
		if rec.Gen <= s.tableGen[key] {
			continue // 该表文件已包含这一代
		}
		byTable[key] = append(byTable[key], rec)
	}
	if len(byTable) == 0 {
		return nil
	}

	// 3) 多表并行 apply；单表时直接串行（避免 goroutine 开销）
	if len(byTable) == 1 {
		for _, tbl := range byTable {
			for _, r := range tbl {
				if err := s.applyRec(r); err != nil {
					return fmt.Errorf("重放 WAL 失败: %w", err)
				}
			}
		}
		return nil
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, tbl := range byTable {
		tbl := tbl
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, r := range tbl {
				if err := s.applyRec(r); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("重放 WAL 失败: %w", err)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// bumpSeq 让自增计数器至少抬到这一行里的值。
//
// 加载快照时会按现有行恢复 Seq，但 WAL 里往往还有比快照更新的行：重放只把行
// 加进来而不跟着抬 Seq，重启后第一笔 INSERT 就会拿到旧值撞主键（压测留下
// 60 万行后被踩到：max(id)=600691，不指定 id 的 INSERT 却撞了已有的 id=1）。
func bumpSeq(si int, td *TableData, row types.Row) {
	if si < 0 || si >= len(row) {
		return
	}
	if row[si].K == types.KInt && row[si].I > td.Seq {
		td.Seq = row[si].I
	}
}

func (s *Store) applyRec(rec walRec) error {
	key := cleanName(rec.Table)
	if !s.isLoaded(key) {
		s.ensureLoaded(key)
	}
	td := s.Data[key]
	if td == nil {
		return nil
	}
	t, ok := s.Cat.Get(rec.Table)
	if !ok {
		return nil
	}
	// 重放的改动只存在于内存里，必须标脏，否则接下来的轮转会把它丢掉
	s.loadMu.Lock()
	s.dirty[key] = true
	s.loadMu.Unlock()
	switch rec.Op {
	case opIns:
		row, rep := castRow(t, rec.New)
		s.Repaired.Add(int64(rep))
		td.Rows = append(td.Rows, row)
		td.Live++
		bumpSeq(t.SerialIdx(), td, row)
	case opDel:
		row, rep := castRow(t, rec.Old)
		s.Repaired.Add(int64(rep))
		for i := range td.Rows {
			if td.Rows[i] == nil {
				continue
			}
			if types.RowSame(td.Rows[i], row) {
				// 置空而不是搬移：重放一条删除就是 O(1)，
				// 搬移会让「重放 N 条删除」退化成 O(n²)。收尾的 RebuildIndex
				// 会顺手把空槽登记进 free。
				td.Rows[i] = nil
				td.Live--
				break
			}
		}
	case opUpd:
		oldRow, rep := castRow(t, rec.Old)
		s.Repaired.Add(int64(rep))
		newRow, rep2 := castRow(t, rec.New)
		s.Repaired.Add(int64(rep2))
		for i := range td.Rows {
			if td.Rows[i] == nil {
				continue
			}
			if types.RowSame(td.Rows[i], oldRow) {
				td.Rows[i] = newRow
				break
			}
		}
		// UPDATE 也可能把自增列改成更大的值
		bumpSeq(t.SerialIdx(), td, newRow)
	}
	return nil
}
