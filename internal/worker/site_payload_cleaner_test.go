package worker

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/observability"
)

type sitePayloadCleanupStoreStub struct {
	candidates []sitePayloadCandidate
	purged     map[uuid.UUID]bool
	seen       []sitePayloadCandidate
	err        error
}

type ownerBlockingSitePayloadStore struct {
	entered chan struct{}
}

func (s *ownerBlockingSitePayloadStore) ListExpired(ctx context.Context, _ int) ([]sitePayloadCandidate, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*ownerBlockingSitePayloadStore) PurgeExpired(context.Context, uuid.UUID) (bool, error) {
	panic("PurgeExpired must not run when discovery is blocked")
}

func TestSitePayloadCleanerRecordsOnlyAggregatePurgeResults(t *testing.T) {
	metrics := observability.NewMetrics()
	good, stale := uuid.New(), uuid.New()
	store := &sitePayloadCleanupStoreStub{candidates: []sitePayloadCandidate{{linkID: good}, {linkID: stale}}, purged: map[uuid.UUID]bool{good: true, stale: false}}
	cleaner := newSitePayloadCleanerWithMetrics(store, 0, 0, nil, metrics)
	if _, err := cleaner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.candidates = []sitePayloadCandidate{{linkID: uuid.New()}}
	store.err = errors.New("write failed")
	if _, err := cleaner.RunOnce(context.Background()); !errors.Is(err, store.err) {
		t.Fatalf("RunOnce error = %v", err)
	}
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`webtag_site_payload_purge_total{result="success",trigger="deadline"} 1`,
		`webtag_site_payload_purge_total{result="skipped",trigger="deadline"} 1`,
		`webtag_site_payload_purge_total{result="error",trigger="deadline"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
}

func (s *sitePayloadCleanupStoreStub) ListExpired(context.Context, int) ([]sitePayloadCandidate, error) {
	return s.candidates, nil
}

func (s *sitePayloadCleanupStoreStub) PurgeExpired(_ context.Context, linkID uuid.UUID) (bool, error) {
	candidate := sitePayloadCandidate{linkID: linkID}
	s.seen = append(s.seen, candidate)
	if s.err != nil {
		return false, s.err
	}
	return s.purged[linkID], nil
}

func TestSitePayloadCleanerPurgesOnlyCandidatesWhoseFreshPredicateStillMatches(t *testing.T) {
	t.Parallel()
	linkA, linkB := uuid.New(), uuid.New()
	store := &sitePayloadCleanupStoreStub{
		candidates: []sitePayloadCandidate{{linkID: linkA}, {linkID: linkB}},
		// linkB models a row that was finalized as reading after discovery: its
		// conditional UPDATE returns zero rows and is harmless.
		purged: map[uuid.UUID]bool{linkA: true, linkB: false},
	}
	cleaner := newSitePayloadCleaner(store, 0, 0, nil)

	purged, err := cleaner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("RunOnce() purged = %d, want 1", purged)
	}
	if len(store.seen) != 2 || store.seen[0].linkID != linkA || store.seen[1].linkID != linkB {
		t.Fatalf("install-scoped purge calls = %#v, want both discovered links", store.seen)
	}
}

func TestSitePayloadCleanerQueriesDoNotDependOnTenantIdentity(t *testing.T) {
	t.Parallel()
	for _, query := range []string{listExpiredSitePayloadsSQL, purgeExpiredSitePayloadSQL} {
		lower := strings.ToLower(query)
		if strings.Contains(lower, "tenant") || strings.Contains(lower, "app.") {
			t.Fatalf("site payload cleanup query retains tenant identity: %s", query)
		}
	}
}

func TestSitePayloadCleanerStopsAtMutationError(t *testing.T) {
	t.Parallel()
	store := &sitePayloadCleanupStoreStub{
		candidates: []sitePayloadCandidate{{linkID: uuid.New()}},
		purged:     map[uuid.UUID]bool{},
		err:        errors.New("database unavailable"),
	}
	cleaner := newSitePayloadCleaner(store, 0, 0, nil)

	if _, err := cleaner.RunOnce(context.Background()); !errors.Is(err, store.err) {
		t.Fatalf("RunOnce() error = %v, want %v", err, store.err)
	}
}

func TestSitePayloadCleanerUsesBoundedLinearLifecycle(t *testing.T) {
	t.Parallel()

	cleaner := newSitePayloadCleaner(&sitePayloadCleanupStoreStub{purged: make(map[uuid.UUID]bool)}, time.Hour, 1, nil)
	if err := cleaner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if err := cleaner.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := cleaner.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop error = %v, want ErrBackgroundStopped", err)
	}
}

func TestSitePayloadCleanerOwnerCancellationInterruptsActiveStoreCall(t *testing.T) {
	t.Parallel()

	store := &ownerBlockingSitePayloadStore{entered: make(chan struct{}, 1)}
	cleaner := newSitePayloadCleaner(store, time.Hour, 1, nil)
	cleaner.timeout = time.Hour
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()

	if err := cleaner.Start(ownerCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("site payload cleanup store call did not start")
	}

	cancelOwner()
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := cleaner.Stop(joinCtx); err != nil {
		t.Fatalf("Stop() after owner cancellation error = %v", err)
	}
}
