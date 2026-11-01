package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 备份导出：GET /api/backup?name=<库名>
//
// 只做导出，「恢复」走的是现成的 /api/query —— 导入一份 SQL 就是执行一段 SQL，
// 没必要再开一个接口（也就不会多出一条没人测的路径）。
//
// 特意同步返回而不是流式：教学库的量级是几万行、几 MB，一次写完也就几十毫秒；
// 流式要处理「写一半失败」的半截文件，反而更难解释。
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "godb"
	}
	d := s.currentDB(r)
	text, err := d.DumpSQL(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "导出失败：" + err.Error(),
		})
		return
	}
	fn := fmt.Sprintf("godb-%s-%s.sql", safeFileName(name), time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/sql; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fn+`"`)
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	_, _ = w.Write([]byte(text))
}

// safeFileName 只留字母数字和 _-，其余换成 _。
//
// 中文库名也能下，但文件名的编码在各浏览器 / 各系统上不一致，与其赌，
// 不如让磁盘上的文件名永远是 ASCII（库名在文件头注释里，丢不了）。
func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "godb"
	}
	return out
}
