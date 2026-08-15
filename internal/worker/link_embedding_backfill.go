package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"webtag/internal/service"
)

type linkEmbeddingBackfillRunner interface {
	Run(context.Context) (filled int, failed int, skipped int, err error)
}

// LinkEmbeddingBackfillWorker runs one idempotent installation-level vector
// scan. River supplies durable scheduling and retry; the service runner owns
// batch limits, cancellation, and fail-soft embedding behavior.
type LinkEmbeddingBackfillWorker struct {
	river.WorkerDefaults[service.LinkEmbeddingBackfillJobArgs]
	runner  linkEmbeddingBackfillRunner
	timeout time.Duration
}

func NewLinkEmbeddingBackfillWorker(runner linkEmbeddingBackfillRunner, timeout time.Duration) *LinkEmbeddingBackfillWorker {
	return &LinkEmbeddingBackfillWorker{runner: runner, timeout: timeout}
}

func newLinkEmbeddingBackfillWorkerForTest(runner linkEmbeddingBackfillRunner, timeout time.Duration) *LinkEmbeddingBackfillWorker {
	return &LinkEmbeddingBackfillWorker{runner: runner, timeout: timeout}
}

func (w *LinkEmbeddingBackfillWorker) Timeout(*river.Job[service.LinkEmbeddingBackfillJobArgs]) time.Duration {
	return w.timeout
}

func (w *LinkEmbeddingBackfillWorker) Work(ctx context.Context, job *river.Job[service.LinkEmbeddingBackfillJobArgs]) error {
	if w == nil || w.runner == nil {
		return fmt.Errorf("link embedding backfill runner is not configured")
	}
	if job == nil {
		return fmt.Errorf("link embedding backfill job is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, _, err := w.runner.Run(ctx)
	return err
}
