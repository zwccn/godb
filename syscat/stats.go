package syscat

import (
	"strings"
	"sync"
	"time"
)

// TableStats 是 ANALYZE 收集的简单统计（内存态，进程内有效）。
type TableStats struct {
	Rows        int64
	SeqScans    int64
	IdxScans    int64
	LastAnalyze time.Time
	// Cols 记列级统计：非空数、粗略 distinct（哈希上限 1024）。
	Cols map[string]ColStats
}

// ColStats 是一列的统计。
type ColStats struct {
	NotNull  int64
	Distinct int64
}

var (
	statsMu sync.RWMutex
	stats   = map[string]*TableStats{}
)

func statKey(table string) string { return strings.ToLower(strings.TrimSpace(table)) }

// NoteSeqScan / NoteIdxScan 累加扫描计数（EXPLAIN 与 pg_stat 用）。
func NoteSeqScan(table string) {
	statsMu.Lock()
	s := statsFor(table)
	s.SeqScans++
	statsMu.Unlock()
}

func NoteIdxScan(table string) {
	statsMu.Lock()
	s := statsFor(table)
	s.IdxScans++
	statsMu.Unlock()
}

// RecordAnalyze 写入 ANALYZE 结果。
func RecordAnalyze(table string, rows int64, cols map[string]ColStats) {
	statsMu.Lock()
	s := statsFor(table)
	s.Rows = rows
	s.LastAnalyze = time.Now()
	if len(cols) > 0 {
		s.Cols = cols
	}
	statsMu.Unlock()
}

// GetStats 返回表统计的副本（没有则零值）。
func GetStats(table string) TableStats {
	statsMu.RLock()
	defer statsMu.RUnlock()
	s := stats[statKey(table)]
	if s == nil {
		return TableStats{Cols: map[string]ColStats{}}
	}
	out := TableStats{Rows: s.Rows, SeqScans: s.SeqScans, IdxScans: s.IdxScans, LastAnalyze: s.LastAnalyze}
	out.Cols = make(map[string]ColStats, len(s.Cols))
	for k, v := range s.Cols {
		out.Cols[k] = v
	}
	return out
}

// DropStats 表被删时清掉统计。
func DropStats(table string) {
	statsMu.Lock()
	delete(stats, statKey(table))
	statsMu.Unlock()
}

func statsFor(table string) *TableStats {
	k := statKey(table)
	s := stats[k]
	if s == nil {
		s = &TableStats{Cols: map[string]ColStats{}}
		stats[k] = s
	}
	return s
}
