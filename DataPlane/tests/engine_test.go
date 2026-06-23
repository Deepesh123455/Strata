package tests

import (
	"testing"
	"time"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/engine"
)

// --- Shard-level unit tests ---

func TestShard_SetAndGet(t *testing.T) {
	s := engine.NewShard()
	s.Set("hello", []byte("world"), 0)
	val, ok := s.Get("hello")
	if !ok {
		t.Fatal("expected key to exist")
	}
	if string(val) != "world" {
		t.Fatalf("expected 'world', got %q", val)
	}
}

func TestShard_GetMissing(t *testing.T) {
	s := engine.NewShard()
	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected key to be missing")
	}
}

func TestShard_SetWithTTL_KeyExpiresAfterDeadline(t *testing.T) {
	s := engine.NewShard()
	// Set a very short TTL so the test doesn't have to wait long.
	s.Set("expiring", []byte("value"), 50*time.Millisecond)

	// Immediately after setting: key should exist.
	_, ok := s.Get("expiring")
	if !ok {
		t.Fatal("key should exist right after Set")
	}

	// Wait for the TTL to pass.
	time.Sleep(80 * time.Millisecond)

	// Now the key should have been lazily evicted on Get.
	_, ok = s.Get("expiring")
	if ok {
		t.Fatal("key should have expired but was still returned")
	}
}

func TestShard_TTL_NoExpiry(t *testing.T) {
	s := engine.NewShard()
	s.Set("permanent", []byte("v"), 0)
	ttl := s.TTL("permanent")
	if ttl != -1*time.Second {
		t.Fatalf("expected -1s for no-expiry key, got %v", ttl)
	}
}

func TestShard_TTL_Missing(t *testing.T) {
	s := engine.NewShard()
	ttl := s.TTL("ghost")
	if ttl != -2*time.Second {
		t.Fatalf("expected -2s for missing key, got %v", ttl)
	}
}

func TestShard_TTL_WithExpiry(t *testing.T) {
	s := engine.NewShard()
	s.Set("counted", []byte("v"), 5*time.Second)
	ttl := s.TTL("counted")
	// Should be something between 4s and 5s (test latency).
	if ttl <= 0 || ttl > 5*time.Second {
		t.Fatalf("expected TTL between 0 and 5s, got %v", ttl)
	}
}

func TestShard_Persist(t *testing.T) {
	s := engine.NewShard()
	s.Set("fleeting", []byte("v"), 10*time.Second)

	// Removing the TTL should succeed.
	if !s.Persist("fleeting") {
		t.Fatal("Persist returned false for a key with TTL")
	}

	// Key should now report no expiry.
	ttl := s.TTL("fleeting")
	if ttl != -1*time.Second {
		t.Fatalf("expected -1s after Persist, got %v", ttl)
	}

	// Calling Persist again should return false (no TTL to remove).
	if s.Persist("fleeting") {
		t.Fatal("Persist returned true on an already-persistent key")
	}
}

func TestShard_Expire(t *testing.T) {
	s := engine.NewShard()
	s.Set("resettable", []byte("v"), 0)

	// Setting a new TTL on a persistent key.
	if !s.Expire("resettable", 50*time.Millisecond) {
		t.Fatal("Expire returned false for an existing key")
	}

	// Key should exist just after Expire.
	if _, ok := s.Get("resettable"); !ok {
		t.Fatal("key should still be alive immediately after Expire")
	}

	// Wait for the new TTL to lapse.
	time.Sleep(80 * time.Millisecond)

	if _, ok := s.Get("resettable"); ok {
		t.Fatal("key should have expired after the new TTL elapsed")
	}
}

func TestShard_Expire_MissingKey(t *testing.T) {
	s := engine.NewShard()
	if s.Expire("ghost", time.Second) {
		t.Fatal("Expire should return false for a missing key")
	}
}

func TestShard_EvictExpired(t *testing.T) {
	s := engine.NewShard()
	s.Set("a", []byte("1"), 30*time.Millisecond)
	s.Set("b", []byte("2"), 30*time.Millisecond)
	s.Set("c", []byte("3"), 0) // permanent

	time.Sleep(50 * time.Millisecond)

	evicted := s.EvictExpired()
	if evicted != 2 {
		t.Fatalf("expected 2 keys evicted by EvictExpired, got %d", evicted)
	}

	// Permanent key should still be there.
	if _, ok := s.Get("c"); !ok {
		t.Fatal("permanent key 'c' should survive EvictExpired")
	}
}

// --- Cache-level integration tests ---

func TestCache_SetGetWithTTL(t *testing.T) {
	c := engine.NewPowerhouseCache(0)
	c.Set([]byte("user:1"), []byte("Deepesh"), 100*time.Millisecond)

	// Immediate get should succeed.
	val, ok := c.Get([]byte("user:1"))
	if !ok || string(val) != "Deepesh" {
		t.Fatalf("expected 'Deepesh', got %q ok=%v", val, ok)
	}

	// After expiry the key should vanish.
	time.Sleep(150 * time.Millisecond)
	_, ok = c.Get([]byte("user:1"))
	if ok {
		t.Fatal("key should have expired")
	}
}

func TestCache_TTL(t *testing.T) {
	c := engine.NewPowerhouseCache(0)
	c.Set([]byte("session"), []byte("abc"), 10*time.Second)

	ttl := c.TTL([]byte("session"))
	if ttl <= 0 || ttl > 10*time.Second {
		t.Fatalf("unexpected TTL %v", ttl)
	}
}

func TestCache_Persist(t *testing.T) {
	c := engine.NewPowerhouseCache(0)
	c.Set([]byte("token"), []byte("xyz"), 10*time.Second)
	if !c.Persist([]byte("token")) {
		t.Fatal("Persist should succeed")
	}
	if c.TTL([]byte("token")) != -1*time.Second {
		t.Fatal("expected -1s TTL after Persist")
	}
}

func TestCache_ExpiryWorker_CleansUpKeys(t *testing.T) {
	c := engine.NewPowerhouseCache(0)
	quit := make(chan struct{})
	c.StartExpiryWorker(quit)
	defer close(quit)

	c.Set([]byte("tmp"), []byte("gone"), 80*time.Millisecond)

	// Let the worker run at least two ticks (100ms each) plus TTL.
	time.Sleep(300 * time.Millisecond)

	// The key should have been swept by active expiry even without a Get.
	// We verify by doing a direct shard lookup (bypassing Get's lazy eviction).
	shard := c.GetShard([]byte("tmp"))
	if shard.Has("tmp") {
		t.Fatal("expiry worker should have evicted 'tmp' by now")
	}
}
