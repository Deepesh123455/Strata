package engine

import "time"

// expiryTickInterval controls how often the background worker sweeps all shards.
// 100ms gives sub-second expiry accuracy while keeping CPU overhead negligible.
const expiryTickInterval = 100 * time.Millisecond

// StartExpiryWorker launches a background goroutine that periodically evicts
// expired keys from all 32 shards (the ACTIVE EXPIRY prong).
//
// This complements lazy expiry (which fires on Get) by reclaiming memory for
// keys that are written with a TTL but never read again.
//
// The goroutine exits automatically when the quit channel is closed, making
// its lifecycle tied to the server's graceful shutdown.
func (c *PowerhouseCache) StartExpiryWorker(quit chan struct{}) {
	go func() {
		ticker := time.NewTicker(expiryTickInterval)
		defer ticker.Stop()

		for {
			select {
			case <-quit:
				// Server is shutting down — stop the worker cleanly.
				return
			case <-ticker.C:
				// Advance the coarse LRU clock so access-recency moves forward.
				c.TickClock()
				// Time to sweep. This is non-blocking; each shard holds its
				// own write lock only for the duration of its own sweep.
				c.runExpiryPass()
			}
		}
	}()
}

// runExpiryPass iterates over all 32 shards and evicts expired keys from each.
// Called every 100ms by the ticker inside StartExpiryWorker.
func (c *PowerhouseCache) runExpiryPass() {
	for i := 0; i < ShardCount; i++ {
		c.shards[i].EvictExpired()
	}
}
