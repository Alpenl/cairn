package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"webtag/internal/observability"
	"webtag/internal/service"
)

type readerInboxOrphanCounterStub struct {
	mu    sync.Mutex
	count int64
	calls int
	kind  string
}

func (s *readerInboxOrphanCounterStub) CountInboxDispatchOrphans(_ context.Context, kind string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.kind = kind
	return s.count, nil
}

func TestReaderInboxOrphanBacklogGaugeUsesCachedDatabaseCount(t *testing.T) {
	t.Parallel()
	metrics := observability.NewMetrics()
	counter := &readerInboxOrphanCounterStub{count: 6}
	registerReaderInboxOrphanBacklogGauge(metrics, counter)

	for range 2 {
		recorder := httptest.NewRecorder()
		metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !strings.Contains(recorder.Body.String(), "webtag_reader_inbox_dispatch_orphan_backlog 6") {
			t.Fatalf("metrics body missing Inbox orphan backlog:\n%s", recorder.Body.String())
		}
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.calls != 1 || counter.kind != service.ReaderInboxSummaryJobKind {
		t.Fatalf("counter calls = %d kind = %q, want one call for %q", counter.calls, counter.kind, service.ReaderInboxSummaryJobKind)
	}
}
