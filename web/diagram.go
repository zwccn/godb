package web

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"godb/db"
)

// 模型图的侧车文件：<数据目录>/diagram.json。
//
// 这里存的是**模型**，不是库的镜像。
//
// 逻辑图是源头：一张表归哪个域、哪两列之间是 1-N 还是 M-N，全由用户定。
// 库里的外键在这份文件里没有位置 —— 它落到图上属于物理层，是第二阶段的事。
// 所以这里没有「外键边」「基数标注」「被否认的约束」这些概念，只有四样东西：
// 分组、坐标、表在不在图上、以及用户画的端点对端点关系。
//
// 为什么不把外键也记进来「方便同步」：一旦这张图能自动长出线，它就不再是
// 模型而是快照了 —— 用户删掉一条线，下次打开又回来，那这张图谁也不敢改。
//
// 表一律用**表 ID** 做键，不用表名：RENAME 之后布局和归属要跟着那张表走。
// ID 只增不复用（见 catalog.Table.ID）。
const diagramFile = "diagram.json"

const (
	// v1「组拥有表 + 按外键自动分组」→ v2「表拥有组 + 手画关系 + 外键边」
	// → v3「关系一律端点对端点，去掉一切从外键推导出来的东西」。
	// 读旧版本自动迁移，不需要用户做任何事。
	diagramVersion = 3

	// 画布坐标的合理上限。超过这个值的不是图，是坏数据 —— 一个 1e300
	// 的坐标会让整个 canvas 撑到浏览器算不出来的尺寸，表现成「打开就白屏」。
	diagramMaxCoord = 100000
	// 请求体上限：坐标加关系也就几十 KB，1MB 已经两个数量级富余。
	diagramMaxBody = 1 << 20
	// 分组名最长多少字符（按 rune 算）。够写「订单与结算」这类域名字了。
	diagramMaxName = 40
	// 关系条数上限。手工编辑的文件塞几万条会把前端画到卡死，
	// 一张模型图也放不下这么多线。
	diagramMaxLinks = 2000
)

// 基数取值。只认这三种：前端和下发的走同一份定义，写错的一律退回 1N
// （一对多视觉上最保守，不会让人以为唯一）。
const (
	card1to1 = "11"
	card1toN = "1N"
	cardMtoN = "NM"
)

func validCard(v string) bool {
	return v == card1to1 || v == card1toN || v == cardMtoN
}

// diagramCard 是一张表在图上的状态。Group 为空 = 未分组。
//
// 坐标用指针：表可以被「移到某个分组」但还没被拖过，这时它有归属没有位置，
// 前端按自动布局摆。用 0 表示「没位置」不行 —— 那是一个合法的坐标，
// 会把卡片摞在画布左上角。
type diagramCard struct {
	Group string   `json:"group,omitempty"`
	X     *float64 `json:"x,omitempty"`
	Y     *float64 `json:"y,omitempty"`
	// Hidden 是「用户把这张表从图上收起了」。
	//
	// 为什么记「收起」而不是「加入」：表本身只能来自库，默认全在图上，
	// 否则第一次打开是一张空白画布，用户不知道该干什么。收起是个明确的
	// 减动作，语义比「加入」清楚。
	//
	// 记录要留着：表被 DROP 掉之后这条记录会跟着剪掉，但用户重新建一张
	// 同名的表时，它是一张新 ID 的表，本来就该重新出现。
	Hidden bool `json:"hidden,omitempty"`
}

func roundPtr(v float64) *float64 {
	r := math.Round(v)
	return &r
}

type diagramGroup struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Color int    `json:"color"` // 0..7 是固定色相；-1 表示「按顺序取」
}

// diagramEndpoint 是关系的一端：哪张表、哪几列。
//
// 用列名而不是列序号：ALTER TABLE 之后序号会漂，列名至少还能对上；
// 列被删掉时这条关系随之失效，那是应该失效的 —— 它指向的东西已经不存在。
type diagramEndpoint struct {
	Table   int64    `json:"table"`
	Columns []string `json:"columns"`
}

// diagramLink 是用户画的一条关系，两个端点都落在字段上。
//
// 没有实线/虚线之分。这条线不声称库里有对应约束，它只声称「我理解的模型
// 是这样」；反过来，库里有的外键也不保证出现在图上 —— 那是用户的选择。
//
// Card 描述**从 From 看向 To** 的基数：'1N' = From 端一行、To 端多行。
// 「一对多」和「多对一」因此是同一个存储值，但方向没有丢 —— 将来生成外键时
// To 端是子表，方向得留得住。
type diagramLink struct {
	ID   string          `json:"id"`
	From diagramEndpoint `json:"from"`
	To   diagramEndpoint `json:"to"`
	Card string          `json:"card"`
}

type diagramDoc struct {
	Version int                    `json:"version"`
	Groups  []diagramGroup         `json:"groups,omitempty"`
	Tables  map[string]diagramCard `json:"tables,omitempty"`
	Links   []diagramLink          `json:"links,omitempty"`
}

// diagramMu 让「读 → 剪枝 → 写回」这一串不互相踩。
// 一把全局锁就够：文件是每库一份，但写一次只有几十 KB，没必要按库分锁。
var diagramMu sync.Mutex

func validCoord(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= diagramMaxCoord
}

// diagramLinkV2 是 v2 的边形状：child 指向 parent，列数组分别挂在两端。
//
// 它读进来只为一件事：把用户手画过的关系搬到 v3 的端点结构上。
// v2 里从外键推导出来的那些边不在这份数据里（它们是每次渲染时现算的），
// 所以迁移不会把「库的结构」偷偷塞进模型。
type diagramLinkV2 struct {
	ID         string   `json:"id"`
	Child      int64    `json:"child"`
	Columns    []string `json:"columns"`
	Parent     int64    `json:"parent"`
	RefColumns []string `json:"refColumns"`
	Card       string   `json:"card"`
}

// toV3 把「child 指向 parent」翻成「From 看向 To」。
//
// 两种形状对 card 的理解正好相反：v2 的 '1N' 说的是「子端多」，
// v3 的 '1N' 说的是「From 端一」。所以 1:N 这一支要把父表放到 From，
// 否则迁完之后每条一对多都会画反。
func (v2 diagramLinkV2) toV3() diagramLink {
	if v2.Card == card1toN {
		return diagramLink{
			ID:   v2.ID,
			From: diagramEndpoint{Table: v2.Parent, Columns: v2.RefColumns},
			To:   diagramEndpoint{Table: v2.Child, Columns: v2.Columns},
			Card: card1toN,
		}
	}
	// 1:1 和 M:N 两端对称，方向随便定，保持 v2 的子→父更接近用户当年的画法。
	return diagramLink{
		ID:   v2.ID,
		From: diagramEndpoint{Table: v2.Child, Columns: v2.Columns},
		To:   diagramEndpoint{Table: v2.Parent, Columns: v2.RefColumns},
		Card: v2.Card,
	}
}

// diagramRaw 是磁盘上的原始形状，同时兼容 v1 / v2 / v3。
//
// 用一个 union 结构一次解完，比先探版本号再解三遍省事，也少一处漏字段的机会。
type diagramRaw struct {
	Version int                    `json:"version"`
	Tables  map[string]diagramCard `json:"tables,omitempty"`
	Groups  []diagramGroupRaw      `json:"groups,omitempty"`

	// links 在 v2 和 v3 里同名不同形，先当原始字节收着，再由 decodeLinks
	// 按形状分派 —— 同一个 json 键没法挂两个 Go 字段。
	Links json.RawMessage `json:"links,omitempty"`

	// 以下只有 v1 才有
	Cards map[string]struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"cards,omitempty"`
}

type diagramGroupRaw struct {
	ID     string  `json:"id"`
	Name   string  `json:"name,omitempty"`
	Color  int     `json:"color"`
	Tables []int64 `json:"tables,omitempty"` // v1：组里的成员
}

// decodeLinks 认两种边形状。
//
// 不看 version 字段而看形状，是因为手工编辑过的文件版本号常常没改；
// 而「端点有没有表」这件事骗不了人。
func decodeLinks(raw json.RawMessage) []diagramLink {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var probe []map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}

	out := make([]diagramLink, 0, len(probe))
	for _, m := range probe {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		if _, ok := m["from"]; ok {
			var ln diagramLink
			if json.Unmarshal(b, &ln) == nil {
				out = append(out, ln)
			}
			continue
		}
		var v2 diagramLinkV2
		if json.Unmarshal(b, &v2) == nil {
			out = append(out, v2.toV3())
		}
	}
	return out
}

// toDoc 把原始形状整成 v3。返回是否发生了迁移（迁移过就要写回，
// 否则每次打开都要重新折腾一遍旧格式）。
func (raw *diagramRaw) toDoc() (diagramDoc, bool) {
	doc := diagramDoc{
		Version: diagramVersion,
		Tables:  map[string]diagramCard{},
		Links:   decodeLinks(raw.Links),
	}
	migrated := raw.Version < diagramVersion

	for _, g := range raw.Groups {
		doc.Groups = append(doc.Groups, diagramGroup{ID: g.ID, Name: g.Name, Color: g.Color})
	}

	for k, c := range raw.Tables {
		doc.Tables[k] = c
	}
	// v1 的坐标字段叫 cards，形状和 v3 的 tables 一样，只是没有 group。
	for k, c := range raw.Cards {
		if _, ok := doc.Tables[k]; ok {
			continue
		}
		doc.Tables[k] = diagramCard{X: roundPtr(c.X), Y: roundPtr(c.Y)}
		migrated = true
	}
	// v1 的"组拥有表"铺平成"表拥有组"。同一张表被两组认领时先到先得 ——
	// 前端当年也是这么画的，迁移必须和当年的显示一致，否则用户会发现
	// 一张表突然换了组。
	for _, g := range raw.Groups {
		if len(g.Tables) == 0 {
			continue
		}
		migrated = true
		for _, id := range g.Tables {
			if id <= 0 {
				continue
			}
			k := strconv.FormatInt(id, 10)
			c := doc.Tables[k]
			if c.Group != "" {
				continue
			}
			c.Group = g.ID
			doc.Tables[k] = c
		}
	}
	return doc, migrated
}

// normalize 清洗客户端传上来的文档。
//
// 前端是本项目自己的代码，但侧车文件同时是「可以手工编辑的」设计稿元数据，
// 而且它直接决定 DOM 的 left/top —— 脏数据进来会表现成卡片飞到屏幕外，
// 比报错还难查。所以进门先归一化，不信任何一条坐标。
func (doc *diagramDoc) normalize() {
	if doc.Tables == nil {
		doc.Tables = map[string]diagramCard{}
	}

	seenGroup := map[string]bool{}
	groups := doc.Groups[:0]
	for _, g := range doc.Groups {
		g.ID = strings.TrimSpace(g.ID)
		g.Name = strings.TrimSpace(g.Name)
		// 空 id 或重复 id 的组没法被前端引用，直接丢掉而不是顺手改一个 ——
		// 改了 id 前端手里的旧引用就全对不上了。
		if g.ID == "" || seenGroup[g.ID] {
			continue
		}
		seenGroup[g.ID] = true
		if g.Color < -1 || g.Color > 7 {
			g.Color = -1
		}
		if r := []rune(g.Name); len(r) > diagramMaxName {
			g.Name = string(r[:diagramMaxName])
		}
		groups = append(groups, g)
	}
	doc.Groups = groups

	for k, c := range doc.Tables {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || id <= 0 {
			delete(doc.Tables, k)
			continue
		}
		// 半个坐标没法用：只有 x 或只有 y 时两个都丢掉，让它回到自动布局，
		// 而不是留在一半的位置上。
		if c.X != nil && !validCoord(*c.X) {
			c.X = nil
		}
		if c.Y != nil && !validCoord(*c.Y) {
			c.Y = nil
		}
		if (c.X == nil) != (c.Y == nil) {
			c.X, c.Y = nil, nil
		} else if c.X != nil {
			c.X, c.Y = roundPtr(*c.X), roundPtr(*c.Y)
		}
		// 指向不存在的组一律当成未分组，而不是顺手建一个空组：
		// 分组是用户显式建的，凭空冒出来的组没人认领。
		if c.Group != "" && !seenGroup[c.Group] {
			c.Group = ""
		}
		doc.Tables[k] = diagramCard{Group: c.Group, X: c.X, Y: c.Y, Hidden: c.Hidden}
	}

	seenLink := map[string]bool{}
	links := doc.Links[:0]
	for _, ln := range doc.Links {
		ln.ID = strings.TrimSpace(ln.ID)
		if ln.ID == "" || seenLink[ln.ID] {
			continue
		}
		from := cleanEndpoint(ln.From)
		to := cleanEndpoint(ln.To)
		if from == nil || to == nil {
			continue
		}
		// 同一张表的同一列自己连自己，什么也表达不了。
		// 同表不同列是合法的：employees.manager_id → employees.id 就是它。
		if from.Table == to.Table && sameCols(from.Columns, to.Columns) {
			continue
		}
		// 两端列数必须相等：一列不可能对上两列，那是两条关系被写进了一条。
		if len(from.Columns) != len(to.Columns) {
			continue
		}
		if !validCard(ln.Card) {
			ln.Card = card1toN
		}
		ln.From, ln.To = *from, *to
		seenLink[ln.ID] = true
		links = append(links, ln)
		if len(links) >= diagramMaxLinks {
			break
		}
	}
	doc.Links = links
	if len(doc.Links) == 0 {
		doc.Links = nil
	}
}

func cleanEndpoint(e diagramEndpoint) *diagramEndpoint {
	if e.Table <= 0 || len(e.Columns) == 0 {
		return nil
	}
	cols := trimAll(e.Columns)
	if hasEmpty(cols) {
		return nil
	}
	return &diagramEndpoint{Table: e.Table, Columns: cols}
}

func sameCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func trimAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

func hasEmpty(in []string) bool {
	for _, s := range in {
		if s == "" {
			return true
		}
	}
	return false
}

// prune 把已经不存在的表、列和关系摘掉，返回是否有改动
// （调用方据此决定要不要写回）。
//
// 这是「生命周期剪枝」的落点：DROP TABLE / DROP COLUMN 之后没人会去通知
// 这张图，等下次打开时按当前 catalog 对一遍账最省事。RENAME 不会命中这里
// —— 表和列的名字都跟着 catalog 变，ID 和「哪一列」都还在。
//
// 分组本身**不因为空了就删**：用户建了组还没放表，那也是他的设计意图，
// 顺手删掉会让"新建分组"看起来毫无反应。
func (doc *diagramDoc) prune(cols map[int64]map[string]bool) bool {
	changed := false
	for k := range doc.Tables {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			delete(doc.Tables, k)
			changed = true
			continue
		}
		if _, ok := cols[id]; !ok {
			delete(doc.Tables, k)
			changed = true
		}
	}

	seen := map[string]bool{}
	links := doc.Links[:0]
	for _, ln := range doc.Links {
		if seen[ln.ID] || !endpointAlive(ln.From, cols) || !endpointAlive(ln.To, cols) {
			changed = true
			continue
		}
		seen[ln.ID] = true
		links = append(links, ln)
	}
	doc.Links = links
	if len(doc.Links) == 0 {
		doc.Links = nil
	}
	return changed
}

// endpointAlive 判断关系的一端还指不指得着实物。
//
// 列名大小写不敏感：PG 会把未加引号的标识符折成小写，用户手工改文件时
// 写成 CUSTOMER_ID 也不该让整条关系消失。
func endpointAlive(e diagramEndpoint, cols map[int64]map[string]bool) bool {
	have, ok := cols[e.Table]
	if !ok {
		return false
	}
	for _, c := range e.Columns {
		if !have[strings.ToLower(c)] {
			return false
		}
	}
	return true
}

// tableIndex 把「表 ID → 列名（小写）」做成一张对照表，供剪枝对账。
func tableIndex(d *db.DB) map[int64]map[string]bool {
	tables := d.Tables()
	out := make(map[int64]map[string]bool, len(tables))
	for _, t := range tables {
		if t.ID <= 0 {
			continue
		}
		set := make(map[string]bool, len(t.Columns))
		for _, c := range t.Columns {
			set[strings.ToLower(c.Name)] = true
		}
		out[t.ID] = set
	}
	return out
}

func diagramPath(d *db.DB) string {
	return filepath.Join(d.Dir(), diagramFile)
}

// loadDiagram 读侧车文件并顺手对账。
// 文件不存在/坏掉都返回空文档：模型图本身是「锦上添花」的视图，
// 不能让一个坏 JSON 把整张图变成打不开的错误页。
func loadDiagram(d *db.DB) diagramDoc {
	doc := diagramDoc{Version: diagramVersion, Tables: map[string]diagramCard{}}
	migrated := false
	if b, err := os.ReadFile(diagramPath(d)); err == nil {
		var raw diagramRaw
		if json.Unmarshal(b, &raw) == nil {
			doc, migrated = raw.toDoc()
			doc.normalize()
		}
	}
	if doc.prune(tableIndex(d)) {
		migrated = true
	}
	if migrated {
		_ = saveDiagram(d, doc)
	}
	return doc
}

// saveDiagram 走 tmp + fsync + rename，和配置文件同一套写法：
// 半截文件读回来是坏 JSON，表现为「布局突然全丢了」。
func saveDiagram(d *db.DB, doc diagramDoc) error {
	doc.Version = diagramVersion
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := diagramPath(d)
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
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// handleDiagram 是模型图元数据的读写接口。
//
//	GET  /api/diagram → 当前库的布局（已按当前表剪枝）
//	POST /api/diagram → 整份覆盖
//
// 整份覆盖而不是增量：一张图的坐标是相互关联的（拖动一张卡片时其它卡片
// 的相对位置才是「布局」），增量更新反而要处理更多冲突。
func (s *Server) handleDiagram(w http.ResponseWriter, r *http.Request) {
	d := s.currentDB(r)
	switch r.Method {
	case http.MethodGet:
		diagramMu.Lock()
		doc := loadDiagram(d)
		diagramMu.Unlock()
		writeJSON(w, http.StatusOK, doc)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, diagramMaxBody)
		var doc diagramDoc
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析模型图数据"})
			return
		}
		doc.Version = diagramVersion
		doc.normalize()
		doc.prune(tableIndex(d))
		diagramMu.Lock()
		err := saveDiagram(d, doc)
		diagramMu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, doc)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
