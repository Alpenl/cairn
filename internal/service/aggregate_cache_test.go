package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

type countingTreeStore struct {
	repotest.BaseTreeStore
	calls       int
	scopedCalls map[model.LibraryKind]int
}

func newCountingTreeStore() *countingTreeStore {
	return &countingTreeStore{scopedCalls: make(map[model.LibraryKind]int)}
}

func (s *countingTreeStore) ListDomains(context.Context) (repository.DomainTreeSummarySet, error) {
	s.calls++
	return repository.DomainTreeSummarySet{
		Domains: []repository.DomainTreeSummary{{Domain: "library.example", Count: 4}},
		Total:   4,
	}, nil
}

func (s *countingTreeStore) ListDomainsScoped(_ context.Context, kind model.LibraryKind) (repository.DomainTreeSummarySet, error) {
	s.scopedCalls[kind]++
	return repository.DomainTreeSummarySet{
		Domains: []repository.DomainTreeSummary{{Domain: string(kind) + ".example", Count: 1}},
		Total:   1,
	}, nil
}

func TestDomainSummaryCacheCollapsesRepeatedReads(t *testing.T) {
	t.Parallel()
	store := newCountingTreeStore()
	service := NewTreeReadService(store, nil).WithDomainCache(NewDomainSummaryCache(time.Minute, nil))
	for range 10 {
		envelope, err := service.ListDomains(context.Background())
		if err != nil {
			t.Fatalf("ListDomains() error = %v", err)
		}
		if envelope.Total != 4 || len(envelope.Domains) != 1 {
			t.Fatalf("ListDomains() = %#v", envelope)
		}
	}
	if store.calls != 1 {
		t.Fatalf("domain aggregate loads = %d, want 1", store.calls)
	}
}

func TestDomainSummaryCacheKeepsLibraryScopesSeparate(t *testing.T) {
	t.Parallel()
	store := newCountingTreeStore()
	service := NewTreeReadService(store, nil).WithDomainCache(NewDomainSummaryCache(time.Minute, nil))
	for _, kind := range []model.LibraryKind{model.LibraryKindReading, model.LibraryKindSite} {
		for range 2 {
			got, err := service.ListDomainsScoped(context.Background(), string(kind))
			if err != nil {
				t.Fatalf("ListDomainsScoped(%s) error = %v", kind, err)
			}
			if got.Domains[0].Domain != string(kind)+".example" {
				t.Fatalf("ListDomainsScoped(%s) = %#v", kind, got)
			}
		}
		if store.scopedCalls[kind] != 1 {
			t.Fatalf("scope %s loads = %d, want 1", kind, store.scopedCalls[kind])
		}
	}
}

func TestDomainSummaryCacheDisabledKeepsDirectReads(t *testing.T) {
	t.Parallel()
	store := newCountingTreeStore()
	service := NewTreeReadService(store, nil).WithDomainCache(NewDomainSummaryCache(0, nil))
	for range 3 {
		if _, err := service.ListDomains(context.Background()); err != nil {
			t.Fatalf("ListDomains() error = %v", err)
		}
	}
	if store.calls != 3 {
		t.Fatalf("domain aggregate loads = %d, want 3", store.calls)
	}
}

type scopedCountingTagStore struct{ calls map[string]int }

func newScopedCountingTagStore() *scopedCountingTagStore {
	return &scopedCountingTagStore{calls: make(map[string]int)}
}

func (s *scopedCountingTagStore) ListDistinct(context.Context) ([]string, error) { return nil, nil }
func (s *scopedCountingTagStore) ListCounts(context.Context) ([]repository.TagCount, error) {
	s.calls[""]++
	return []repository.TagCount{{Tag: "global", Count: 1}}, nil
}
func (s *scopedCountingTagStore) ListScopedCounts(_ context.Context, scope string) ([]repository.ScopedTagCount, error) {
	s.calls[scope]++
	return []repository.ScopedTagCount{{Tag: scope + "-tag", Count: 5, ReadingCount: 3, SiteCount: 2}}, nil
}

func TestTagCacheKeepsScopesSeparateAndInvalidatesTogether(t *testing.T) {
	t.Parallel()
	store := newScopedCountingTagStore()
	cache := NewTagCache(time.Minute, nil)
	service := NewTagReadService(store, cache)
	for _, scope := range []string{"", "reading", "site"} {
		for range 2 {
			var err error
			if scope == "" {
				_, err = service.List(context.Background())
			} else {
				_, err = service.ListScoped(context.Background(), scope)
			}
			if err != nil {
				t.Fatalf("load scope %q: %v", scope, err)
			}
		}
		if store.calls[scope] != 1 {
			t.Fatalf("scope %q loads = %d, want 1", scope, store.calls[scope])
		}
	}

	cache.Invalidate(context.Background())
	if _, err := service.List(context.Background()); err != nil {
		t.Fatalf("List() after invalidation: %v", err)
	}
	if _, err := service.ListScoped(context.Background(), "reading"); err != nil {
		t.Fatalf("ListScoped() after invalidation: %v", err)
	}
	if store.calls[""] != 2 || store.calls["reading"] != 2 {
		t.Fatalf("loads after invalidation = %#v, want global/reading=2", store.calls)
	}
}

func TestMultiCacheInvalidatorFansOutAndSkipsNil(t *testing.T) {
	t.Parallel()
	tagCache := NewTagCache(time.Minute, nil)
	domainCache := NewDomainSummaryCache(time.Minute, nil)
	invalidator := MultiCacheInvalidator{tagCache, nil, domainCache}
	tagStore := newScopedCountingTagStore()
	treeStore := newCountingTreeStore()
	tagService := NewTagReadService(tagStore, tagCache)
	treeService := NewTreeReadService(treeStore, nil).WithDomainCache(domainCache)
	if _, err := tagService.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := treeService.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	invalidator.Invalidate(context.Background())
	if _, err := tagService.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := treeService.ListDomains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tagStore.calls[""] != 2 || treeStore.calls != 2 {
		t.Fatalf("loads after fan-out = tags:%d domains:%d, want 2/2", tagStore.calls[""], treeStore.calls)
	}
}

func TestSnapshotCacheEvictsLeastRecentlyUsedVariant(t *testing.T) {
	t.Parallel()
	clock := time.Unix(0, 0).UTC()
	cache := NewSnapshotCache(time.Hour, func() time.Time { return clock }, func(v int) int { return v })
	cache.maxEntries = 2
	loads := 0
	loader := func(context.Context) (int, error) { loads++; return loads, nil }
	if _, err := cache.Get(context.Background(), "one", loader); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if _, err := cache.Get(context.Background(), "two", loader); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if _, err := cache.Get(context.Background(), "one", loader); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if _, err := cache.Get(context.Background(), "three", loader); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(context.Background(), "one", loader); err != nil {
		t.Fatal(err)
	}
	if loads != 3 {
		t.Fatalf("loads with retained LRU entry = %d, want 3", loads)
	}
	if _, err := cache.Get(context.Background(), "two", loader); err != nil {
		t.Fatal(err)
	}
	if loads != 4 {
		t.Fatalf("loads after evicted variant = %d, want 4", loads)
	}
}

func TestSnapshotCacheReturnsStaleOnLoaderError(t *testing.T) {
	t.Parallel()
	clock := time.Unix(0, 0).UTC()
	cache := NewSnapshotCache(time.Minute, func() time.Time { return clock }, func(v string) string { return v })
	if _, err := cache.Get(context.Background(), "", func(context.Context) (string, error) { return "fresh", nil }); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	boom := errors.New("db down")
	value, err := cache.Get(context.Background(), "", func(context.Context) (string, error) { return "", boom })
	if !errors.Is(err, boom) || value != "fresh" {
		t.Fatalf("stale result = %q, %v", value, err)
	}
}

func TestSnapshotCacheSingleflightMergesInstallationReads(t *testing.T) {
	t.Parallel()
	cache := NewSnapshotCache(time.Minute, nil, func(v string) string { return v })
	var loads int64
	entered := make(chan struct{})
	release := make(chan struct{})
	loader := func(context.Context) (string, error) {
		if atomic.AddInt64(&loads, 1) == 1 {
			close(entered)
		}
		<-release
		return "shared", nil
	}
	values := make([]string, 2)
	var done sync.WaitGroup
	done.Add(2)
	for i := range values {
		go func(index int) {
			defer done.Done()
			values[index], _ = cache.Get(context.Background(), "", loader)
		}(i)
	}
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(release)
	done.Wait()
	if atomic.LoadInt64(&loads) != 1 || values[0] != "shared" || values[1] != "shared" {
		t.Fatalf("loads=%d values=%v, want one shared load", loads, values)
	}
}

func TestSnapshotCacheInvalidateDuringLoadIsNotSwallowed(t *testing.T) {
	t.Parallel()
	cache := NewSnapshotCache(time.Minute, nil, func(v string) string { return v })
	entered := make(chan struct{})
	release := make(chan struct{})
	var loads int64
	var first string
	done := make(chan struct{})
	go func() {
		first, _ = cache.Get(context.Background(), "", func(context.Context) (string, error) {
			atomic.AddInt64(&loads, 1)
			close(entered)
			<-release
			return "before-write", nil
		})
		close(done)
	}()
	<-entered
	cache.Invalidate(context.Background())
	close(release)
	<-done
	second, err := cache.Get(context.Background(), "", func(context.Context) (string, error) {
		atomic.AddInt64(&loads, 1)
		return "after-write", nil
	})
	if err != nil || first != "before-write" || second != "after-write" || atomic.LoadInt64(&loads) != 2 {
		t.Fatalf("first=%q second=%q loads=%d err=%v", first, second, loads, err)
	}
}
