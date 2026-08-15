package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"webtag/internal/service"
)

type linkEmbeddingBackfillRunnerStub struct {
	calls int
	err   error
}

func (s *linkEmbeddingBackfillRunnerStub) Run(ctx context.Context) (int, int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, err
	}
	s.calls++
	if s.err != nil {
		return 0, 0, 0, s.err
	}
	return 1, 0, 0, nil
}

func TestLinkEmbeddingBackfillWorkerRunsOnceForInstallation(t *testing.T) {
	runner := &linkEmbeddingBackfillRunnerStub{}
	w := newLinkEmbeddingBackfillWorkerForTest(runner, 2*time.Minute)

	job := &river.Job[service.LinkEmbeddingBackfillJobArgs]{}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("LinkEmbeddingBackfillWorker.Work() error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestLinkEmbeddingBackfillWorkerStopsBeforeRunningAfterCancellation(t *testing.T) {
	runner := &linkEmbeddingBackfillRunnerStub{}
	w := newLinkEmbeddingBackfillWorkerForTest(runner, 2*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.Work(ctx, &river.Job[service.LinkEmbeddingBackfillJobArgs]{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want none after cancellation", runner.calls)
	}
}

func TestLinkEmbeddingBackfillWorkerReturnsRunnerFailure(t *testing.T) {
	wantErr := errors.New("installation scan failed")
	runner := &linkEmbeddingBackfillRunnerStub{err: wantErr}
	w := newLinkEmbeddingBackfillWorkerForTest(runner, 2*time.Minute)

	err := w.Work(context.Background(), &river.Job[service.LinkEmbeddingBackfillJobArgs]{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work() error = %v, want runner failure", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestLinkEmbeddingBackfillWorkerRejectsNilJob(t *testing.T) {
	runner := &linkEmbeddingBackfillRunnerStub{}
	w := newLinkEmbeddingBackfillWorkerForTest(runner, time.Minute)
	if err := w.Work(context.Background(), nil); err == nil {
		t.Fatal("Work(nil) error = nil, want configuration error")
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d, want none for nil job", runner.calls)
	}
}

func TestLinkEmbeddingBackfillWorkerKeepsConfiguredTimeout(t *testing.T) {
	const timeout = 37 * time.Second
	w := newLinkEmbeddingBackfillWorkerForTest(&linkEmbeddingBackfillRunnerStub{}, timeout)
	if got := w.Timeout(nil); got != timeout {
		t.Fatalf("Timeout() = %s, want %s", got, timeout)
	}
}

func TestNewRiverQueueRegistersLinkEmbeddingBackfillPeriodicJob(t *testing.T) {
	queue, err := NewRiverQueue(RiverQueueOptions{
		Pool:                  &pgxpool.Pool{},
		Processor:             &discardRecordingProcessor{},
		LinkEmbeddingBackfill: &linkEmbeddingBackfillRunnerStub{},
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	configValue := reflect.ValueOf(queue.client).Elem().FieldByName("config").Elem()
	periodicJobs := configValue.FieldByName("PeriodicJobs")
	if !periodicJobs.IsValid() || periodicJobs.Len() != 1 {
		t.Fatalf("River periodic jobs = %v, want one backfill schedule", periodicJobs)
	}
}
