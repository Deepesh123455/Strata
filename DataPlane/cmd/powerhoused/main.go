package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/applier"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/engine"
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

// resolveMaxMemory reads the memory cap from the environment, in bytes.
func resolveMaxMemory() int64 {
	v := os.Getenv(maxMemoryEnv)
	if v == "" {
		return 0
	}
	mb, err := strconv.ParseInt(v, 10, 64)
	if err != nil || mb <= 0 {
		fmt.Printf("[WARN] ignoring invalid %s=%q\n", maxMemoryEnv, v)
		return 0
	}
	return mb * 1024 * 1024
}

func main() {
	fmt.Println("========================================")
	fmt.Println("    POWERHOUSE CACHE - BOOT SEQUENCE    ")
	fmt.Println("========================================")

	// 1. Boot up the 32-Shard Memory Engine
	maxMem := resolveMaxMemory()
	if maxMem > 0 {
		fmt.Printf("[SYSTEM] Allocating 32-shard memory map (maxmemory: %d MB, LRU eviction)...\n", maxMem/(1024*1024))
	} else {
		fmt.Println("[SYSTEM] Allocating 32-shard memory map (maxmemory: unlimited)...")
	}
	cacheEngine := engine.NewPowerhouseCache(maxMem)

	// 2. RECOVERY: replay the WAL into the engine BEFORE accepting any traffic.
	// This rebuilds everything that was durable at the last shutdown/crash and
	// truncates any torn tail left by an unclean exit. Returns the last
	// sequence number so the live log continues the monotonic sequence.
	fmt.Println("[SYSTEM] Replaying write-ahead log...")
	lastSeq, err := applier.LoadFromWAL(walPath, cacheEngine)
	if err != nil {
		fmt.Printf("[FATAL] WAL recovery failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[SYSTEM] Recovery complete. Last sequence: %d\n", lastSeq)

	// 3. Open the live WAL for appends (everysec fsync) and build the applier.
	walLog, err := wal.Open(walPath, lastSeq, wal.SyncEverySec)
	if err != nil {
		fmt.Printf("[FATAL] Failed to open WAL: %v\n", err)
		os.Exit(1)
	}
	app := applier.New(cacheEngine, walLog)
	fmt.Println("[SYSTEM] Write-ahead log online (fsync: everysec).")

	// 4. Initialize the TCP Edge
	fmt.Println("[SYSTEM] Initializing TCP Edge server...")
	tcpServer := server.NewServer(Port, app)

	// 5. Start the server in the background
	// We run this in a Goroutine so it doesn't block our main thread!
	go func() {
		if err := tcpServer.Start(); err != nil {
			fmt.Printf("[FATAL] Server crashed: %v\n", err)
			os.Exit(1)
		}
	}()

	// 6. The OS Intercept (Graceful Shutdown)
	// We create a channel to listen for Linux/Windows termination signals.
	quit := make(chan os.Signal, 1)

	// SIGINT = Ctrl+C
	// SIGTERM = Docker/Kubernetes shutdown signal
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// 7. The Main Thread goes to sleep here, waiting for a shutdown signal.
	<-quit

	// 8. We caught a signal! Drain connections, then flush + fsync + close the
	// WAL so no acknowledged write is lost on a clean shutdown.
	tcpServer.Stop()
	if err := walLog.Close(); err != nil {
		fmt.Printf("[ERROR] WAL close failed: %v\n", err)
	} else {
		fmt.Println("[SYSTEM] Write-ahead log flushed and closed.")
	}
}
