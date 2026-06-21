package engine

// fnv32a is our custom, 100% zero-allocation hashing function.
// It mathematically converts a word like "mykey" into a giant number.
// We keep this isolated here so we can easily swap algorithms in the future
// without touching the core cache logic.
func fnv32a(key []byte) uint32 {
	var hash uint32 = 2166136261 // FNV offset basis
	
	for _, b := range key {
		hash ^= uint32(b)
		hash *= 16777619 // FNV prime
	}
	
	return hash
}