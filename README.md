# godb

用 Go 从零实现的教学级关系数据库：手写词法分析 → 递归下降语法分析 → 逻辑/物理计划 → 火山模型执行 → 内存行存 + WAL 持久化，并发控制采用**表级 S/X 两阶段封锁**，附带一个内嵌的 Web 控制台。

![控制台](docs/shot.png)

**零第三方依赖、零 cgo、不直接加载任何系统 DLL**：只用 Go 标准库（`net/http` + `go:embed` + `time`），`CGO_ENABLED=0` 直接编出一个可运行的 exe。

## 快速开始

```bash
cd godb
go build -o godb.exe .
./godb.exe                     # 默认监听 127.0.0.1:8080，数据落在 ./godb-data
```

浏览器打开 <http://127.0.0.1:8080>。首次启动会自动建两张示例表（`departments` / `employees`）并灌入数据。右上角「锁状态」可打开锁面板（也支持 `#locks` 直链）。

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-http` | `127.0.0.1:8080` | 监听地址 |
| `-data` | `godb-data` | 数据目录（catalog / 快照 / WAL 都在里面） |
| `-no-sample` | `false` | 首次启动不灌示例数据 |
| `-isolation` | `table` | `table`＝表级 S/X 锁；`serial`＝全局串行（旧行为，用于对照） |
| `-lock-timeout` | `3s` | 等待表锁的最长时间，超时自动放弃并回滚整个事务 |
| `-lock-retry` | `20ms` | 等待时的轮询间隔（带 ±50% 抖动） |
| `-deadlock-detect` | `true` | 等待成环时让最年轻的事务提前放弃 |
| `-deadlock-after` | `500ms` | 等待多久后开始做死锁检测 |

## 功能

| 类别 | 支持 |
|---|---|
| 表管理 | `CREATE TABLE`（`SERIAL`、`PRIMARY KEY`、`UNIQUE`、`NOT NULL`、`DEFAULT`）、`DROP TABLE [IF EXISTS]`、`ALTER TABLE ADD/DROP/RENAME COLUMN`、`ALTER TABLE RENAME TO`、`ALTER COLUMN TYPE`、`TRUNCATE` |
| 查询 | `SELECT [DISTINCT]`、`WHERE`、`GROUP BY`、`HAVING`、`ORDER BY`（`ASC/DESC/NULLS FIRST/LAST`、别名、序号）、`LIMIT/OFFSET`、`EXPLAIN` |
| 连接 | `INNER` / `LEFT` / `RIGHT` / `CROSS JOIN`，多表串联；等值条件走哈希连接，其余走嵌套循环 |
| 聚合 | `count(*)`、`count(x)`、`sum`、`avg`、`min`、`max`，支持 `DISTINCT` |
| 增删改 | `INSERT ... VALUES`（多行、`DEFAULT`）、`UPDATE ... SET ... WHERE`、`DELETE ... WHERE` |
| 表达式 | 算术 `+ - * / %`、比较、`AND/OR/NOT`、`IS [NOT] NULL`、`IN`、`BETWEEN`、`LIKE/ILIKE`、`CASE WHEN`、`CAST`/`::`、`\|\|` |
| 函数 | `lower/upper/length/trim/ltrim/rtrim/abs/round/ceil/floor/replace/substring/concat/coalesce/nullif/greatest/least/now` |
| 事务 | `BEGIN`/`START TRANSACTION`、`COMMIT`、`ROLLBACK`；语句级原子性；崩溃恢复 |
| 并发 | 表级 `S`/`X` 锁、严格两阶段封锁、锁等待与超时、等待图死锁检测 |
| 类型 | `smallint/integer/bigint/serial/bigserial/numeric(p,s)/real/double precision/text/varchar(n)/char(n)/boolean/date/timestamp` |

**明确不支持**（会报清晰的错误）：子查询、CTE、窗口函数、`UNION`、外键、`CHECK`、`FULL JOIN`、`NATURAL JOIN`、多列主键、行级锁、二级索引、`RETURNING`、`INSERT ... SELECT`。

## 并发控制（表级两阶段封锁）

### 语义

对每张表独立判定「这个事务在这张表上是读还是写」：

| 我这个事务对表 t 做了什么 | t 上加的锁 | 别人能读 t | 别人能写 t |
|---|---|---|---|
| 读过 | `S`（共享） | ✓ | ✗ |
| 写过 | `X`（排它） | ✗ | ✗ |
| 先读后写 | `S` 升级为 `X` | ✗ | ✗ |

- 锁从第一次触达该表起持有，**到 COMMIT/ROLLBACK 才释放**（严谨两阶段封锁，rigorous 2PL），因此隔离级别等价于 **SERIALIZABLE**：脏读、不可重复读、幻读都不可能。
- 两个事务的表集合不相交时**完全并行**（事务 A 改 `t1`、事务 B 改 `t2` 互不影响）。
- 同一张表上：读读并发 ✓、读写互斥 ✗、写写互斥 ✗。

### 保守预扫描：把死锁消掉

控制台通常把 `BEGIN; …; COMMIT;` **整批**发给服务端，服务端手上有全部语句，于是可以：

1. 静态扫一遍整批，得出每张表是「只读」还是「要写」；
2. `BEGIN` 那一刻**一次性申请全部表锁**（保守 2PL）。

这样既不会中途发生 `S → X` 升级（升级冲突是死锁的主要来源），也不会出现双方交叉加锁的环。逐条交互式提交时做不到预判，才会退化成增量加锁，此时靠等待图检测兜底。

### 拿不到锁怎么办

1. 立即尝试授予；冲突则等待，每 `20ms`（±抖动）重试一次，期间**不释放**已持有的锁。
2. 等待超过 `-lock-timeout`（默认 3s）→ **整个事务自动放弃**：回滚、释放全部锁、返回错误并指出是谁卡着、卡在哪张表。
3. 等待超过 `-deadlock-after`（默认 500ms）→ 扫一次等待图；若成环，让环里**最年轻**（序号最大）的事务放弃，其余继续。

错误长这样：

```
等待锁超时（已等 3.008s）：表 employees 被 4f0b55(S) 持有
检测到死锁（等待环涉及 2 个事务：8262bb → 4f0b55），本事务被回滚，请重试
```

### 锁面板

右上角「锁状态」实时显示每张表的持有者（`S: 会话1、会话2` / `X: 会话3`）、正在等待的事务、累计等待次数。测试并发要用**两个浏览器窗口/无痕窗口**（同一浏览器的多个标签共享 cookie，等于同一个会话）。

### 实测开销

`go test ./db -run XXX -bench . -benchtime=300x`（2000 行表，同一张表、同一条聚合查询）：

| 场景 | 耗时 | 说明 |
|---|---|---|
| 单会话，`-isolation=serial` | 268 µs/op | 全局锁 |
| 单会话，`-isolation=table` | 264 µs/op | 表级锁，**开销在噪声内** |
| 8 路并发纯读 | 55 µs/op | 读读相容，约 **4.9×** 吞吐 |
| 4 路并发写同一张表 | 411 µs/op | 互斥，被表锁串行化 |
| 4 路并发写不同表 | 375 µs/op | 只快 9%，瓶颈是 **WAL fsync**，不是锁 |

补充一个实测：20000 行一次性 `INSERT` 从 12.8s 优化到 **0.152s**（约 85×）。
原因是语句内唯一性检查原本对每行扫描已插入的行（O(n²) 且每次都编码字符串），
改成「语句内用局部哈希集合 + 只扫描本语句之前插入的行」后是 O(n)。

结论：加锁本身的开销可以忽略（每条语句 0.2µs 量级，而一条查询是几百 µs），真正的瓶颈一直是 WAL fsync 和「物化整表再算」的扫描方式。写并发的进一步提升要靠 group commit（多个事务的日志合并一次 fsync），与锁无关。

## 耗时显示

每条语句的结果卡右上角会显示它自己的耗时，粒度到微秒：

```
SELECT 5    5 行 · 14.1 ms
```

- 服务端只用了标准库 `time`（`hrtime` 包，见下），不涉及 cgo 或系统 DLL。
- 服务端耗时**含等待锁**、**不含 SQL 解析**，鼠标悬停可以看到精确到纳秒的原值和本机时钟步进。
- 浏览器还会显示一个「往返」耗时（`performance.now()` 测的，含网络与 JSON 编码），用来交叉验证。

**关于精度的一句实话**：`time.Now()` 的分辨率取决于操作系统时间源。正常电脑上是亚微秒到微秒级，
但本项目的开发沙箱里时钟步进约 **0.2 ms**，所以比它更快的操作测不出来。服务端启动时会实测这个步进，
前端把测不准的值显示成 `<0.20 ms` 而不是骗人的 `0 ms`：

```
SELECT 1    1 行 · <0.20 ms
```

要看到真实的微秒级数字，跑一条耗时超过时钟步进的查询即可（例如 `big` 表上的 `ORDER BY`）。

## 架构

```
main.go        启动装配、参数解析
  db/          会话管理、语句分发、事务控制、整批预扫描、示例数据
  sql/         lexer → parser → AST（递归下降 + Pratt 表达式优先级）
  lock/        表级 S/X 锁管理器：批量原子申请、等待重试、超时、等待图
  catalog/     表/列元数据，catalog.json 持久化
  hrtime/      计时入口（只用标准库 time，无 cgo / 无 DLL），含时钟步进探测
  types/       值（Datum）、类型系统、CAST、比较/哈希
  storage/     内存堆表 + 主键哈希索引 + 写集事务 + WAL + 快照
  txn/         会话状态、隔离模式、语句级回滚、持锁集合
  exec/        表意图分析、计划构建、表达式编译、算子（Scan/Filter/Join/Agg/Sort/…）
  web/         HTTP API + go:embed 内嵌前端（原生 JS，无构建步骤）
```

执行链路：`SQL 文本 → Lex → Parse(AST) → TableIntents(要锁哪些表) → 拿锁 → BuildSelect(算子树) → Open/Next 拉取行 → JSON → 前端表格`。

### 并发实现要点

- **锁持有期**：`S`/`X` 都持到事务结束（这就是 rigorous 2PL，也是可串行化的保证）。
- **加锁顺序**：`SchemaMu（读）→ CommitMu → 锁管理器内部锁`，全程单向，不会互相等待。
- **schema 锁**：语句执行期间持 `SchemaMu` 读锁（保护 catalog 与表数据；DDL 持写锁），但**表锁的等待发生在拿 schema 锁之前**，避免一个 DDL 把所有人堵在门口。
- **提交串行化**：`CommitMu` 串行化「应用写集 + WAL fsync + checkpoint」。不同表的事务可以并行执行，提交时排队。
- **语句级回滚**：每条语句执行前 `tx.Mark()` 浅拷贝写集，出错就 `RestoreTo`。

### 事务与崩溃恢复

1. 写操作先落到事务私有写集（overlay），提交时才应用。
2. 提交：先把整批行变更写进 WAL 并 `fsync`，再应用到内存并重建主键索引。
3. 恢复：启动时 `snapshot.json` + 重放 `wal.log` 中**带 commit 标记**的事务，未提交的直接丢弃 —— 这就是原子性。重放完立刻 checkpoint 并截断 WAL。
4. 中毒事务：语句出错后会话进入 aborted 状态，后续语句一律被拒绝，只能 `ROLLBACK`（`COMMIT` 等价于回滚），与 PostgreSQL 一致。**但锁冲突是例外**：按“超时即放弃”的约定，直接回滚整个事务并释放锁。

### 为什么 DDL 不能在事务里执行

`ALTER TABLE` 要改写所有现存行，如果允许回滚，就会出现"表结构变了但数据宽度没变"的损坏状态。所以本库明确拒绝在事务块里执行 `CREATE/DROP/ALTER TABLE`，换来的是 DDL 路径简单可靠。

### 数据文件

| 文件 | 内容 |
|---|---|
| `catalog.json` | 表结构、列类型、约束、默认值 |
| `snapshot.json` | 全量行数据的快照（checkpoint 产物） |
| `wal.log` | 追加写的提交日志，一行一条 JSON 记录 |

checkpoint 触发点：启动完成、每 128 次提交、每次 DDL、进程退出。

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/query` | `{"sql": "..."}`，支持多语句，逐条返回结果 |
| `GET` | `/api/schema` | 表结构与行数 |
| `GET` | `/api/locks` | 锁状态快照（表锁持有者、等待者、累计等待次数） |
| `POST` | `/api/tx` | `{"action":"begin\|commit\|rollback"}` |
| `POST` | `/api/sample` | 重建示例数据 |
| `POST` | `/api/reset` | 清空所有表 |

会话用 cookie `godb_sid` 区分。事务打开期间前端每 25s 发心跳；标签关掉后服务端 3 分钟回收并强制回滚、释放锁。

## 测试

```bash
go test ./...        # 26 个用例
go test ./db -run XXX -bench . -benchtime=300x   # 锁开销与并发吞吐
```

- `lock/`：S-S 相容、S-X / X-X 冲突、批量申请原子性、释放唤醒、S→X 升级、等待图死锁（只让年轻者放弃）、快照。
- `db/`：CRUD 与约束、NULL 三值逻辑、排序分页、分组聚合、四种 JOIN、DISTINCT、表达式与函数、事务提交回滚、中毒事务、WAL 崩溃恢复、ALTER 四种动作、错误路径；并发部分覆盖读读并行、跨表写并行、读写互斥、写挡读、保守预扫描消除升级、死锁检测、未提交不可见、20 次并发自增不丢更新。

> `-race` 需要 cgo（`gcc`），本机未安装 C 编译器，因此并发正确性靠上述压力用例保证。

## 已知限制

- 数据全部驻留内存，适合几十万行以内的演示/学习场景；没有页式存储和缓冲池。
- 锁粒度只到表：一张表同一时刻只允许一个写者，读还会挡住写。要提并发得先引入稳定 RowID 才能做行级锁。
- 写并发受 WAL `fsync` 限制，不同表并行写只快约 9%；下一步优化是 group commit。
- `NUMERIC(p,s)` 内部用 `float64`，**不是精确十进制**。
- 耗时精度受操作系统时钟限制：小于本机时钟步进（沙箱里约 0.2ms）的操作只能显示上界 `<0.20 ms`。
- JOIN 会物化两侧数据；连接结果超过 200 万行会直接报错而不是把内存吃光。
- 没有查询优化器（不做谓词下推、连接顺序重排），执行计划就是 SQL 的直译。
- DDL 不能在事务块内执行；二级索引、多列主键、外键、子查询都在待办清单里。
