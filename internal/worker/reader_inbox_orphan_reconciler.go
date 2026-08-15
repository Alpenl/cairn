package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"webtag/internal/observability"
	"webtag/internal/service"
)

const (
	defaultReaderInboxOrphanReconcileInterval = time.Minute
	defaultReaderInboxOrphanReconcileBatch    = 100
	defaultReaderInboxOrphanRoundTimeout      = 30 * time.Second
)

type ReaderInboxOrphanReconcilerOptions struct {
	Repairer     service.InboxProposalOrphanRepairer
	Interval     time.Duration
	BatchSize    int
	RoundTimeout time.Duration
	Logger       *slog.Logger
	Metrics      *observability.Metrics
}

// ReaderInboxOrphanReconciler repairs historical Inbox proposal attempts at
// startup and periodically thereafter. Cross-replica work division and the
// product/River transaction are owned by the repairer.
type ReaderInboxOrphanReconciler struct {
	repairer     service.InboxProposalOrphanRepairer
	interval     time.Duration
	batchSize    int
	roundTimeout time.Duration
	logger       *slog.Logger
	metrics      *observability.Metrics
	runGate      chan struct{}
	lifecycle    backgroundLoop
}

func NewReaderInboxOrphanReconciler(opts ReaderInboxOrphanReconcilerOptions) (*ReaderInboxOrphanReconciler, error) {
	if opts.Repairer == nil {
		return nil, errors.New("reader Inbox orphan reconciler: repairer is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultReaderInboxOrphanReconcileInterval
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 || batchSize > defaultReaderInboxOrphanReconcileBatch {
		batchSize = defaultReaderInboxOrphanReconcileBatch
	}
	roundTimeout := opts.RoundTimeout
	if roundTimeout <= 0 {
		roundTimeout = defaultReaderInboxOrphanRoundTimeout
	}
	runGate := make(chan struct{}, 1)
	runGate <- struct{}{}
	return &ReaderInboxOrphanReconciler{
		repairer:     opts.Repairer,
		interval:     interval,
		batchSize:    batchSize,
		roundTimeout: roundTimeout,
		logger:       opts.Logger,
		metrics:      opts.Metrics,
		runGate:      runGate,
		lifecycle:    newBackgroundLoop("reader_inbox_orphan_reconciler", opts.Logger),
	}, nil
}

func (r *ReaderInboxOrphanReconciler) Start(ctx context.Context) error {
	if r == nil || r.repairer == nil {
		return errors.New("reader Inbox orphan reconciler is not configured")
	}
	return r.lifecycle.Start(ctx, r.loop)
}

func (r *ReaderInboxOrphanReconciler) loop(ctx context.Context) {
	r.runIteration(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runIteration(ctx)
		}
	}
}

func (r *ReaderInboxOrphanReconciler) runIteration(ctx context.Context) {
	result, err := r.RunOnce(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) && r.logger != nil {
			r.logger.Warn("reader Inbox orphan reconciliation failed",
				"claimed", result.Claimed,
				"error", observability.SafeError(err),
			)
		}
		return
	}
	if result.Repaired > 0 && r.logger != nil {
		r.logger.Info("reader Inbox orphans repaired",
			"claimed", result.Claimed,
			"repaired", result.Repaired,
		)
	}
}

func (r *ReaderInboxOrphanReconciler) RunOnce(ctx context.Context) (service.InboxProposalOrphanRepairResult, error) {
	var result service.InboxProposalOrphanRepairResult
	if r == nil || r.repairer == nil {
		return result, errors.New("reader Inbox orphan reconciler is not configured")
	}
	roundCtx, cancel := context.WithTimeout(ctx, r.roundTimeout)
	defer cancel()
	select {
	case <-roundCtx.Done():
		return result, roundCtx.Err()
	case <-r.runGate:
	}
	defer func() { r.runGate <- struct{}{} }()

	result, err := r.repairer.RepairInboxProposalOrphans(roundCtx, r.batchSize)
	if r.metrics != nil {
		if err != nil {
			r.metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("failure").Inc()
		} else if result.Repaired > 0 {
			r.metrics.ReaderInboxDispatchRepairsTotal.WithLabelValues("success").Add(float64(result.Repaired))
		}
	}
	return result, err
}

func (r *ReaderInboxOrphanReconciler) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.lifecycle.Stop(ctx)
}
