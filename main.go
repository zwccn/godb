// godb 是一个用 Go 从零实现的教学级关系数据库：
// 手写词法/语法分析 → 逻辑计划 → 火山模型执行 → 内存行存 + WAL 持久化，
// 并发控制采用表级 S/X 两阶段封锁。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"godb/config"
	"godb/db"
	"godb/lock"
	"godb/netsrv"
	"godb/storage"
	"godb/syscat"
	"godb/txn"
	"godb/web"
)

func main() {
	addr := flag.String("http", "127.0.0.1:8080", "HTTP 监听地址（仅在首次生成配置文件时生效）")
	dir := flag.String("data", "godb-data", "数据目录")
	noSample := flag.Bool("no-sample", false, "首次启动时不载入示例数据")
	pgAddr := flag.String("pg", "127.0.0.1:6432", "PostgreSQL 协议监听地址（仅在首次生成配置文件时生效，留空表示不启用）")
	pgLog := flag.String("pg-log", "", "把 PG 客户端发来的每条 SQL 追加写到该文件（排查兼容性问题用）")
	iso := flag.String("isolation", "table", "并发控制模式：table（表级 S/X 锁）或 serial（全局串行）")
	lockTimeout := flag.Duration("lock-timeout", 0, "等待表锁的最长时间（0 = 不限；仅在首次生成配置文件时生效）")
	lockRetry := flag.Duration("lock-retry", 20*time.Millisecond, "等待表锁的轮询间隔")
	deadlockDetect := flag.Bool("deadlock-detect", true, "等待成环时让最年轻的事务提前放弃")
	deadlockAfter := flag.Duration("deadlock-after", 500*time.Millisecond, "等待多久后开始做死锁检测")
	slow := flag.Duration("slow", 0, "把耗时超过该值的语句记进日志（如 200ms；0 表示不记录）")
	restoreTrash := flag.String("restore-trash", "", "从回收站恢复一次备份后退出：给备份目录名，或 latest 表示最新一份")
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionInfo())
		return
	}

	// 回收站恢复是离线操作：直接改磁盘上的 catalog.json 和表文件，
	// 运行中的进程内存里那份不会跟着变，所以必须在服务停止时跑。
	if *restoreTrash != "" {
		if err := runRestoreTrash(*dir, *restoreTrash); err != nil {
			fmt.Fprintf(os.Stderr, "恢复失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	isolation, err := txn.ParseIsolation(*iso)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	// 配置：唯一来源是 <数据目录>/config.json。命令行里的地址只作为
	// 首次生成文件时的种子，之后以文件为准（下面会明确提示）。
	//
	// 先读配置再开库：HTTP 监听地址在配置里，要在 WAL 重放之前就把端口
	// 占上并显示启动页（重放可能要几十秒，不能让浏览器一直连接被拒）。
	seed := config.Seed(*addr, *pgAddr, int(lockTimeout.Seconds()))
	cfg, err := config.Load(*dir, seed, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}
	conf := cfg.Get()

	// 启动门：HTTP 先起，未就绪时回启动页；OpenManager 完成后换上真路由。
	gate := web.NewBootGate()
	gate.SetPhase("正在启动", "准备打开数据目录")

	// 打开 PG 查询日志（诊断开关，留在命令行）。提前到这里打开，
	// 这样 netsrv.New 创建时就能带上。
	var pgLogFile *os.File
	if *pgLog != "" {
		f, err := os.OpenFile(*pgLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("无法打开 PG 查询日志 %s: %v", *pgLog, err)
		} else {
			pgLogFile = f
			defer f.Close()
		}
	}

	mgr := netsrv.New(cfg, nil, gate, log.Printf, pgLogFile)
	if err := mgr.StartHTTP(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	log.Printf("HTTP 已就绪（启动页）: %s —— 正在恢复数据…", mgr.State().HTTPAddr)

	// 多库：默认库在 <data> 根，其它库在 <data>/db/<名字>/
	gate.SetPhase("打开数据库", "载入表结构")
	dbs, warns, err := db.OpenManager(*dir, db.Options{
		Isolation: isolation,
		Lock: lock.Options{
			Timeout:        lockWaitTimeout(conf.Timeouts.LockWaitSec),
			Retry:          *lockRetry,
			DeadlockDetect: *deadlockDetect,
			DeadlockAfter:  *deadlockAfter,
		},
		Progress:        func(msg string) { gate.SetPhase("打开数据库", msg) },
		BufferPoolPages: conf.Storage.BufferPoolPages,
	})
	if err != nil {
		gate.SetPhase("启动失败", err.Error())
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		// 启动页留在端口上一会儿，让人能看到错误；然后照常退出。
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}
	defer dbs.Close()
	for _, w := range warns {
		log.Printf("注意: %v", w)
	}
	d := dbs.Default()
	d.Store.SetCheckpointPolicy(storagePolicy(conf.Storage))
	d.SetMaxConcurrent(conf.Performance.MaxConcurrent, conf.Performance.MaxQueued, 0)
	d.SetTimeouts(db.TimeoutStatus{
		SlowSQLSec:    conf.Timeouts.SlowSQLSec,
		SlowTxIdleSec: conf.Timeouts.SlowTxIdleSec,
		LockWaitSec:   conf.Timeouts.LockWaitSec,
		QueueWaitSec:  conf.Timeouts.QueueWaitSec,
	})

	// 命令行 -lock-timeout 只作种子：配置文件里已经有等锁超时，之后以文件为准。
	if *lockTimeout != 0 && int(lockTimeout.Seconds()) != conf.Timeouts.LockWaitSec {
		log.Printf("注意: -lock-timeout 只在首次生成配置时生效，当前以配置文件为准（等锁超时 %s）",
			secLabel(conf.Timeouts.LockWaitSec))
	}

	// 慢语句日志：诊断开关，走命令行，不进配置文件。
	if *slow > 0 {
		d.SlowThreshold = *slow
		d.SlowLog = func(text string, took time.Duration, stmts int) {
			log.Printf("慢语句 %s（%d 条）：%s",
				took.Truncate(time.Millisecond), stmts, oneLine(text, 400))
		}
	}

	if len(d.Tables()) == 0 && !*noSample {
		// 只有「目录里根本没有 catalog.json」才算新库。
		// 原来判的是 len(Tables()) == 0，等于把「catalog 被清空 / 读空」也当成新库：
		// 一次异常重启就会把真实数据覆盖成样例（2026-10-28 真踩过，丢了两张表）。
		if hasCatalog(*dir) {
			log.Printf("注意: %s 里没有任何表，但 catalog.json 存在 —— 不自动载入示例数据。"+
				"需要样例请用控制台的「载入示例数据」，误删可从 %s 恢复",
				*dir, filepath.Join(*dir, storage.TrashDirName()))
		} else {
			gate.SetPhase("载入示例数据", "departments / employees")
			init := d.Session("init")
			if err := d.LoadSample(init); err != nil {
				log.Printf("载入示例数据失败: %v", err)
			} else {
				log.Printf("已载入示例数据（departments / employees）")
			}
		}
	}

	gate.SetPhase("启动服务", "绑定 PG 协议")
	srv := web.New(d)
	srv.DBs = dbs
	mgr.SetDBs(dbs)
	srv.WithConfig(cfg, mgr)
	// 换上真正的控制台路由：同一个端口，启动页原地切换到正式界面。
	gate.SetReady(srv.Routes())
	if err := mgr.StartPG(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	conf = cfg.Get()
	state := mgr.State()

	log.Printf("godb 启动完成，用时 %s", time.Since(start).Truncate(time.Millisecond))
	log.Printf("配置文件: %s", cfg.Path())
	if seed.Network.HTTPAddr() != conf.Network.HTTPAddr() && *addr != "" {
		log.Printf("注意：命令行 -http %s 已被配置文件覆盖（现为 %s），请到设置页修改",
			seed.Network.HTTPAddr(), conf.Network.HTTPAddr())
	}
	if seed.Network.PGAddr() != conf.Network.PGAddr() && *pgAddr != "" {
		log.Printf("注意：命令行 -pg %s 已被配置文件覆盖（现为 %s），请到设置页修改",
			seed.Network.PGAddr(), conf.Network.PGAddr())
	}
	// 这里打的是**配置文件里的生效值**，不是命令行那个种子 —— 否则重启后
	// 日志会写「锁超时 0s」，而实际在按文件里的值跑。
	log.Printf("并发控制: %s（等锁超时 %s / 轮询 %s / 死锁检测 %v）",
		isolation, secLabel(conf.Timeouts.LockWaitSec), *lockRetry, *deadlockDetect)
	log.Printf("控制台: http://%s（%s）", state.HTTPAddr, listenScope(conf.Network.HTTPRemote))
	if state.PGEnabled {
		auth := "trust 认证，任意用户名、无密码"
		if conf.Auth.RequirePassword {
			auth = fmt.Sprintf("密码认证（用户 %s）", conf.Auth.User)
		}
		log.Printf("PG 协议: %s（%s；%s）", state.PGAddr, listenScope(conf.Network.PGRemote), auth)
	} else {
		log.Printf("PG 协议: 未启用")
	}
	log.Printf("落盘策略: %s", describePolicy(conf.Storage))

	info := d.Store.Info()
	log.Printf("数据目录: %s（%d 张表 / %d 行，已加载 %d 张，数据 %.1f MB，WAL %d 字节）",
		*dir, info.Tables, info.Rows, info.Loaded,
		float64(info.Bytes)/(1<<20), info.WALBytes)
	if names := dbs.Names(); len(names) > 1 {
		log.Printf("数据库: %d 个（%s）；默认库 %s，其它库在 %s/db/ 下",
			len(names), strings.Join(names, ", "), dbs.Default().Name, *dir)
	} else {
		log.Printf("数据库: 只有默认库 %s（CREATE DATABASE 可新建，放在 %s/db/ 下）",
			dbs.Default().Name, *dir)
	}
	if n := info.Repaired; n > 0 {
		log.Printf("注意：启动时修复了 %d 个单元格（表结构与磁盘数据不一致）", n)
	}
	if broken := d.Store.BrokenTables(); len(broken) > 0 {
		// 这些表不是「空的」，是「打不开」。查它们会直接报错，直到 DROP 掉重来。
		// 启动日志必须说清楚，否则用户看到的就是「我的表怎么没数据了」。
		log.Printf("⚠ 有 %d 张表的数据读不出来（表定义还在，数据文件丢了或坏了）：", len(broken))
		for name, err := range broken {
			log.Printf("   · %s —— %v", name, err)
		}
		log.Printf("   这些表暂时不能查也不能写（会直接报错，避免把空数据写回去）。")
		log.Printf("   先停服务备份整个数据目录，再决定是 DROP 重建还是从备份恢复。")
	} else if err := d.Store.LoadError(); err != nil {
		log.Printf("注意：读取表文件时出错: %v", err)
	}

	// 等待 Ctrl+C / SIGTERM，然后优雅退出（Flush 会把 WAL 刷盘并写快照）。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在保存数据...")
	mgr.Close()
	if err := dbs.Close(); err != nil {
		log.Printf("退出时保存失败: %v", err)
	}
	log.Printf("已退出")
}

func storagePolicy(s config.Storage) storage.CheckpointPolicy {
	return storage.CheckpointPolicy{
		Commits:  s.CheckpointCommits,
		Seconds:  s.CheckpointSeconds,
		WALBytes: int64(s.CheckpointWALMB) << 20,
	}
}

func listenScope(remote bool) string {
	if remote {
		return "允许远程访问"
	}
	return "仅本机"
}

func describePolicy(s config.Storage) string {
	out := ""
	add := func(v string) {
		if out != "" {
			out += "，"
		}
		out += v
	}
	if s.CheckpointCommits > 0 {
		add(fmt.Sprintf("每 %d 次提交", s.CheckpointCommits))
	}
	if s.CheckpointSeconds > 0 {
		add(fmt.Sprintf("每 %d 秒", s.CheckpointSeconds))
	}
	if s.CheckpointWALMB > 0 {
		add(fmt.Sprintf("WAL 超过 %d MB", s.CheckpointWALMB))
	}
	if out == "" {
		return "仅手动 CHECKPOINT（自动落盘已全部关闭）"
	}
	return out + "（任一满足即落盘）"
}

// versionInfo 是 -version 的输出。git 提交靠 Go 编进二进制的 VCS 信息，
// 本机因为上级目录有 svn 是用 -buildvcs=false 编的，所以可能读不到。
func versionInfo() string {
	out := fmt.Sprintf("godb %s\n%s\n%s", syscat.GodbVersion, syscat.ServerVersionLine, runtime.Version())
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				out += "\ncommit " + s.Value
			case "vcs.time":
				out += "\nbuilt  " + s.Value
			}
		}
	}
	return out
}

// hasCatalog 判断数据目录里是否已有表结构文件。
//
// 「目录里有没有 catalog.json」才是「这是不是一个新库」的判据。用表数判断会把
// 「catalog 被清空 / 解析出 0 张表」也当成新库，从而静默覆盖真实数据。
func hasCatalog(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "catalog.json"))
	return err == nil
}

// runRestoreTrash 从回收站恢复一次备份并打印结果。恢复后需要重启服务才能看到表。
func runRestoreTrash(dir, entry string) error {
	log.Printf("数据目录: %s", dir)
	// 先抢独占锁：拿不到说明还有 godb 在跑，这时改磁盘上的表文件会被它
	// 内存里那份反向覆盖 —— 所以「请确认服务已停止」改成硬性检查，不靠人记。
	unlock, err := db.LockDataDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	rep, err := db.RestoreTrash(dir, entry)
	if err != nil {
		return err
	}
	log.Printf("备份 %s（%s，原因 %s）", rep.Entry, rep.At, rep.Reason)
	if len(rep.Restored) == 0 {
		log.Printf("没有需要恢复的表")
	}
	for _, t := range rep.Restored {
		if t.Bytes > 0 {
			log.Printf("  已恢复 %s（%d 行，%s）", t.Name, t.Rows, humanBytes(t.Bytes))
		} else {
			log.Printf("  已恢复 %s（仅表结构，没有数据文件）", t.Name)
		}
	}
	for _, name := range rep.Skipped {
		log.Printf("  跳过 %s（同名表已存在，未覆盖）", name)
	}
	if len(rep.Restored) > 0 {
		log.Printf("完成。代次已对齐到 %d，重启 godb 即可看到这些表", rep.Generation)
	}
	return nil
}

// humanBytes 把字节数格式化成便于阅读的形式（日志用）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// oneLine 把多行 SQL 压成一行，便于日志里看。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// lockWaitTimeout 把「秒，0 = 不限」翻译成 lock.Options.Timeout。
// lock 包把 <= 0 都当不限，这里用 -1 明确表达，避免 0 被误当成「没设置」
// 而被 db.Open 换成默认 3 秒。
func lockWaitTimeout(sec int) time.Duration {
	if sec <= 0 {
		return -1
	}
	return time.Duration(sec) * time.Second
}

// secLabel 把秒数渲染成人话（0 显示成「不限」），给启动日志用。
func secLabel(sec int) string {
	if sec <= 0 {
		return "不限"
	}
	return (time.Duration(sec) * time.Second).String()
}
