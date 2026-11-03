package storage

import (
	"fmt"

	"godb/catalog"
	"godb/types"
)

// vecAlloc 记一个已分配的向量槽位，提交失败时用它在回收。
type vecAlloc struct {
	col VecColumn
	id  int64
}

// materializeVectors 把写集里的内联向量写进列文件，行里换成槽位引用。
//
// 必须在写 WAL 之前完成，两个理由都是硬的：
//
//  1. 日志里只记 11 字节的槽位引用。768 维一行是 3 KB，1000 万行就是 30 GB
//     日志 —— 而 checkpoint 阈值是 64 MB，等于「写几十行就落一次盘」。
//  2. 先写数据文件、再写日志。反过来会有一个窗口：日志说这行有向量，
//     文件里却还没有，恢复之后得到「行在、向量是全零」。静默错数据比报错难查得多。
//
// 返回值是本次分配的槽位。调用方在提交失败时必须全部 Free，
// 否则这些槽位既不在空闲位图里、也没有任何行引用，永久漏掉。
func (s *Store) materializeVectors(tx *Tx) ([]vecAlloc, error) {
	var allocs []vecAlloc
	touched := make(map[vecKey]bool)
	fail := func(err error) ([]vecAlloc, error) {
		s.freeVecAllocs(allocs)
		return nil, err
	}
	for name, o := range tx.ov {
		t, ok := s.Cat.Get(name)
		if !ok {
			continue
		}
		cols := vectorColsOf(t)
		if len(cols) == 0 {
			continue
		}
		for i, row := range o.ins {
			nr, changed, err := s.materializeRow(row, cols, &allocs, touched)
			if err != nil {
				return fail(err)
			}
			if changed {
				o.ins[i] = nr
			}
		}
		for slot, row := range o.upd {
			nr, changed, err := s.materializeRow(row, cols, &allocs, touched)
			if err != nil {
				return fail(err)
			}
			if changed {
				o.upd[slot] = nr
			}
		}
		for slot, row := range o.insUpd {
			nr, changed, err := s.materializeRow(row, cols, &allocs, touched)
			if err != nil {
				return fail(err)
			}
			if changed {
				o.insUpd[slot] = nr
			}
		}
	}
	// 全部写完再统一刷一次：每行一次 fsync 会让批量导入慢两个数量级。
	for k := range touched {
		vf, ok := s.vecFileByKey(k)
		if !ok {
			continue
		}
		if err := vf.Sync(); err != nil {
			return fail(err)
		}
	}
	return allocs, nil
}

// materializeRow 把一行里的内联向量写进列文件。
//
// 返回的行是新复制的：调用方（exec 的 RETURNING、触发器等）可能还持有原行，
// 原地改会把它们看到的向量变成 "[vector:7]"。
func (s *Store) materializeRow(row types.Row, cols []vecColumnRef,
	allocs *[]vecAlloc, touched map[vecKey]bool) (types.Row, bool, error) {
	var out types.Row
	for _, vc := range cols {
		if vc.idx >= len(row) {
			continue
		}
		v := row[vc.idx]
		if v.K != types.KVector || !v.IsVecInline() {
			continue
		}
		vec, err := types.ParseVector(v.S)
		if err != nil {
			return nil, false, err
		}
		if vc.col.Dim > 0 && len(vec) != vc.col.Dim {
			return nil, false, fmt.Errorf("vector dimension mismatch: column is %d, value has %d",
				vc.col.Dim, len(vec))
		}
		vf, ok := s.vecFile(vc.col)
		if !ok {
			return nil, false, fmt.Errorf("向量列 %s 的文件没有打开，无法写入", vecKey{vc.col.TableID, vc.col.ColumnID})
		}
		id, err := vf.Append(vec)
		if err != nil {
			return nil, false, err
		}
		*allocs = append(*allocs, vecAlloc{col: vc.col, id: id})
		touched[vecKey{vc.col.TableID, vc.col.ColumnID}] = true
		if out == nil {
			out = append(make(types.Row, 0, len(row)), row...)
		}
		out[vc.idx] = types.VectorRef(id, len(vec))
	}
	if out == nil {
		return row, false, nil
	}
	return out, true, nil
}

// freeVecAllocs 回收一批槽位（提交失败时用）。
func (s *Store) freeVecAllocs(allocs []vecAlloc) {
	for _, a := range allocs {
		if vf, ok := s.vecFile(a.col); ok {
			vf.Free(a.id)
		}
	}
}

// freeVecRow 回收一行引用的所有向量槽位（这一行被删除时调用）。
func (s *Store) freeVecRow(t *catalog.Table, row types.Row) {
	if t == nil || row == nil || !s.hasVecFiles() {
		return
	}
	for _, vc := range vectorColsOf(t) {
		if vc.idx >= len(row) {
			continue
		}
		if id, ok := row[vc.idx].VecID(); ok {
			if vf, ok2 := s.vecFile(vc.col); ok2 {
				vf.Free(id)
			}
		}
	}
}

// freeVecReplaced 在更新时回收旧槽位：只有新行不再引用同一个槽位才回收。
//
// 「同一个槽位」是常态还是例外？目前每次更新都会 Append 一个新槽位
// （向量是定长原地可写的，但走 Append 更简单），所以通常会命中回收分支。
// 判断一下而不是无条件 Free：万一以后改成原地改写，这里不会把还在用的槽位放回去。
func (s *Store) freeVecReplaced(t *catalog.Table, oldRow, newRow types.Row) {
	if t == nil || oldRow == nil || newRow == nil || !s.hasVecFiles() {
		return
	}
	for _, vc := range vectorColsOf(t) {
		if vc.idx >= len(oldRow) || vc.idx >= len(newRow) {
			continue
		}
		oldID, ok := oldRow[vc.idx].VecID()
		if !ok {
			continue
		}
		if newID, ok2 := newRow[vc.idx].VecID(); ok2 && newID == oldID {
			continue
		}
		if vf, ok3 := s.vecFile(vc.col); ok3 {
			vf.Free(oldID)
		}
	}
}

// vecFileByKey 按内部 key 取文件。
func (s *Store) vecFileByKey(k vecKey) (*VecFile, bool) {
	s.vecMu.RLock()
	vf, ok := s.vecFiles[k]
	s.vecMu.RUnlock()
	return vf, ok
}
