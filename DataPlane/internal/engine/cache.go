package engine

import (
	"sync/atomic"
	"time"
)

// ShardCount is fixed to 32.
// Power of 2 sizes are great for memory alignment.
const ShardCount = 32

// PowerhouseCache is the overarching orchestrator.
type PowerhouseCache struct {
	// A fixed-size array holding exactly 32 pointers to our Shards
	shards [ShardCount]*Shard

	// clock is the coarse, cache-wide LRU clock. It is advanced once per tick
	// by the expiry worker; entries record its value on access. Coarse on
	// purpose (cheap to read, no per-op contention) — Redis does the same.
	clock atomic.Int64
}

// NewPowerhouseCache boots up the entire memory engine.
//
// maxMemoryBytes is the global memory budget; 0 means unlimited (no eviction).
// The budget is split evenly across the 32 shards, each of which enforces its
// own slice independently under its own lock — no global eviction lock needed.
// With FNV hashing keys spread evenly, so per-shard budgeting closely tracks a
// global cap while staying fully concurrent.
func NewPowerhouseCache(maxMemoryBytes int64) *PowerhouseCache {
	c := &PowerhouseCache{}

	var perShard int64
	if maxMemoryBytes > 0 {
		perShard = maxMemoryBytes / ShardCount
		if perShard < 1 {
			perShard = 1
		}
	}

	// Initialize all 32 independent shards
	for i := 0; i < ShardCount; i++ {
		c.shards[i] = newShardBudgeted(perShard, &c.clock)
	}

	return c
}

// MemoryUsage returns the approximate total live bytes across all shards.
func (c *PowerhouseCache) MemoryUsage() int64 {
	var total int64
	for i := 0; i < ShardCount; i++ {
		s := c.shards[i]
		s.lock.RLock()
		total += s.memBytes
		s.lock.RUnlock()
	}
	return total
}

// TickClock advances the coarse LRU clock by one. Called by the expiry worker.
func (c *PowerhouseCache) TickClock() {
	c.clock.Add(1)
}

// GetShard uses the hash to instantly find the correct bucket.
func (c *PowerhouseCache) GetShard(key []byte) *Shard {
	// 1. Get the giant random number (e.g., 4,123,987,122)
	hashNum := fnv32a(key)

	// 2. Use modulo to wrap that number cleanly between 0 and 31
	index := hashNum % uint32(ShardCount)

	// 3. Return the pointer to that specific Shard
	return c.shards[index]
}

// --- The Public API ---

// Set routes the key to the correct shard and stores the value.
// A ttl of 0 means the key never expires.
func (c *PowerhouseCache) Set(key []byte, value []byte, ttl time.Duration) {
	shard := c.GetShard(key)
	shard.Set(string(key), value, ttl)
}

// SetWithDeadline routes the key and stores the value with an ABSOLUTE expiry.
// A zero expiresAt means the key never expires. Used by WAL recovery.
func (c *PowerhouseCache) SetWithDeadline(key []byte, value []byte, expiresAt time.Time) {
	shard := c.GetShard(key)
	shard.SetWithDeadline(string(key), value, expiresAt)
}

// Get routes the key and fetches the data. Lazy expiry happens inside the shard.
func (c *PowerhouseCache) Get(key []byte) ([]byte, bool) {
	shard := c.GetShard(key)
	return shard.Get(string(key))
}

// Delete routes the key and safely removes it.
func (c *PowerhouseCache) Delete(key []byte) {
	shard := c.GetShard(key)
	shard.Delete(string(key))
}

// TTL returns the remaining time-to-live for a key.
// Returns -2*time.Second if missing, -1*time.Second if no expiry.
func (c *PowerhouseCache) TTL(key []byte) time.Duration {
	shard := c.GetShard(key)
	return shard.TTL(string(key))
}

// Persist removes the TTL from a key, making it live forever.
func (c *PowerhouseCache) Persist(key []byte) bool {
	shard := c.GetShard(key)
	return shard.Persist(string(key))
}

// Expire sets a new TTL on an existing key.
func (c *PowerhouseCache) Expire(key []byte, ttl time.Duration) bool {
	shard := c.GetShard(key)
	return shard.Expire(string(key), ttl)
}

// ExpireAt sets an ABSOLUTE expiry deadline on an existing key. Used by WAL recovery.
func (c *PowerhouseCache) ExpireAt(key []byte, expiresAt time.Time) bool {
	shard := c.GetShard(key)
	return shard.ExpireAt(string(key), expiresAt)
}