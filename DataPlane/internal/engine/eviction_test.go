package engine

import (
	"fmt"
	"testing"
)

// TestMaxMemory_EnforcesCap proves the cache stays under its memory budget by
// evicting entries instead of growing unbounded — the Free Tier OOM guard.
func TestMaxMemory_EnforcesCap(t *testing.T) {
	// Small global cap so the test is fast. Split across 32 shards internally.
	const cap = 256 * 1024 // 256 KB
	c := NewPowerhouseCache(cap)

	// Write far more data than the cap allows.
	val := make([]byte, 512) // 512-byte values
	for i := 0; i < 5000; i++ {
		c.Set([]byte(fmt.Sprintf("key:%d", i)), val, 0)
	}

	used := c.MemoryUsage()
	if used > cap {
		t.Fatalf("memory usage %d exceeded cap %d — eviction failed", used, cap)
	}
	if used == 0 {
		t.Fatal("expected some data to remain after eviction")
	}
	t.Logf("after 5000 writes of 512B under a %dB cap: using %dB", cap, used)
}

// TestMaxMemory_Unlimited confirms a 0 cap means no eviction.
func TestMaxMemory_Unlimited(t *testing.T) {
	c := NewPowerhouseCache(0)
	for i := 0; i < 1000; i++ {
		c.Set([]byte(fmt.Sprintf("k:%d", i)), []byte("v"), 0)
	}
	// All 1000 keys must survive.
	for i := 0; i < 1000; i++ {
		if _, ok := c.Get([]byte(fmt.Sprintf("k:%d", i))); !ok {
			t.Fatalf("key k:%d evicted under unlimited cap", i)
		}
	}
}

// TestMaxMemory_EvictsColdBeforeHot checks the LRU bias: a key kept warm by
// repeated access should outlive cold keys when the cap forces eviction.
func TestMaxMemory_EvictsColdBeforeHot(t *testing.T) {
	const cap = 64 * 1024
	c := NewPowerhouseCache(cap)

	// Advance the LRU clock manually (no expiry worker running in this test).
	hot := []byte("hot-key")
	val := make([]byte, 256)
	c.Set(hot, val, 0)

	for i := 0; i < 2000; i++ {
		c.tickClock()                                  // time moves forward
		c.Set([]byte(fmt.Sprintf("cold:%d", i)), val, 0)
		c.Get(hot)                                     // keep the hot key warm
	}

	// The hot key should still be present far more often than not. Because LRU
	// is sampled (approximate), we assert it survived.
	if _, ok := c.Get(hot); !ok {
		t.Fatal("hot key was evicted despite constant access (LRU not biasing)")
	}
}
