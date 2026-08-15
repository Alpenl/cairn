package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"webtag/internal/observability"
)

const (
	defaultSiteEmbeddingBackfillInterval = 15 * time.Minute
	defaultSiteEmbeddingBackfillTimeout  = 10 * time.Minute
)

type siteEmbeddingBackfillRunner interface {
	Run(context.Context) (filled int, failed int, err error)
}

// SiteEmbeddingBackfillWorker owns periodic scheduling and lifecycle. The
// service runner retains candidate traversal, embedding calls, and CAS writes.
type SiteEmbeddingBackfillWorker struct {
	runner   siteEmbeddingBackfillRunner
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	lifecycle backgroundLoop
}

type SiteEmbeddingBackfillWorkerOptions struct {
	Runner   siteEmbeddingBackfillRunner
	Interval time.Duration
	Timeout  time.Duration
	Logger   *slog.Logger
}

func NewSiteEmbeddingBackfillWorker(options SiteEmbeddingBackfillWorkerOptions) (*SiteEmbeddingBackfillWorker, error) {
	if options.Runner == nil {
		return nil, errors.New("site embedding backfill worker: runner is required")
	}
	if options.Interval <= 0 {
		options.Interval = defaultSiteEmbeddingBackfillInterval
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultSiteEmbeddingBackfillTimeout
	}
	return &SiteEmbeddingBackfillWorker{
		runner: options.Runner, interval: options.Interval, timeout: options.Timeout, logger: options.Logger,
		lifecycle: newBackgroundLoop("site_embedding_backfill", options.Logger),
	}, nil
}

func (w *SiteEmbeddingBackfillWorker) Start(ctx context.Context) error {
	if w == nil || w.runner == nil {
		return errors.New("site embedding backfill worker is not configured")
	}
	return w.lifecycle.Start(ctx, w.loop)
}

func (w *SiteEmbeddingBackfillWorker) loop(ctx context.Context) {
	w.runIteration(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runIteration(ctx)
		}
	}
}

func (w *SiteEmbeddingBackfillWorker) runIteration(ctx context.Context) {
	iterationCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	filled, failed, err := w.runner.Run(iterationCtx)
	if w.logger == nil || errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		w.logger.Warn("site embedding backfill incomplete",
			"filled", filled, "failed", failed, "error", observability.SafeError(err))
		return
	}
	if failed > 0 {
		w.logger.Warn("site embedding backfill skipped candidates", "filled", filled, "failed", failed)
		return
	}
	if filled > 0 {
		w.logger.Info("site embedding backfill completed", "filled", filled)
	}
}

func (w *SiteEmbeddingBackfillWorker) RunOnce(ctx context.Context) (filled, failed int, err error) {
	if w == nil || w.runner == nil {
		return 0, 0, errors.New("site embedding backfill worker is not configured")
	}
	return w.runner.Run(ctx)
}

func (w *SiteEmbeddingBackfillWorker) Stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return w.lifecycle.Stop(ctx)
}
