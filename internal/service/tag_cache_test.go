package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTagCacheReturnsCachedValuesBeforeTTLExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cache := NewTagCache(5*time.Minute, func() time.Time { return now })

	loads := 0
	loader := func(context.Context) ([]TagCount, error) {
		loads++
		return []TagCount{{Tag: "go", Count: 2}}, nil
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}

	second, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loads)
	}

	assertTagCountsEqual(t, first, []TagCount{{Tag: "go", Count: 2}})
	assertTagCountsEqual(t, second, []TagCount{{Tag: "go", Count: 2}})
}

func TestTagCacheReloadsAfterTTLExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cache := NewTagCache(5*time.Minute, func() time.Time { return now })

	loads := 0
	loader := func(context.Context) ([]TagCount, error) {
		loads++
		return []TagCount{{Tag: "go", Count: loads}}, nil
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}

	now = now.Add(5*time.Minute + time.Second)

	second, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if loads != 2 {
		t.Fatalf("loader calls = %d, want 2", loads)
	}

	assertTagCountsEqual(t, first, []TagCount{{Tag: "go", Count: 1}})
	assertTagCountsEqual(t, second, []TagCount{{Tag: "go", Count: 2}})
}

func TestTagCacheDoesNotCacheLoaderErrors(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(time.Minute, time.Now)
	loads := 0
	boom := errors.New("boom")

	loader := func(context.Context) ([]TagCount, error) {
		loads++
		if loads == 1 {
			return nil, boom
		}
		return []TagCount{{Tag: "go", Count: 1}}, nil
	}

	_, err := cache.Get(context.Background(), loader)
	if !errors.Is(err, boom) {
		t.Fatalf("first Get() error = %v, want %v", err, boom)
	}

	got, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if loads != 2 {
		t.Fatalf("loader calls = %d, want 2", loads)
	}

	assertTagCountsEqual(t, got, []TagCount{{Tag: "go", Count: 1}})
}

// TestTagCacheIsolatesLoaderSlice locks in the caller-isolation half of
// L2: the slice returned by the loader is the only one the cache fully
// owns, so a loader that retains a writable handle and mutates it after
// returning must not corrupt the cached snapshot. Get() return values are
// shared read-only across waiters (see TagCache godoc); the defensive copy
// happens once on the write path, not three times across miss + return.
func TestTagCacheIsolatesLoaderSlice(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(time.Minute, time.Now)
	loaderSlice := []TagCount{{Tag: "go", Count: 1}}
	loader := func(context.Context) ([]TagCount, error) {
		return loaderSlice, nil
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	// Mutate the slice the loader returned. The cache cloned it on the way
	// in, so subsequent reads must still see the original tag value.
	loaderSlice[0].Tag = "mutated-by-loader"

	second, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	assertTagCountsEqual(t, first, []TagCount{{Tag: "go", Count: 1}})
	assertTagCountsEqual(t, second, []TagCount{{Tag: "go", Count: 1}})
}

func TestTagCacheReturnsStaleValuesWhenRefreshFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cache := NewTagCache(5*time.Minute, func() time.Time { return now })
	loads := 0
	boom := errors.New("boom")

	loader := func(context.Context) ([]TagCount, error) {
		loads++
		if loads == 1 {
			return []TagCount{{Tag: "go", Count: 1}}, nil
		}
		return nil, boom
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}

	now = now.Add(5*time.Minute + time.Second)

	second, err := cache.Get(context.Background(), loader)
	if !errors.Is(err, boom) {
		t.Fatalf("second Get() error = %v, want %v", err, boom)
	}

	assertTagCountsEqual(t, first, []TagCount{{Tag: "go", Count: 1}})
	assertTagCountsEqual(t, second, []TagCount{{Tag: "go", Count: 1}})
}

func TestTagCacheCachesSuccessfulEmptyResults(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(time.Minute, time.Now)
	loads := 0
	loader := func(context.Context) ([]TagCount, error) {
		loads++
		return []TagCount{}, nil
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}

	second, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loads)
	}
	assertTagCountsEqual(t, first, []TagCount{})
	assertTagCountsEqual(t, second, []TagCount{})
}

func TestTagCacheCoalescesConcurrentColdLoads(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(time.Minute, time.Now)
	start := make(chan struct{})
	loads := 0
	loader := func(context.Context) ([]TagCount, error) {
		loads++
		<-start
		return []TagCount{{Tag: "go", Count: 1}}, nil
	}

	var wg sync.WaitGroup
	results := make(chan []TagCount, 3)
	errorsCh := make(chan error, 3)

	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.Get(context.Background(), loader)
			results <- got
			errorsCh <- err
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)

	if loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loads)
	}

	for err := range errorsCh {
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}

	for got := range results {
		assertTagCountsEqual(t, got, []TagCount{{Tag: "go", Count: 1}})
	}
}

func TestTagCacheDisablesCachingWhenTTLIsZero(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(0, time.Now)
	loads := 0
	loader := func(context.Context) ([]TagCount, error) {
		loads++
		return []TagCount{{Tag: "go", Count: loads}}, nil
	}

	first, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	second, err := cache.Get(context.Background(), loader)
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if loads != 2 {
		t.Fatalf("loader calls = %d, want 2", loads)
	}
	assertTagCountsEqual(t, first, []TagCount{{Tag: "go", Count: 1}})
	assertTagCountsEqual(t, second, []TagCount{{Tag: "go", Count: 2}})
}

func TestTagCacheSingleflightDeduplicates(t *testing.T) {
	t.Parallel()

	cache := NewTagCache(time.Minute, time.Now)

	var calls int32
	start := make(chan struct{})
	loader := func(context.Context) ([]TagCount, error) {
		atomic.AddInt32(&calls, 1)
		// Block until released so that all 50 goroutines pile up on the
		// same in-flight load and singleflight has a chance to coalesce
		// them into a single loader invocation.
		<-start
		return []TagCount{{Tag: "go", Count: 1}}, nil
	}

	const n = 50
	var wg sync.WaitGroup
	results := make(chan []TagCount, n)
	errs := make(chan error, n)

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.Get(context.Background(), loader)
			results <- got
			errs <- err
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	close(errs)

	got := atomic.LoadInt32(&calls)
	if got > 2 {
		t.Fatalf("loader calls = %d, want <= 2 (singleflight should coalesce)", got)
	}

	for err := range errs {
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}

	for got := range results {
		assertTagCountsEqual(t, got, []TagCount{{Tag: "go", Count: 1}})
	}
}

func assertTagCountsEqual(t *testing.T, got, want []TagCount) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%#v want=%#v", len(got), len(want), got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d = %#v, want %#v; got=%#v want=%#v", i, got[i], want[i], got, want)
		}
	}
}
