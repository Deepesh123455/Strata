package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/applier"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/engine"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/logger"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/server"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/wal"
)

// The standard Redis port is 6379.
// By using this, standard Redis clients will think we are a native Redis server!
const Port = ":6379"

// walPath is where the write-ahead log lives. Restarting the process replays
// this file to rebuild the keyspace.
const walPath = "./data/powerhouse.wal"

// maxMemoryEnv is the env var that sets the global memory cap, in megabytes.
// 0 / unset means unlimited. On the 1GB AWS Free Tier box, set this (e.g. 700)
// to keep the process from being OOM-killed.
const maxMemoryEnv = "POWERHOUSE_MAXMEMORY_MB"

// Build metadata, injected at link time by the Docker build / CI via
//
//	-ldflags "-X main.version=... -X main.gitSHA=... -X main.buildDate=..."
//
// They default to placeholders for `go run` / local builds. Stamping the binary
// makes every running container traceable back to an exact git commit, which is
// the auditability/rollback story in DEPLOYMENT.md.
var (
	version   = "dev"
	gitSHA    = "unknown"
	buildDate = "unknown"
)

// resolveMaxMemory reads the memory cap in bytes, preferring the CLI flag
// (in MB) over the environment variable. A non-positive flag value falls back
// to POWERHOUSE_MAXMEMORY_MB; if that is unset/invalid, the cache is unlimited.
func resolveMaxMemory(flagMB int64) int64 {
	if flagMB > 0 {
		return flagMB * 1024 * 1024
	}

	v := os.Getenv(maxMemoryEnv)
	if v == "" {
		return 0
	}
	mb, err := strconv.ParseInt(v, 10, 64)
	if err != nil || mb <= 0 {
		logger.Warn("ignoring invalid maxmemory env var", "env", maxMemoryEnv, "value", v)
		return 0
	}
	return mb * 1024 * 1024
}

func main() {
	// -maxmemory sets the global memory cap in MEGABYTES. A positive value wins
	// over POWERHOUSE_MAXMEMORY_MB; 0 (default) means unlimited unless the env
	// var is set. flag.Parse must run before we read the value.
	maxMemMB := flag.Int64("maxmemory", 0, "global memory cap in MB (0 = unlimited)")
	flag.Parse()

	// Bring the structured logger online before anything else logs. Level/format
	// come from LOG_LEVEL / LOG_FORMAT (default info/text); prod sets LOG_FORMAT=json.
	logger.InitFromEnv()

	// The ASCII banner is decorative dev sugar — keep it only in text mode so JSON
	// log streams stay clean and machine-parseable.
	if logger.TextMode() {
		fmt.Println("========================================")
		fmt.Println("    POWERHOUSE CACHE - BOOT SEQUENCE    ")
		fmt.Println("========================================")
	}
	logger.Info("starting powerhouse cache",
		"version", version, "git_sha", gitSHA, "build_date", buildDate, "pid", os.Getpid())

	// 1. Boot up the 32-Shard Memory Engine
	maxMem := resolveMaxMemory(*maxMemMB)
	if maxMem > 0 {
		logger.Info("allocating memory engine",
			"shards", 32, "maxmemory_mb", maxMem/(1024*1024), "eviction", "lru")
	} else {
		logger.Info("allocating memory engine", "shards", 32, "maxmemory", "unlimited")
	}
	cacheEngine := engine.NewPowerhouseCache(maxMem)

	// 2. RECOVERY: replay the WAL into the engine BEFORE accepting any traffic.
	// This rebuilds everything that was durable at the last shutdown/crash and
	// truncates any torn tail left by an unclean exit. Returns the last
	// sequence number so the live log continues the monotonic sequence.
	logger.Info("replaying write-ahead log", "path", walPath)
	lastSeq, err := applier.LoadFromWAL(walPath, cacheEngine)
	if err != nil {
		logger.Fatal("wal recovery failed", "path", walPath, "err", err)
	}
	logger.Info("recovery complete", "last_seq", lastSeq)

	// 3. Open the live WAL for appends (everysec fsync) and build the applier.
	walLog, err := wal.Open(walPath, lastSeq, wal.SyncEverySec)
	if err != nil {
		logger.Fatal("failed to open wal", "path", walPath, "err", err)
	}
	app := applier.New(cacheEngine, walLog)
	logger.Info("write-ahead log online", "fsync", "everysec")

	// 4. Initialize the TCP Edge
	logger.Info("initializing tcp edge", "addr", Port)
	tcpServer := server.NewServer(Port, app)

	// 5. Start the server in the background
	// We run this in a Goroutine so it doesn't block our main thread!
	go func() {
		if err := tcpServer.Start(); err != nil {
			logger.Fatal("server crashed", "err", err)
		}
	}()

	// 6. The OS Intercept (Graceful Shutdown)
	// We create a channel to listen for Linux/Windows termination signals.
	quit := make(chan os.Signal, 1)

	// SIGINT = Ctrl+C
	// SIGTERM = Docker/Kubernetes shutdown signal
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// 7. The Main Thread goes to sleep here, waiting for a shutdown signal.
	sig := <-quit
	logger.Info("shutdown signal received", "signal", sig.String())

	// 8. We caught a signal! Drain connections, then flush + fsync + close the
	// WAL so no acknowledged write is lost on a clean shutdown.
	tcpServer.Stop()
	if err := walLog.Close(); err != nil {
		logger.Error("wal close failed", "err", err)
	} else {
		logger.Info("write-ahead log flushed and closed")
	}
	logger.Info("powerhouse cache stopped")
}
