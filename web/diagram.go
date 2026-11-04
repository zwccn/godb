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
// 为什么用侧车文件而不是建一张表：
//   - 它是「图」的元数据，不是业务数据。混进 catalog 会让 SHOW TABLES、
//     备份、空间统计里多出一张用户没建过的表；
//   - 布局坏了最多是图难看，不该影响数据库能不能用 —— 独立文件可以
//     直接删掉重来，也不用进 WAL。
//
// 这份文件存三类东西，性质不同，必须分得清：
//   - **布局**（坐标、分组）：纯视觉，和库对不对得上都无所谓；
//   - **逻辑关系**（links）与**基数标注**（overrides）：是用户对业务的判断，
//     库里可能没有对应约束。前端靠实线/虚线把它们和外键区分开，
//     绝不能混成一份「看起来像 schema」的数据；
//   - **隐藏的边**（hidden）：同样是判断，方向相反 —— 用户说这条关系在他的
//     模型里不成立。只作用于逻辑视图。
//
// 表一律用**表 ID** 做键，不用表名：RENAME 之后布局和归属要跟着那张表走。
// ID 只增不复用（见 catalog.Table.ID）。
const diagramFile = "diagram.json"

const (
	// v1 是「组拥有表 + 外键自动分组」，v2 反转成「表拥有组」，并加了
	// 逻辑关系和基数标注。读 v1 会自动迁移，不需要用户做任何事。
	diagramVersion = 2

	// 画布坐标的合理上限。超过这个值的不是图，是坏数据 —— 一个 1e300
	// 的坐标会让整个 canvas 撑到浏览器算不出来的尺寸，表现成「打开就白屏」。
	diagramMaxCoord = 100000
	// 请求体上限：坐标加关系也就几十 KB，1MB 已经两个数量级富余。
	diagramMaxBody = 1 << 20
	// 分组名最长多少字符（按 rune 算）。够写「订单与结算」这类域名字了。
	diagramMaxName = 40
	// 逻辑关系条数上限。手工编辑的文件塞几万条会把前端画到卡死，
	// 一张模型图也放不下这么多线。
	diagramMaxLinks = 2000
)

// 基数取值。只认这三种：前端和下发的走同一份定义，写错的一律退回 1N
// （多对一视觉上最保守，不会让人以为唯一）。
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

// diagramLink 是用户手画的一条关系：库里没有对应外键。
// 前端用虚线画，和实线的外键边区分开 —— 两种真相混在一起就没人敢信这张图了。
type diagramLink struct {
	ID         string   `json:"id"`
	Child      int64    `json:"child"`
	Columns    []string `json:"columns"`
	Parent     int64    `json:"parent"`
	RefColumns []string `json:"refColumns"`
	Card       string   `json:"card"`
}

type diagramDoc struct {
	Version int                    `json:"version"`
	Groups  []diagramGroup         `json:"groups,omitempty"`
	Tables  map[string]diagramCard `json:"tables,omitempty"`
	Links   []diagramLink          `json:"links,omitempty"`
	// Overrides 是「用户改过的基数」，键是外键边的稳定标识
	// fk|<子表ID>|<列名,列名>，值是三种基数之一。
	//
	// 为什么不直接改 schema：改约束是数据变更，得有 WAL、锁、校验、
	// 失败回滚；而「我认为这里业务上是 1-1」只是个标注。把标注写进
	// 侧车，改错了删掉文件就恢复，也不会有半条约束留在库里。
	Overrides map[string]string `json:"overrides,omitempty"`
	// Hidden 是「用户说这条关系在我的模型里不成立」的边。
	//
	// 键和边本身同一套拼法：fk|<子表ID>|<列名,列名> 或 j|<关联表ID>。
	// 只影响**逻辑视图画不画这条线**，不动库 —— 物理视图照实画，那里问的
	// 是「库里有什么」，用户怎么看不算数。
	//
	// 为什么逻辑层必须能删边：模型层的判断是「这两张表业务上有没有关系」，
	// 和外键存不存在是两句话。不许否认，就是让约束反过来管住模型。
	Hidden []string `json:"hidden,omitempty"`
}

// diagramMu 让「读 → 剪枝 → 写回」这一串不互相踩。
// 一把全局锁就够：文件是每库一份，但写一次只有几十 KB，没必要按库分锁。
var diagramMu sync.Mutex

func validCoord(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= diagramMaxCoord
}

// fkEdgeKey 是外键边的稳定标识：子表 ID + 列名。
//
// 不用外键名：名字可能由引擎自动生成，也可能被 ALTER 掉，而「哪几列指向
// 谁」才是这条边本身。RENAME COLUMN 会让它变，那时标注失效、退回推导值，
// 这是可接受的 —— 列都改了名，标注本来就该重新看一眼。
func fkEdgeKey(childID int64, cols []string) string {
	return "fk|" + strconv.FormatInt(childID, 10) + "|" + strings.Join(cols, ",")
}

// 模型图里「被隐藏的边」用同一套键：外键边用 fk|…，折叠出来的多对多用 j|<关联表ID>。
// 手工画的逻辑关系不进这里 —— 它本来就能直接删掉，没有「隐藏」这层。
func validHiddenKey(k string) bool {
	if strings.HasPrefix(k, "fk|") {
		return true
	}
	if strings.HasPrefix(k, "j|") {
		id, err := strconv.ParseInt(k[2:], 10, 64)
		return err == nil && id > 0
	}
	return false
}

// hiddenKeyAlive 判断一条隐藏记录还指不指得着实物。
//
// fk|… 要求这条外键还在（DROP / RENAME COLUMN 之后它就不在了，标注该失效）；
// j|… 只能验到表还在 —— 折叠是前端按外键算出来的，服务端不重复实现一遍
// 「四条件」。宁可在那边多一句判断，也不要两处逻辑各写一份、慢慢跑偏。
func hiddenKeyAlive(k string, ids map[int64]bool, fkKeys map[string]bool) bool {
	if strings.HasPrefix(k, "fk|") {
		return fkKeys[k]
	}
	id, err := strconv.ParseInt(k[2:], 10, 64)
	return err == nil && ids[id]
}

// diagramRaw 是磁盘上的原始形状，同时兼容 v1 和 v2。
//
// v1 用 cards（坐标）+ groups[].tables（组拥有表）；v2 用 tables（表拥有组）。
// 用一个 union 结构一次解完，比先探版本号再解两遍省事，也少一处漏字段的机会。
type diagramRaw struct {
	Version   int                    `json:"version"`
	Tables    map[string]diagramCard `json:"tables,omitempty"`
	Groups    []diagramGroupRaw      `json:"groups,omitempty"`
	Links     []diagramLink          `json:"links,omitempty"`
	Overrides map[string]string      `json:"overrides,omitempty"`
	Hidden    []string               `json:"hidden,omitempty"`

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

// toDoc 把原始形状整成 v2。返回是否发生了迁移（迁移过就要写回，
// 否则每次打开都要重新折腾一遍旧格式）。
func (raw *diagramRaw) toDoc() (diagramDoc, bool) {
	doc := diagramDoc{
		Version:   diagramVersion,
		Tables:    map[string]diagramCard{},
		Links:     raw.Links,
		Overrides: raw.Overrides,
		Hidden:    raw.Hidden,
	}
	migrated := raw.Version < diagramVersion

	for _, g := range raw.Groups {
		doc.Groups = append(doc.Groups, diagramGroup{ID: g.ID, Name: g.Name, Color: g.Color})
	}

	for k, c := range raw.Tables {
		doc.Tables[k] = c
	}
	// v1 的坐标字段叫 cards，形状和 v2 的 tables 一样，只是没有 group。
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
		doc.Tables[k] = diagramCard{Group: c.Group, X: c.X, Y: c.Y}
	}

	seenLink := map[string]bool{}
	links := doc.Links[:0]
	for _, ln := range doc.Links {
		ln.ID = strings.TrimSpace(ln.ID)
		if ln.ID == "" || seenLink[ln.ID] || ln.Child <= 0 || ln.Parent <= 0 {
			continue
		}
		if len(ln.Columns) == 0 || len(ln.Columns) != len(ln.RefColumns) {
			continue
		}
		ln.Columns = trimAll(ln.Columns)
		ln.RefColumns = trimAll(ln.RefColumns)
		if hasEmpty(ln.Columns) || hasEmpty(ln.RefColumns) {
			continue
		}
		if !validCard(ln.Card) {
			ln.Card = card1toN
		}
		seenLink[ln.ID] = true
		links = append(links, ln)
		if len(links) >= diagramMaxLinks {
			break
		}
	}
	doc.Links = links

	for k, v := range doc.Overrides {
		// 键必须是我们认得的形状，否则前端查不到、留着只会越积越多。
		if !strings.HasPrefix(k, "fk|") || !validCard(v) {
			delete(doc.Overrides, k)
		}
	}
	if len(doc.Overrides) == 0 {
		doc.Overrides = nil
	}

	seenHide := map[string]bool{}
	hid := doc.Hidden[:0]
	for _, k := range doc.Hidden {
		k = strings.TrimSpace(k)
		// 形状不认得的键前端永远查不到，留着只会越积越多。
		if k == "" || seenHide[k] || !validHiddenKey(k) {
			continue
		}
		seenHide[k] = true
		hid = append(hid, k)
		if len(hid) >= diagramMaxLinks {
			break
		}
	}
	doc.Hidden = hid
	if len(doc.Hidden) == 0 {
		doc.Hidden = nil
	}
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

// prune 把已经不存在的表从布局里摘掉，返回是否有改动（调用方据此决定要不要写回）。
//
// 这是「生命周期剪枝」的落点：DROP TABLE 之后没人会去通知这张图，等下次
// 打开时按当前 catalog 对一遍账最省事。RENAME 不会命中这里 —— ID 没变。
//
// 分组本身**不因为空了就删**：用户建了组还没放表，那也是他的设计意图，
// 顺手删掉会让"新建分组"看起来毫无反应。
func (doc *diagramDoc) prune(ids map[int64]bool, fkKeys map[string]bool) bool {
	changed := false
	for k := range doc.Tables {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || !ids[id] {
			delete(doc.Tables, k)
			changed = true
		}
	}

	seen := map[string]bool{}
	links := doc.Links[:0]
	for _, ln := range doc.Links {
		if !ids[ln.Child] || !ids[ln.Parent] || seen[ln.ID] {
			changed = true
			continue
		}
		seen[ln.ID] = true
		links = append(links, ln)
	}
	doc.Links = links

	for k := range doc.Overrides {
		// 外键可能已被 DROP 或 RENAME COLUMN 改掉，标注跟着失效 ——
		// 留着一个指向不存在约束的标注，下次它可能恰好和另一条边同名。
		if !fkKeys[k] {
			delete(doc.Overrides, k)
			changed = true
		}
	}
	if len(doc.Overrides) == 0 {
		doc.Overrides = nil
	}

	hid := doc.Hidden[:0]
	for _, k := range doc.Hidden {
		if !hiddenKeyAlive(k, ids, fkKeys) {
			changed = true
			continue
		}
		hid = append(hid, k)
	}
	doc.Hidden = hid
	if len(doc.Hidden) == 0 {
		doc.Hidden = nil
	}
	return changed
}

// tableIndex 把「表 ID」和「外键边的键」各做成一个集合，供剪枝对账。
func tableIndex(d *db.DB) (map[int64]bool, map[string]bool) {
	tables := d.Tables()
	ids := make(map[int64]bool, len(tables))
	fks := map[string]bool{}
	for _, t := range tables {
		if t.ID > 0 {
			ids[t.ID] = true
		}
		for _, fk := range t.ForeignKeys {
			fks[fkEdgeKey(t.ID, fk.Columns)] = true
		}
	}
	return ids, fks
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
	ids, fks := tableIndex(d)
	if doc.prune(ids, fks) {
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
		ids, fks := tableIndex(d)
		doc.prune(ids, fks)
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
