package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/alloc"
	"webtag/internal/database"
	"webtag/internal/observability"
	"webtag/internal/service"
)

const (
	defaultParseTerminalReconcileInterval = time.Minute
	defaultParseTerminalReconcileBatch    = 100
	defaultParseTerminalIterationTimeout  = 30 * time.Second
	defaultParseMissingJobAfter           = 6 * time.Hour
	parseMissingJobCode                   = "parse_job_missing"
)

// Terminal rows are joined by the two immutable IDs in River args. Text
// equality keeps malformed JSON identities from reaching a UUID cast.
const listParseTerminalMismatchesSQL = `
SELECT r.id, r.state::text, p.link_id, p.id
FROM river_job r
JOIN parse_jobs p
  ON p.id::text = r.args->>'parse_job_id'
 AND p.link_id::text = r.args->>'link_id'
JOIN links l ON l.id = p.link_id
WHERE r.id > $1
  AND r.kind = $2
  AND r.state IN ('cancelled', 'completed', 'discarded')
  AND p.status IN ('pending', 'processing')
  AND l.status IN ('pending', 'processing')
ORDER BY r.id ASC
LIMIT $3`

// An attempt is missing only when its exact River identity is absent and no
// active job for the same link can be a replacement. The latest-attempt and
// link-state guards prevent an old parse_jobs row from downgrading newer work.
const listParseMissingAttemptsSQL = `
SELECT p.id, p.link_id, p.updated_at
FROM parse_jobs p
JOIN links l ON l.id = p.link_id
WHERE p.status IN ('pending', 'processing')
  AND l.status IN ('pending', 'processing')
  AND p.updated_at < $1
  AND (p.updated_at, p.id) > ($2::timestamptz, $3::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM parse_jobs newer
      WHERE newer.link_id = p.link_id
        AND (newer.created_at, newer.id) > (p.created_at, p.id)
  )
  AND NOT EXISTS (
      SELECT 1 FROM river_job current_job
      WHERE current_job.kind = $4
        AND current_job.args->>'parse_job_id' = p.id::text
        AND current_job.args->>'link_id' = p.link_id::text
  )
  AND NOT EXISTS (
      SELECT 1 FROM river_job active_job
      WHERE active_job.kind = $4
        AND active_job.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
        AND active_job.args->>'link_id' = p.link_id::text
  )
ORDER BY p.updated_at ASC, p.id ASC
LIMIT $5`

// ParseTerminalProjector is the product-state operation used after River has
// finalized or lost a parse job. ParsePipeline's exact link/job conditional
// update makes duplicate work across replicas a harmless no-op.
type ParseTerminalProjector interface {
	RecordDiscard(context.Context, uuid.UUID, uuid.UUID, error) error
}

type ParseTerminalReconcilerOptions struct {
	Pool         *pgxpool.Pool
	Projector    ParseTerminalProjector
	Interval     time.Duration
	BatchSize    int
	RoundTimeout time.Duration
	MissingAfter time.Duration
	Logger       *slog.Logger
}

type ParseTerminalReconcileResult struct {
	Scanned    int
	Reconciled int
	Missing    int
	Invalid    int
	Failed     int
}

type parseTerminalCandidate struct {
	riverJobID int64
	state      rivertype.JobState
	linkID     uuid.UUID
	parseJobID uuid.UUID
}

type parseMissingCursor struct {
	updatedAt  time.Time
	parseJobID uuid.UUID
}

type parseMissingAttempt struct {
	linkID     uuid.UUID
	parseJobID uuid.UUID
	updatedAt  time.Time
}

type parseTerminalStore interface {
	ListMismatches(context.Context, int64, int) ([]parseTerminalCandidate, error)
	ListMissingAttempts(context.Context, time.Time, parseMissingCursor, int) ([]parseMissingAttempt, error)
}

type parseTerminalDB interface {
	database.TxBeginner
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type pgParseTerminalStore struct {
	db parseTerminalDB
}

func (s *pgParseTerminalStore) ListMismatches(ctx context.Context, afterID int64, limit int) ([]parseTerminalCandidate, error) {
	var candidates []parseTerminalCandidate
	err := database.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, listParseTerminalMismatchesSQL, afterID, (service.ParseLinkArgs{}).Kind(), limit)
		if err != nil {
			return fmt.Errorf("query parse terminal mismatches: %w", err)
		}
		defer rows.Close()

		candidates = make([]parseTerminalCandidate, 0, alloc.Hint(limit))
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var candidate parseTerminalCandidate
			var state string
			if err := rows.Scan(&candidate.riverJobID, &state, &candidate.linkID, &candidate.parseJobID); err != nil {
				return fmt.Errorf("scan parse terminal mismatch: %w", err)
			}
			candidate.state = rivertype.JobState(state)
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate parse terminal mismatches: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func (s *pgParseTerminalStore) ListMissingAttempts(
	ctx context.Context,
	before time.Time,
	cursor parseMissingCursor,
	limit int,
) ([]parseMissingAttempt, error) {
	var attempts []parseMissingAttempt
	err := database.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(
			ctx,
			listParseMissingAttemptsSQL,
			before,
			cursor.updatedAt,
			cursor.parseJobID,
			(service.ParseLinkArgs{}).Kind(),
			limit,
		)
		if err != nil {
			return fmt.Errorf("query missing parse jobs: %w", err)
		}
		defer rows.Close()

		attempts = make([]parseMissingAttempt, 0, alloc.Hint(limit))
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var item parseMissingAttempt
			if err := rows.Scan(&item.parseJobID, &item.linkID, &item.updatedAt); err != nil {
				return fmt.Errorf("scan missing parse job: %w", err)
			}
			attempts = append(attempts, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate missing parse jobs: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return attempts, nil
}

// ParseTerminalReconciler closes the gap between River's durable state and
// links/parse_jobs. Every pass restarts from the still-unreconciled set, while
// keyset cursors keep failures from starving later rows within that pass.
type ParseTerminalReconciler struct {
	store        parseTerminalStore
	projector    ParseTerminalProjector
	interval     time.Duration
	batchSize    int
	roundTimeout time.Duration
	missingAfter time.Duration
	logger       *slog.Logger
	runGate      chan struct{}
	lifecycle    backgroundLoop
}

func NewParseTerminalReconciler(opts ParseTerminalReconcilerOptions) (*ParseTerminalReconciler, error) {
	if opts.Pool == nil {
		return nil, errors.New("parse terminal reconciler: pool is required")
	}
	if opts.Projector == nil {
		return nil, errors.New("parse terminal reconciler: projector is required")
	}
	reconciler := newParseTerminalReconciler(
		&pgParseTerminalStore{db: opts.Pool},
		opts.Projector,
		opts.Interval,
		opts.BatchSize,
		opts.Logger,
	)
	if opts.RoundTimeout > 0 {
		reconciler.roundTimeout = opts.RoundTimeout
	}
	if opts.MissingAfter > 0 {
		reconciler.missingAfter = opts.MissingAfter
	}
	return reconciler, nil
}

func newParseTerminalReconciler(
	store parseTerminalStore,
	projector ParseTerminalProjector,
	interval time.Duration,
	batchSize int,
	logger *slog.Logger,
) *ParseTerminalReconciler {
	if interval <= 0 {
		interval = defaultParseTerminalReconcileInterval
	}
	if batchSize <= 0 {
		batchSize = defaultParseTerminalReconcileBatch
	}
	runGate := make(chan struct{}, 1)
	runGate <- struct{}{}
	return &ParseTerminalReconciler{
		store:        store,
		projector:    projector,
		interval:     interval,
		batchSize:    batchSize,
		roundTimeout: defaultParseTerminalIterationTimeout,
		missingAfter: defaultParseMissingJobAfter,
		logger:       logger,
		runGate:      runGate,
		lifecycle:    newBackgroundLoop("parse_terminal_reconciler", logger),
	}
}

func (r *ParseTerminalReconciler) Start(ctx context.Context) error {
	if r == nil || r.store == nil || r.projector == nil {
		return errors.New("parse terminal reconciler is not configured")
	}
	return r.lifecycle.Start(ctx, r.loop)
}

func (r *ParseTerminalReconciler) loop(ctx context.Context) {
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

func (r *ParseTerminalReconciler) runIteration(ctx context.Context) {
	result, err := r.RunOnce(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && r.logger != nil {
		r.logger.Warn("parse terminal reconciliation incomplete",
			"scanned", result.Scanned,
			"reconciled", result.Reconciled,
			"missing", result.Missing,
			"invalid", result.Invalid,
			"failed", result.Failed,
			"error", observability.SafeError(err),
		)
		return
	}
	if result.Reconciled > 0 && r.logger != nil {
		r.logger.Info("parse terminal reconciliation complete",
			"scanned", result.Scanned,
			"reconciled", result.Reconciled,
			"missing", result.Missing,
			"invalid", result.Invalid,
		)
	}
}

func (r *ParseTerminalReconciler) RunOnce(ctx context.Context) (ParseTerminalReconcileResult, error) {
	var result ParseTerminalReconcileResult
	if r == nil || r.store == nil || r.projector == nil {
		return result, errors.New("parse terminal reconciler is not configured")
	}
	roundCtx, cancelRound := context.WithTimeout(ctx, r.roundTimeout)
	defer cancelRound()
	select {
	case <-roundCtx.Done():
		return result, roundCtx.Err()
	case <-r.runGate:
	}
	defer func() { r.runGate <- struct{}{} }()

	terminalCtx, cancelTerminal := parseTerminalPassContext(roundCtx)
	terminalErr := r.reconcileTerminalJobs(terminalCtx, &result)
	cancelTerminal()
	missingErr := r.reconcileMissingJobs(roundCtx, &result)
	return result, errors.Join(terminalErr, missingErr)
}

func parseTerminalPassContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, remaining/2)
}

func (r *ParseTerminalReconciler) reconcileTerminalJobs(ctx context.Context, result *ParseTerminalReconcileResult) error {
	var passErr error
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(passErr, err)
		}
		candidates, err := r.store.ListMismatches(ctx, afterID, r.batchSize)
		if err != nil {
			return errors.Join(passErr, err)
		}
		if len(candidates) > r.batchSize {
			return errors.Join(passErr, fmt.Errorf("parse terminal store returned %d rows above limit %d", len(candidates), r.batchSize))
		}
		if len(candidates) == 0 {
			return passErr
		}
		for _, candidate := range candidates {
			if candidate.riverJobID <= afterID {
				return errors.Join(passErr, errors.New("parse terminal store returned non-increasing cursor"))
			}
			afterID = candidate.riverJobID
			if candidate.linkID == uuid.Nil || candidate.parseJobID == uuid.Nil {
				result.Invalid++
				passErr = errors.Join(passErr, errors.New("parse terminal store returned invalid attempt identity"))
				continue
			}
			result.Scanned++
			if err := r.reconcileCandidate(ctx, candidate); err != nil {
				result.Failed++
				passErr = errors.Join(passErr, err)
				continue
			}
			result.Reconciled++
		}
		if len(candidates) < r.batchSize {
			return passErr
		}
	}
}

func (r *ParseTerminalReconciler) reconcileCandidate(ctx context.Context, candidate parseTerminalCandidate) error {
	cause := fmt.Errorf(
		"river parse job %d reached terminal state %s before its product state was reconciled",
		candidate.riverJobID,
		candidate.state,
	)
	if err := r.projector.RecordDiscard(ctx, candidate.linkID, candidate.parseJobID, cause); err != nil {
		return fmt.Errorf("project River parse terminal %d: %w", candidate.riverJobID, err)
	}
	return nil
}

func (r *ParseTerminalReconciler) reconcileMissingJobs(ctx context.Context, result *ParseTerminalReconcileResult) error {
	cutoff := time.Now().Add(-r.missingAfter)
	var cursor parseMissingCursor
	var passErr error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(passErr, err)
		}
		attempts, err := r.store.ListMissingAttempts(ctx, cutoff, cursor, r.batchSize)
		if err != nil {
			return errors.Join(passErr, err)
		}
		if len(attempts) > r.batchSize {
			return errors.Join(passErr, fmt.Errorf("parse missing-job store returned %d rows above limit %d", len(attempts), r.batchSize))
		}
		if len(attempts) == 0 {
			return passErr
		}
		for _, item := range attempts {
			if !parseMissingCursorAfter(item, cursor) {
				return errors.Join(passErr, errors.New("parse missing-job store returned non-increasing cursor"))
			}
			cursor = parseMissingCursor{updatedAt: item.updatedAt, parseJobID: item.parseJobID}
			if item.linkID == uuid.Nil || item.parseJobID == uuid.Nil {
				result.Invalid++
				passErr = errors.Join(passErr, errors.New("parse missing-job store returned invalid attempt identity"))
				continue
			}
			result.Scanned++
			if err := r.reconcileMissingAttempt(ctx, item); err != nil {
				result.Failed++
				passErr = errors.Join(passErr, err)
				continue
			}
			result.Reconciled++
			result.Missing++
		}
		if len(attempts) < r.batchSize {
			return passErr
		}
	}
}

func parseMissingCursorAfter(item parseMissingAttempt, cursor parseMissingCursor) bool {
	return item.updatedAt.After(cursor.updatedAt) ||
		(item.updatedAt.Equal(cursor.updatedAt) && bytesAfterUUID(item.parseJobID, cursor.parseJobID))
}

func bytesAfterUUID(left, right uuid.UUID) bool {
	for index := range left {
		if left[index] == right[index] {
			continue
		}
		return left[index] > right[index]
	}
	return false
}

func (r *ParseTerminalReconciler) reconcileMissingAttempt(ctx context.Context, item parseMissingAttempt) error {
	cause := fmt.Errorf("river row absent beyond parse recovery threshold: %w", service.ErrParseJobMissing)
	if err := r.projector.RecordDiscard(ctx, item.linkID, item.parseJobID, cause); err != nil {
		return fmt.Errorf("project missing parse job %s: %w", item.parseJobID, err)
	}
	if r.logger != nil {
		r.logger.InfoContext(ctx, "parse missing job reconciled",
			"link_id", item.linkID.String(),
			"parse_job_id", item.parseJobID.String(),
			"code", parseMissingJobCode,
		)
	}
	return nil
}

func (r *ParseTerminalReconciler) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.lifecycle.Stop(ctx)
}
