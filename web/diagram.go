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

// 关系图的侧车文件：<数据目录>/diagram.json。
//
// 为什么用侧车文件而不是建一张表：
//   - 它是「图」的元数据，不是业务数据。混进 catalog 会让 SHOW TABLES、
//     备份、空间统计里多出一张用户没建过的表；
//   - 布局坏了最多是图难看，不该影响数据库能不能用 —— 独立文件可以
//     直接删掉重来，也不用进 WAL。
//
// 卡片和分组的成员都用**表 ID** 做键，不用表名：RENAME 之后布局要跟着
// 那张表走，颜色和分组名也得留着。ID 只增不复用（见 catalog.Table.ID）。
const diagramFile = "diagram.json"

const (
	diagramVersion = 1
	// 画布坐标的合理上限。超过这个值的不是图，是坏数据 —— 一个 1e300
	// 的坐标会让整个 canvas 撑到浏览器算不出来的尺寸，表现成「打开就白屏」。
	diagramMaxCoord = 100000
	// 请求体上限：几十张表的坐标也就几 KB，1MB 已经是三个数量级的富余。
	diagramMaxBody = 1 << 20
	// 分组名最长多少字符（按 rune 算）。够写「订单与结算」这类域名字了。
	diagramMaxName = 40
)

type diagramCard struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type diagramGroup struct {
	ID     string  `json:"id"`
	Name   string  `json:"name,omitempty"`
	Color  int     `json:"color"` // 0..7 是固定色相；-1 表示「跟随自动配色」
	Tables []int64 `json:"tables"`
}

type diagramDoc struct {
	Version int                    `json:"version"`
	Cards   map[string]diagramCard `json:"cards,omitempty"`
	Groups  []diagramGroup         `json:"groups,omitempty"`
}

// diagramMu 让「读 → 剪枝 → 写回」这一串不互相踩。
// 一把全局锁就够：文件是每库一份，但写一次只有几十 KB，没必要按库分锁。
var diagramMu sync.Mutex

func validCoord(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= diagramMaxCoord
}

// normalize 清洗客户端传上来的文档。
//
// 前端是本项目自己的代码，但侧车文件同时是「可以手工编辑的」设计稿元数据，
// 而且它直接决定 DOM 的 left/top —— 脏数据进来会表现成卡片飞到屏幕外，
// 比报错还难查。所以进门先归一化，不信任何一条坐标。
func (doc *diagramDoc) normalize() {
	if doc.Cards == nil {
		doc.Cards = map[string]diagramCard{}
	}
	for k, c := range doc.Cards {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || id <= 0 || !validCoord(c.X) || !validCoord(c.Y) {
			delete(doc.Cards, k)
			continue
		}
		doc.Cards[k] = diagramCard{X: math.Round(c.X), Y: math.Round(c.Y)}
	}

	seenGroup := map[string]bool{}
	out := doc.Groups[:0]
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
		kept := g.Tables[:0]
		for _, id := range g.Tables {
			if id > 0 {
				kept = append(kept, id)
			}
		}
		g.Tables = kept
		if len(g.Tables) == 0 {
			continue
		}
		out = append(out, g)
	}
	doc.Groups = out
}

// prune 把已经不存在的表从布局里摘掉，返回是否有改动（调用方据此决定要不要写回）。
//
// 这是「生命周期剪枝」的落点：DROP TABLE 之后没人会去通知这张图，等下次
// 打开关系图时按当前 catalog 对一遍账最省事。RENAME 不会命中这里 —— ID 没变。
func (doc *diagramDoc) prune(names map[int64]bool) bool {
	changed := false
	for k := range doc.Cards {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || !names[id] {
			delete(doc.Cards, k)
			changed = true
		}
	}
	seen := map[int64]bool{}
	out := doc.Groups[:0]
	for _, g := range doc.Groups {
		kept := g.Tables[:0]
		for _, id := range g.Tables {
			// 同一张表不能被两个组同时认领：手工编辑过的文件里很容易出现，
			// 前端按「先到先得」处理，这里也按同一个规则裁掉后面的。
			if !names[id] || seen[id] {
				changed = true
				continue
			}
			seen[id] = true
			kept = append(kept, id)
		}
		g.Tables = kept
		if len(g.Tables) == 0 {
			changed = true
			continue
		}
		out = append(out, g)
	}
	doc.Groups = out
	return changed
}

func tableIDs(d *db.DB) map[int64]bool {
	tables := d.Tables()
	ids := make(map[int64]bool, len(tables))
	for _, t := range tables {
		if t.ID > 0 {
			ids[t.ID] = true
		}
	}
	return ids
}

func diagramPath(d *db.DB) string {
	return filepath.Join(d.Dir(), diagramFile)
}

// loadDiagram 读侧车文件并顺手对账。
// 文件不存在/坏掉都返回空文档：关系图本身是「锦上添花」的视图，
// 不能让一个坏 JSON 把整张图变成打不开的错误页。
func loadDiagram(d *db.DB) diagramDoc {
	doc := diagramDoc{Version: diagramVersion, Cards: map[string]diagramCard{}}
	if b, err := os.ReadFile(diagramPath(d)); err == nil {
		var got diagramDoc
		if json.Unmarshal(b, &got) == nil {
			got.Version = diagramVersion
			got.normalize()
			doc = got
		}
	}
	if doc.prune(tableIDs(d)) {
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

// handleDiagram 是关系图布局的读写接口。
//   GET  /api/diagram → 当前库的布局（已按当前表剪枝）
//   POST /api/diagram → 整份覆盖
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
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法解析关系图数据"})
			return
		}
		doc.Version = diagramVersion
		doc.normalize()
		doc.prune(tableIDs(d))
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
