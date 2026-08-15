package worker

import (
	"context"
	"errors"
	"time"

	"github.com/riverqueue/river"

	"webtag/internal/service"
)

// ReaderInboxExpiryWorker turns the periodic River job into one bounded
// installation-level maintenance run. Row-level coordination remains in the
// repository lease SQL.
type ReaderInboxExpiryWorker struct {
	river.WorkerDefaults[service.ReaderInboxExpiryJobArgs]
	processor service.ReaderInboxExpiryJobProcessor
	timeout   time.Duration
}

func NewReaderInboxExpiryWorker(processor service.ReaderInboxExpiryJobProcessor, timeout time.Duration) *ReaderInboxExpiryWorker {
	return &ReaderInboxExpiryWorker{processor: processor, timeout: timeout}
}

func (w *ReaderInboxExpiryWorker) Timeout(*river.Job[service.ReaderInboxExpiryJobArgs]) time.Duration {
	return w.timeout
}

func (w *ReaderInboxExpiryWorker) Work(ctx context.Context, job *river.Job[service.ReaderInboxExpiryJobArgs]) error {
	if w == nil || w.processor == nil {
		return errors.New("reader inbox expiry processor is not configured")
	}
	batchSize := job.Args.BatchSize
	if batchSize <= 0 || batchSize > 500 {
		batchSize = service.ReaderInboxExpiryBatchSize
	}
	return w.processor.RunReaderInboxExpiryJob(ctx, batchSize)
}
