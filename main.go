// godb 是一个用 Go 从零实现的教学级关系数据库：
// 手写词法/语法分析 → 逻辑计划 → 火山模型执行 → 内存行存 + WAL 持久化，
// 并发控制采用表级 S/X 两阶段封锁。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"godb/db"
	"godb/lock"
	"godb/pgwire"
	"godb/txn"
	"godb/web"
)

func main() {
	addr := flag.String("http", "127.0.0.1:8080", "HTTP 监听地址")
	dir := flag.String("data", "godb-data", "数据目录")
	noSample := flag.Bool("no-sample", false, "首次启动时不载入示例数据")
	pgAddr := flag.String("pg", "127.0.0.1:6432", "PostgreSQL 协议监听地址（留空则不启用）")
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

	if len(d.Tables()) == 0 && !*noSample {
		init := d.Session("init")
		if err := d.LoadSample(init); err != nil {
			log.Printf("载入示例数据失败: %v", err)
		} else {
			log.Printf("已载入示例数据（departments / employees）")
		}
	}

	if *pgAddr != "" {
		pg := &pgwire.Server{DB: d, Addr: *pgAddr}
		if *pgLog != "" {
			f, err := os.OpenFile(*pgLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				log.Printf("无法打开 PG 查询日志 %s: %v", *pgLog, err)
			} else {
				defer f.Close()
				pg.QueryLog = f
				log.Printf("PG 查询日志: %s", *pgLog)
			}
		}
		go func() {
			if err := pg.ListenAndServe(); err != nil {
				log.Printf("PostgreSQL 协议监听失败: %v", err)
			}
		}()
	}

	srv := web.New(d)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("godb 启动完成，用时 %s", time.Since(start).Truncate(time.Millisecond))
	log.Printf("并发控制: %s（锁超时 %s / 轮询 %s / 死锁检测 %v）",
		isolation, *lockTimeout, *lockRetry, *deadlockDetect)
	log.Printf("控制台: http://%s", *addr)
	if *pgAddr != "" {
		log.Printf("PG 协议: %s（trust 认证，任意用户名）", *pgAddr)
	}
	log.Printf("数据目录: %s", *dir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP 服务异常: %v\n", err)
		os.Exit(1)
	}
}
