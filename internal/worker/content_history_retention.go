package worker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
	"webtag/internal/observability"
)

const (
	// ContentHistoryRetentionJobKind is persisted in river_job and is also the
	// unique periodic registration ID. Changing it would strand retries.
	ContentHistoryRetentionJobKind        = "reader_content_history_retention"
	ContentHistoryRetentionJobMaxAttempts = 3
	ContentHistoryRetentionJobTimeout     = 15 * time.Minute
	ContentHistoryRetentionInterval       = 15 * time.Minute
	ContentHistoryRetentionBatchSize      = 100
	ContentHistoryRetentionRunLimit       = 1000
)

// ContentHistoryRetentionJobArgs is intentionally empty. A single periodic
// job owns bounded cleanup for the installation.
type ContentHistoryRetentionJobArgs struct{}

func (ContentHistoryRetentionJobArgs) Kind() string { return ContentHistoryRetentionJobKind }

func (ContentHistoryRetentionJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: ContentHistoryRetentionJobMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

type contentHistoryCleanupStore interface {
	CleanupContentHistoryBatch(context.Context, int) (int, error)
}

// ContentHistoryRetentionWorker runs bounded installation-level repository
// transactions. Any failure lets River retry the durable, idempotent job.
type ContentHistoryRetentionWorker struct {
	river.WorkerDefaults[ContentHistoryRetentionJobArgs]
	store   contentHistoryCleanupStore
	timeout time.Duration
	logger  *slog.Logger
	metrics *observability.Metrics
}

func NewContentHistoryRetentionWorker(
	store contentHistoryCleanupStore,
	timeout time.Duration,
	logger *slog.Logger,
	metrics *observability.Metrics,
) *ContentHistoryRetentionWorker {
	return &ContentHistoryRetentionWorker{
		store:   store,
		timeout: timeout,
		logger:  logger,
		metrics: metrics,
	}
}

func newContentHistoryRetentionWorkerForTest(
	store contentHistoryCleanupStore,
	timeout time.Duration,
	logger *slog.Logger,
	metrics *observability.Metrics,
) *ContentHistoryRetentionWorker {
	return &ContentHistoryRetentionWorker{
		store: store, timeout: timeout, logger: logger, metrics: metrics,
	}
}

func (w *ContentHistoryRetentionWorker) Timeout(*river.Job[ContentHistoryRetentionJobArgs]) time.Duration {
	return w.timeout
}

func (w *ContentHistoryRetentionWorker) Work(ctx context.Context, job *river.Job[ContentHistoryRetentionJobArgs]) error {
	if w == nil || w.store == nil {
		return errors.New("content history retention store is not configured")
	}
	if job == nil {
		return errors.New("content history retention job is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.runInstallation(ctx)
}

func (w *ContentHistoryRetentionWorker) runInstallation(ctx context.Context) error {
	deletedTotal := 0
	backlog := false
	for deletedTotal < ContentHistoryRetentionRunLimit {
		if err := ctx.Err(); err != nil {
			category := contentHistoryRetentionFailureCategory(err)
			w.observe(ctx, deletedTotal, backlog, category)
			return newContentHistoryRetentionRetryError(err, category)
		}

		deleted, err := w.store.CleanupContentHistoryBatch(ctx, ContentHistoryRetentionBatchSize)
		if err != nil {
			category := contentHistoryRetentionFailureCategory(err)
			w.observe(ctx, deletedTotal, backlog, category)
			return newContentHistoryRetentionRetryError(err, category)
		}
		if deleted < 0 || deleted > ContentHistoryRetentionBatchSize {
			err := errors.New("content history cleanup returned an invalid batch count")
			w.observe(ctx, deletedTotal, backlog, "invalid_result")
			return newContentHistoryRetentionRetryError(err, "invalid_result")
		}

		deletedTotal += deleted
		// A full final batch is a conservative backlog signal. It may produce
		// one harmless no-op on the next tick, but it can never hide remaining
		// rows or exceed this tick's hard limit to find out.
		backlog = deleted == ContentHistoryRetentionBatchSize
		if !backlog {
			break
		}
	}

	w.observe(ctx, deletedTotal, backlog, "none")
	return nil
}

func (w *ContentHistoryRetentionWorker) observe(
	ctx context.Context,
	deleted int,
	backlog bool,
	failureCategory string,
) {
	if w.metrics != nil {
		if w.metrics.ReaderContentHistoryDeletedTotal != nil {
			w.metrics.ReaderContentHistoryDeletedTotal.WithLabelValues().Add(float64(deleted))
		}
		if w.metrics.ReaderContentHistoryCleanupRunsTotal != nil {
			w.metrics.ReaderContentHistoryCleanupRunsTotal.WithLabelValues(
				strconv.FormatBool(backlog),
				failureCategory,
			).Inc()
		}
	}
	if w.logger == nil {
		return
	}
	attrs := []any{
		"deleted", deleted,
		"backlog", backlog,
		"failure_category", failureCategory,
	}
	if failureCategory == "none" {
		w.logger.InfoContext(ctx, "Reader content history retention completed", attrs...)
		return
	}
	w.logger.WarnContext(ctx, "Reader content history retention failed", attrs...)
}

func contentHistoryRetentionFailureCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		category := errsafe.ClassifyError(err)
		if category == "none" || category == "unknown" {
			return "database"
		}
		return category
	}
}

type contentHistoryRetentionRetryError struct {
	cause    error
	category string
}

func newContentHistoryRetentionRetryError(cause error, category string) error {
	return &contentHistoryRetentionRetryError{cause: cause, category: category}
}

func (e *contentHistoryRetentionRetryError) Error() string {
	return "reader content history retention failed: " + e.category
}

func (e *contentHistoryRetentionRetryError) Unwrap() error { return e.cause }
