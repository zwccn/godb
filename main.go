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
	"syscall"
	"time"

	"godb/config"
	"godb/db"
	"godb/lock"
	"godb/netsrv"
	"godb/storage"
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
	lockTimeout := flag.Duration("lock-timeout", 3*time.Second, "等待表锁的最长时间")
	lockRetry := flag.Duration("lock-retry", 20*time.Millisecond, "等待表锁的轮询间隔")
	deadlockDetect := flag.Bool("deadlock-detect", true, "等待成环时让最年轻的事务提前放弃")
	deadlockAfter := flag.Duration("deadlock-after", 500*time.Millisecond, "等待多久后开始做死锁检测")
	flag.Parse()

	isolation, err := txn.ParseIsolation(*iso)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	d, err := db.Open(*dir, db.Options{
		Isolation: isolation,
		Lock: lock.Options{
			Timeout:        *lockTimeout,
			Retry:          *lockRetry,
			DeadlockDetect: *deadlockDetect,
			DeadlockAfter:  *deadlockAfter,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()

	// 配置：唯一来源是 <数据目录>/config.json。命令行里的地址只作为
	// 首次生成文件时的种子，之后以文件为准（下面会明确提示）。
	seed := config.Seed(*addr, *pgAddr)
	cfg, err := config.Load(*dir, seed, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}
	conf := cfg.Get()
	d.Store.SetCheckpointPolicy(storagePolicy(conf.Storage))

	// 打开 PG 查询日志（诊断开关，留在命令行）
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

	if len(d.Tables()) == 0 && !*noSample {
		init := d.Session("init")
		if err := d.LoadSample(init); err != nil {
			log.Printf("载入示例数据失败: %v", err)
		} else {
			log.Printf("已载入示例数据（departments / employees）")
		}
	}

	srv := web.New(d)
	mgr := netsrv.New(cfg, d, srv.Routes(), log.Printf, pgLogFile)
	srv.WithConfig(cfg, mgr)
	if err := mgr.Start(); err != nil {
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
	log.Printf("并发控制: %s（锁超时 %s / 轮询 %s / 死锁检测 %v）",
		isolation, *lockTimeout, *lockRetry, *deadlockDetect)
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
	if n := info.Repaired; n > 0 {
		log.Printf("注意：启动时修复了 %d 个单元格（表结构与磁盘数据不一致）", n)
	}
	if err := d.Store.LoadError(); err != nil {
		log.Printf("注意：读取表文件时出错: %v", err)
	}

	// 等待 Ctrl+C / SIGTERM，然后优雅退出（Flush 会把 WAL 刷盘并写快照）。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在保存数据...")
	mgr.Close()
	if err := d.Close(); err != nil {
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
