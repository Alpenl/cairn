package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type siteEmbeddingBackfillRunnerStub struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (s *siteEmbeddingBackfillRunnerStub) Run(ctx context.Context) (int, int, error) {
	s.mu.Lock()
	s.calls++
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	return 1, 0, ctx.Err()
}

func (s *siteEmbeddingBackfillRunnerStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestSiteEmbeddingBackfillWorkerStartsImmediatelyAndStops(t *testing.T) {
	t.Parallel()
	runner := &siteEmbeddingBackfillRunnerStub{started: make(chan struct{}, 1)}
	worker, err := NewSiteEmbeddingBackfillWorker(SiteEmbeddingBackfillWorkerOptions{
		Runner: runner, Interval: time.Hour, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewSiteEmbeddingBackfillWorker() error = %v", err)
	}
	if err := worker.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("backfill did not run immediately after Start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want one immediate iteration", runner.callCount())
	}
}

func TestSiteEmbeddingBackfillWorkerLifecycleRejectsRepeatedStart(t *testing.T) {
	t.Parallel()
	worker, err := NewSiteEmbeddingBackfillWorker(SiteEmbeddingBackfillWorkerOptions{
		Runner: &siteEmbeddingBackfillRunnerStub{}, Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(t.Context()); !errors.Is(err, ErrBackgroundAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrBackgroundAlreadyStarted", err)
	}
	if err := worker.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSiteEmbeddingBackfillWorkerRequiresRunner(t *testing.T) {
	t.Parallel()
	if _, err := NewSiteEmbeddingBackfillWorker(SiteEmbeddingBackfillWorkerOptions{}); err == nil {
		t.Fatal("NewSiteEmbeddingBackfillWorker() error = nil")
	}
}
