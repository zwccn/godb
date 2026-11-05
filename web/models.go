// 模型：与库解耦的逻辑设计。
//
// 和 diagram.json 的区别（这是这次改动的全部意义）：
//   - diagram.json 是「某个库里那些表的摆法」，表本身来自库，库里没有的表画不出来；
//   - 模型是一份**独立的**设计，自己带表、列、类型、关系，库只是它可以去对齐的对象。
//
// 于是模型不跟着库走：存在数据根目录下的 models/，一个模型一个目录，
//
//	<data>/models/<id>/model.json      工作副本（前端自动保存）
//	<data>/models/<id>/rev-0003.json   定版快照，序号递增
//
// 为什么一个模型一个目录、版本一个文件：删掉某一版只是删一个文件，不用重写
// 别的版本；也不用维护「版本索引」这种会和实际文件对不上的第二份真相。
package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"net/http"

	"godb/db"
)

const (
	modelsDirName   = "models"
	modelFileName   = "model.json"
	modelDocVersion = 1
	modelNameMax    = 60
	modelMaxTables  = 400
	modelMaxColumns = 200
	modelMaxLinks   = 4000
	modelMaxBody    = 4 << 20 // 一份模型带全部列，比 diagram.json 大得多
	modelIDLen      = 12
	modelRevPrefix  = "rev-"
	modelRevSuffix  = ".json"
)

// modelColumn 是模型里的一列。
//
// 类型只存**用户写的那个字符串**（`INTEGER` / `NUMERIC(12,2)` / `TEXT`），
// 不在模型里另建一套类型枚举：模型要能表达库里还没有的类型，也要能原样
// 生成 DDL；中间再翻译一次只会多一处对不上的地方。
type modelColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	PK      bool   `json:"pk,omitempty"`
	NotNull bool   `json:"notNull,omitempty"`
	Uniq    bool   `json:"uniq,omitempty"`
	Def     string `json:"def,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// modelTable 是模型里的一张表。ID 是模型内部的稳定标识（t1/t2…），
// 关系、分组都按它引用 —— 改名不会让连线散架。
type modelTable struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Comment string   `json:"comment,omitempty"`
	Hidden  bool     `json:"hidden,omitempty"`
	X       *float64 `json:"x,omitempty"`
	Y       *float64 `json:"y,omitempty"`
	Group   string   `json:"group,omitempty"`
	// Color 是这张卡片**自己的**色号（0..15）；null 表示跟随所属分组的颜色。
	// 用指针而不是 -1：0 号色是合法值，用 int 的零值分不清「没设过」和「选了 0 号」。
	Color   *int          `json:"color,omitempty"`
	Columns []modelColumn `json:"columns"`
}

type modelEndpoint struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

type modelLink struct {
	ID   string        `json:"id"`
	From modelEndpoint `json:"from"`
	To   modelEndpoint `json:"to"`
	Card string        `json:"card"`
}

// modelDefaultGroup 是「默认分组」的外观：没归组的表都待在这里。
//
// 它不是一个真正的组（成员是「group 为空」的那些表，不走 groups 列表），
// 但用户可以给它改名、挑色 —— 所以名字和颜色得存下来。
type modelDefaultGroup struct {
	Name  string `json:"name,omitempty"`
	Color *int   `json:"color,omitempty"`
}

type modelDoc struct {
	Version      int                `json:"version"`
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	CreatedAt    string             `json:"createdAt,omitempty"`
	UpdatedAt    string             `json:"updatedAt,omitempty"`
	Groups       []diagramGroup     `json:"groups,omitempty"`
	DefaultGroup *modelDefaultGroup `json:"defaultGroup,omitempty"`
	Tables       []modelTable       `json:"tables"`
	Links        []modelLink        `json:"links,omitempty"`
}

// modelSummary 是列表页要的那些数：不把整份模型带上，模型多了列表也不卡。
type modelSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Tables     int    `json:"tables"`
	Columns    int    `json:"columns"`
	Links      int    `json:"links"`
	Groups     int    `json:"groups"`
	UpdatedAt  string `json:"updatedAt"`
	Revisions  int    `json:"revisions"`
	LastRevAt  string `json:"lastRevision,omitempty"`
	LastRevSeq int    `json:"lastRevisionSeq,omitempty"`
}

// modelMu 让「读 → 改 → 写」这一串不互相踩，和 diagramMu 一个道理。
var modelMu sync.Mutex

func modelsRootDir(root *db.DB) string { return filepath.Join(root.Dir(), modelsDirName) }

func modelDir(root *db.DB, id string) string { return filepath.Join(modelsRootDir(root), id) }

func modelPath(root *db.DB, id string) string {
	return filepath.Join(modelDir(root, id), modelFileName)
}

func revPath(root *db.DB, id string, seq int) string {
	return filepath.Join(modelDir(root, id), fmt.Sprintf("%s%04d%s", modelRevPrefix, seq, modelRevSuffix))
}

func nowStr() string { return time.Now().UTC().Format(time.RFC3339) }

// validModelID 只允许我们自己生成的那种 id：小写字母数字。
//
// 路径是拼出来的，不做校验就等于把 `../` 让给客户端 —— 侧车文件是「可以手工
// 编辑」的元数据，但接口不能被拿来读写数据目录以外的任何文件。
func validModelID(id string) bool {
	if len(id) < 3 || len(id) > modelIDLen+8 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func newModelID() string {
	return "m" + strconv.FormatInt(time.Now().UnixNano()/1e6, 36) +
		strconv.FormatInt(int64(os.Getpid()%1296), 36) +
		strconv.FormatInt(time.Now().UnixNano()%1296, 36)
}

// normalize 清洗客户端传上来的模型。
//
// 和 diagram.normalize 同一套态度：不信任何一条坐标、不信任任何引用。
// 不同的是这里还要洗**结构**（表名、列名、类型），因为它会直接变成 DDL。
func (m *modelDoc) normalize() {
	if m.Version <= 0 {
		m.Version = modelDocVersion
	}
	m.Name = strings.TrimSpace(m.Name)
	if r := []rune(m.Name); len(r) > modelNameMax {
		m.Name = string(r[:modelNameMax])
	}
	if m.Name == "" {
		m.Name = "未命名模型"
	}
	if m.Tables == nil {
		m.Tables = []modelTable{}
	}

	// 组：和模型图一样，空 id / 重复 id 丢掉，色号夹到 0..7 或 -1。
	seenGroup := map[string]bool{}
	groups := m.Groups[:0]
	for _, g := range m.Groups {
		g.ID = strings.TrimSpace(g.ID)
		if g.ID == "" || seenGroup[g.ID] {
			continue
		}
		seenGroup[g.ID] = true
		if g.Color < -1 || g.Color > 15 {
			g.Color = -1
		}
		if r := []rune(g.Name); len(r) > diagramMaxName {
			g.Name = string(r[:diagramMaxName])
		}
		if g.X != nil && !validCoord(*g.X) {
			g.X = nil
		}
		if g.Y != nil && !validCoord(*g.Y) {
			g.Y = nil
		}
		if (g.X == nil) != (g.Y == nil) {
			g.X, g.Y = nil, nil
		}
		groups = append(groups, g)
	}
	m.Groups = groups

	// 默认分组：只有外观，成员是「group 为空」的那些表
	if dg := m.DefaultGroup; dg != nil {
		dg.Name = strings.TrimSpace(dg.Name)
		if r := []rune(dg.Name); len(r) > diagramMaxName {
			dg.Name = string(r[:diagramMaxName])
		}
		if dg.Color != nil && (*dg.Color < 0 || *dg.Color > 15) {
			dg.Color = nil
		}
		if dg.Name == "" && dg.Color == nil {
			m.DefaultGroup = nil // 什么都没设，别在文件里留一个空壳
		}
	}

	// 表：ID 补齐、表名去重（大小写不敏感 —— DDL 里本来就是不敏感的）、
	// 列名同表内去重、类型必填。
	seenTable := map[string]bool{}
	seenID := map[string]bool{}
	tables := m.Tables[:0]
	for _, t := range m.Tables {
		t.ID = strings.TrimSpace(t.ID)
		if t.ID == "" || seenID[t.ID] {
			t.ID = fmt.Sprintf("t%d", len(seenID)+1)
			for seenID[t.ID] {
				t.ID += "x"
			}
		}
		seenID[t.ID] = true

		t.Name = strings.TrimSpace(t.Name)
		if t.Name == "" {
			continue
		}
		key := strings.ToLower(t.Name)
		if seenTable[key] {
			continue
		}
		seenTable[key] = true

		if t.X != nil && !validCoord(*t.X) {
			t.X = nil
		}
		if t.Y != nil && !validCoord(*t.Y) {
			t.Y = nil
		}
		if (t.X == nil) != (t.Y == nil) {
			t.X, t.Y = nil, nil
		}
		if t.Group != "" && !seenGroup[t.Group] {
			t.Group = ""
		}
		// 色号越界就当「没设过」= 跟随分组，而不是夹到边界：
		// 客户端写坏一个 99 时，用户看到的是「跟我挑的不一样」，夹到 15 只会更费解。
		if t.Color != nil && (*t.Color < 0 || *t.Color > 15) {
			t.Color = nil
		}
		if r := []rune(t.Comment); len(r) > 200 {
			t.Comment = string(r[:200])
		}

		seenCol := map[string]bool{}
		cols := t.Columns[:0]
		for _, c := range t.Columns {
			c.Name = strings.TrimSpace(c.Name)
			if c.Name == "" {
				continue
			}
			ck := strings.ToLower(c.Name)
			if seenCol[ck] {
				continue
			}
			seenCol[ck] = true
			c.Type = strings.TrimSpace(c.Type)
			if c.Type == "" {
				c.Type = "TEXT"
			}
			c.Def = strings.TrimSpace(c.Def)
			if r := []rune(c.Comment); len(r) > 200 {
				c.Comment = string(r[:200])
			}
			cols = append(cols, c)
			if len(cols) >= modelMaxColumns {
				break
			}
		}
		t.Columns = cols
		tables = append(tables, t)
		if len(tables) >= modelMaxTables {
			break
		}
	}
	m.Tables = tables

	// 关系：两端都得落在存在的表/列上，列数相等。指向已删表的关系整条丢掉。
	colOf := map[string]map[string]bool{}
	for _, t := range m.Tables {
		set := map[string]bool{}
		for _, c := range t.Columns {
			set[strings.ToLower(c.Name)] = true
		}
		colOf[t.ID] = set
	}
	seenLink := map[string]bool{}
	links := m.Links[:0]
	for _, ln := range m.Links {
		ln.ID = strings.TrimSpace(ln.ID)
		if ln.ID == "" || seenLink[ln.ID] {
			continue
		}
		if !validCard(ln.Card) {
			ln.Card = card1toN
		}
		from, ok1 := cleanModelEndpoint(colOf, ln.From)
		to, ok2 := cleanModelEndpoint(colOf, ln.To)
		if !ok1 || !ok2 {
			continue
		}
		if from.Table == to.Table && sameCols(from.Columns, to.Columns) {
			continue
		}
		ln.From, ln.To = *from, *to
		seenLink[ln.ID] = true
		links = append(links, ln)
		if len(links) >= modelMaxLinks {
			break
		}
	}
	m.Links = links
	if len(m.Links) == 0 {
		m.Links = nil
	}
}

func cleanModelEndpoint(colOf map[string]map[string]bool, e modelEndpoint) (*modelEndpoint, bool) {
	e.Table = strings.TrimSpace(e.Table)
	set, ok := colOf[e.Table]
	if !ok || len(e.Columns) == 0 {
		return nil, false
	}
	cols := trimAll(e.Columns)
	if hasEmpty(cols) {
		return nil, false
	}
	for _, c := range cols {
		if !set[strings.ToLower(c)] {
			return nil, false
		}
	}
	return &modelEndpoint{Table: e.Table, Columns: cols}, true
}

// ---------- 读写 ----------

func loadModel(root *db.DB, id string) (modelDoc, error) {
	var m modelDoc
	b, err := os.ReadFile(modelPath(root, id))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	m.ID = id
	m.normalize()
	return m, nil
}

// saveModel 走 tmp + fsync + rename，和配置/侧车同一套写法。
func saveModel(root *db.DB, m modelDoc) error {
	m.Version = modelDocVersion
	dir := modelDir(root, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := modelPath(root, m.ID)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func listModels(root *db.DB) []modelSummary {
	out := []modelSummary{}
	entries, err := os.ReadDir(modelsRootDir(root))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || !validModelID(e.Name()) {
			continue
		}
		m, err := loadModel(root, e.Name())
		if err != nil {
			continue
		}
		s := modelSummary{
			ID: m.ID, Name: m.Name, Tables: len(m.Tables), Links: len(m.Links),
			Groups: len(m.Groups), UpdatedAt: m.UpdatedAt,
		}
		for _, t := range m.Tables {
			s.Columns += len(t.Columns)
		}
		if revs := listRevisions(root, m.ID); len(revs) > 0 {
			s.Revisions = len(revs)
			s.LastRevSeq = revs[0].Seq
			s.LastRevAt = revs[0].At
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---------- 版本 ----------

type modelRev struct {
	Seq    int    `json:"seq"`
	At     string `json:"at"`
	Note   string `json:"note,omitempty"`
	Tables int    `json:"tables"`
	Links  int    `json:"links"`
}

func listRevisions(root *db.DB, id string) []modelRev {
	out := []modelRev{}
	entries, err := os.ReadDir(modelDir(root, id))
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, modelRevPrefix) || !strings.HasSuffix(name, modelRevSuffix) {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, modelRevPrefix), modelRevSuffix))
		if err != nil {
			continue
		}
		rev := modelRev{Seq: seq}
		// 版本时间取文件的修改时间：快照落盘那一刻，比文档里的 updatedAt 准 ——
		// 后者是「模型最后一次保存」的时间，一次定版可能对应半小时前的保存。
		if info, err := e.Info(); err == nil {
			rev.At = info.ModTime().UTC().Format(time.RFC3339)
		}
		if b, err := os.ReadFile(filepath.Join(modelDir(root, id), name)); err == nil {
			var m modelDoc
			if json.Unmarshal(b, &m) == nil {
				rev.Tables = len(m.Tables)
				rev.Links = len(m.Links)
			}
		}
		// 备注单独存一行 `note` 字段会污染模型本体，所以放在同目录的
		// rev-0003.note 里 —— 一行文本，删版本时一起删。
		if b, err := os.ReadFile(strings.TrimSuffix(revPath(root, id, seq), ".json") + ".note"); err == nil {
			rev.Note = strings.TrimSpace(string(b))
		}
		out = append(out, rev)
	}
	// 新的在前
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

func nextRevSeq(root *db.DB, id string) int {
	revs := listRevisions(root, id)
	if len(revs) == 0 {
		return 1
	}
	return revs[0].Seq + 1
}

func saveRevision(root *db.DB, m modelDoc, note string) (modelRev, error) {
	seq := nextRevSeq(root, m.ID)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return modelRev{}, err
	}
	b = append(b, '\n')
	path := revPath(root, m.ID, seq)
	if err := os.MkdirAll(modelDir(root, m.ID), 0o755); err != nil {
		return modelRev{}, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return modelRev{}, err
	}
	if note = strings.TrimSpace(note); note != "" {
		if r := []rune(note); len(r) > 200 {
			note = string(r[:200])
		}
		_ = os.WriteFile(strings.TrimSuffix(path, ".json")+".note", []byte(note+"\n"), 0o644)
	}
	return modelRev{Seq: seq, At: nowStr(), Note: note, Tables: len(m.Tables), Links: len(m.Links)}, nil
}

func deleteRevision(root *db.DB, id string, seq int) error {
	err := os.Remove(revPath(root, id, seq))
	_ = os.Remove(strings.TrimSuffix(revPath(root, id, seq), ".json") + ".note")
	return err
}

// ---------- 从库导入 ----------

// importFromDB 把当前库的结构导成一份模型。布局来自这个库的 diagram.json
// （如果有），这样老用户的摆放不会丢。
func importFromDB(d *db.DB, name string) modelDoc {
	m := modelDoc{
		Version: modelDocVersion,
		ID:      newModelID(),
		Name:    name,
		Tables:  []modelTable{},
	}
	if m.Name == "" {
		m.Name = "默认模型"
	}

	byID := map[int64]string{}
	i := 0
	for _, t := range d.Tables() {
		if t.ID <= 0 {
			continue
		}
		i++
		mt := modelTable{ID: fmt.Sprintf("t%d", i), Name: t.Name, Comment: t.Comment}
		for _, c := range t.Columns {
			mt.Columns = append(mt.Columns, modelColumn{
				Name: c.Name, Type: c.Type, PK: c.PrimaryKey, NotNull: c.NotNull,
				Uniq: c.Unique, Def: c.Default, Comment: c.Comment,
			})
		}
		byID[t.ID] = mt.ID
		m.Tables = append(m.Tables, mt)
	}

	// 布局 / 分组 / 手画的关系：从老侧车搬过来，按表 ID 换算成模型表 ID。
	old := loadDiagram(d)
	for _, g := range old.Groups {
		m.Groups = append(m.Groups, g)
	}
	midByDBID := map[string]string{}
	for dbID, mid := range byID {
		midByDBID[strconv.FormatInt(dbID, 10)] = mid
	}
	idxByMID := map[string]int{}
	for i, t := range m.Tables {
		idxByMID[t.ID] = i
	}
	for k, c := range old.Tables {
		mid, ok := midByDBID[k]
		if !ok {
			continue
		}
		i, ok := idxByMID[mid]
		if !ok {
			continue
		}
		m.Tables[i].X = c.X
		m.Tables[i].Y = c.Y
		m.Tables[i].Group = c.Group
		m.Tables[i].Hidden = c.Hidden
	}
	for _, ln := range old.Links {
		from, ok1 := byID[ln.From.Table]
		to, ok2 := byID[ln.To.Table]
		if !ok1 || !ok2 {
			continue
		}
		m.Links = append(m.Links, modelLink{
			ID:   ln.ID,
			From: modelEndpoint{Table: from, Columns: ln.From.Columns},
			To:   modelEndpoint{Table: to, Columns: ln.To.Columns},
			Card: ln.Card,
		})
	}
	m.normalize()
	return m
}

// migrateLegacyDiagrams 把「按库的 diagram.json」变成一个模型，只做一次。
//
// 判定「做过」很简单：models 目录存在就不动。所以迁移是幂等的，也不需要
// 在别处记一个 migrated 标志 —— 那种标志和真实文件迟早会对不上。
func migrateLegacyDiagrams(root *db.DB) {
	if _, err := os.Stat(modelsRootDir(root)); err == nil {
		return
	}
	if _, err := os.Stat(diagramPath(root)); err != nil {
		return // 没摆过图，没什么可迁的
	}
	m := importFromDB(root, "默认模型")
	m.CreatedAt = nowStr()
	m.UpdatedAt = m.CreatedAt
	if err := saveModel(root, m); err != nil {
		return
	}
}

// ---------- HTTP ----------

func (s *Server) modelsRoot() *db.DB {
	if s.DBs != nil {
		if d := s.DBs.Default(); d != nil {
			return d
		}
	}
	return s.DB
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	root := s.modelsRoot()
	switch r.Method {
	case http.MethodGet:
		modelMu.Lock()
		migrateLegacyDiagrams(root)
		list := listModels(root)
		modelMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"models": list})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var req struct {
			Name   string `json:"name"`
			FromDB bool   `json:"fromDB"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		modelMu.Lock()
		defer modelMu.Unlock()

		var m modelDoc
		if req.FromDB {
			m = importFromDB(s.currentDB(r), req.Name)
		} else {
			m = modelDoc{Version: modelDocVersion, ID: newModelID(), Name: req.Name, Tables: []modelTable{}}
			m.normalize()
		}
		m.CreatedAt = nowStr()
		m.UpdatedAt = m.CreatedAt
		if err := saveModel(root, m); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, m)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleModel 处理 /api/models/{id}[/revisions[/{seq}[/restore]]]。
//
// 不用 Go 1.22 的方法路由（go.mod 还是 1.20），手工切路径。
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/models/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	if !validModelID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "模型 id 不合法"})
		return
	}
	root := s.modelsRoot()

	switch {
	case len(parts) == 1:
		s.handleOneModel(w, r, root, id)
	case len(parts) == 2 && parts[1] == "revisions":
		s.handleRevisions(w, r, root, id)
	case len(parts) == 3 && parts[1] == "revisions":
		s.handleOneRevision(w, r, root, id, parts[2])
	case len(parts) == 4 && parts[1] == "revisions" && parts[3] == "restore":
		s.handleRestoreRevision(w, r, root, id, parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleOneModel(w http.ResponseWriter, r *http.Request, root *db.DB, id string) {
	switch r.Method {
	case http.MethodGet:
		modelMu.Lock()
		m, err := loadModel(root, id)
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "模型不存在"})
			return
		}
		writeJSON(w, http.StatusOK, m)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, modelMaxBody)
		var m modelDoc
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析模型数据"})
			return
		}
		m.ID = id
		m.UpdatedAt = nowStr()
		m.normalize()

		modelMu.Lock()
		if m.CreatedAt == "" {
			if old, err := loadModel(root, id); err == nil {
				m.CreatedAt = old.CreatedAt
			}
		}
		err := saveModel(root, m)
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, m)

	case http.MethodDelete:
		modelMu.Lock()
		err := os.RemoveAll(modelDir(root, id))
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRevisions(w http.ResponseWriter, r *http.Request, root *db.DB, id string) {
	switch r.Method {
	case http.MethodGet:
		modelMu.Lock()
		revs := listRevisions(root, id)
		modelMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"revisions": revs})

	case http.MethodPost:
		// 定版：把**当前工作副本**整份复制成一个快照。
		//
		// 为什么不用请求体里的内容：定版的对象必须是「已经存下来的那份」，
		// 否则前端手里一份没保存的改动会被悄悄定版，和用户在画布上看到的不一致。
		var req struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)

		modelMu.Lock()
		m, err := loadModel(root, id)
		if err != nil {
			modelMu.Unlock()
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "模型不存在"})
			return
		}
		rev, err := saveRevision(root, m, req.Note)
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rev)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleOneRevision(w http.ResponseWriter, r *http.Request, root *db.DB, id, seqStr string) {
	seq, err := strconv.Atoi(seqStr)
	if err != nil || seq <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "版本号不合法"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		modelMu.Lock()
		b, err := os.ReadFile(revPath(root, id, seq))
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "版本不存在"})
			return
		}
		var m modelDoc
		if err := json.Unmarshal(b, &m); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "版本文件坏了"})
			return
		}
		m.ID = id
		m.normalize()
		writeJSON(w, http.StatusOK, m)

	case http.MethodDelete:
		modelMu.Lock()
		err := deleteRevision(root, id, seq)
		modelMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "版本不存在"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRestoreRevision(w http.ResponseWriter, r *http.Request, root *db.DB, id, seqStr string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	seq, err := strconv.Atoi(seqStr)
	if err != nil || seq <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "版本号不合法"})
		return
	}
	modelMu.Lock()
	defer modelMu.Unlock()
	b, err := os.ReadFile(revPath(root, id, seq))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "版本不存在"})
		return
	}
	var m modelDoc
	if err := json.Unmarshal(b, &m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "版本文件坏了"})
		return
	}
	m.ID = id
	m.Version = modelDocVersion
	m.UpdatedAt = nowStr()
	m.normalize()
	if err := saveModel(root, m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}
