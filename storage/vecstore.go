package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"godb/catalog"
	"godb/types"
)

// 向量列的磁盘文件管理。
//
// 一个向量列一个文件：tables/t<表ID>.c<列ID>.vec。用 ID 而不是名字，
// 是因为 RENAME TABLE / RENAME COLUMN 不该让 28 GB 的文件搬家；
// 用列 ID 而不是 attnum，是因为 DROP COLUMN 之后 attnum 会整体前移
// （见 catalog.Column.ID 的注释）。
//
// 文件的生命周期跟着 catalog 走：SyncVectorFiles 把「catalog 里有的向量列」
// 和「打开着的文件」对一次账，多的移进回收站、少的补上。所有 DDL 之后调一次即可，
// 比起在每个 DDL 分支里各写一遍，漏掉某个分支的概率小得多。

// VecColumn 定位一个向量列。
type VecColumn struct {
	TableOID int32 // 仅用于诊断（PG 的 OID）
	TableID  int64 // 文件定位：表 ID
	ColumnID int64 // 文件定位：列 ID
	Dim      int

	// AttNum 是这一列在客户端眼里的列号（pg_attribute.attnum），只用于报错时
	// 把话说清楚 —— 定位文件不用它。
	AttNum int16
}

type vecKey struct {
	tableID  int64
	columnID int64
}

func (k vecKey) String() string { return fmt.Sprintf("t%d.c%d", k.tableID, k.columnID) }

// VecHandle 是一次查询内反复使用的列读取句柄。
//
// 存在的理由是每行都要读一次向量：VecAt 每次都「查 map + 判空」，
// 十万行就是十万次多余的查找和锁。句柄的有效期由表级 2PL 保证 ——
// 扫描期间持有 S 锁，没有事务能拿 X 锁去 DDL 删这个列。
type VecHandle interface {
	Read(id int64, dst []float32) ([]float32, error)
	// View 返回槽位的零拷贝视图（指向文件映射）。ok=false 时调用方退回 Read。
	View(id int64) ([]float32, bool)
}

// VecSource 是执行层读向量的接口（exec 里以类型别名引用）。
type VecSource interface {
	VecAt(col VecColumn, id int64, dst []float32) ([]float32, error)
	// VecHandle 取一个可复用的列读取句柄；列没有打开时返回 false。
	VecHandle(col VecColumn) (VecHandle, bool)
}

// VecHandle 返回一列的读取句柄。
func (s *Store) VecHandle(col VecColumn) (VecHandle, bool) {
	vf, ok := s.vecFile(col)
	if !ok {
		return nil, false
	}
	return vf, true
}

// VecSource 返回本事务可用的向量来源；库里一个向量列都没有时返回 nil。
//
// 返回 nil 而不是「总是返回 Store」是有意义的：为 nil 时执行层连尝试都不会尝试，
// 槽位引用会明确报「没有绑定向量列」，而不是每行都去查一次空 map。
func (tx *Tx) VecSource() VecSource {
	if tx == nil || tx.st == nil || !tx.st.hasVecFiles() {
		return nil
	}
	return tx.st
}

// vecPath 返回向量列文件的路径。
func vecPath(dir string, tableID, columnID int64) string {
	return filepath.Join(dir, "tables", fmt.Sprintf("t%06d.c%03d.vec", tableID, columnID))
}

// VecAt 读一个槽位的向量，实现 exec 侧的向量来源。
func (s *Store) VecAt(col VecColumn, id int64, dst []float32) ([]float32, error) {
	vf, ok := s.vecFile(col)
	if !ok {
		return nil, fmt.Errorf("%w（%s）", ErrVecNoFile, vecKey{col.TableID, col.ColumnID})
	}
	return vf.Read(id, dst)
}

// vecFile 取一列已打开的向量文件。
func (s *Store) vecFile(col VecColumn) (*VecFile, bool) {
	s.vecMu.RLock()
	vf, ok := s.vecFiles[vecKey{col.TableID, col.ColumnID}]
	s.vecMu.RUnlock()
	return vf, ok
}

// hasVecFiles 报告当前是否打开了任何向量列文件。
//
// 提交路径用它做一次 O(1) 的短路：绝大多数库一个向量列都没有，
// 不该为它们每次提交都去遍历写集找向量值。
func (s *Store) hasVecFiles() bool {
	s.vecMu.RLock()
	n := len(s.vecFiles)
	s.vecMu.RUnlock()
	return n > 0
}

// SyncVectorFiles 让磁盘上的向量列文件与 catalog 一致。
//
// 幂等，可以在任何 DDL 之后无条件调用：
//   - catalog 里有、文件没打开的 → 打开（不存在就创建）；
//   - 打开着、catalog 里已经没有的 → 关闭并移进回收站。
//
// 为什么是「移进回收站」而不是直接删：DROP COLUMN 只回一句 ALTER TABLE，
// 后面跟着 28 GB 的向量数据。直接删掉，误操作就再也没有回头路 ——
// 这和 DROP TABLE 走回收站是同一个理由。
func (s *Store) SyncVectorFiles() error {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	if s.vecFiles == nil {
		s.vecFiles = make(map[vecKey]*VecFile)
	}
	want := make(map[vecKey]struct{}, len(s.vecFiles))
	if s.Cat != nil {
		// tables/ 目录可能还不存在（空库 + 第一张表就是向量表），
		// 不能指望别的地方先建好。
		if len(s.vecFiles) == 0 && len(s.Cat.Tables) > 0 {
			if err := os.MkdirAll(filepath.Join(s.Dir, "tables"), 0o755); err != nil {
				return fmt.Errorf("创建 tables 目录失败: %w", err)
			}
		}
		for _, t := range s.Cat.Tables {
			for _, c := range t.Columns {
				dim := vectorDimOf(c)
				if dim <= 0 {
					continue
				}
				k := vecKey{t.ID, c.ID}
				want[k] = struct{}{}
				if _, ok := s.vecFiles[k]; ok {
					continue
				}
				vf, err := OpenVecFile(vecPath(s.Dir, t.ID, c.ID), dim)
				if err != nil {
					// 区分「新建失败」和「已有文件坏了」：
					//   - 文件本来不存在却建不出来（磁盘满、权限）→ 必须报错，
					//     否则 DDL 成功了却存不下向量，用户完全看不出来；
					//   - 文件在但读不了（维度对不上、头损坏）→ 不能因此让整个库
					//     打不开，记下原因继续启动，查询这一列时按 NULL 处理。
					if _, statErr := os.Stat(vecPath(s.Dir, t.ID, c.ID)); statErr != nil {
						return fmt.Errorf("创建向量列 %s.%s 失败: %w", t.Name, c.Name, err)
					}
					if s.vecErr == nil {
						s.vecErr = fmt.Errorf("向量列 %s.%s 打不开（按 NULL 处理）: %w", t.Name, c.Name, err)
					}
					continue
				}
				s.vecFiles[k] = vf
			}
		}
	}
	var orphans []string
	for k, vf := range s.vecFiles {
		if _, ok := want[k]; ok {
			continue
		}
		path := vf.Path()
		_ = vf.Close()
		delete(s.vecFiles, k)
		orphans = append(orphans, path)
	}
	// 文件系统的活放在锁外做：删文件可能要几秒，不该卡住并发的查询。
	for _, p := range orphans {
		s.trashVecFile(p)
	}
	return nil
}

// trashVecFile 把一个不再被引用的向量文件移进回收站。
//
// 移不动（文件被占用 / 权限）就留在原地：留个孤儿文件浪费空间，
// 总比「DROP 报错但数据其实已经没了」强。启动时的 SyncVectorFiles 会再试一次。
func (s *Store) trashVecFile(path string) {
	dir := filepath.Join(s.Dir, ".trash", "vec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	dst := filepath.Join(dir, filepath.Base(path))
	// 同名文件已经存在（回收站里躺过一个同名列）就先加个后缀，不覆盖。
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	_ = os.Rename(path, dst)
}

// CloseVecFiles 关掉所有向量列文件（Close 与测试用）。
func (s *Store) CloseVecFiles() error {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	var first error
	for k, vf := range s.vecFiles {
		if err := vf.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.vecFiles, k)
	}
	return first
}

// VecFileStats 汇总所有打开的向量列文件（监控面板与备份用）。
func (s *Store) VecFileStats() (cols int, bytes int64) {
	s.vecMu.RLock()
	defer s.vecMu.RUnlock()
	for _, vf := range s.vecFiles {
		cols++
		bytes += vf.Bytes()
	}
	return cols, bytes
}

// vecColumnRef 是「某张表某个向量列」的定位信息，附带列在行里的下标。
type vecColumnRef struct {
	idx int
	col VecColumn
}

// vectorColsOf 返回一张表里所有向量列。没有向量列时返回 nil（绝大多数表）。
func vectorColsOf(t *catalog.Table) []vecColumnRef {
	if t == nil {
		return nil
	}
	var out []vecColumnRef
	for i, c := range t.Columns {
		dim := vectorDimOf(c)
		if dim <= 0 {
			continue
		}
		out = append(out, vecColumnRef{idx: i, col: VecColumn{
			TableID:  t.ID,
			ColumnID: c.ID,
			Dim:      dim,
			AttNum:   int16(i + 1),
		}})
	}
	return out
}

// vectorDimOf 返回列定义里的向量维度，非向量列返回 0。
func vectorDimOf(c *catalog.Column) int {
	if c == nil || types.Kind(c.Kind) != types.KVector {
		return 0
	}
	return c.Len
}
