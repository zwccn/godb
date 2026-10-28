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
	TupIns      int64 // 累计 INSERT 行数
	TupUpd      int64 // 累计 UPDATE 行数
	TupDel      int64 // 累计 DELETE 行数
	LastAnalyze time.Time
	// Cols 记列级统计：非空数、粗略 distinct（哈希上限 1024）。
	Cols map[string]ColStats
}

// ColStats 是一列的统计。
type ColStats struct {
	NotNull  int64
	Distinct int64
}

// DBStats 是库级计数器（pg_stat_database 用）。
type DBStats struct {
	XactCommit   int64
	XactRollback int64
	TupReturned  int64
	TupFetched   int64
	TupInserted  int64
	TupUpdated   int64
	TupDeleted   int64
}

var (
	statsMu sync.RWMutex
	stats   = map[string]*TableStats{}

	dbStatsMu sync.Mutex
	dbStats   DBStats
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

// NoteDML 累加 DML 行数（pg_stat_user_tables 的 n_tup_ins/upd/del）。
func NoteDML(table string, ins, upd, del int64) {
	if ins == 0 && upd == 0 && del == 0 {
		return
	}
	statsMu.Lock()
	s := statsFor(table)
	s.TupIns += ins
	s.TupUpd += upd
	s.TupDel += del
	statsMu.Unlock()
	dbStatsMu.Lock()
	dbStats.TupInserted += ins
	dbStats.TupUpdated += upd
	dbStats.TupDeleted += del
	dbStatsMu.Unlock()
}

// NoteXact 事务结束计数（pg_stat_database 的 xact_commit / xact_rollback）。
func NoteXact(commit bool) {
	dbStatsMu.Lock()
	if commit {
		dbStats.XactCommit++
	} else {
		dbStats.XactRollback++
	}
	dbStatsMu.Unlock()
}

// NoteRows 读行计数（pg_stat_database 的 tup_returned / tup_fetched）。
func NoteRows(returned, fetched int64) {
	dbStatsMu.Lock()
	dbStats.TupReturned += returned
	dbStats.TupFetched += fetched
	dbStatsMu.Unlock()
}

// GetDBStats 返回库级计数器快照。
func GetDBStats() DBStats {
	dbStatsMu.Lock()
	defer dbStatsMu.Unlock()
	return dbStats
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
	out := TableStats{Rows: s.Rows, SeqScans: s.SeqScans, IdxScans: s.IdxScans,
		TupIns: s.TupIns, TupUpd: s.TupUpd, TupDel: s.TupDel, LastAnalyze: s.LastAnalyze}
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
