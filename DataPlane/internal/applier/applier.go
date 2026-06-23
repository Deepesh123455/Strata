// Package applier is the single boundary where a mutation is made durable and
// then applied to the in-memory engine.
//
// Every write command flows through here: the applier normalizes any relative
// TTL into an ABSOLUTE deadline, applies the change to the 32-shard engine, and
// appends a canonical record to the WAL. Reads pass straight through to the
// engine — they never touch disk.
//
// Why a dedicated layer: keeping the engine a pure in-memory data structure and
// funnelling all mutations through one place gives us exactly one "apply a
// command" seam. That seam is where replication/Raft will later wrap consensus
// around each write without disturbing the engine or the server.
//
// Durability model (current): apply-to-memory, then append-to-WAL with an
// everysec fsync. This is the pragmatic Redis-compatible default; at most ~1s
// of acknowledged writes are at risk on a crash. Switching to log-before-apply
// (true WAL ordering) for Raft is a localized change to the methods below.
package applier

import (
	"bytes"
	"strconv"
	"time"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/engine"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/resp"
	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/wal"
)

// Applier wraps the engine and (optionally) a WAL. If log is nil, the applier
// runs as a pure in-memory cache with no persistence.
type Applier struct {
	cache *engine.PowerhouseCache
	log   *wal.Log
}

// New builds an Applier over the given engine and log. log may be nil.
func New(cache *engine.PowerhouseCache, log *wal.Log) *Applier {
	return &Applier{cache: cache, log: log}
}

// --- Reads (no WAL involvement) ---

// Get fetches a value, honouring lazy expiry inside the shard.
func (a *Applier) Get(key []byte) ([]byte, bool) { return a.cache.Get(key) }

// TTL returns the remaining time-to-live for a key (Redis sentinel convention).
func (a *Applier) TTL(key []byte) time.Duration { return a.cache.TTL(key) }

// StartExpiryWorker launches the background active-expiry sweep on the engine.
func (a *Applier) StartExpiryWorker(quit chan struct{}) { a.cache.StartExpiryWorker(quit) }

// --- Writes (apply to memory, then append to WAL) ---

// Set stores a value. A ttl of 0 means no expiry. The relative ttl is converted
// to an absolute deadline so the logged record stays correct across restarts.
func (a *Applier) Set(key, value []byte, ttl time.Duration) error {
	var deadline time.Time
	if ttl > 0 {
		deadline = time.Now().Add(ttl)
	}
	a.cache.SetWithDeadline(key, value, deadline)

	if a.log == nil {
		return nil
	}
	var rec []byte
	if deadline.IsZero() {
		rec = encodeCommand([]byte("SET"), key, value)
	} else {
		ms := strconv.AppendInt(nil, deadline.UnixMilli(), 10)
		rec = encodeCommand([]byte("SET"), key, value, []byte("PXAT"), ms)
	}
	_, err := a.log.Append(rec)
	return err
}

// Delete removes a key.
func (a *Applier) Delete(key []byte) error {
	a.cache.Delete(key)
	if a.log == nil {
		return nil
	}
	_, err := a.log.Append(encodeCommand([]byte("DEL"), key))
	return err
}

// Expire applies a relative TTL to an existing key. Returns whether the key
// existed. Only effective expiries are logged (a no-op on a missing key would
// replay as a no-op anyway).
func (a *Applier) Expire(key []byte, ttl time.Duration) (bool, error) {
	deadline := time.Now().Add(ttl)
	ok := a.cache.ExpireAt(key, deadline)
	if !ok || a.log == nil {
		return ok, nil
	}
	ms := strconv.AppendInt(nil, deadline.UnixMilli(), 10)
	_, err := a.log.Append(encodeCommand([]byte("PEXPIREAT"), key, ms))
	return ok, err
}

// Persist removes the TTL from a key. Returns whether a TTL was actually removed.
func (a *Applier) Persist(key []byte) (bool, error) {
	ok := a.cache.Persist(key)
	if !ok || a.log == nil {
		return ok, nil
	}
	_, err := a.log.Append(encodeCommand([]byte("PERSIST"), key))
	return ok, err
}

// --- Recovery ---

// LoadFromWAL replays a log file into the engine WITHOUT re-logging, returning
// the highest sequence number recovered. Call this before serving traffic and
// before opening the live wal.Log for appends.
func LoadFromWAL(path string, cache *engine.PowerhouseCache) (uint64, error) {
	return wal.Replay(path, func(_ uint64, payload []byte) error {
		return ApplyRecord(cache, payload)
	})
}

// ApplyRecord decodes one canonical WAL payload and applies it directly to the
// engine. Records carry absolute deadlines (PXAT/PEXPIREAT), so any key whose
// deadline has already passed is dropped instead of being resurrected.
func ApplyRecord(cache *engine.PowerhouseCache, payload []byte) error {
	args, _, err := resp.Parse(payload)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == nil {
		return nil
	}

	switch string(bytes.ToUpper(args[0])) {
	case "SET":
		if len(args) < 3 {
			return nil
		}
		var deadline time.Time
		if len(args) >= 5 && bytes.EqualFold(args[3], []byte("PXAT")) {
			ms, perr := strconv.ParseInt(string(args[4]), 10, 64)
			if perr != nil {
				return nil
			}
			deadline = time.UnixMilli(ms)
			if !deadline.After(time.Now()) {
				return nil // already expired — do not load
			}
		}
		cache.SetWithDeadline(args[1], args[2], deadline)

	case "DEL":
		if len(args) >= 2 {
			cache.Delete(args[1])
		}

	case "PEXPIREAT":
		if len(args) < 3 {
			return nil
		}
		ms, perr := strconv.ParseInt(string(args[2]), 10, 64)
		if perr != nil {
			return nil
		}
		deadline := time.UnixMilli(ms)
		if !deadline.After(time.Now()) {
			cache.Delete(args[1])
			return nil
		}
		cache.ExpireAt(args[1], deadline)

	case "PERSIST":
		if len(args) >= 2 {
			cache.Persist(args[1])
		}
	}
	return nil
}

// encodeCommand builds a RESP array of bulk strings — the canonical on-disk
// form of a mutation. It copies every argument into a fresh buffer, so it is
// safe to pass slices that alias a pooled network buffer.
func encodeCommand(args ...[]byte) []byte {
	// Pre-size: array header + per-arg ($len\r\n ... \r\n).
	size := len(strconv.Itoa(len(args))) + 3
	for _, a := range args {
		size += 1 + len(strconv.Itoa(len(a))) + 2 + len(a) + 2
	}

	buf := make([]byte, 0, size)
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	return buf
}
