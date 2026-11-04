# godb 向量能力 Phase 1 落地设计

> 配套文档：`godb 向量设计.md`（总体方案）。这一份只讲 Phase 1 怎么落地 ——
> 哪几个接口定死、文件长什么样、崩溃在哪一步会丢什么、任务怎么切。
>
> 结论先放前面：**Phase 1 不新增查询算子**。`sortOp` 已经有 Top-N 堆（`topNHint`），
> 向量检索只需要「一个能返回 float 距离的表达式 + 一个能按 vecID 顺序读文件的存储」。
> 工作量集中在存储和类型系统，不在执行器。

## 0. 范围

Phase 1 交付：

- `VECTOR(n)` 列类型（n ∈ [1, 4096]），建表 / 增删列 / 显示；
- 三种距离：`<->` L2、`<=>` 余弦、`<#>` 负内积（与 pgvector 同语义）；
- `ORDER BY 距离 LIMIT k` 的**精确** Top-K（全表扫描 + Top-N 堆）；
- 标量过滤 + 向量排序（先过滤再算距离）；
- 向量随事务提交、崩溃后由 WAL + `.vec` 文件恢复；
- 备份 / 回收站 / `-restore-trash` 带上向量文件。

Phase 1 不做：IVF / HNSW 索引、量化、多向量列联合索引、PG 自定义类型 OID、
`<->` 之外的向量函数（`l2_normalize`、`subvector` 等）。

---

## 1. 四个定死的决策

### 决策 1：`Value` 只存引用，不存数据

```
Value{K: KVector, I: vecID, z: dim}
```

- `I` 是定长文件里的槽位号（int64，永不复用给别的行，删除后进回收位图）；
- `z` 原本是 time 的时区分钟 / interval 的月数，这里复用为维度（int16，够 4096）；
- **`S` 不指向向量数据**。曾经考虑用 `unsafe.String` 指向 `.vec` 内存块省一次拷贝，
  但底层数组一旦扩容/移动就悬垂，且 GC 看不见这块内存 —— 放弃。

为什么必须这样：`Value` 是 32 字节，1000 万 × 768 维 = 28.6 GB。若内联进行，
每行一次 3 KB 分配，GC 元数据 + 离散布局会让扫描掉一个数量级。

代价（这是 Phase 1 主要的复杂度来源）：**任何要看到向量内容的地方都要有存储上下文**。
需要走专用格式化路径的用户可见输出：

| 路径 | 处理 |
|---|---|
| 文本结果集（HTTP / 文本协议） | 投影时按列读 `.vec` → `[0.1,0.2,…]` |
| 二进制结果集（PG 协议） | 同上，但按 text OID 编码（首期不注册 vector OID） |
| `COPY … TO STDOUT` | 同上 |
| 错误信息里打印值 | `Value.TextForm()` 对 KVector 返回 `[vector:12345]`，**不进用户可见输出** |

`Value.TextForm()` 保留兜底实现（`[vector:<id>]`），凡是漏改的路径会立刻在
`ui_check` / 结果断言里暴露成 `[vector:` 前缀，不会静默出错。

### 决策 2：`.vec` 是定长槽位文件，不经过页、不进 WAL 数据

```
tables/t000123_c02.vec      # 表 ID + 列序号，RENAME 不改名，DROP COLUMN 删文件
```

文件头 32 字节：

```
0   "GDBV"
4   uint16  version = 1
6   uint16  dim
8   uint32  reserved（对齐）
12  uint64  count          # 已分配槽位数
20  uint64  reserved
28  uint32  crc32(头前 20 字节)
```

槽位 `i` 的数据偏移固定为 `32 + i*dim*4`，一次 `pread` 直达，不需要索引。

为什么不复用 `.lob`：LOB 是追加写 + 行内 13 字节指针，更新一次就多一份垃圾，
1000 万行的表更新一轮就是 30 GB 垃圾；而且 LOB 无固定偏移，没法顺序扫。
定长槽位同时解决「原地更新」和「列式顺序扫描」两件事。

**不逐槽存 CRC。** 1000 万 × 768 维存 CRC 是 40 MB 额外读写，换来的只是
「个别位翻转能发现」。向量本来就是近似计算的数据，翻转一位对 Top-K 的影响
远小于一次 ANN 的召回损失；WAL 的 CRC 已经覆盖了「这一行引用了哪个槽」。
需要强校验时用 `VACUUM VERIFY` 全量重算（Phase 2）。

槽位分配：单调递增的 `count`，删除的槽位进内存位图 `free []uint64`，
新插入优先复用。位图不落盘 —— 重启时扫一遍 WAL 里的删除记录重建，
或者干脆从头分配（浪费可控，Phase 1 选后者，加一个 `wasted_bytes` 指标）。

### 决策 3：写入顺序「先 `.vec`、后 WAL」，提交时先 fsync 文件、再 fsync WAL

一次带向量的事务提交：

```
1. 写 .vec 槽位（pwrite，不 fsync）
2. 写 WAL 帧（行里只有 vecID）
3. 提交：SyncVec() → SyncTo(seq)
```

崩溃窗口：

| 崩在哪 | 结果 |
|---|---|
| 1 之后、2 之前 | 事务没提交 → 行不存在；`.vec` 里多一个没人引用的槽（垃圾，无害） |
| 2 之后、fsync 前 | WAL 未落盘 → 同上，行不存在 |
| fsync `.vec` 后、fsync WAL 前 | 槽在盘上、行不存在 → 垃圾，无害 |
| WAL fsync 之后 | 行在，槽一定已在盘上（顺序保证） |

反向顺序（先 WAL 后 `.vec`）会在「WAL 已落盘、文件没落盘」时得到一行指向
零数据的向量 —— 检索时静默返回全零向量，比报错更糟。所以顺序不能反。

事务回滚：本事务写过的槽位记在 `tx.vecGarbage`，Rollback 时归还位图
（不物理擦除，下次覆盖写）。WAL 里没有对应行，重启后这些槽位自然变成垃圾。

### 决策 4：查询完全复用现有 `sortOp` 的 Top-N 堆

`exec/builder.go:1584` 起的排序路径已经有：

- `topNHint(s)`：从 `LIMIT + OFFSET` 推 N（上限 1 M）；
- `sortOp.topN > 0` 时用「大小为 N 的最大堆」边扫边淘汰，不物化全表。

向量检索就是「排序键是一个 float 距离」。于是 Phase 1 **不需要新算子**，
只需要让 `p.orderKey()` 能编译出距离表达式。`EXPLAIN` 里自然显示
`Top-N Sort (n=10)`。

这带来一个必须遵守的约束：**距离表达式必须能一次算完并返回 `float64`**，
不能把向量比较推迟到 `types.Compare` 里（那会在堆调整时反复算距离）。

---

## 2. SQL 接口

### 2.1 类型

```sql
CREATE TABLE questions (
    id        BIGINT PRIMARY KEY,
    title     TEXT,
    subject   VARCHAR(32),
    embedding VECTOR(768)
);
```

- `n` 省略或 ≤0 → 报错 `vector dimension must be between 1 and 4096`；
- 与 `VARCHAR(n)` 共用 `catalog.Column.Len` 存放维度（零迁移）；
- `types.ParseType` 返回 `Type{Kind: KVector, Name: "vector(768)", Len: 768}`。

### 2.2 字面量

```sql
INSERT INTO questions (id, title, embedding)
VALUES (1, '集合运算', '[0.12,-0.34,0.56,…]'),
       (2, '数列极限', '[0.01,0.02,0.03,…]'::vector);
```

- 未加 `::vector` 时，插入/比较的目标列是 vector → 隐式转换；
- `SELECT '[1,2]'::vector` 也支持（投影里显示回 `[1,2]`）；
- 元素分隔符 `,`，允许空格与换行；不接受 `NaN` / `Inf`；
- 长度不等于列维度 → `vector dimension mismatch: column is 768, value has 512`。

### 2.3 距离算子

| 算子 | 含义 | 典型用法 |
|---|---|---|
| `<->` | L2 欧氏距离 | `ORDER BY e <-> q` |
| `<=>` | 余弦距离（1 - cos） | `ORDER BY e <=> q` |
| `<#>` | 负内积（`-a·b`） | `ORDER BY e <#> q` |

只看距离、不排序也支持：`SELECT id, e <=> q AS d FROM t`。

`ORDER BY` 里的方向：距离是「越小越近」，所以默认 `ASC`；写 `DESC` 就是反着要
最远的（合法，但通常没意义）。`NULLS FIRST/LAST` 沿用现有排序语义：NULL 向量
不参与距离计算，排在最前或最后。

### 2.4 PG 协议

首期**不注册** `vector` 类型 OID（pgvector 用的是 13000 + 扩展 OID）。
向量列在 `RowDescription` 里按 `text`（OID 25）下发，值编码为 `[0.1,0.2]` 文本。

好处：psql / Navicat / JDBC 都能直接看到值，不会因为「未知 OID」报错。
坏处：客户端拿不到 pgvector 的类型，`::vector` 在客户端侧不可用 ——
首期接受，Phase 3 再评估自定义 OID。

这个「按 text 下发」不只影响 `RowDescription`，**元数据也一样**：
`information_schema.columns` 的 `data_type` / `udt_name` 都是 `text`，
`pg_attribute.atttypid` 是 25，`pg_type` 里没有 `vector` 这一行（否则 OID 就对不上了）。
所以「读元数据生成 DDL」的工具会把向量列写成 `text`。

这条路的危险后果已经被挡住：把有数据的向量列 `ALTER COLUMN … TYPE text` 会报
`cannot cast a stored vector to text`（`types.Cast` 里显式拒绝），数据不会丢也不会被
误解成文本。真要换类型只能新建列 + 回填。等注册了类型 OID（Phase 3）再谈元数据一致。

### 2.5 不支持的用法（明确报错）

向量**不参与**「等值 / 分组 / 排序 / 索引」这四件事，全部在规划期或 DDL 期报错：

| 写法 | 报错 | 原因 |
|---|---|---|
| `WHERE v = '[1,2,3]'` | `operator does not exist: vector = text` | 文本侧没有隐式转成向量（只有距离算子的常量侧放宽） |
| `ORDER BY v` / `MIN(v)` / `MAX(v)` | `… has no comparable order` | 向量没有全序，排出来的顺序没有含义 |
| `SELECT DISTINCT v` | `SELECT DISTINCT cannot be applied to vector values` | 去重要「内容身份」，行里只有槽位号 |
| `GROUP BY v` / `COUNT(DISTINCT v)` | `GROUP BY … / …(DISTINCT …)` | 同上 |
| `UNION` / `INTERSECT` / `EXCEPT` | `UNION needs to compare rows …` | 去重同上；**`UNION ALL` 不受限** |
| `a JOIN b ON a.v = b.v` | `JOIN ... ON cannot be applied to vector values` | 哈希连接会拿槽位号配对 |
| `v IN (SELECT v FROM …)` | `… cannot be used as equality or grouping keys` | 同上 |
| `CREATE INDEX … (v)` | `data type vector has no default operator class for access method btree` | 索引键按列值编码，槽位号编出来的键没有语义 |
| `lower(v)` / `concat(v)` / `v \|\| '…'` / `length(v)` / `string_agg(v, ',')` / `v::text` | `… has no text form` | 存盘向量没有文本形式，硬转会漏出 `[vector:0]`（`length(v)` 会返回 10） |
| `v vector PRIMARY KEY / UNIQUE` | `PRIMARY KEY / UNIQUE cannot be applied to vector column` | 唯一性检查同样要内容身份 |
| `ALTER COLUMN v TYPE vector`（该列已有索引/约束） | `cannot change … to vector` | 会把已有索引变成错的东西 |

**为什么必须报错，而不是「尽量给个答案」**：行里的向量值只有一个 `.vec` 槽位号
（决策 1），槽位每次 `INSERT` / `UPDATE` 都会重新分配、删除后还会被别的行复用。
拿它当身份，`DISTINCT` 的组数、唯一约束的判重结果都取决于物理写入时机 ——
同一份数据在不同时刻能给出不同答案，而且看不出来是错的。

修复前这三条路径是**静默错误**的：同内容的 `[1,2,3]` 插入两行，`SELECT DISTINCT v`
返回两行；`CREATE UNIQUE INDEX ON t(v)` 能把不同向量当成同一个键，之后插入任何
新向量都报 `duplicate key`。如果在这个版本之前建过向量索引，请 `DROP INDEX` 重建 ——
那条索引里所有向量被编码成了同一个键。

正确的用法是距离算子排序：

```sql
SELECT id, 1 - (embedding <=> '[0.1,0.2,…]') AS score
FROM questions
ORDER BY embedding <=> '[0.1,0.2,…]'
LIMIT 10;
```

用户侧的说明在帮助弹窗「10 向量检索」（`web/assets/index.html` 的 `#h10`）：
那里列了同样的清单，并给了 Top-K 与「先按距离筛、再排序」两段能直接跑的例子。
以后改这条限制，两处一起改（`tools/ui_check.js` 会检查帮助里是否写到了这些关键字）。

---

## 3. 存储实现

### 3.1 不把 `.vec` 读进 Go 堆，用只读映射扫

28.6 GB 的 `[]float32` 进 Go 堆，即使是无指针大对象（GC 不扫内容），
也会让 GOGC=100 的下一个 GC 目标变成 57 GB，直接把机器打爆。

**（原方案「只用 ReadAt 分块读」已被实测推翻，见下。）** 现在做成两层：

```go
vf, _ := storage.OpenVecFile(path, dim)
view, _ := vf.NewView()      // 一次查询一个只读映射
defer view.Close()
rows := view.Slice(from, 1024) // 零拷贝，直接是 []float32
```

实测（本机，512MB 定长文件，整块扫描，`go test ./storage -run VecFileScan`）：

| 读取方式 | 1 线程 | 4 | 8 |
| --- | --- | --- | --- |
| 堆内切片（基线）| 10.2 GB/s | 19.6 | 35.7 |
| **只读映射** | **8.1 GB/s** | **17.9** | 37.0 |
| `ReadAt` 单句柄 | 2.3 GB/s | — | — |
| `ReadAt` 多句柄 | — | 5.6 | 7.1 |

原来写的「ReadAt 能到内存带宽的 60%（18–20 GB/s）」是错的：Windows 上同一句柄
的 I/O 被 Go 的 poll 层串行化，单句柄 2.3 GB/s，多开句柄也只有 7.1 GB/s。
映射路径与堆内切片同速，所以顺序扫描走映射，`ReadAt` 只留给单行投影。

映射的代价与约束：

- 各平台一份实现（`vecmap_windows.go` / `vecmap_unix.go`，各约 60 行，全在
  `syscall` 里，无第三方依赖）；
- 首次触碰每页要陷缺页（约 2 GB/s），同一次查询复用同一个视图即可规避；
- 视图有效性由**表级 2PL** 保证：持 S 锁扫描时没有事务能写这张表。

读取接口（批量，不按行）：

```go
// VecView 是一次查询期间的只读映射。
type VecView struct{ ... }
func (v *VecView) Dim() int
func (v *VecView) Count() int64
func (v *VecView) Slice(from, n int64) []float32  // 零拷贝，越界自动收窄
func (v *VecView) At(id int64) []float32          // 单槽位
func (v *VecView) Close() error

// VecFile 负责写入与单行读取。
func (vf *VecFile) Append(vec []float32) (int64, error)
func (vf *VecFile) WriteRange(from int64, data []float32) error  // 批量导入
func (vf *VecFile) Read(id int64, dst []float32) ([]float32, error)
func (vf *VecFile) ReadRange(from int64, n int, dst []float32) ([]float32, error)
```

### 3.2 写入

```go
func (vf *vecFile) Write(id int64, v []float32) error   // pwrite 到定长偏移
func (vf *vecFile) Sync() error                          // 提交路径调用
func (vf *vecFile) TruncateTo(count int64) error         // 恢复时按文件长度修正头
```

- 写入不 fsync（由提交路径统一 sync，和 WAL 组提交同一时机）；
- 文件按需扩：`Write` 到 `id >= count` 时 `Truncate((id+1)*dim*4+32)` 并更新头；
- 头里的 `count` 每 256 次扩容落一次（避免每次插入都写头），恢复时以
  `文件长度` 为准并修正 `count`。

### 3.3 WAL 编码

`storage/binwal.go` 加 `tagVector = 8`：

```
tag 8 | uint64 vecID | uint16 dim
```

11 字节/向量，1000 万行就是 110 MB WAL —— 在 64 MB 轮转阈值下触发 2 次
checkpoint，正常。

**不存 float32 数据**：768 维一行 3 KB，1000 万行 30 GB，且每次 checkpoint
都要重写全表快照，导入会退化成「写 20 行落一次盘」。

页文件里的行用同一种编码（和 `tagLOB=7` 并列）。快照 `.snp`（备份/回收站
交换格式）里向量也只能存 vecID，**必须把 `.vec` 文件一起拷**，否则恢复出
一堆指向空槽的引用。`storage.TrashTables` 与 `/api/backup` 的改动在 T8。

> **T8 补齐（2026-11-04）**：作者提交时这部分漏了，实测两个后果 ——
> `.trash/vec` 目录被 `TrashEntries` 当成备份（`'v' > '2'`，倒序排第一），
> `-restore-trash latest` 去读 `vec/manifest.json` 直接报错；指定正确备份目录
> 恢复也只剩 `[vector:0]`（引用指向空槽，原数据全丢）。
>
> 已修：`TrashTables` 把 `t<表ID>.c<列>.vec` 拷进备份的 `vec/` 子目录；
> `RestoreTrash` 用 `InstallVecFiles` 按**新表 ID** 装回去（恢复相当于重新建表，
> ID 会变，文件名里编着表 ID）；`TrashEntries` 只认含 `manifest.json` 的目录。
> 回归用例：`db/trash_test.go` 的 `TestRestoreTrashKeepsVectorData` /
> `TestTrashEntriesSkipsVecDir`（修复前两条都红）。
>
> `/api/backup` 不需要改：它导出 SQL 文本，向量按 `'[1,2,3]'` 输出、列类型写
> `vector(n)`，恢复时走普通 INSERT 即可（实测验证过）。

### 3.4 恢复

启动/打开表时：

1. 读 `.vec` 头，校验 magic/version/crc；`dim` 与 catalog 不符 → 整个文件作废，
   该列全部置 NULL 并计入 `Store.Repaired`；
2. `count = (文件长度-32)/(dim*4)`，**以文件长度为准、不用头里的 count**
   （写实现时改的，理由见下）；对不上就重写一次头；
3. 重放 WAL，行里的 vecID 照常写入；
4. **不校验每个 vecID 是否 < count** —— 校验放在第一次读：越界返回
   `ErrVecMissing`，该行投影成 NULL。理由：1000 万行的启动校验是
   1000 万次额外判断，而越界本身就是罕见情况。

为什么不用 `min(头, 文件长度)`：头只在每 256 次扩展后落盘，崩溃时会比文件短，
取 min 就把已提交的槽位丢了（实测用例 `TestVecFileRecoverFromStaleHeader` 抓的
就是这个）。反过来只看头也不行：文件被截断时头还停在旧值，会把读不到的槽位当作可用。
文件长度是唯一自洽的权威 —— 写入必然先扩文件，任何「写过」的槽位一定在长度之内；
而「分配了但没写」的槽位没有行引用，重发一次序号无害。

---

## 4. 执行路径

### 4.1 词法

`sql/lexer.go:236` 的三字符分支（现在只有 `!~*`）加：

```go
switch tri {
case "!~*", "<->", "<=>", "<#>":
```

`<#>` 与 `#`（已有单字符 Punct）不冲突：三字符先匹配。
注意 `<=>` 必须在 `<=` 之前判断 —— 三字符分支本来就在二字符 switch 前面，
顺序天然正确。

### 4.2 语法与类型检查

`<->` 等作为普通二元运算表达式进 `sql.BinaryExpr`，`Op` 记为 `"<->"` 等。
`exec` 编译表达式时（`p.orderKey` → `compileExpr`）：

1. 两侧类型都必须是 `KVector`，否则 `operator does not exist: vector <-> text`；
2. 维度必须一致，否则 `vector dimension mismatch: 768 vs 512`；
3. 任一侧是常量字面量 → **编译期**解析成 `[]float32` 并缓存进节点，
   绝不能在每行 Eval 里 `strconv.ParseFloat`。

### 4.3 表达式节点

```go
type vecDist struct {
    col   int          // 行内第几列（vecID 所在位置）
    dim   int
    metric byte        // distL2 / distCosine / distNegIP
    q     []float32    // 编译期解析好的查询向量
    rd    VecReader    // 该表的向量读取器（来自 *storage.Store）
    tmp   []float32    // 复用 buffer，避免每行分配
}
func (v *vecDist) Eval(row types.Row) (types.Value, error) {
    id := row[v.col].I
    buf, err := v.rd.Read(id, v.tmp)
    if err != nil { return types.Null, nil }   // 缺失向量不参与排序
    return types.Float(float64(distance(v.metric, v.q, buf))), nil
}
```

`tmp` 复用是必须的：1000 万行 × 每次 3 KB 分配 = 三次 GC 就崩。

### 4.4 计划选择

`ORDER BY e <=> $q LIMIT 10` 走现有路径：

```
Limit(10)
  └─ Top-N Sort (n=10)        ← topNHint 已经给出 10
       └─ Filter(WHERE …)     ← 有过滤时先过滤，省距离计算
            └─ TableScan
```

有 `WHERE` 时顺序不能反（先排序再过滤会把过滤代价乘以 N 倍）。
`ORDER BY` 里不出现距离表达式时，一切照旧 —— 向量列只是普通列。

### 4.5 距离实现

`types/vector.go` 新增：

```go
func DistL2(a, b []float32) float32      // 四路累加，见实测
func DistCosine(a, b []float32) float32  // 1 - dot/(|a||b|)，任一非零向量时
func DistNegIP(a, b []float32) float32   // -dot
```

四个累加器不是可选项：单累加器有 5 cycle 的 FMA 依赖链，实测慢 1.3 倍。
`|b|`（查询向量模长）在编译期算好，余弦只剩一次点积。

---

## 5. 改动清单

| 文件 | 改动 | 量级 | 状态 |
|---|---|---|---|
| `types/value.go` | `KVector` + 构造 + `TextForm` 兜底 | +60 | ✅ T1 |
| `types/type.go` | `ParseType` 认 `vector(n)`、`Type.String` | +25 | ✅ T1 |
| `types/vector.go`（新） | 解析 `[..]` 文本、三种距离、点积 | +220 | ✅ T1 |
| `types/cast.go` | 文本 → vector、维度校验 | +30 | ✅ T1 |
| `types/oid.go` | `vector` 按 text OID 下发 | +5 | ✅ T1 |
| `catalog/catalog.go` | 列校验（维度、非空约束） | +30 | T5 |
| `storage/vecfile.go`（新） | 定长槽位文件、批量读写、sync、截断、位图、映射视图 | +560 | ✅ T3 |
| `storage/vecmap_windows.go` / `vecmap_unix.go`（新） | 只读映射（+ 无 mmap 平台的空壳） | +120 | ✅ T3 |
| `storage/binwal.go` | `tagVector` 编解码（WAL 与堆页共用 `appendValue`，tag 7 已被 LOB 占用，向量用 8） | +30 | T4 |
| `storage/tablepages.go` / `browse.go` | 页内行读写认 tag 8（`appendValue` 已在 1031 行被调用，只需在 tag 分派处加分支） | +50 | T4 |
| `storage/engine.go` | `Tx` 提交顺序、`vecGarbage`、`Store` 级向量表注册 | +120 | T4 |
| `storage/snapshot.go` | `Info()` 带上向量文件字节数；备份拷贝 | +40 | T8 |
| `sql/lexer.go` | 三字符算子 | +5 | T6 |
| `sql/parser.go` | `::vector`（类型转换已有机制，确认走通） | +10 | T1/T6 |
| `sql/ast.go` | 无需改（沿用 BinaryExpr） | 0 | — |
| `exec/eval.go` | `vecDist` 节点编译 + 求值 | +180 | T2 |
| `exec/builder.go` | `orderKey` 允许 KVector 距离；表扫描挂映射视图 | +80 | T6 |
| `exec/engine.go` | 投影/结果输出走 `formatVector` | +120 | T8 |
| `pgwire` | RowDescription 用 text OID；二进制结果按文本编码 | +60 | T8 |
| `web/bench.go` 或新脚本 | 向量导入/查询基准（可选） | +150 | T9 |
| 测试 | 单测 + 崩溃恢复 + 端到端 SQL | +600 | 随任务 |

合计约 **2000 行**（含测试）。

---

## 6. 任务拆分

按依赖顺序，每个任务独立可测。

### T1 类型与解析（半天）
`KVector`、`ParseType("vector", [768])`、`types.ParseVector("[1,2]")`、`Vector.TextForm` 兜底。

验收：`SELECT '[1,2,3]'::vector` 返回 `[1,2,3]`；`'[1,a]'` 报错；`'[1,2]'::vector(3)` 报维度错。

### T2 表达式与距离（半天）
三种距离函数 + `vecDist` 节点编译 + 常量预解析 + `tmp` 复用。

验收：已知向量的三种距离与手算一致（误差 < 1e-5）；`EXPLAIN` 里 `Filter` 在下、`Top-N` 在上。

### T3 定长文件（1 天，最核心）
`vecfile.go`：头、扩文件、pwrite/pread、`ReadRange` 分块、`Sync`、`TruncateTo`、位图；
`vecmap_{windows,unix}.go`：只读映射视图。

验收（已实跑，`go test ./storage -run VecFile` 共 13 项）：
- 100 万 × 128（0.48 GB）写入 2.2 GB/s，顺序扫描映射视图**单线程 8.1 GB/s、4 线程 17.9 GB/s**
  （`ReadAt` 单线程只有 2.3 GB/s，对照组）；
- 单槽读写逐位一致；截断文件后 `count` 按实际长度自动修正、越界读返回 `ErrVecMissing`；
- 头里的 count 落后于文件（模拟崩溃）时按文件长度恢复，已提交的槽位不丢；
- 8 goroutine 并发写 800 条，读回内容不串。

### T4 WAL 与恢复（1 天）
`tagVector` 编解码、提交顺序、`vecGarbage`、打开时校验、越界置 NULL。

验收：提交后 `kill -9`，重启后行与向量都在；在 `.vec` 写完但 WAL 未写时
`kill -9`，重启后行不存在、无报错；回滚后槽位被复用。

### T5 DDL（半天）
`CREATE TABLE … VECTOR(n)`、`ALTER TABLE ADD/DROP COLUMN`、`\d` 显示
`vector(768)`、`information_schema.columns.data_type` 推导。

验收：建表/加列/删列（删列要删 `.vec` 文件）；`\d` 输出 `vector(768)`；
`information_schema.columns` 按 pgvector 的习惯输出 `data_type='USER-DEFINED'`、
`udt_name='vector'`（客户端认这个组合，按未知类型也能显示）。

### T6 Top-K 查询打通（半天）
词法三算子、`orderKey` 允许距离、投影格式化、`LIMIT` 生效。

验收：
```sql
SELECT id FROM t ORDER BY e <=> '[…]' LIMIT 10;
```
结果与「全量算距离排序取前 10」一致；100 万行 128 维 P95 < 200 ms。

### T7 过滤与边界（半天）
`WHERE` + 向量排序、`NULL` 向量、空表、`LIMIT 0`、`OFFSET`、`DISTINCT` 冲突提示。

验收：过滤后 Top-K 与先过滤再排序一致；NULL 向量不参与也不报错。

### T8 输出与备份（1 天）
PG 文本/二进制结果、`COPY TO`、`/api/backup`、`TrashTables` 拷 `.vec`、
`-restore-trash` 还原、监控「存储」页显示向量文件大小。

验收：psql 能看到 `[0.1,0.2]`；备份 → 删表 → 还原后向量完整；
`ui_check` 全绿（新增 6 条向量断言）。

### T9 基线（半天）
导入 100 万 × 128 / 100 万 × 768 / 1000 万 × 128，记录：导入吞吐、全扫延迟、
Top-10 延迟、WAL 增长量、`.vec` 文件大小、页缓存命中后的第二轮延迟。

验收：1000 万 × 128 全扫 < 250 ms；WAL 增长 < 数据量的 0.5%；
第二轮（热缓存）比第一轮快 ≥ 3 倍。

---

## 7. 与总体设计的差异（需要同步回 `godb 向量设计.md`）

1. 6.3 节「WAL 帧记录 new_vector」→ 改成「只记 vecID」。
   理由：768 维一行 3 KB，1000 万行 30 GB 日志，checkpoint 会疯。
2. 6.1 节「列式存储 + 内存 `Data []float32`」→ 改成「定长槽位文件 + OS 页缓存」。
   理由：28.6 GB 进 Go 堆会让 GC 目标翻倍。
3. 9 节性能目标按维度重写（原表只给了 128 维）。

## 8. 未决

- 维度上限 4096 是否够？（1536 维 × 1000 万 = 57 GB，超出常见机器内存）
- 是否允许 `vector` 列做 `DEFAULT` / 唯一索引？**已定**：都拒绝（见 2.5）。
  真要「按内容去重」得等 Phase 2 的向量索引（按内容编码键 + 回表校验），
  而不是复用 btree 的槽位号键。
- 删除向量的空间回收：Phase 1 只记账不回收，Phase 2 跟 VACUUM 一起做。
- `.vec` 是否纳入 checkpoint 的 fsync 范围（当前设计：提交时已 fsync，checkpoint 不管）。
