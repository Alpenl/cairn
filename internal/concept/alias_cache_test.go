package concept

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestAliasCacheGetMissReturnsFalse(t *testing.T) {
	c := NewAliasCache()
	if id, ok := c.Get("RAG"); ok || id != uuid.Nil {
		t.Errorf("Get on empty cache returned (%v, %v); want (uuid.Nil, false)", id, ok)
	}
}

func TestAliasCachePutThenGetRoundTrips(t *testing.T) {
	c := NewAliasCache()
	want := uuid.New()
	c.Put("RAG", want)
	// Same casing as Put.
	if id, ok := c.Get("RAG"); !ok || id != want {
		t.Errorf("Get(RAG) = (%v, %v); want (%v, true)", id, ok, want)
	}
	// Case-insensitive lookup — the resolver hands in raw surface
	// tags whose casing varies between callers; normalisation in
	// Put / Get must agree.
	if id, ok := c.Get("rag"); !ok || id != want {
		t.Errorf("Get(rag) = (%v, %v); want (%v, true) — case sensitivity regression", id, ok, want)
	}
	// Whitespace trim — same reason.
	if id, ok := c.Get("  RAG  "); !ok || id != want {
		t.Errorf("Get(' RAG ') = (%v, %v); want (%v, true) — trim regression", id, ok, want)
	}
}

func TestAliasCachePutIgnoresEmptyAndNil(t *testing.T) {
	c := NewAliasCache()
	c.Put("", uuid.New())    // empty alias
	c.Put("   ", uuid.New()) // whitespace alias
	c.Put("real", uuid.Nil)  // nil id
	// None of the above should have written anything.
	for _, k := range []string{"", "   ", "real"} {
		if _, ok := c.Get(k); ok {
			t.Errorf("Get(%q) returned a hit; empty/nil Put should be a noop", k)
		}
	}
}

func TestAliasCachePutFirstWriterWins(t *testing.T) {
	// Concurrent Puts for the same key keep the first writer's value.
	// In production both writers observed the same DB row so the
	// invariant only matters when the two values differ — which would
	// indicate a caller bug. Pinning the behaviour here documents the
	// chosen tie-break.
	c := NewAliasCache()
	first := uuid.New()
	second := uuid.New()
	c.Put("RAG", first)
	c.Put("RAG", second)
	if id, _ := c.Get("RAG"); id != first {
		t.Errorf("Get(RAG) = %v; want %v (LoadOrStore first-write-wins regression)", id, first)
	}
}

func TestAliasCacheInvalidateAllDropsEverything(t *testing.T) {
	c := NewAliasCache()
	a, b := uuid.New(), uuid.New()
	c.Put("RAG", a)
	c.Put("LLM", b)
	c.InvalidateAll()
	for _, k := range []string{"RAG", "LLM"} {
		if id, ok := c.Get(k); ok {
			t.Errorf("Get(%q) = (%v, true) after InvalidateAll; want miss", k, id)
		}
	}
	// Cache is reusable after invalidation.
	again := uuid.New()
	c.Put("RAG", again)
	if id, _ := c.Get("RAG"); id != again {
		t.Errorf("Get(RAG) after refill = %v; want %v", id, again)
	}
}

func TestAliasCacheNilReceiverIsNoop(t *testing.T) {
	// nil-receiver guards exist so resolver wiring without a cache
	// (tests / opt-out config) can call cache methods unconditionally.
	var c *AliasCache
	c.Put("RAG", uuid.New())
	if id, ok := c.Get("RAG"); ok || id != uuid.Nil {
		t.Errorf("nil cache Get returned (%v, %v); want miss", id, ok)
	}
	c.InvalidateAll() // must not panic
}

func TestAliasCacheConcurrentGetPut(t *testing.T) {
	// Smoke test for the sync.Map happy path: N goroutines hammering
	// distinct keys must not race detector trip. Run with -race.
	c := NewAliasCache()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			alias := uuid.NewString()
			id := uuid.New()
			c.Put(alias, id)
			if got, ok := c.Get(alias); !ok || got != id {
				t.Errorf("goroutine %d: round-trip failed got=(%v,%v) want=(%v,true)", i, got, ok, id)
			}
		}(i)
	}
	wg.Wait()
}

// Compile-time guard: *AliasCache satisfies AliasInvalidator so
// MergeAdminService can hold the cache through the interface without
// dragging in the concrete type.
var _ AliasInvalidator = (*AliasCache)(nil)
