package engine

import (
	"sync"
	"time"
)

// Entry holds a cached value along with its optional expiry time.
type Entry struct {
	Value     []byte
	ExpiresAt time.Time // Zero value means no expiry (key lives forever)
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

	// The actual data store.
	// Map keys are strings; values are Entry wrappers holding payload + expiry.
	items map[string]Entry
}

// NewShard initializes a ready-to-use Shard.
func NewShard() *Shard {
	return &Shard{
		items: make(map[string]Entry),
	}
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
// expiry correct across restarts — replaying "SET k v EX 60" days later would
// otherwise hand the key a fresh 60s.
func (s *Shard) SetWithDeadline(key string, value []byte, expiresAt time.Time) {
	// Clone the value because the source slice may be part of a pooled network
	// buffer that will be recycled as soon as the connection reads more data.
	var valCopy []byte
	if value != nil {
		valCopy = make([]byte, len(value))
		copy(valCopy, value)
	}

	// Lock the shard for WRITING. Nobody else can read or write right now.
	s.lock.Lock()
	defer s.lock.Unlock()

	s.items[key] = Entry{
		Value:     valCopy,
		ExpiresAt: expiresAt,
	}
}

// Get safely retrieves a value from this specific shard.
// Implements LAZY EXPIRY: if the key has expired, it is deleted here and
// nil is returned — the client never sees a stale value.
func (s *Shard) Get(key string) ([]byte, bool) {
	// Phase 1: Fast path — read lock only.
	s.lock.RLock()
	entry, exists := s.items[key]
	s.lock.RUnlock()

	if !exists {
		return nil, false
	}

	// Phase 2: Key found — is it still alive?
	if entry.isExpired() {
		// Upgrade to a write lock to evict the stale entry.
		// We MUST re-check under the write lock: another goroutine may have
		// already deleted or refreshed this key in the window between locks.
		s.lock.Lock()
		if e, ok := s.items[key]; ok && e.isExpired() {
			delete(s.items, key)
		}
		s.lock.Unlock()
		return nil, false
	}

	return entry.Value, true
}

// Delete safely removes a key from this specific shard.
func (s *Shard) Delete(key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.items, key)
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
	s.items[key] = entry
	return true
}

// Expire sets a new TTL on an existing key without changing its value.
// Returns true if the key existed and the new deadline was applied.
func (s *Shard) Expire(key string, ttl time.Duration) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	entry, exists := s.items[key]
	if !exists {
		return false
	}
	// Treat an already-expired entry the same as missing.
	if entry.isExpired() {
		delete(s.items, key)
		return false
	}

	entry.ExpiresAt = time.Now().Add(ttl)
	s.items[key] = entry
	return true
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
		delete(s.items, key)
		return false
	}

	entry.ExpiresAt = expiresAt
	s.items[key] = entry
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
			delete(s.items, key)
			count++
		}
	}
	return count
}