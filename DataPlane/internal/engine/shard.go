package engine

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// approxEntryOverhead is a rough fixed cost (in bytes) charged to every entry on
// top of its key and value: the Entry struct, the map bucket slot, and the
// string/slice headers. It does not have to be exact — memory accounting only
// needs to be proportional so the maxmemory cap and LRU eviction kick in at a
// sane point.
const approxEntryOverhead = 64

// evictionSampleSize is how many keys we sample per eviction round. Like Redis,
// we approximate LRU: instead of maintaining a global recency list (which would
// force a write lock on every read), we sample a handful of keys and evict the
// least-recently-used among them. Bigger samples = closer to true LRU, more CPU.
const evictionSampleSize = 5

// Entry holds a cached value along with its optional expiry time.
//
// Entries are stored as *Entry (pointers) so that a read can refresh the access
// clock with a single atomic store — without taking the shard's write lock or
// rewriting the map slot. That preserves the RWMutex read concurrency.
type Entry struct {
	Value     []byte
	ExpiresAt time.Time    // Zero value means no expiry (key lives forever)
	atime     atomic.Int64 // last-access tick, used for approximated LRU
}

// isExpired reports whether this entry has passed its deadline.
// An entry with a zero ExpiresAt never expires.
func (e *Entry) isExpired() bool {
	return !e.ExpiresAt.IsZero() && time.Now().After(e.ExpiresAt)
}

// Shard is a single, isolated bucket of data.
// By having 32 of these, we divide our lock contention by 32.
type Shard struct {
	// RWMutex allows MULTIPLE readers at the same time,
	// but only ONE writer at a time.
	lock sync.RWMutex

	// The actual data store. Values are *Entry so reads can update the access
	// clock cheaply.
	items map[string]*Entry

	// memBytes is the approximate live memory used by this shard, maintained
	// incrementally on every insert/delete/eviction. Guarded by lock.
	memBytes int64

	// maxBytes is this shard's slice of the global maxmemory budget. 0 means
	// unlimited (no eviction).
	maxBytes int64

	// clock points to the cache-wide coarse LRU clock. May be nil for a
	// standalone shard, in which case access times stay 0 and eviction
	// degrades to (still bounded) effectively-random sampling.
	clock *atomic.Int64
}

// NewShard initializes a ready-to-use, unbounded Shard (no eviction).
// Used by unit tests and as the simplest construction path.
func NewShard() *Shard {
	return &Shard{items: make(map[string]*Entry)}
}

// newShardBudgeted builds a shard with a memory budget and a shared LRU clock.
// Used by the cache to wire up maxmemory eviction.
func newShardBudgeted(maxBytes int64, clock *atomic.Int64) *Shard {
	return &Shard{
		items:    make(map[string]*Entry),
		maxBytes: maxBytes,
		clock:    clock,
	}
}

// nowTick returns the current coarse LRU clock value (0 if no clock is wired).
func (s *Shard) nowTick() int64 {
	if s.clock == nil {
		return 0
	}
	return s.clock.Load()
}

// entrySize estimates the memory footprint of a key/entry pair.
func entrySize(key string, e *Entry) int64 {
	return int64(len(key)) + int64(cap(e.Value)) + approxEntryOverhead
}

// Set safely adds or updates a key-value pair in this specific shard.
// A ttl of 0 means the key never expires.
func (s *Shard) Set(key string, value []byte, ttl time.Duration) {
	// Compute the absolute deadline. Zero ttl → zero ExpiresAt → no expiry.
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	s.SetWithDeadline(key, value, expiresAt)
}

// SetWithDeadline adds or updates a key with an ABSOLUTE expiry deadline.
// A zero expiresAt means the key never expires.
//
// This is the canonical write path: SET computes a deadline from a relative
// TTL and delegates here, while WAL recovery replays the absolute deadline
// directly. Persisting the absolute time (not a relative TTL) is what keeps
// expiry correct across restarts.
func (s *Shard) SetWithDeadline(key string, value []byte, expiresAt time.Time) {
	// Clone the value because the source slice may be part of a pooled network
	// buffer that will be recycled as soon as the connection reads more data.
	var valCopy []byte
	if value != nil {
		valCopy = make([]byte, len(value))
		copy(valCopy, value)
	}

	e := &Entry{Value: valCopy, ExpiresAt: expiresAt}

	s.lock.Lock()
	defer s.lock.Unlock()

	e.atime.Store(s.nowTick())

	// Adjust memory accounting: replace old size (if overwriting) with new.
	if old, ok := s.items[key]; ok {
		s.memBytes -= entrySize(key, old)
	}
	s.items[key] = e
	s.memBytes += entrySize(key, e)

	// Enforce the budget, protecting the key we just wrote from being its own
	// eviction victim.
	s.evictIfNeeded(key)
}

// Get safely retrieves a value from this specific shard.
// Implements LAZY EXPIRY: if the key has expired, it is deleted here and
// nil is returned — the client never sees a stale value.
func (s *Shard) Get(key string) ([]byte, bool) {
	// Phase 1: Fast path — read lock only.
	s.lock.RLock()
	entry, exists := s.items[key]
	if !exists {
		s.lock.RUnlock()
		return nil, false
	}

	if entry.isExpired() {
		s.lock.RUnlock()
		// Upgrade to a write lock to evict the stale entry.
		// We MUST re-check under the write lock: another goroutine may have
		// already deleted or refreshed this key in the window between locks.
		s.lock.Lock()
		if e, ok := s.items[key]; ok && e.isExpired() {
			s.memBytes -= entrySize(key, e)
			delete(s.items, key)
		}
		s.lock.Unlock()
		return nil, false
	}

	// Refresh the LRU access clock. Safe under the read lock because atime is
	// atomic and the map slot itself is not mutated.
	entry.atime.Store(s.nowTick())
	val := entry.Value
	s.lock.RUnlock()
	return val, true
}

// Has reports whether the key is physically present in the shard's map,
// WITHOUT triggering lazy expiry. It is an introspection helper for tests that
// need to verify the active-expiry worker actually removed an entry.
func (s *Shard) Has(key string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	_, ok := s.items[key]
	return ok
}

// Delete safely removes a key from this specific shard.
// Returns true if the key existed and was removed, false if it was absent.
func (s *Shard) Delete(key string) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if e, ok := s.items[key]; ok {
		s.memBytes -= entrySize(key, e)
		delete(s.items, key)
		return true
	}
	return false
}

// TTL returns the remaining time-to-live for a key.
//
// Return convention (mirrors Redis):
//   - (-2 * time.Second) → key does not exist
//   - (-1 * time.Second) → key exists but has no expiry
//   - positive duration  → remaining time until the key expires
func (s *Shard) TTL(key string) time.Duration {
	s.lock.RLock()
	defer s.lock.RUnlock()

	entry, exists := s.items[key]
	if !exists {
		return -2 * time.Second
	}
	if entry.ExpiresAt.IsZero() {
		return -1 * time.Second // Persistent key
	}
	remaining := time.Until(entry.ExpiresAt)
	if remaining <= 0 {
		// Expired but not yet cleaned up by lazy/active expiry.
		// Treat it the same as missing.
		return -2 * time.Second
	}
	return remaining
}

// Persist removes the expiry from a key, making it live forever.
// Returns true if the TTL was removed, false if the key didn't exist or had no TTL.
func (s *Shard) Persist(key string) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	entry, exists := s.items[key]
	if !exists || entry.isExpired() {
		return false
	}
	if entry.ExpiresAt.IsZero() {
		return false // Already persistent — nothing to remove
	}
	entry.ExpiresAt = time.Time{} // Zero means "no expiry"
	return true
}

// Expire sets a new TTL on an existing key without changing its value.
// Returns true if the key existed and the new deadline was applied.
func (s *Shard) Expire(key string, ttl time.Duration) bool {
	return s.ExpireAt(key, time.Now().Add(ttl))
}

// ExpireAt sets an ABSOLUTE expiry deadline on an existing key.
// Returns true if the key existed and the deadline was applied.
//
// This is the deadline-based counterpart to Expire, used by WAL recovery so
// the replayed expiry matches the one that was originally durable.
func (s *Shard) ExpireAt(key string, expiresAt time.Time) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	entry, exists := s.items[key]
	if !exists {
		return false
	}
	if entry.isExpired() {
		s.memBytes -= entrySize(key, entry)
		delete(s.items, key)
		return false
	}

	entry.ExpiresAt = expiresAt
	return true
}

// EvictExpired performs an ACTIVE EXPIRY sweep of this shard.
// It is called by the background expiry goroutine every 100 ms.
// Returns the count of keys that were deleted.
func (s *Shard) EvictExpired() int {
	// Snapshot the current time ONCE outside the loop for consistency.
	now := time.Now()

	s.lock.Lock()
	defer s.lock.Unlock()

	count := 0
	for key, entry := range s.items {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			s.memBytes -= entrySize(key, entry)
			delete(s.items, key)
			count++
		}
	}
	return count
}

// evictIfNeeded enforces the shard's memory budget using approximated LRU.
// Must be called with the write lock held. protect is a key that must not be
// evicted (the one just written), which also prevents an infinite loop when a
// single value is larger than the whole budget.
func (s *Shard) evictIfNeeded(protect string) {
	if s.maxBytes <= 0 {
		return // unbounded — no eviction
	}
	for s.memBytes > s.maxBytes {
		victim := s.sampleLRU(protect)
		if victim == "" {
			return // nothing evictable left
		}
		if e, ok := s.items[victim]; ok {
			s.memBytes -= entrySize(victim, e)
			delete(s.items, victim)
		}
	}
}

// sampleLRU inspects up to evictionSampleSize keys and returns the one with the
// oldest access time. Go randomizes map iteration order, so taking the first N
// keys is an effective random sample. Must be called with the lock held.
func (s *Shard) sampleLRU(protect string) string {
	var victim string
	var oldest int64 = math.MaxInt64

	n := 0
	for k, e := range s.items {
		if k == protect {
			continue
		}
		if a := e.atime.Load(); a < oldest {
			oldest = a
			victim = k
		}
		n++
		if n >= evictionSampleSize {
			break
		}
	}
	return victim
}
