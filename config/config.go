// Package config 保存 godb 的运行时配置。
//
// 配置的**唯一来源**是数据目录下的 config.json：Web 设置页读写它，
// 命令行参数只在文件第一次生成时作为种子，之后不再参与 —— 这样就不会
// 出现「命令行改了却没生效」这种困惑（启动时会明确打印一行提示）。
//
// 不在配置里的东西：数据目录本身（必须在打开数据库之前确定）、
// 隔离级别与锁参数（改它们等价于换一套并发语义，属于启动期决定）。
package config

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// 当前配置文件的版本号。将来字段语义变化时用它做迁移。
//
// v2：排队超时从 performance.waitTimeoutMs 挪到 timeouts.queueWaitSec，
// 并把锁等待超时也收进 timeouts（监控面板要能开关全部四个超时）。
//
// v3：等锁 / 排队的出厂默认从 3 秒改成「不限」（0）。监控面板上四个超时
// 现在出厂全是「不限」——它们都是「主动回滚或放弃用户请求」的保护，
// 默认打开等于替用户做决定。
//
// v4：缓冲池默认从 1024 页（8MB）提到 4096 页（32MB）。
const Version = 4

// Storage 控制 checkpoint（落盘）的触发策略。
//
// 注意这里配的是 **checkpoint 频率**，不是 fsync。提交时的 WAL fsync
// 永远是同步做的（组提交只是把并发事务的 fsync 合并），关掉它就等于
// 放弃「COMMIT 成功即持久」，所以不提供开关。
type Storage struct {
	// CheckpointCommits：每多少次提交触发一次，0 表示关闭这条。
	CheckpointCommits int `json:"checkpointCommits"`
	// CheckpointSeconds：每隔多少秒触发一次（有改动才真的写），0 表示关闭。
	CheckpointSeconds int `json:"checkpointSeconds"`
	// CheckpointWALMB：WAL 增长到多少 MB 触发，0 表示关闭。
	CheckpointWALMB int `json:"checkpointWalMB"`
	// BufferPoolPages 是**全库共享**的缓冲池页数（8KB/页，所有表加在一起）。
	//
	// 它决定「同时能有多少个热页留在内存里」：池子满了以后，最近没被
	// 碰过的页会被淘汰、对应的行从内存里放掉，下次访问再从页文件读回来。
	// 所以它约等于「能从磁盘省掉的页命中预算」，不是某一张表的配额 ——
	// 一张很大的表可以把整个池子占满。
	//
	// 0 表示用默认值（4096 页 = 32MB）。**改完要重启才生效**（缓冲池在
	// 表第一次打开时创建），不像落盘节奏那样能热改。
	BufferPoolPages int `json:"bufferPoolPages"`
}

// Network 控制两个监听端口。
//
// Remote 为 false 时绑定 127.0.0.1（只有本机能连），为 true 时绑定
// 0.0.0.0（同网段都能连）。默认都是 false。
type Network struct {
	HTTPPort   int  `json:"httpPort"`
	HTTPRemote bool `json:"httpRemote"`
	PGEnabled  bool `json:"pgEnabled"`
	PGPort     int  `json:"pgPort"`
	PGRemote   bool `json:"pgRemote"`

	// SSLEnabled 打开 PostgreSQL 协议的 TLS。证书留空时用数据目录里
	// 自签的那张（首次启动自动生成）。
	SSLEnabled  bool   `json:"sslEnabled"`
	SSLCertFile string `json:"sslCertFile,omitempty"`
	SSLKeyFile  string `json:"sslKeyFile,omitempty"`
}

// Auth 控制 PostgreSQL 协议的密码认证。
//
// 只保护 PG 协议：Web 控制台不登录（它是本机管理界面，且配置的写操作
// 本身也只接受来自 127.0.0.1 的请求）。
type Auth struct {
	RequirePassword bool   `json:"requirePassword"`
	User            string `json:"user"`
	// PasswordHash 是 PBKDF2 编码串，永不出现在任何 API 响应里。
	PasswordHash string `json:"passwordHash,omitempty"`
	// PasswordMD5 是 md5(md5(password+user))，供 md5 挑战应答使用（同样是秘密）。
	PasswordMD5 string `json:"passwordMd5,omitempty"`
	// ScramVerifier 是 SCRAM-SHA-256 的验证串（salt / StoredKey / ServerKey）。
	ScramVerifier string `json:"scramVerifier,omitempty"`
}

// Performance 控制执行并发。
type Performance struct {
	// MaxConcurrent：同时执行 SQL 的最大并发数，超出的排队等。
	// 0 表示不限制。防止大量并发大查询把内存吃光（godb 全内存驻留）。
	MaxConcurrent int `json:"maxConcurrent"`
	// MaxQueued：排队上限，超过就快速失败（背压）。
	// 0 = 不限排队（只靠 WaitTimeout 兜底）。
	MaxQueued int `json:"maxQueued"`

	// WaitTimeoutMs 已废弃（v2 起由 Timeouts.QueueWaitSec 承担）。
	// 保留字段只为读得懂 v1 的配置文件，Normalize 会把它搬走后清零。
	WaitTimeoutMs int `json:"waitTimeoutMs,omitempty"`
}

// Timeouts 是监控面板底部的四个超时保护。
//
// 全部 0 = 不限：不限制并发、不主动回滚，让用户看着面板上的时长分布
// 自己决定开哪一个、设多少。它们都是「保护」而不是「限制」，默认不开。
//
// 语义区分（这块最容易搞混）：
//   - SlowSQLSec：单条语句（整批）执行超过它就回滚所在事务。引擎没有语句级
//     中断点，所以是「跑完再回滚」，不打断正在消耗 CPU 的那条语句。
//   - SlowTxIdleSec：事务空闲（距最后一条 SQL）超过它就回滚 —— 兜住
//     「SQL 都快、但事务不提交」的情况，和 SlowSQLSec 不是一回事。
//   - LockWaitSec：等表锁的最长时间，超过则放弃整个事务。
//   - QueueWaitSec：排队等执行槽位的最长时间，超过则快速失败（背压）。
type Timeouts struct {
	SlowSQLSec    int `json:"slowSqlSec"`
	SlowTxIdleSec int `json:"slowTxIdleSec"`
	LockWaitSec   int `json:"lockWaitSec"`
	QueueWaitSec  int `json:"queueWaitSec"`
}

// Config 是完整的配置文件内容。
type Config struct {
	Version     int         `json:"version"`
	Storage     Storage     `json:"storage"`
	Network     Network     `json:"network"`
	Auth        Auth        `json:"auth"`
	Performance Performance `json:"performance"`
	Timeouts    Timeouts    `json:"timeouts"`
}

// Default 返回出厂默认值。
func Default() Config {
	return Config{
		Version: Version,
		Storage: Storage{
			CheckpointCommits: 128,
			CheckpointSeconds: 60,
			CheckpointWALMB:   64,
			BufferPoolPages:   4096,
		},
		Network: Network{
			HTTPPort:   8080,
			HTTPRemote: false,
			PGEnabled:  true,
			PGPort:     6432,
			PGRemote:   false,
		},
		Auth: Auth{
			RequirePassword: false,
			User:            "godb",
		},
		Performance: Performance{
			MaxConcurrent: 32,
			MaxQueued:     256,
		},
		Timeouts: Timeouts{
			// 四个超时出厂全部「不限」：它们都是「主动回滚别人事务」或
			// 「直接拒绝请求」的开关，出厂就打开容易误伤长事务、批量导入
			// 和首次建索引这类慢但是正常的操作。
			// 卡住的时候面板上能直接看到谁在等、等了多久，再按需开。
			SlowSQLSec:    0,
			SlowTxIdleSec: 0,
			// 等锁 / 排队也默认不限：死锁检测（等待图找环）仍然在跑，
			// 互相等待的环会立刻打散，不需要靠超时兜底。
			LockWaitSec:  0,
			QueueWaitSec: 0,
		},
	}
}

// HTTPAddr 返回 HTTP 监听地址。
func (n Network) HTTPAddr() string { return listenAddr(n.HTTPPort, n.HTTPRemote) }

// PGAddr 返回 PG 协议监听地址（未启用时为空串）。
func (n Network) PGAddr() string {
	if !n.PGEnabled {
		return ""
	}
	return listenAddr(n.PGPort, n.PGRemote)
}

func listenAddr(port int, remote bool) string {
	host := "127.0.0.1"
	if remote {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Validate 检查配置是否可用。返回的错误会直接显示在设置页上。
func (c *Config) Validate() error {
	if c.Storage.CheckpointCommits < 0 {
		return fmt.Errorf("「每多少次提交落盘」不能为负数")
	}
	if c.Storage.CheckpointCommits > 1000000 {
		return fmt.Errorf("「每多少次提交落盘」不能超过 1000000")
	}
	if c.Storage.CheckpointSeconds < 0 || c.Storage.CheckpointSeconds > 86400 {
		return fmt.Errorf("「每隔多少秒落盘」必须在 0 到 86400 之间")
	}
	if c.Storage.CheckpointWALMB < 0 || c.Storage.CheckpointWALMB > 65536 {
		return fmt.Errorf("「WAL 超过多少 MB 落盘」必须在 0 到 65536 之间")
	}

	if c.Performance.MaxConcurrent < 0 || c.Performance.MaxConcurrent > 10000 {
		return fmt.Errorf("「最大并发数」必须在 0 到 10000 之间（0 = 不限）")
	}
	if c.Performance.MaxQueued < 0 || c.Performance.MaxQueued > 100000 {
		return fmt.Errorf("「排队上限」必须在 0 到 100000 之间（0 = 不限）")
	}
	// 四个超时：0 = 不限，上限 24 小时（再大的值等于没设，但不必禁止）。
	for _, f := range []struct {
		name string
		v    int
	}{
		{"慢 SQL 超时", c.Timeouts.SlowSQLSec},
		{"慢事务超时", c.Timeouts.SlowTxIdleSec},
		{"等锁超时", c.Timeouts.LockWaitSec},
		{"排队超时", c.Timeouts.QueueWaitSec},
	} {
		if f.v < 0 || f.v > 86400 {
			return fmt.Errorf("「%s」必须在 0 到 86400 秒之间（0 = 不限）", f.name)
		}
	}

	if err := checkPort("HTTP 端口", c.Network.HTTPPort); err != nil {
		return err
	}
	if c.Network.PGEnabled {
		if err := checkPort("PostgreSQL 端口", c.Network.PGPort); err != nil {
			return err
		}
		if c.Network.PGPort == c.Network.HTTPPort {
			return fmt.Errorf("PostgreSQL 端口不能和 HTTP 端口相同（都是 %d）", c.Network.HTTPPort)
		}
	}

	c.Auth.User = strings.TrimSpace(c.Auth.User)
	if c.Auth.RequirePassword {
		if c.Auth.User == "" {
			return fmt.Errorf("启用密码认证时必须填写用户名")
		}
		if c.Auth.PasswordHash == "" {
			return fmt.Errorf("启用密码认证时必须设置密码")
		}
		if !LooksHashed(c.Auth.PasswordHash) {
			return fmt.Errorf("密码哈希格式不对，请通过设置页重新设置密码")
		}
	}
	c.Normalize()
	return nil
}

func checkPort(label string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s %d 不合法（必须在 1-65535 之间）", label, p)
	}
	return nil
}

// Normalize 补齐零值字段并去掉空白。
func (c *Config) Normalize() {
	if c.Version <= 0 {
		c.Version = 1 // 没有版本号的都是 v1，走下面的迁移
	}
	if c.Version < 2 {
		// v1 → v2：把「旧版的实际行为」固化成显式配置，别让升级悄悄放宽保护。
		//
		// 1) 排队超时：v1 存在 performance.waitTimeoutMs，缺失时为 0；
		//    而旧代码在 <= 0 时会兜底 3 秒 —— 所以 0 要迁成 3，不是「不限」。
		if c.Timeouts.QueueWaitSec == 0 {
			sec := (c.Performance.WaitTimeoutMs + 500) / 1000
			if sec < 1 {
				sec = 3
			}
			c.Timeouts.QueueWaitSec = sec
		}
		// 2) 等锁超时：v1 里来自命令行 -lock-timeout，默认 3 秒。
		if c.Timeouts.LockWaitSec == 0 {
			c.Timeouts.LockWaitSec = 3
		}
		// 3) 并发上限：早期版本的配置文件里根本没有 performance 段，
		//    读出来是 0（= 不限）。这里补成出厂默认 32/256 ——
		//    「不限并发」不是当初的意图，只是字段还没被写进文件。
		if c.Performance.MaxConcurrent == 0 && c.Performance.MaxQueued == 0 {
			def := Default().Performance
			c.Performance = def
		}
		c.Performance.WaitTimeoutMs = 0
		c.Version = 2
	}
	if c.Version < 3 {
		// v2 → v3：等锁 / 排队的出厂默认从 3 秒改成「不限」。
		//
		// v2 的 Default() 会把 3 原样写进每一个配置文件，于是「没动过」和
		// 「手设 3 秒」在文件里长得一模一样。这里把恰好等于 3 的当成没动过
		// 迁成 0，其它值（2、5、30…）原样保留 —— 那是用户真的选了。
		//
		// 想恢复 3 秒：到监控面板底部把数字填回去即可，会写进 config.json。
		if c.Timeouts.LockWaitSec == 3 {
			c.Timeouts.LockWaitSec = 0
		}
		if c.Timeouts.QueueWaitSec == 3 {
			c.Timeouts.QueueWaitSec = 0
		}
		c.Version = 3
	}
	if c.Version < 4 {
		// v3 → v4：缓冲池默认从 1024 页（8MB）提到 4096 页（32MB）。
		//
		// 和 v3 一样的问题：v3 的 Default() 把 1024 写进了每个配置文件，
		// 「没动过」和「特意设成 8MB」在文件里看不出区别。恰好等于 1024
		// 的当没动过迁到新默认，其它值（含 0 = 用默认）原样保留。
		// 只影响下次启动 —— 缓冲池在表第一次打开时创建，不热改。
		if c.Storage.BufferPoolPages == 1024 {
			c.Storage.BufferPoolPages = 4096
		}
		c.Version = Version
	}
	c.Auth.User = strings.TrimSpace(c.Auth.User)
}

// StorageWarnings 返回「能跑但可能不是你想要的」提示。
func (c *Config) StorageWarnings() []string {
	var out []string
	if c.Storage.CheckpointCommits == 0 && c.Storage.CheckpointSeconds == 0 && c.Storage.CheckpointWALMB == 0 {
		out = append(out, "三条落盘条件全部关闭：只有执行 CHECKPOINT 或正常退出时才会写快照，WAL 会一直增长。")
	}
	if c.Storage.CheckpointSeconds > 0 && c.Storage.CheckpointSeconds < 5 {
		out = append(out, "落盘间隔短于 5 秒时，checkpoint 会频繁重写有改动的表。")
	}
	return out
}

// Seed 用启动参数构造「第一次生成配置文件」时使用的初始值。
//
// httpAddr / pgAddr 是命令行传进来的 host:port，仅用于推导初始端口与
// 远程开关；文件存在之后它们就被忽略。
func Seed(httpAddr, pgAddr string, lockWaitSec int) Config {
	c := Default()
	if lockWaitSec >= 0 {
		c.Timeouts.LockWaitSec = lockWaitSec
	}
	if p, remote, ok := parseAddr(httpAddr); ok {
		c.Network.HTTPPort, c.Network.HTTPRemote = p, remote
	}
	if strings.TrimSpace(pgAddr) == "" {
		c.Network.PGEnabled = false
	} else if p, remote, ok := parseAddr(pgAddr); ok {
		c.Network.PGPort, c.Network.PGRemote = p, remote
	}
	return c
}

// parseAddr 把 "127.0.0.1:8080" / ":8080" / "0.0.0.0:8080" 拆成端口与远程开关。
func parseAddr(addr string) (port int, remote bool, ok bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0, false, false
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false, false
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return 0, false, false
	}
	host, _, _ := net.SplitHostPort(addr)
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return p, false, true
	default:
		return p, true, true
	}
}

// SortedKeys 供测试与诊断使用。
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
