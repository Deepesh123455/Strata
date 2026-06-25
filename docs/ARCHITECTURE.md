# Powerhouse Cache — Architecture Deep Dive

> A from-scratch, Redis-wire-compatible in-memory cache written in Go.
> Goal: a self-hosted, durable, horizontally-shardable cache that replaces a
> managed service (Upstash) on commodity hardware (e.g. a 1 GB AWS Free Tier box).

This document is the **single source of truth for how the code works**. It covers
the High-Level Design (HLD), the Low-Level Design (LLD), every package, every
file, and every exported (and most unexported) functions, plus the data and
control flows that connect them. The companion document
[`DEPLOYMENT.md`](./DEPLOYMENT.md) covers shipping it to production.

---

## Table of Contents

1. [What this is, in one paragraph](#1-what-this-is-in-one-paragraph)
2. [High-Level Design (HLD)](#2-high-level-design-hld)
3. [Repository & module layout](#3-repository--module-layout)
4. [The request lifecycle (end-to-end trace)](#4-the-request-lifecycle-end-to-end-trace)
5. [Package-by-package, file-by-file, function-by-function](#5-package-by-package-file-by-file-function-by-function)
   - [5.1 `cmd/powerhoused` — the binary / boot sequence](#51-cmdpowerhoused--the-binary--boot-sequence)
   - [5.2 `internal/engine` — the 32-shard memory engine](#52-internalengine--the-32-shard-memory-engine)
   - [5.3 `internal/resp` — the RESP protocol parser](#53-internalresp--the-resp-protocol-parser)
   - [5.4 `internal/pool` — the buffer pool](#54-internalpool--the-buffer-pool)
   - [5.5 `internal/server` — the TCP edge](#55-internalserver--the-tcp-edge)
   - [5.6 `internal/wal` — the write-ahead log](#56-internalwal--the-write-ahead-log)
   - [5.7 `internal/applier` — the durability boundary](#57-internalapplier--the-durability-boundary)
   - [5.8 `tests` — the test suite](#58-tests--the-test-suite)
6. [Cross-cutting concerns](#6-cross-cutting-concerns)
   - [6.1 Concurrency & locking model](#61-concurrency--locking-model)
   - [6.2 Memory accounting & LRU eviction](#62-memory-accounting--lru-eviction)
   - [6.3 Expiry: lazy + active](#63-expiry-lazy--active)
   - [6.4 Durability & crash recovery](#64-durability--crash-recovery)
   - [6.5 Zero-allocation hot path](#65-zero-allocation-hot-path)
   - [6.6 Security posture of the data plane](#66-security-posture-of-the-data-plane)
7. [Known limitations & the road ahead](#7-known-limitations--the-road-ahead)
8. [Glossary](#8-glossary)

---

## 1. What this is, in one paragraph

Powerhouse Cache is a TCP server that speaks the **RESP** (REdis Serialization
Protocol) wire format on port `6379`, so any off-the-shelf Redis client
(`redis-cli`, `ioredis`, `go-redis`, …) can talk to it unmodified. Internally it
stores key/value pairs in a **sharded, lock-striped in-memory hash map** (32
shards), supports **TTL expiry** (lazy + active sweeping), enforces a **global
memory budget with approximated-LRU eviction**, and persists every mutation to a
**CRC-checked, sequence-numbered write-ahead log (WAL)** so the keyspace survives
restarts and crashes. The architecture is intentionally factored so that
replication / Raft can later be slotted in at one seam (the `applier`) without
touching the engine or the server.

Supported commands today: `SET` (with `EX`/`PX`), `GET`, `DEL`, `EXPIRE`,
`PEXPIRE`, `TTL`, `PTTL`, `PERSIST`.

---

## 2. High-Level Design (HLD)

### 2.1 The four layers

```
            ┌───────────────────────────────────────────────────────────────┐
            │                      Redis clients (any)                       │
            │             redis-cli · go-redis · ioredis · jedis             │
            └───────────────────────────────┬───────────────────────────────┘
                                             │  TCP :6379, RESP framing
   ┌─────────────────────────────────────────▼──────────────────────────────────┐
   │  EDGE LAYER  ── internal/server                                              │
   │  • Accept loop (1 goroutine) → 1 goroutine per connection                    │
   │  • Pooled read buffer, buffered writer, pipelining, read deadlines           │
   │  • internal/resp parses RESP frames into zero-copy [][]byte arg slices       │
   │  • Per-connection panic recovery, graceful drain on shutdown                 │
   └─────────────────────────────────────────┬──────────────────────────────────┘
                                             │  parsed command → method call
   ┌─────────────────────────────────────────▼──────────────────────────────────┐
   │  DURABILITY BOUNDARY  ── internal/applier                                    │
   │  • Reads pass straight through to the engine (no disk)                       │
   │  • Writes: normalize TTL→absolute deadline, apply to engine, append to WAL   │
   │  • THE one seam where consensus/replication will later wrap each write       │
   └──────────────────────┬───────────────────────────────┬───────────────────────┘
                          │ in-memory apply               │ append record
   ┌───────────────────────▼─────────────────────┐  ┌──────▼───────────────────────┐
   │  ENGINE  ── internal/engine                  │  │  WAL  ── internal/wal         │
   │  • 32 shards, each an RWMutex + map          │  │  • single ordered byte stream │
   │  • FNV-1a hash → shard selection             │  │  • [magic|seq|crc32c|len|payload]
   │  • lazy + active TTL expiry                  │  │  • everysec group-commit fsync│
   │  • per-shard memory budget + sampled LRU     │  │  • torn-tail truncation on    │
   │    (internal/pool feeds it nothing; pool is  │  │    replay (crash recovery)    │
   │     edge-only)                               │  │                               │
   └──────────────────────────────────────────────┘  └───────────────────────────────┘
```

### 2.2 Design principles (the "why")

| Principle | Manifestation in code |
|---|---|
| **Wire compatibility beats novelty** | RESP on `:6379`; clients can't tell us apart from Redis. |
| **Concurrency through sharding, not one big lock** | 32 independent `Shard`s, each with its own `sync.RWMutex`. Contention is divided ~32×. |
| **One ordered write stream** | A *single* global WAL (not per-shard) so it can double as the replication log later. |
| **One mutation seam** | The `applier` is the only place a write becomes durable. Raft wraps *here*, nothing else moves. |
| **Pragmatic durability** | `everysec` fsync (Redis default): ≤1 s of acknowledged writes at risk on crash, high throughput. |
| **Zero-allocation hot path** | Pooled read buffers, in-place upper-casing, stack-allocated reply headers, zero-copy parse slices. |
| **Bounded everything** | Max bulk length, max array length, max command size, max payload, max memory — all capped to resist hostile input and OOM. |
| **Crash is a normal event** | Replay tolerates and truncates a torn tail; it is *not* treated as an error. |

### 2.3 Process & goroutine model

At runtime a single `powerhoused` process runs these goroutines:

- **1 main goroutine** — boots, then blocks on the OS signal channel awaiting shutdown.
- **1 accept-loop goroutine** — `Server.Start()` running `listener.Accept()` forever.
- **N connection-worker goroutines** — one per connected client (`handleConnection`).
- **1 expiry-worker goroutine** — sweeps all 32 shards every 100 ms and ticks the LRU clock.
- **1 WAL sync goroutine** — group-commit fsync every 1 s (only under `SyncEverySec`).

Everything is coordinated by channels (`quit`) and a `sync.WaitGroup` for
graceful shutdown.

---

## 3. Repository & module layout

```
Redis-Cache/
├── go.work                     # Go workspace; pins ./DataPlane as a module
├── .gitignore                  # Go + OS + secrets + redis data-file ignores
├── data/
│   └── powerhouse.wal          # the live write-ahead log (runtime artifact)
└── DataPlane/                  # the "data plane" module (the cache server itself)
    ├── go.mod                  # module github.com/Deepesh123455/Redis-Cache/DataPlane
    ├── cmd/
    │   └── powerhoused/
    │       └── main.go         # process entrypoint / boot sequence
    ├── internal/               # private packages (not importable outside the module)
    │   ├── applier/
    │   │   └── applier.go      # durability boundary: apply + WAL append + recovery
    │   ├── engine/
    │   │   ├── cache.go        # PowerhouseCache: orchestrates 32 shards + LRU clock
    │   │   ├── shard.go        # Shard: the actual map, locking, memory accounting, eviction
    │   │   ├── expiry.go       # background active-expiry worker
    │   │   └── hash.go         # FNV-1a hash for shard selection
    │   ├── pool/
    │   │   └── buffer.go       # sync.Pool of 4 KB read buffers
    │   ├── resp/
    │   │   ├── parser.go       # RESP array/bulk-string parser (zero-copy)
    │   │   └── errors.go       # (package decl placeholder)
    │   ├── server/
    │   │   ├── tcp.go          # listener, accept loop, connection registry, shutdown
    │   │   └── worker.go       # per-connection read loop + command dispatch
    │   └── wal/
    │       ├── wal.go          # Log: append-only writer, framing, fsync policy
    │       └── replay.go       # Replay: read records, verify CRC, truncate torn tail
    └── tests/                  # all tests, centralized in one package
        ├── engine_test.go      # shard + cache unit/integration tests
        ├── eviction_test.go    # maxmemory / LRU tests
        ├── parser_test.go      # RESP parser table tests
        ├── applier_test.go     # WAL round-trip + torn-tail recovery
        └── server_test.go      # end-to-end server tests over net.Pipe
```

**Why a `DataPlane` module + `go.work`?** The naming anticipates a future
**control plane** (multi-tenant routing, auth, billing, a management API — the
"Strata" product the GitHub remote is named for). The workspace lets you add
sibling modules (`ControlPlane/`, `Proxy/`, …) later without restructuring.

**Why `internal/`?** Go enforces that anything under `internal/` can only be
imported by code rooted at the parent of `internal/`. This guarantees no external
consumer accidentally couples to the engine/WAL internals — the *only* public
surface is the binary itself.

---

## 4. The request lifecycle (end-to-end trace)

Follow a single `SET name deepesh EX 60` from socket to disk and back.

```
1.  Client writes RESP bytes to the socket:
        *5\r\n$3\r\nSET\r\n$4\r\nname\r\n$7\r\ndeepesh\r\n$2\r\nEX\r\n$2\r\n60\r\n

2.  server/worker.go  handleConnection()
      • conn.SetReadDeadline(now+5m)         ← DoS watchdog
      • n, _ := conn.Read(buffer[unread:])    ← one syscall into a pooled 4 KB buffer

3.  server/worker.go  inner parse loop
      • resp.Parse(dataStream[cursor:])       ← returns [][]byte arg slices that POINT
                                                 INTO the read buffer (zero copy),
                                                 plus 'consumed' byte count
      • Loops to drain ALL complete commands in the packet (pipelining)

4.  server/worker.go  executeCommand(w, cmdSlices)
      • upperInPlace(cmdSlices[0]) → "SET"
      • parses EX/PX → ttl = 60 * time.Second
      • calls s.app.Set(key, value, ttl)

5.  applier/applier.go  (*Applier).Set()
      • deadline = time.Now().Add(60s)        ← relative TTL → ABSOLUTE deadline
      • cache.SetWithDeadline(key, value, deadline)   ← apply to memory FIRST
      • rec = encodeCommand("SET", key, value, "PXAT", <deadline-ms>)
      • log.Append(rec)                        ← append to WAL (buffered)

6.  engine/cache.go  (*PowerhouseCache).SetWithDeadline()
      • shard = GetShard(key)  → fnv32a(key) % 32
      • shard.SetWithDeadline(string(key), value, deadline)

7.  engine/shard.go  (*Shard).SetWithDeadline()
      • COPIES value out of the pooled buffer (it will be recycled!)
      • builds *Entry, stores atime = LRU clock
      • updates memBytes accounting
      • evictIfNeeded(key)                     ← sampled-LRU if over budget

8.  wal/wal.go  (*Log).Append()
      • seq++; frames [magic|seq|crc32c|len|payload]; writes to 1 MB bufio buffer
      • under SyncEverySec, the fsync happens ~1 s later on the sync goroutine

9.  Back up the stack → worker writes "+OK\r\n" into the buffered writer w

10. After the whole packet is drained, worker calls w.Flush() ONCE
        → a single socket syscall returns all batched replies to the client
```

A `GET name` is the same path minus steps 5b/8 — reads never touch the WAL and
never take a write lock (read lock only).

---

## 5. Package-by-package, file-by-file, function-by-function

### 5.1 `cmd/powerhoused` — the binary / boot sequence

**File: `cmd/powerhoused/main.go`** — the only `package main`. It wires every
subsystem together in a deliberate order and owns the process lifecycle.

**Top-level constants**

- `Port = ":6379"` — the standard Redis port, so existing clients connect with zero config.
- `walPath = "./data/powerhouse.wal"` — where durability lives; relative to the process CWD.
- `maxMemoryEnv = "POWERHOUSE_MAXMEMORY_MB"` — env var for the memory cap (MB); `0`/unset = unlimited.

**`resolveMaxMemory(flagMB int64) int64`**
Resolves the effective memory budget **in bytes**. Precedence: a positive
`-maxmemory` CLI flag (MB) wins; otherwise it falls back to the env var; an
invalid env value logs a `[WARN]` and is treated as unlimited (`0`). Converts MB
→ bytes (`× 1024 × 1024`). This single helper centralizes the "flag beats env,
both optional" policy so the rest of the program just sees a byte count.

**`main()`** — the boot sequence, in 8 explicit, ordered steps:

1. **Parse flags.** `-maxmemory` (MB). `flag.Parse()` must run before reading the value.
2. **Boot the engine.** `engine.NewPowerhouseCache(maxMem)` allocates the 32 shards, splitting the budget evenly.
3. **RECOVERY — replay the WAL BEFORE accepting traffic.** `applier.LoadFromWAL(walPath, cacheEngine)` rebuilds the keyspace from the durable prefix of the log and truncates any torn tail. It returns `lastSeq`, the highest recovered sequence number, so the live log continues monotonically. A failure here is `[FATAL]` → `os.Exit(1)` (better to refuse to start than to serve a corrupt keyspace).
4. **Open the live WAL.** `wal.Open(walPath, lastSeq, wal.SyncEverySec)` opens the file in append mode and starts the 1 s group-commit goroutine.
5. **Build the applier.** `applier.New(cacheEngine, walLog)` — binds engine + log into the mutation boundary.
6. **Build the server.** `server.NewServer(Port, app)`.
7. **Start the server in a goroutine** so `main` doesn't block; a server crash is `[FATAL]`.
8. **Block on OS signals.** A buffered `chan os.Signal` is registered for `SIGINT` (Ctrl-C) and `SIGTERM` (Docker/Kubernetes stop). `<-quit` parks the main goroutine.

   On signal: `tcpServer.Stop()` drains connections, then `walLog.Close()`
   flushes + fsyncs + closes the log so **no acknowledged write is lost on a
   clean shutdown**. The ordering (drain clients → close WAL) matters: clients
   must stop producing writes before the log is sealed.

> **Connection to the rest of the system:** `main.go` is the composition root. It
> is the only place that knows about *all* of engine, applier, wal, and server at
> once. Every other package depends "downward" only.

---

### 5.2 `internal/engine` — the 32-shard memory engine

The engine is a **pure in-memory data structure**. It knows nothing about TCP,
RESP, or the WAL. That isolation is what makes it trivially unit-testable and is
why the applier (not the engine) owns durability.

#### File: `engine/hash.go`

**`fnv32a(key []byte) uint32`**
A hand-rolled **FNV-1a 32-bit** hash. Starts from the FNV offset basis
(`2166136261`), and for each byte: `hash ^= byte; hash *= 16777619` (FNV prime).
Zero allocations, no `hash/fnv` interface overhead. Isolated in its own file so
the hash can be swapped (e.g. for xxHash) without touching cache logic. Its
output feeds shard selection. FNV-1a gives good avalanche/distribution for short
keys, which keeps the 32 shards evenly loaded — important because per-shard
memory budgeting only approximates a global cap *if* keys spread evenly.

#### File: `engine/cache.go`

**`const ShardCount = 32`** — fixed, and a power of two (good for alignment; lets
`% 32` be a cheap mask in principle). Trades a little memory for ~32× less lock
contention.

**`type PowerhouseCache struct`**

- `shards [ShardCount]*Shard` — fixed array of 32 shard pointers. A *value* array
  (not a slice) so the backing storage is contiguous and there's no slice-header
  indirection.
- `clock atomic.Int64` — the **coarse, cache-wide LRU clock**. Advanced once per
  expiry tick (every 100 ms), read on every access. Coarse on purpose: a precise
  per-op timestamp would force a write or contended atomic on every read; a
  shared monotonic counter is cheap and "recent enough" for sampled LRU. Redis
  uses the same coarse-clock trick.

**`NewPowerhouseCache(maxMemoryBytes int64) *PowerhouseCache`**
Constructs the cache. If a budget is given, it's split evenly: `perShard =
maxMemoryBytes / 32` (floored to ≥1). Each shard then independently enforces its
own slice **under its own lock — no global eviction lock exists**, which is what
keeps eviction fully concurrent. Builds all 32 shards via `newShardBudgeted`,
handing each a pointer to the shared `clock`.

**`MemoryUsage() int64`**
Sums `memBytes` across all 32 shards, taking each shard's **read** lock briefly.
Used by tests and (later) an `INFO`-style stat. O(32), cheap.

**`TickClock()`**
`clock.Add(1)`. Called by the expiry worker once per tick. This is the *only*
writer of the clock, so a plain atomic add is sufficient.

**`GetShard(key []byte) *Shard`**
The routing function: `fnv32a(key) % 32` → `shards[index]`. Every public
operation begins here. Deterministic: the same key always maps to the same shard,
which is what makes per-shard locking correct.

**The public API** — each method is a thin router that delegates to the chosen
shard, converting `[]byte` keys to `string` map keys at the boundary:

| Method | Delegates to | Notes |
|---|---|---|
| `Set(key, value, ttl)` | `shard.Set` | `ttl == 0` ⇒ no expiry. |
| `SetWithDeadline(key, value, expiresAt)` | `shard.SetWithDeadline` | Absolute deadline; used by **WAL recovery** so expiry survives restart. |
| `Get(key) ([]byte, bool)` | `shard.Get` | Triggers lazy expiry inside the shard. |
| `Delete(key) bool` | `shard.Delete` | `true` if it existed. |
| `TTL(key) time.Duration` | `shard.TTL` | Redis sentinels: `-2s` missing, `-1s` no-expiry. |
| `Persist(key) bool` | `shard.Persist` | Strips TTL. |
| `Expire(key, ttl) bool` | `shard.Expire` | Relative TTL on existing key. |
| `ExpireAt(key, expiresAt) bool` | `shard.ExpireAt` | Absolute deadline; used by recovery. |

> **Why both relative and absolute variants?** The live path takes relative TTLs
> from clients. Recovery replays *absolute* deadlines so a key written with `EX
> 60` an hour before a crash doesn't get a fresh 60 s lease on restart — it stays
> expired. Persisting absolute time is the linchpin of correct cross-restart expiry.

#### File: `engine/shard.go` — the heart of the engine

**Constants**

- `approxEntryOverhead = 64` — flat per-entry byte cost charged on top of key+value
  (the `Entry` struct, map bucket slot, slice/string headers). Memory accounting
  only needs to be *proportional*, not exact, for the cap to trip at a sane point.
- `evictionSampleSize = 5` — how many keys are sampled per eviction round. This is
  the **approximated-LRU** knob: bigger samples ≈ truer LRU but more CPU. Redis
  defaults to 5 too.

**`type Entry struct`**

- `Value []byte` — the stored value (always a *copy*, never aliasing a network buffer).
- `ExpiresAt time.Time` — zero value ⇒ never expires.
- `atime atomic.Int64` — last-access tick (the LRU clock value at last touch).

  Entries are stored as **`*Entry` pointers** specifically so a read can refresh
  `atime` with a single atomic store **without taking the write lock or rewriting
  the map slot** — preserving multi-reader concurrency.

**`(e *Entry) isExpired() bool`** — `!ExpiresAt.IsZero() && time.Now().After(ExpiresAt)`. The expiry predicate used everywhere.

**`type Shard struct`**

- `lock sync.RWMutex` — many readers OR one writer.
- `items map[string]*Entry` — the actual store.
- `memBytes int64` — running approximate live byte count, maintained incrementally
  on every insert/delete/evict. Guarded by `lock`.
- `maxBytes int64` — this shard's slice of the global budget; `0` ⇒ unbounded.
- `clock *atomic.Int64` — pointer to the cache-wide LRU clock (may be `nil` for a
  standalone test shard, in which case `atime` stays `0` and eviction degrades to
  bounded ~random sampling — still correct, just less LRU-biased).

**Constructors**

- `NewShard()` — unbounded, no clock. The simplest path; used by unit tests.
- `newShardBudgeted(maxBytes, clock)` — wires up budget + shared clock; used by the cache.

**Helpers**

- `nowTick() int64` — current LRU clock value, or `0` if no clock is wired.
- `entrySize(key, e) int64` — `len(key) + cap(e.Value) + 64`. Uses `cap` (not
  `len`) of the value because that's the memory actually held.

**Write path**

- **`Set(key, value, ttl)`** — converts a relative `ttl` to an absolute deadline
  (zero ttl ⇒ zero deadline) and delegates to `SetWithDeadline`.
- **`SetWithDeadline(key, value, expiresAt)`** — *the canonical write*:
  1. **Clone the value** into a fresh `[]byte`. Critical: the source slice may
     alias a pooled network buffer that gets recycled the instant the connection
     reads more data. Skipping this copy would be a use-after-free / data-corruption bug.
  2. Build the `*Entry`, take the write lock.
  3. Record `atime = nowTick()`.
  4. Memory accounting: subtract the old entry's size if overwriting, add the new size.
  5. `evictIfNeeded(key)` — enforce the budget, protecting the just-written key
     from being its own victim (also prevents an infinite loop when one value
     alone exceeds the whole budget).

**Read path**

- **`Get(key) ([]byte, bool)`** — two-phase, lock-minimizing:
  - *Phase 1 (read lock):* look up. Missing ⇒ return `(nil, false)`. Present and
    **not** expired ⇒ refresh `atime` (atomic store, safe under RLock since the
    map slot isn't mutated), grab the value, return.
  - *Phase 2 (write lock):* if expired, drop the read lock, take the write lock,
    **re-check** (another goroutine may have deleted/refreshed it in the gap),
    then evict and return `(nil, false)`. This is **lazy expiry** — the client
    never sees a stale value, and the double-check makes the lock upgrade race-safe.
- **`Has(key) bool`** — read-lock membership test that **does not** trigger lazy
  expiry. An introspection helper so tests can verify the *active* expiry worker
  (not `Get`) removed a key.

**Delete & TTL management**

- **`Delete(key) bool`** — write lock; if present, decrement `memBytes` and
  `delete`. Returns whether it existed.
- **`TTL(key) time.Duration`** — read lock. Redis sentinel convention: `-2s` if
  missing, `-1s` if no expiry, else `time.Until(deadline)`. If the remaining time
  is already ≤0 (expired but not yet swept), reports `-2s` (treated as missing).
- **`Persist(key) bool`** — write lock; zeroes `ExpiresAt`. Returns `false` if the
  key is missing, expired, or already persistent.
- **`Expire(key, ttl) bool`** — sugar over `ExpireAt(key, now+ttl)`.
- **`ExpireAt(key, expiresAt) bool`** — write lock. If missing ⇒ `false`. If
  already expired ⇒ evict it and return `false` (you can't set a TTL on a corpse).
  Otherwise set the new deadline and return `true`. The absolute-deadline form is
  what recovery replays.

**Eviction & active expiry**

- **`EvictExpired() int`** — the **active-expiry** sweep. Snapshots `time.Now()`
  once, takes the write lock, iterates the whole map, deletes every expired entry,
  fixes `memBytes`, returns the count. Called every 100 ms per shard by the worker.
- **`evictIfNeeded(protect string)`** — budget enforcement under approximated LRU.
  Must hold the write lock. While `memBytes > maxBytes`: sample an LRU victim
  (excluding `protect`), delete it, fix accounting. Stops if nothing is evictable.
- **`sampleLRU(protect string) string`** — inspects up to `evictionSampleSize`
  keys (Go randomizes map iteration, so "first N" *is* a random sample) and
  returns the one with the smallest `atime` (oldest access). This is the
  Redis-style approximate LRU: no global recency list (which would force a write
  lock on every read), just a cheap random sample.

#### File: `engine/expiry.go`

**`const expiryTickInterval = 100 * time.Millisecond`** — sweep cadence;
sub-second expiry accuracy at negligible CPU.

**`(c *PowerhouseCache) StartExpiryWorker(quit chan struct{})`**
Launches the background goroutine. Each tick it (1) `TickClock()` — advancing LRU
recency — and (2) `runExpiryPass()`. Exits cleanly when `quit` is closed, tying
its lifecycle to server shutdown.

**`(c *PowerhouseCache) runExpiryPass()`**
Iterates all 32 shards calling `EvictExpired()`. Non-blocking overall: each shard
holds *its own* write lock only for the duration of *its own* sweep, so a slow
shard never blocks the others' readers.

> **Why both lazy and active expiry?** Lazy expiry (in `Get`) reclaims a key the
> moment someone looks at it, but a key written with a TTL and never read again
> would leak memory forever. The active sweep guarantees bounded staleness and
> reclaims that memory. Together they mirror Redis's dual strategy.

---

### 5.3 `internal/resp` — the RESP protocol parser

#### File: `resp/errors.go`
Currently just `package resp` (a placeholder; `ErrIncomplete` actually lives in
`parser.go`). Reserved for future protocol error types.

#### File: `resp/parser.go`

**`var ErrIncomplete`** — sentinel meaning "the TCP packet was split; wait for
more bytes." The worker treats this specially (break and re-read), distinct from
a real protocol error (drop the client).

**Security caps**

- `maxBulkLen = 512 << 20` (512 MB) — mirrors the Redis value ceiling.
- `maxArrayLen = 1 << 20` — bounds the element count of one command.

These exist to stop a hostile client declaring a near-`MaxInt` length: without
the cap, the later `cursor+strLen+2` arithmetic could **overflow to a negative
index**, slip past the bounds check, and **panic the whole server**. Bounding the
declared length keeps that arithmetic safe and rejects garbage early. (There's a
dedicated regression test, `TestServer_OversizedBulkLenRejected`.)

**`parseLen(b []byte) (int, error)`**
Hand-rolled ASCII-int parser (no `strconv` allocation). Handles a leading `-`,
rejects empty / "-only" / non-digit fields, and detects overflow by checking
`nextVal < val` after each `val = val*10 + digit`. Returns the signed integer.

**`Parse(buffer []byte) ([][]byte, int, error)`** — the core. Parses **one**
complete RESP command (a RESP *array of bulk strings*) from the front of
`buffer`, returning the arg slices, the number of bytes consumed, and an error.
The arg slices **point directly into `buffer`** — zero allocation, zero copy.

Algorithm:
1. Must start with `*` (array marker); else protocol error.
2. Read up to `\r` → `argCount` via `parseLen`. No `\r` yet ⇒ `ErrIncomplete`.
3. `argCount == -1` ⇒ null array (returns `nil` args, valid). Negative-other or
   `> maxArrayLen` ⇒ protocol error.
4. For each of `argCount` arguments:
   - Expect `$` (bulk-string marker).
   - Read length up to `\r` (`ErrIncomplete` if not present yet).
   - `strLen == -1` ⇒ null bulk string (append `nil`). Negative-other ⇒ error.
   - `strLen > maxBulkLen` ⇒ error **before** any arithmetic (overflow guard).
   - Bounds check: need `strLen + 2` more bytes; if not present ⇒ `ErrIncomplete`.
   - Verify the trailing `\r\n` actually terminates the bulk string; else protocol error.
   - Slice `buffer[cursor:cursor+strLen]` as the arg (the zero-copy "window frame").
5. Return args + the final `cursor` (consumed byte count) so the worker knows
   exactly where the next pipelined command begins.

> **The single most important property here** is that `Parse` is a *pure,
> incremental, allocation-free* frame decoder. Every defensive branch maps to a
> real attack or a real TCP fragmentation case, and each has a test in
> `parser_test.go`.

---

### 5.4 `internal/pool` — the buffer pool

#### File: `pool/buffer.go`

**`const BufferSize = 4096`** — initial per-connection read buffer (4 KB, a common
page-friendly size that covers the vast majority of commands in one read).

**`var bufferPool = sync.Pool{...}`** — a `sync.Pool` whose `New` allocates a
`*[]byte` of `BufferSize`. Pooling `*[]byte` (a pointer) rather than `[]byte`
avoids the slice-header boxing allocation that `sync.Pool` would otherwise incur
on every `Put`.

**`Get() *[]byte` / `Put(b *[]byte)`** — lease and return. The worker leases one
buffer per connection on entry and returns it on `defer`. This recycles read
buffers across the lifetime of the process so steady-state request handling does
**no heap allocation** for the read path — the GC pressure (and tail-latency GC
pauses) that would otherwise come from per-request buffers simply doesn't exist.

> **Subtlety that ties pool → engine:** because the parser hands out slices that
> alias this pooled buffer, the engine **must** copy values on `Set`
> (`shard.SetWithDeadline` does). The pool's recycling is exactly *why* that copy
> is mandatory.

---

### 5.5 `internal/server` — the TCP edge

#### File: `server/tcp.go` — listener, lifecycle, connection registry

**`type Server struct`**

- `listenAddr string`, `listener net.Listener` — the bind address and live socket.
- `app *applier.Applier` — the durability boundary + engine (the server never
  touches the engine or WAL directly; it only knows the applier).
- `wg sync.WaitGroup` — counts active client connections for graceful drain.
- `quit chan struct{}` — closed on shutdown; signals every internal loop.
- `connsMu sync.Mutex` + `conns map[net.Conn]struct{}` — registry of live
  connections so `Stop()` can force-close them to unblock their read loops.

**`NewServer(listenAddr, app)`** — constructor; initializes `quit` and `conns`.

**`registerConn` / `deregisterConn`** — mutex-guarded add/remove from the registry.

**`Start() error`** — the accept loop:
1. `app.StartExpiryWorker(s.quit)` — launches the background sweeper, lifecycle-tied to `quit`.
2. `net.Listen("tcp", addr)` — opens the socket.
3. Loop on `listener.Accept()`. On error, distinguish *intentional* shutdown
   (`<-s.quit` ⇒ return cleanly) from a transient accept error (log and continue).
4. On a new connection: **`wg.Add(1)` synchronously, before** spawning the
   goroutine. Doing the `Add` on the accept goroutine (not inside the worker)
   guarantees it *happens-before* `Stop()`'s `wg.Wait()`, eliminating the classic
   "Add races a zero counter" shutdown bug. Then `go handleConnection(conn)`.

**`ServeConn(conn net.Conn)`** — the same `Add(1)+handle` pairing exposed for
tests / alternative transports (e.g. a `net.Pipe`). The server tests use this to
drive a worker over an in-memory pipe with no real socket.

**`Stop()`** — graceful shutdown, in order:
1. `close(s.quit)` — signal all loops.
2. `listener.Close()` — stop accepting new connections.
3. Force-close every registered connection to unblock workers parked in `conn.Read`.
4. `wg.Wait()` — block until every in-flight command finishes (prevents
   mid-command corruption during a deploy).

#### File: `server/worker.go` — the per-connection engine room

**Constants**

- `maxCommandSize = 512 << 20` — hard ceiling on how large one pipelined command
  may grow the per-connection buffer; bounds memory against a hostile client
  while still allowing 512 MB Redis-sized values.
- `writeBufSize = 16 << 10` (16 KB) — the per-connection buffered-writer size.
  Responses for a whole pipelined batch accumulate here and flush in **one
  syscall**, collapsing up to `3N` writes (a GET writes header+value+CRLF) down to ~1.

**Preallocated reply literals** — `respOK`, `respNullBulk`, `respOne`, `respZero`,
`respCRLF` are built **once at startup** and shared (written, never mutated)
across all connection goroutines. Zero per-command allocation for the common replies.

**`upperInPlace(b []byte)`** — ASCII upper-cases a slice in place, no allocation.
Safe because the slice aliases the read buffer and is fully consumed before reuse.
Used to make command/option matching case-insensitive (`set`, `Set`, `SET` all work).

**`writeInt(w *bufio.Writer, n int64) error`** — writes a RESP integer reply
(`:<n>\r\n`) by formatting digits into a **stack** array (`var b [24]byte`) that
never escapes — allocation-free integer replies.

**`(s *Server) handleConnection(conn net.Conn)`** — the per-client worker:

1. **Register** the connection; stack up deferred cleanup: `wg.Done()`,
   `deregisterConn`, `conn.Close()`.
2. **Panic firewall:** a deferred `recover()` so a panic while parsing/executing
   *one* connection's data logs and drops *only that connection* — it can never
   take down the server or other clients' data.
3. **Lease a pooled read buffer** (`pool.Get`, returned on defer).
4. Wrap the connection in a 16 KB `bufio.Writer`.
5. **The read loop:**
   - Set a 5-minute read deadline (DoS / dead-client watchdog) before each read.
   - **Buffer-growth:** if the buffer is entirely full of unparsed bytes (one
     command bigger than the current buffer), double it (capped at
     `maxCommandSize`); the original pooled buffer is still returned to the pool,
     the grown slice is a throwaway GC'd allocation.
   - `conn.Read(buffer[unread:])` — read into the free tail.
   - **Error handling:** `io.EOF` ⇒ polite drop; if `s.quit` is closed ⇒ quiet
     drop (server shutting down); `net.Error.Timeout()` ⇒ silent-client drop; else
     drop.
   - **Inner parse loop:** repeatedly `resp.Parse` the unparsed window. `ErrIncomplete`
     ⇒ break and read more; a real error ⇒ log + drop the client; success ⇒
     `executeCommand(w, cmdSlices)` then advance the cursor. This drains *every*
     complete command in the packet → **pipelining**.
   - `w.Flush()` once after the batch — the single-syscall response flush.
   - **Compaction:** shift any leftover partial command to the front of the buffer
     and set `unread`, so the next read appends to it.

**`(s *Server) executeCommand(w *bufio.Writer, cmdSlices [][]byte) error`** — the
command dispatcher. Upper-cases the verb in place and `switch string(cmdSlices[0])`
(a compiler-optimized, non-allocating `[]byte`→string switch). Each case validates
arity and null args, calls the applier, and writes the RESP reply to `w`:

| Command | Behaviour | Reply |
|---|---|---|
| `SET key value [EX s\|PX ms]` | Parses optional `EX`(seconds)/`PX`(ms); rejects non-positive/garbage TTLs with `-ERR`. Calls `app.Set`. | `+OK` (or `-ERR persistence error` if the WAL append fails). |
| `DEL key` | `app.Delete`. | `:1` if existed, `:0` if not. |
| `GET key` | `app.Get`. Builds the bulk-string header on a stack buffer. | `$<len>\r\n<data>\r\n`, or `$-1` if missing/nil. |
| `EXPIRE key seconds` | Rejects non-positive. `app.Expire`. | `:1` set, `:0` missing. |
| `PEXPIRE key ms` | Same in milliseconds. | `:1`/`:0`. |
| `TTL key` | `app.TTL`; maps sentinels; **rounds up** so any positive remainder reports ≥1 s (Redis behaviour). | `:<seconds>` / `:-1` / `:-2`. |
| `PTTL key` | Same in milliseconds. | `:<ms>` / `:-1` / `:-2`. |
| `PERSIST key` | `app.Persist`. | `:1` removed, `:0` none. |
| *unknown* | Off the hot path. | `-ERR unknown command '<verb>'`. |

Every handler returns an `error` only when the **write to the socket** fails
(client gone) — that propagates up and drops the connection. Protocol/usage
errors are written as `-ERR` replies and the connection stays open.

---

### 5.6 `internal/wal` — the write-ahead log

#### File: `wal/wal.go` — the append-only writer

**Record framing (big-endian):**

```
┌────────┬──────────┬───────────┬────────────┬───────────────┐
│ magic  │   seq    │  crc32c   │ payloadLen │    payload    │
│  u8    │   u64    │    u32    │    u32     │   (RESP cmd)  │
│ 0xA7   │ monotonic│ Castagnoli│  ≤ 512 MB  │  variable     │
└────────┴──────────┴───────────┴────────────┴───────────────┘
         └──────────────── headerSize = 17 bytes ───────────┘
```

- **`magic = 0xA7`** — a recognizable record-start byte; a mismatch on replay means corruption.
- **`seq`** — monotonic sequence number = the future **replication offset**. A follower can tail from a known seq.
- **`crc32c`** — Castagnoli CRC over the payload; detects torn/partial writes from a crash mid-append.
- **`payloadLen ≤ maxPayload (512 MB)`** — bounds a corrupt length field so recovery can't attempt a multi-GB allocation.
- **`payload`** — a RESP-encoded canonical command, so replay reuses the same `resp.Parse`.

**Why one global log, not per-shard?** A single totally-ordered stream is what
consensus needs: the same framing becomes the replication entry, and ordering is
preserved. Per-shard logs would parallelize writes but destroy total ordering, so
the writer is kept off the hot path (the 32-shard concurrency still applies to
reads and in-memory applies).

**`type SyncPolicy int`** with three values:

- `SyncEverySec` (default) — flush + fsync ~once/second (Redis default). ≤1 s of
  acknowledged writes at risk on crash; fsync amortized across many appends ⇒ high throughput.
- `SyncAlways` — fsync on every append. Max durability, lowest throughput (used by tests for determinism).
- `SyncNo` — never explicitly fsync; durability left to the OS page cache.

**`type Log struct`** — `mu` (guards the bufio writer + seq counter), `f` (file),
`w` (1 MB `bufio.Writer`), `seq`, `policy`, `quit`/`wg` (for the sync goroutine).
The critical section under `mu` is just a memory copy into the bufio buffer, so
it stays short even under heavy concurrency; the expensive fsync happens off-lock
on the ticker.

**`Open(path, startSeq, policy) (*Log, error)`** — `MkdirAll` the parent dir,
open the file `O_CREATE|O_WRONLY|O_APPEND` (0644), wrap in a 1 MB bufio writer,
seed `seq = startSeq` (continuing the recovered sequence), and — under
`SyncEverySec` — launch `syncLoop`.

**`Append(payload) (uint64, error)`** — under `mu`: `seq++`, frame the 17-byte
header (magic, seq, CRC-of-payload, len), write header then payload into the
bufio buffer. Under `SyncAlways`, flush + `f.Sync()` immediately. Returns the
record's seq. The payload is fully copied into the buffer before returning, so
the caller may recycle its source slice at once.

**`syncLoop()`** — 1 s ticker; each tick calls `flushAndSync()`; exits on `quit`.
This is **group commit** — many appends are made durable by one fsync.

**`flushAndSync()`** — under `mu`: `w.Flush()` (bufio → OS) then `f.Sync()` (OS → disk).

**`Close()`** — stops the sync goroutine (`close(quit)` + `wg.Wait()`), then a
final flush + fsync + `f.Close()`. This is what guarantees a *clean* shutdown
loses nothing. Called from `main` after the server drains.

#### File: `wal/replay.go`

**`Replay(path, apply func(seq, payload) error) (uint64, error)`** — reads every
intact record in order, invoking `apply` for each, returning the highest seq
applied. The crash-recovery centerpiece:

- A **missing file** ⇒ `(0, nil)` ("nothing to recover yet").
- Reads with a 1 MB `bufio.Reader`. For each record: `io.ReadFull` the 17-byte
  header. A clean `EOF` ends the loop. An `ErrUnexpectedEOF` (torn header), a
  bad magic, an implausible `plen > maxPayload`, a truncated payload, or a **CRC
  mismatch** all `break` — treating everything *before* that point as the durable
  prefix.
- Tracks `goodBytes` = the byte length of the valid prefix.
- After the loop, **`os.Truncate(path, goodBytes)`** drops any torn/corrupt tail.
  Truncating to the current size is a no-op, so this is safe even on a perfectly
  clean log.

> **The key insight:** a torn tail after a crash is *normal*, not an error.
> Recovery's job is to find the last fully-durable record, replay up to there,
> and physically chop off the garbage so the next `Open` starts clean. Tested by
> `TestTornTailTruncated`.

---

### 5.7 `internal/applier` — the durability boundary

#### File: `applier/applier.go`

This is the **one seam** where a mutation becomes durable and then visible. The
package doc spells out the rationale: keep the engine a pure data structure,
funnel all writes through one place, and that place becomes where Raft/replication
wraps consensus later with zero disturbance to engine or server.

**`type Applier struct { cache *engine.PowerhouseCache; log *wal.Log }`** — if
`log` is `nil`, the applier is a pure in-memory cache (this is exactly what the
server tests use). 

**`New(cache, log)`** — constructor.

**Reads (no WAL):**

- `Get(key)` → `cache.Get` (lazy expiry honored inside the shard).
- `TTL(key)` → `cache.TTL`.
- `StartExpiryWorker(quit)` → `cache.StartExpiryWorker` (pass-through so the server only needs the applier).

**Writes (apply to memory, then append to WAL):**

- **`Set(key, value, ttl) error`** — converts relative `ttl` to an absolute
  `deadline`, `cache.SetWithDeadline(...)`, then logs. If no deadline ⇒ logs
  `SET key value`; else logs `SET key value PXAT <deadline-ms>`. Returns the WAL
  append error (surfaced to the client as `-ERR persistence error`).
- **`Delete(key) (bool, error)`** — `cache.Delete`; logs `DEL key` **only if the
  key actually existed** (a DEL-on-missing is a no-op that would replay as a no-op,
  so it's kept out of the log — smaller WAL, faster replay).
- **`Expire(key, ttl) (bool, error)`** — computes an absolute deadline,
  `cache.ExpireAt`; logs `PEXPIREAT key <ms>` only if effective.
- **`Persist(key) (bool, error)`** — `cache.Persist`; logs `PERSIST key` only if a TTL was actually removed.

> **Durability ordering note (current model):** apply-to-memory *then*
> append-to-WAL with everysec fsync. The package doc is explicit that this is the
> pragmatic Redis-compatible default (≤1 s at risk), and that switching to
> log-before-apply for true WAL ordering / Raft is a *localized* change to these
> four methods.

**Recovery:**

- **`LoadFromWAL(path, cache) (uint64, error)`** — calls `wal.Replay`, applying
  each payload via `ApplyRecord`, **without re-logging**. Returns the last seq.
  Called by `main` before serving traffic.
- **`ApplyRecord(cache, payload) error`** — decodes one canonical WAL payload with
  `resp.Parse` and applies it. The replay command set:
  - `SET key value [PXAT ms]` — if a `PXAT` absolute deadline is present and
    **already in the past, the key is dropped** (not resurrected). Otherwise
    `SetWithDeadline`.
  - `DEL key` — `cache.Delete`.
  - `PEXPIREAT key ms` — if the deadline already passed, `Delete` the key; else `ExpireAt`.
  - `PERSIST key` — `cache.Persist`.
  
  Each `case` is defensive (arity checks, parse-failure ⇒ skip the record rather
  than abort recovery).

**`encodeCommand(args ...[]byte) []byte`** — builds a RESP array of bulk strings
(the canonical on-disk form). Pre-sizes the buffer exactly (`*<n>\r\n` header +
per-arg `$<len>\r\n…\r\n`), then appends. **Copies every argument** into the fresh
buffer, so it's safe to pass slices that alias the pooled network buffer — the WAL
record never aliases live memory.

> **Why this matters for the whole system:** `encodeCommand` + `ApplyRecord` are
> symmetric — the same RESP framing is written by the applier and read back by
> both `ApplyRecord` (recovery) and, in future, a replication follower. The WAL,
> the recovery path, and the client protocol all share **one** serialization
> format. That symmetry is the architectural payoff of the whole design.

---

### 5.8 `tests` — the test suite

All tests live in one external `tests` package (black-box: they import the
internal packages by path and exercise only the exported API). This centralization
is a deliberate choice noted in the latest commit ("centralize tests under
DataPlane/tests").

| File | Covers | Notable cases |
|---|---|---|
| `engine_test.go` | Shard + cache: set/get, missing keys, TTL expiry, `Persist`, `Expire`, `EvictExpired`, and the **active expiry worker** (via `Has`, bypassing lazy expiry). | `TestCache_ExpiryWorker_CleansUpKeys` proves the background sweeper works without any `Get`. |
| `eviction_test.go` | The maxmemory budget + LRU. | `TestMaxMemory_EnforcesCap` (5000×512 B under a 256 KB cap stays bounded), `TestMaxMemory_Unlimited` (0 cap = no eviction), `TestMaxMemory_EvictsColdBeforeHot` (a constantly-accessed hot key survives). |
| `parser_test.go` | RESP parser, table-driven. | Valid command, null bulk string, incomplete fragments, empty/garbage input, non-digit length, invalid negative lengths, missing/incorrect CRLF. |
| `applier_test.go` | WAL durability end-to-end. | `TestPersistenceRoundTrip` (SET → "restart" → replay → value + TTL survive; DEL is honored; DEL-on-missing reports false). `TestTornTailTruncated` (a crash-injected half-record is truncated and the good record survives). |
| `server_test.go` | Full server over `net.Pipe`. | `TestServer_PipelinedBatchFlush` (6 commands in one write, exact ordered replies). `TestServer_SetWithExpiryAndPersist`. `TestServer_ProtocolErrorDropsClient`. `TestServer_OversizedBulkLenRejected` (the parser overflow regression guard). |

> **CI note:** the memory file records that **AppLocker on the dev Windows box
> blocks `go test` from running the compiled test binary** — an engine test
> "FAIL" there is the OS, not the code. CI runs on Linux (see `DEPLOYMENT.md`)
> where this does not apply, so CI is the source of truth for test results.

---

## 6. Cross-cutting concerns

### 6.1 Concurrency & locking model

- **Lock striping:** 32 shards × 1 `RWMutex` each ⇒ writes to different shards
  never contend; reads to the same shard run in parallel.
- **Read path takes only the read lock**, and refreshes `atime` via an *atomic*
  store — so even LRU bookkeeping doesn't serialize readers.
- **Lock upgrade in `Get`** (read → write to evict an expired key) always
  **re-checks** under the write lock to stay race-safe.
- **WAL** has its own `mu`; the critical section is a memory copy, fsync is off-lock.
- **Shutdown** is fully ordered via `quit` channel + `WaitGroup`; the `Add(1)`
  happens on the accept goroutine to avoid the Add-vs-Wait race.
- **Panic isolation** per connection means one malformed client can't crash the process.

### 6.2 Memory accounting & LRU eviction

- Each shard tracks `memBytes` incrementally; `entrySize = len(key)+cap(value)+64`.
- Global budget is split 1/32 per shard; each shard evicts independently — **no
  global eviction lock**, fully concurrent.
- Eviction is **sampled approximate LRU** (`evictionSampleSize = 5`): pick the
  oldest-`atime` among a random sample, evict, repeat until under budget. No
  global recency list ⇒ reads never take a write lock for bookkeeping.

### 6.3 Expiry: lazy + active

- **Lazy** (`Get`): an expired key is evicted the moment it's read; the client
  never sees stale data.
- **Active** (`EvictExpired` every 100 ms): reclaims memory for TTL'd keys that
  are never read again. Bounds staleness to ~100 ms.
- Expiry uses **absolute deadlines** everywhere, which is what makes it correct
  across restarts (a relative TTL would reset on every replay).

### 6.4 Durability & crash recovery

- Every mutation → a CRC-checked, sequence-numbered WAL record.
- `everysec` group-commit fsync: ≤1 s at risk on crash, high throughput.
- Clean shutdown flushes + fsyncs everything (zero loss).
- Crash recovery replays the durable prefix and **truncates the torn tail** — a
  crash is a normal, fully-handled event.

### 6.5 Zero-allocation hot path

The read/write hot path is engineered to allocate (almost) nothing per request:
pooled read buffers, zero-copy parse slices, in-place upper-casing,
stack-allocated reply headers, preallocated reply literals, a buffered writer
with one flush per batch, and pre-sized WAL encoding. The *only* mandatory
allocation per write is the value copy in the engine (required for correctness
because the source aliases a pooled buffer).

### 6.6 Security posture of the data plane

What's already hardened **in the code**:

- **Parser hardening:** bounded array/bulk lengths, overflow-safe length parsing,
  strict CRLF termination, explicit `ErrIncomplete` vs. protocol-error
  distinction. Regression-tested against the integer-overflow crash.
- **Resource bounds:** `maxCommandSize` (per-connection buffer ceiling),
  `maxPayload` (WAL), `maxBulkLen`/`maxArrayLen` (parser), `maxmemory` (engine) —
  every unbounded input is capped.
- **DoS watchdog:** 5-minute read deadline drops silent/dead clients.
- **Panic firewall:** one bad connection can't crash the server.
- **Integrity:** CRC32C on every WAL record detects silent corruption.
- **Graceful shutdown:** no mid-command corruption on deploy/restart.

What is **deliberately NOT in the data plane yet** (and must be provided by the
deployment, see `DEPLOYMENT.md`):

- **No AUTH / ACLs** — anyone who can reach `:6379` has full access. → must run on
  a **private subnet**, never publicly exposed; access gated by security groups.
- **No TLS** — wire traffic is plaintext RESP. → terminate TLS at a proxy or keep
  traffic inside the VPC; never traverse the public internet unencrypted.
- **No rate limiting / per-tenant quotas** — single-tenant trust model today.

This split is intentional: the data plane is a fast, trusted-network primitive;
network-level security is the deployment's job.

---

## 7. Known limitations & the road ahead

From the project roadmap memory and the code's own TODO-shaped comments:

1. **WAL compaction / snapshotting** — the log grows forever; there's no
   rewrite/snapshot yet, so recovery time and disk grow with history. (Next big item.)
2. **Replication / Raft** — the single ordered WAL and the applier seam are *built
   for this*, but consensus isn't implemented. The applier doc names the exact
   change (log-before-apply) needed.
3. **More commands** — no `INCR`, `MGET/MSET`, hashes/lists/sets, `SCAN`, pub/sub, etc.
4. **AUTH / TLS / multi-tenancy** — the "control plane" half of "Strata."
5. **Observability** — no metrics/`INFO`/tracing endpoint yet (a natural add at the server layer).
6. **Per-shard vs. global maxmemory skew** — the 1/32 split assumes even key
   distribution; pathological key sets could evict in one shard while another has headroom.

None of these are bugs; they're the deliberately-deferred scope of a system being
built up in correct, testable layers.

---

## 8. Glossary

| Term | Meaning |
|---|---|
| **RESP** | REdis Serialization Protocol — the `*`/`$` framed wire format clients speak. |
| **Shard** | One of 32 independent `RWMutex`+map buckets; the unit of lock striping. |
| **WAL** | Write-Ahead Log — the append-only, ordered, CRC'd durability stream. |
| **Applier** | The single boundary where a write is applied to memory and appended to the WAL. |
| **Lazy expiry** | Removing an expired key when it's read. |
| **Active expiry** | Background sweep removing expired keys every 100 ms. |
| **Approximated LRU** | Evicting the oldest of a small random sample, not a true global LRU list. |
| **Coarse LRU clock** | A shared monotonic counter advanced per tick; cheap recency proxy. |
| **Torn tail** | A partial record left by a crash mid-append; truncated on recovery. |
| **everysec** | Fsync policy: group-commit once per second (Redis default). |
| **Group commit** | Making many buffered appends durable with a single fsync. |
| **Data plane** | The cache server itself (this module). |
| **Control plane** | (Future) management/routing/auth/billing tier — the "Strata" product. |
</content>
</invoke>
