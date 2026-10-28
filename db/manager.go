package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"godb/catalog"
	"godb/exec"
	"godb/sql"
	"godb/syscat"
	"godb/txn"
)

// Manager 管理同一个数据目录下的多个数据库。
//
// 目录约定（故意不对称，为了老部署升级时零迁移）：
//
//	<root>/catalog.json, tables/, wal.log …   默认库（godb），就是原来的布局
//	<root>/db/<名字>/                          其它库，各自一套一样的文件
//
// 每个库是**完全独立**的一套：自己的 catalog、WAL、快照、锁管理器和事务管理器。
// 库之间没有任何共享状态，所以一个库的写事务不会挡住另一个库，DROP 一个库
// 也不会影响别的库。代价是跨库查询做不了（PostgreSQL 也要 dblink/FDW 才行）。
type Manager struct {
	root string
	opts Options

	mu  sync.RWMutex
	dbs map[string]*DB // 小写库名 → DB

	// lock 是整个数据根目录的独占锁：一个进程一个目录，第二个进程直接起不来。
	// 按库加锁是不够的 —— CREATE/DROP DATABASE、配置写入都在根上动文件。
	lock *dirLock
}

// 库名规则和普通标识符一致：小写字母/数字/下划线，首字符不能是数字，最长 63。
// 更严格一点是为了直接当目录名用 —— 挡住 `../` 这类路径穿越。
var dbNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// reservedDBNames 是不能占用的库名（对齐 PostgreSQL 的模板库）。
var reservedDBNames = map[string]bool{"template0": true, "template1": true}

// OpenManager 打开数据目录下的所有库。
//
// 默认库打不开是致命错误（整个服务没法用）；某个普通库打不开只记一条警告，
// 不让一个坏库把服务拖垮 —— 那个库后续 Get 会报「does not exist」。
func OpenManager(root string, o Options) (*Manager, []error, error) {
	// 独占锁必须在读 catalog 之前拿到：否则两个进程会同时在内存里改表结构，
	// 各自 checkpoint，最后谁写坏了谁完全查不出来。
	lk, err := lockDataDir(root)
	if err != nil {
		return nil, nil, err
	}
	m := &Manager{root: root, opts: o, dbs: map[string]*DB{}, lock: lk}
	def, err := m.openAt(syscat.DefaultDatabaseName, root)
	if err != nil {
		// 默认库打不开时整个服务起不来，锁要放掉，否则用户改完配置重试会被
		// 自己上一把锁挡住（同进程内 flock 也会互斥）。
		lk.Release()
		return nil, nil, err
	}
	m.dbs[strings.ToLower(def.Name)] = def

	var warns []error
	dir := filepath.Join(root, "db")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			warns = append(warns, fmt.Errorf("读取 %s 失败: %w", dir, err))
		}
		return m, warns, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if name == strings.ToLower(syscat.DefaultDatabaseName) || !dbNameRe.MatchString(name) {
			continue
		}
		d, err := m.openAt(name, m.Dir(name))
		if err != nil {
			warns = append(warns, fmt.Errorf("打开库 %s 失败（已跳过）: %w", name, err))
			continue
		}
		m.dbs[name] = d
	}
	return m, warns, nil
}

func (m *Manager) openAt(name, dir string) (*DB, error) {
	o := m.opts
	o.Name = name
	// 把启动阶段带上库名，启动页能分清在恢复哪个库。
	if o.Progress != nil {
		base := o.Progress
		o.Progress = func(msg string) {
			if name == "" || strings.EqualFold(name, syscat.DefaultDatabaseName) {
				base(msg)
			} else {
				base(name + " · " + msg)
			}
		}
	}
	d, err := Open(dir, o)
	if err != nil {
		return nil, err
	}
	// 库列表是动态的（CREATE/DROP 会变），所以给函数而不是切片。
	d.Engine.Info.All = m.Names
	// CREATE/DROP DATABASE 是跨库操作，得回过来找 Manager
	d.mgr = m
	return d, nil
}

// Dir 返回某个库的数据目录。默认库就在 root 本身。
func (m *Manager) Dir(name string) string {
	n := strings.ToLower(name)
	if n == strings.ToLower(syscat.DefaultDatabaseName) {
		return m.root
	}
	return filepath.Join(m.root, "db", n)
}

// Root 返回数据根目录（配置文件也在这里）。
func (m *Manager) Root() string { return m.root }

// Default 返回默认库。永远非 nil（打不开时进程已经起不来了）。
func (m *Manager) Default() *DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dbs[strings.ToLower(syscat.DefaultDatabaseName)]
}

// Get 按名字取库。空名字表示默认库（客户端没在连接参数里指定 database 时）。
func (m *Manager) Get(name string) (*DB, error) {
	if strings.TrimSpace(name) == "" || name == "postgres" {
		// postgres 当作默认库的别名：客户端工具里默认填的就是它，
		// 报「库不存在」只会让人困惑。
		if d := m.Default(); d != nil {
			return d, nil
		}
	}
	key := strings.ToLower(name)
	m.mu.RLock()
	d := m.dbs[key]
	m.mu.RUnlock()
	if d == nil {
		return nil, fmt.Errorf(`database %q does not exist`, name)
	}
	return d, nil
}

// Exists 判断库是否存在（不区分大小写）。
func (m *Manager) Exists(name string) bool {
	_, err := m.Get(name)
	return err == nil
}

// Names 返回全部库名，默认库排第一，其余按名字排序。
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	def := strings.ToLower(syscat.DefaultDatabaseName)
	out := make([]string, 0, len(m.dbs))
	if d := m.dbs[def]; d != nil {
		out = append(out, d.Name)
	}
	var rest []string
	for k, d := range m.dbs {
		if k != def {
			rest = append(rest, d.Name)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Create 新建一个空库。目录建好、catalog 落盘失败时会清掉残留目录。
func (m *Manager) Create(name string) (*DB, error) {
	key, err := normalizeDBName(name)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	_, dup := m.dbs[key]
	m.mu.RUnlock()
	if dup {
		return nil, fmt.Errorf(`database %q already exists`, key)
	}
	// 建目录和写 catalog 都在锁外做（磁盘 IO 不该卡住别的库的 Get）
	dir := m.Dir(key)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf(`database %q already exists（目录 %s 已存在）`, key, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 先把空 catalog 写下去，Open 才有东西可读
	if err := catalog.New().Save(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	d, err := m.openAt(key, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.dbs[key]; dup {
		// 两个人同时 CREATE：后来者把刚打开的这个关掉，别把别人的库删了
		_ = d.Close()
		return nil, fmt.Errorf(`database %q already exists`, key)
	}
	m.dbs[key] = d
	return d, nil
}

// Drop 删除一个库。默认库不能删；还有活跃会话的库也不能删。
func (m *Manager) Drop(name string) error {
	key, err := normalizeDBName(name)
	if err != nil {
		return err
	}
	if key == strings.ToLower(syscat.DefaultDatabaseName) {
		return fmt.Errorf(`cannot drop the default database %q`, key)
	}
	m.mu.Lock()
	d := m.dbs[key]
	if d == nil {
		m.mu.Unlock()
		return fmt.Errorf(`database %q does not exist`, key)
	}
	if n := d.liveSessions(); n > 0 {
		m.mu.Unlock()
		return fmt.Errorf(`database %q is being accessed by other users（还有 %d 个进行中的事务）`, key, n)
	}
	delete(m.dbs, key)
	m.mu.Unlock()

	dir := m.Dir(key)
	if err := d.Close(); err != nil {
		// 关不上（比如落盘失败）就放回去，至少数据还在
		m.mu.Lock()
		m.dbs[key] = d
		m.mu.Unlock()
		return fmt.Errorf("关闭库 %s 失败: %w", key, err)
	}
	return os.RemoveAll(dir)
}

// Close 关闭所有库（每个库都会 Flush + 写快照）。
func (m *Manager) Close() error {
	m.mu.Lock()
	list := make([]*DB, 0, len(m.dbs))
	for _, d := range m.dbs {
		list = append(list, d)
	}
	m.mu.Unlock()

	var first error
	for _, d := range list {
		if err := d.Close(); err != nil && first == nil {
			first = err
		}
	}
	// 所有库都落盘关闭之后再放锁，否则新进程可能在半关状态里读走一份残缺数据。
	if err := m.lock.Release(); err != nil && first == nil {
		first = err
	}
	return first
}

// DBStat 是单个库的概况（控制台的库列表用）。
type DBStat struct {
	Name     string
	Default  bool
	Tables   int
	Rows     int
	Bytes    int64
	WALBytes int64
}

// List 返回每个库的概况，顺序和 Names 一致（默认库在前）。
//
// 这里会读每个库的表目录来算磁盘占用，所以只给「打开库管理面板」这类低频操作
// 用。顶栏那个下拉只要名字，走 Names() 就够了。
func (m *Manager) List() []DBStat {
	m.mu.RLock()
	list := make([]*DB, 0, len(m.dbs))
	for _, d := range m.dbs {
		list = append(list, d)
	}
	m.mu.RUnlock()

	def := strings.ToLower(syscat.DefaultDatabaseName)
	out := make([]DBStat, 0, len(list))
	for _, d := range list {
		info := d.Store.Info()
		out = append(out, DBStat{
			Name:     d.Name,
			Default:  strings.ToLower(d.Name) == def,
			Tables:   info.Tables,
			Rows:     info.Rows,
			Bytes:    info.Bytes,
			WALBytes: info.WALBytes,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Stats 汇总所有库的存储情况（设置页展示用）。
//
// 逐个库取 Info() 会把每个库的全部表目录扫一遍，所以只在「打开设置页」时调，
// 别放进查询路径。
type Stats struct {
	Databases int
	Tables    int
	Rows      int
	Bytes     int64
	WALBytes  int64
}

func (m *Manager) Stats() Stats {
	m.mu.RLock()
	list := make([]*DB, 0, len(m.dbs))
	for _, d := range m.dbs {
		list = append(list, d)
	}
	m.mu.RUnlock()

	out := Stats{Databases: len(list)}
	for _, d := range list {
		info := d.Store.Info()
		out.Tables += info.Tables
		out.Rows += info.Rows
		out.Bytes += info.Bytes
		out.WALBytes += info.WALBytes
	}
	return out
}

func normalizeDBName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !dbNameRe.MatchString(n) {
		return "", fmt.Errorf(`invalid database name %q：只允许小写字母、数字和下划线，`+
			`首字符不能是数字，最长 63 个字符`, name)
	}
	if reservedDBNames[n] {
		return "", fmt.Errorf(`database name %q is reserved`, n)
	}
	return n, nil
}

// liveSessions 数「真的有东西压着这个库」的会话：只看**进行中的事务**。
//
// 不能数空闲会话：Web 控制台的会话是按 cookie 建的，关掉浏览器不会通知服务端，
// 那些僵尸会话会让库永远删不掉（用户只能等重启）。空闲连接不挡删除 ——
// 库删掉之后它们的下一次操作会拿到 "database has been dropped"，不会写坏数据。
func (d *DB) liveSessions() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.sessions {
		if s.InTx() {
			n++
		}
	}
	return n
}

// ---------- CREATE / DROP DATABASE ----------

// createDatabase 实现 CREATE DATABASE。不能放在事务里（PostgreSQL 也不允许）。
func (d *DB) createDatabase(sess *txn.Session, s *sql.CreateDatabase) (*exec.Result, error) {
	if sess.InTx() {
		return nil, fmt.Errorf("CREATE DATABASE cannot run inside a transaction block")
	}
	if d.mgr == nil {
		return nil, fmt.Errorf("CREATE DATABASE 需要多库模式（单库测试环境不支持）")
	}
	// ENCODING 只有一种是能真的建出来的（UTF-8）。SQL_ASCII 之类也接受，
	// 但得让客户端知道实际编码，不然它会把字节当别的编码解释。
	if enc := strings.ToUpper(s.Options["encoding"]); enc != "" &&
		enc != "UTF8" && enc != "UTF-8" && enc != "UNICODE" && enc != "SQL_ASCII" {
		return nil, fmt.Errorf("encoding %q is not supported; godb only stores UTF-8", enc)
	}
	if _, err := d.mgr.Create(s.Name); err != nil {
		return nil, err
	}
	return &exec.Result{Command: "CREATE DATABASE", Tag: "CREATE DATABASE",
		Message: fmt.Sprintf("数据库 %s 已创建（空库，新连接指定 database=%s 即可使用）",
			strings.ToLower(s.Name), strings.ToLower(s.Name))}, nil
}

// dropDatabase 实现 DROP DATABASE。默认库和当前连着的库都不能删。
func (d *DB) dropDatabase(sess *txn.Session, s *sql.DropDatabase) (*exec.Result, error) {
	if sess.InTx() {
		return nil, fmt.Errorf("DROP DATABASE cannot run inside a transaction block")
	}
	if d.mgr == nil {
		return nil, fmt.Errorf("DROP DATABASE 需要多库模式（单库测试环境不支持）")
	}
	key := strings.ToLower(strings.TrimSpace(s.Name))
	if key == d.Name {
		return nil, fmt.Errorf("cannot drop the currently open database %q", key)
	}
	if !d.mgr.Exists(key) {
		if s.IfExists {
			return &exec.Result{Command: "DROP DATABASE", Tag: "DROP DATABASE"}, nil
		}
		return nil, fmt.Errorf("database %q does not exist", key)
	}
	if err := d.mgr.Drop(key); err != nil {
		return nil, err
	}
	return &exec.Result{Command: "DROP DATABASE", Tag: "DROP DATABASE"}, nil
}
