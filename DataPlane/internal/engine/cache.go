package engine

import "time"

// ShardCount is fixed to 32.
// Power of 2 sizes are great for memory alignment.
const ShardCount = 32

// PowerhouseCache is the overarching orchestrator.
type PowerhouseCache struct {
	// A fixed-size array holding exactly 32 pointers to our Shards
	shards [ShardCount]*Shard
}

// NewPowerhouseCache boots up the entire memory engine.
func NewPowerhouseCache() *PowerhouseCache {
	c := &PowerhouseCache{}

	// Initialize all 32 independent shards
	for i := 0; i < ShardCount; i++ {
		c.shards[i] = NewShard()
	}

	return c
}

// getShard uses the hash to instantly find the correct bucket.
func (c *PowerhouseCache) getShard(key []byte) *Shard {
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
	shard := c.getShard(key)
	shard.Set(string(key), value, ttl)
}

// SetWithDeadline routes the key and stores the value with an ABSOLUTE expiry.
// A zero expiresAt means the key never expires. Used by WAL recovery.
func (c *PowerhouseCache) SetWithDeadline(key []byte, value []byte, expiresAt time.Time) {
	shard := c.getShard(key)
	shard.SetWithDeadline(string(key), value, expiresAt)
}

// Get routes the key and fetches the data. Lazy expiry happens inside the shard.
func (c *PowerhouseCache) Get(key []byte) ([]byte, bool) {
	shard := c.getShard(key)
	return shard.Get(string(key))
}

// Delete routes the key and safely removes it.
func (c *PowerhouseCache) Delete(key []byte) {
	shard := c.getShard(key)
	shard.Delete(string(key))
}

// TTL returns the remaining time-to-live for a key.
// Returns -2*time.Second if missing, -1*time.Second if no expiry.
func (c *PowerhouseCache) TTL(key []byte) time.Duration {
	shard := c.getShard(key)
	return shard.TTL(string(key))
}

// Persist removes the TTL from a key, making it live forever.
func (c *PowerhouseCache) Persist(key []byte) bool {
	shard := c.getShard(key)
	return shard.Persist(string(key))
}

// Expire sets a new TTL on an existing key.
func (c *PowerhouseCache) Expire(key []byte, ttl time.Duration) bool {
	shard := c.getShard(key)
	return shard.Expire(string(key), ttl)
}

// ExpireAt sets an ABSOLUTE expiry deadline on an existing key. Used by WAL recovery.
func (c *PowerhouseCache) ExpireAt(key []byte, expiresAt time.Time) bool {
	shard := c.getShard(key)
	return shard.ExpireAt(string(key), expiresAt)
}