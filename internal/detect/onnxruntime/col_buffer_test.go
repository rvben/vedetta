package onnxruntime

import "testing"

func TestColBufferCacheRetainsOneBufferPerConcurrentBorrower(t *testing.T) {
	var cache colBufferCache
	for _, size := range []int{16, 32, 64, 128} {
		cache.put(cache.get(size))
	}
	if len(cache.bufs) != 1 || cap(cache.bufs[0]) != 128 {
		t.Fatalf("sequential growth retained %d buffers, want one of capacity 128", len(cache.bufs))
	}

	first := cache.get(64)
	second := cache.get(256)
	first[0], second[0] = 1, 2
	if first[0] != 1 {
		t.Fatal("concurrent borrowers share backing storage")
	}
	cache.put(second)
	cache.put(first)

	before := len(cache.bufs)
	cache.put(cache.get(512))
	if before != 2 || len(cache.bufs) != 2 {
		t.Fatalf("retained %d buffers before growth and %d afterward, want 2 throughout", before, len(cache.bufs))
	}
	large, medium := cache.get(512), cache.get(256)
	if &medium[0] != &second[0] {
		t.Fatal("cache discarded the larger existing buffer when replacing a slot")
	}
	cache.put(large)
	cache.put(medium)
}
