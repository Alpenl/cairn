package worker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/alloc"
	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/service/linktranslation"
)

const (
	defaultTranslationTerminalReconcileInterval = time.Minute
	defaultTranslationTerminalReconcileBatch    = 100
	defaultTranslationTerminalRoundTimeout      = 30 * time.Second
	defaultTranslationMissingJobAfter           = 6 * time.Hour
)

// TranslationTerminalCode is persisted in link_translations.error_msg. The
// HTTP v2 field is unchanged; stable values let clients and operators
// distinguish terminal causes without extending the status enum.
type TranslationTerminalCode string

const (
	TranslationTerminalJobCancelled      TranslationTerminalCode = "translation_job_cancelled"
	TranslationTerminalJobDiscarded      TranslationTerminalCode = "translation_job_discarded"
	TranslationTerminalProjectionMissing TranslationTerminalCode = "terminal_projection_missing"
	TranslationTerminalJobMissing        TranslationTerminalCode = "translation_job_missing"

	translationTerminalAlreadyProjected       = "already_projected"
	translationTerminalStaleActiveReplacement = "active_replacement"
)

const listTranslationTerminalJobsSQL = `
SELECT id, kind, state::text, finalized_at, args
FROM river_job
WHERE kind IN ('` + model.TranslationJobKindV2 + `', '` + model.TranslationJobKindLegacy + `')
  AND state IN ('cancelled', 'completed', 'discarded')
  AND finalized_at IS NOT NULL
  AND (finalized_at, id) > ($1::timestamptz, $2::bigint)
ORDER BY finalized_at ASC, id ASC
LIMIT $3`

const latestTranslationTerminalCursorSQL = `
SELECT finalized_at, id
FROM river_job
WHERE kind IN ('` + model.TranslationJobKindV2 + `', '` + model.TranslationJobKindLegacy + `')
  AND state IN ('cancelled', 'completed', 'discarded')
  AND finalized_at IS NOT NULL
ORDER BY finalized_at DESC, id DESC
LIMIT 1`

const inspectTranslationTerminalProjectionSQL = `
SELECT candidate.current_river_job_id, candidate.attempt_generation,
       candidate.source_hash, candidate.source_content_revision, candidate.status,
       candidate.error_msg,
       ($2::boolean AND EXISTS (
           SELECT 1 FROM river_job AS active_job
           WHERE active_job.kind IN ($3, $4)
             AND active_job.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
             AND active_job.args->>'translation_id' = candidate.id::text
             AND (
                 (active_job.kind = $3
                  AND active_job.args->>'attempt_generation' = candidate.attempt_generation::text
                  AND active_job.args->>'source_hash' = candidate.source_hash
                  AND active_job.args->>'source_content_revision'
                      IS NOT DISTINCT FROM candidate.source_content_revision::text)
                 OR
                 (active_job.kind = $4
                  AND ((active_job.args ? 'attempt_generation'
                        AND active_job.args->>'attempt_generation' = candidate.attempt_generation::text)
                       OR (NOT (active_job.args ? 'attempt_generation')
                           AND candidate.attempt_generation = 0))
                  AND (
                      (candidate.attempt_generation = 0 AND NOT (active_job.args ? 'source_hash'))
                      OR (active_job.args->>'source_hash' = candidate.source_hash
                          AND active_job.args->>'source_content_revision'
                              IS NOT DISTINCT FROM candidate.source_content_revision::text)
                  ))
             )
       )) AS active_replacement
FROM link_translations AS candidate
WHERE candidate.id = $1`

const listTranslationMissingAttemptsSQL = `
SELECT t.id, t.attempt_generation, t.current_river_job_id,
       t.source_hash, t.source_content_revision, t.updated_at
FROM link_translations t
WHERE t.status IN ('pending', 'processing')
  AND t.current_river_job_id IS NOT NULL
  AND t.updated_at < $1
  AND (t.updated_at, t.id) > ($2::timestamptz, $3::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM river_job current_job
      WHERE current_job.id = t.current_river_job_id
  )
  AND NOT EXISTS (
      SELECT 1 FROM river_job active_job
      WHERE active_job.kind IN ($4, $5)
        AND active_job.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
        AND active_job.args->>'translation_id' = t.id::text
		AND (
		    (active_job.kind = $4
		     AND active_job.args->>'attempt_generation' = t.attempt_generation::text
		     AND active_job.args->>'source_hash' = t.source_hash
		     AND active_job.args->>'source_content_revision'
		         IS NOT DISTINCT FROM t.source_content_revision::text)
		    OR
		    (active_job.kind = $5
		     AND ((active_job.args ? 'attempt_generation'
		           AND active_job.args->>'attempt_generation' = t.attempt_generation::text)
		          OR (NOT (active_job.args ? 'attempt_generation') AND t.attempt_generation = 0))
		     AND (
		          (t.attempt_generation = 0 AND NOT (active_job.args ? 'source_hash'))
		          OR (
		              active_job.args->>'source_hash' = t.source_hash
		              AND active_job.args->>'source_content_revision'
		                  IS NOT DISTINCT FROM t.source_content_revision::text
		          )
		     ))
		)
  )
ORDER BY t.updated_at ASC, t.id ASC
LIMIT $6`

type translationTerminalCursor struct {
	finalizedAt time.Time
	riverJobID  int64
}

type translationTerminalHistoryFingerprint struct {
	digest   [sha256.Size]byte
	jobCount uint64
	valid    bool
}

type translationTerminalHistoryAccumulator struct {
	digest   hash.Hash
	jobCount uint64
}

// translationTerminalObservationTracker keeps first-observation detection
// constant-sized. A cursor alone cannot notice a transaction that commits
// after an earlier cycle passed its finalized_at. Retaining every River ID
// would detect that exactly but would grow with unbounded terminal history.
// Instead, each completed cycle fingerprints the prior baseline prefix. A
// changed prefix forces one later cycle to inspect that bounded cursor range;
// stale items already in the changed prefix can consequently be observed once
// more in that repair cycle. The greatest unfinished cursor also forces a
// later prefix pass, so retry-lane overflow cannot suppress classification.
type translationTerminalObservationTracker struct {
	observedThrough translationTerminalCursor

	baseline        translationTerminalHistoryFingerprint
	baselineThrough translationTerminalCursor

	cycleFull           translationTerminalHistoryAccumulator
	cyclePrefix         translationTerminalHistoryAccumulator
	cycleCompareThrough translationTerminalCursor
	cycleForceThrough   translationTerminalCursor
	forceNextThrough    translationTerminalCursor
	unfinishedThrough   translationTerminalCursor
}

type translationMissingCursor struct {
	updatedAt     time.Time
	translationID uuid.UUID
}

type translationTerminalJob struct {
	riverJobID  int64
	kind        string
	state       rivertype.JobState
	finalizedAt time.Time
	encodedArgs []byte
}

type translationMissingAttempt struct {
	attempt   model.TranslationAttempt
	updatedAt time.Time
}

type translationTerminalProjectionState struct {
	currentRiverJobID     *int64
	attemptGeneration     int64
	sourceHash            string
	sourceContentRevision *int64
	status                model.TranslationStatus
	errorMsg              *string
	activeReplacement     bool
}

type translationTerminalStaleInspector interface {
	InspectTerminalProjection(
		context.Context,
		model.TranslationAttempt,
		bool,
	) (translationTerminalProjectionState, bool, error)
}

type translationTerminalStore interface {
	LatestTerminalCursor(context.Context) (translationTerminalCursor, bool, error)
	ListTerminalJobs(context.Context, translationTerminalCursor, int) ([]translationTerminalJob, error)
	ListMissingAttempts(context.Context, time.Time, translationMissingCursor, int) ([]translationMissingAttempt, error)
}

// TranslationTerminalProjector is the exact current-attempt conditional
// transition used by both ordinary workers and the reconciler.
type TranslationTerminalProjector interface {
	Fail(context.Context, model.TranslationAttempt, string) (bool, error)
	FailMissing(context.Context, model.TranslationAttempt, string) (bool, error)
}

type translationTerminalDB interface {
	database.TxBeginner
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type pgTranslationTerminalStore struct {
	db translationTerminalDB
}

func (s *pgTranslationTerminalStore) LatestTerminalCursor(
	ctx context.Context,
) (translationTerminalCursor, bool, error) {
	var cursor translationTerminalCursor
	err := s.db.QueryRow(
		ctx,
		latestTranslationTerminalCursorSQL,
	).Scan(&cursor.finalizedAt, &cursor.riverJobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return translationTerminalCursor{}, false, nil
	}
	if err != nil {
		return translationTerminalCursor{}, false, fmt.Errorf("read River translation terminal high-water mark: %w", err)
	}
	return cursor, true, nil
}

func (s *pgTranslationTerminalStore) ListTerminalJobs(
	ctx context.Context,
	cursor translationTerminalCursor,
	limit int,
) ([]translationTerminalJob, error) {
	rows, err := s.db.Query(
		ctx,
		listTranslationTerminalJobsSQL,
		cursor.finalizedAt,
		cursor.riverJobID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list River translation terminal jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]translationTerminalJob, 0, alloc.Hint(limit))
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var job translationTerminalJob
		var state string
		if err := rows.Scan(&job.riverJobID, &job.kind, &state, &job.finalizedAt, &job.encodedArgs); err != nil {
			return nil, fmt.Errorf("scan River translation terminal job: %w", err)
		}
		job.state = rivertype.JobState(state)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate River translation terminal jobs: %w", err)
	}
	return jobs, nil
}

func (s *pgTranslationTerminalStore) ListMissingAttempts(
	ctx context.Context,
	before time.Time,
	cursor translationMissingCursor,
	limit int,
) ([]translationMissingAttempt, error) {
	var attempts []translationMissingAttempt
	err := database.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(
			ctx,
			listTranslationMissingAttemptsSQL,
			before,
			cursor.updatedAt,
			cursor.translationID,
			model.TranslationJobKindV2,
			model.TranslationJobKindLegacy,
			limit,
		)
		if err != nil {
			return fmt.Errorf("query missing translation jobs: %w", err)
		}
		defer rows.Close()

		attempts = make([]translationMissingAttempt, 0, alloc.Hint(limit))
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var item translationMissingAttempt
			var currentRiverJobID *int64
			if err := rows.Scan(
				&item.attempt.TranslationID,
				&item.attempt.AttemptGeneration,
				&currentRiverJobID,
				&item.attempt.SourceHash,
				&item.attempt.SourceContentRevision,
				&item.updatedAt,
			); err != nil {
				return fmt.Errorf("scan missing translation job: %w", err)
			}
			if currentRiverJobID == nil || *currentRiverJobID <= 0 {
				return errors.New("missing translation job candidate has invalid current job ID")
			}
			item.attempt.RiverJobID = *currentRiverJobID
			attempts = append(attempts, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate missing translation jobs: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return attempts, nil
}

func (s *pgTranslationTerminalStore) InspectTerminalProjection(
	ctx context.Context,
	attempt model.TranslationAttempt,
	missing bool,
) (translationTerminalProjectionState, bool, error) {
	var (
		state  translationTerminalProjectionState
		status string
		found  bool
	)
	err := database.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(
			ctx,
			inspectTranslationTerminalProjectionSQL,
			attempt.TranslationID,
			missing,
			model.TranslationJobKindV2,
			model.TranslationJobKindLegacy,
		).Scan(
			&state.currentRiverJobID,
			&state.attemptGeneration,
			&state.sourceHash,
			&state.sourceContentRevision,
			&status,
			&state.errorMsg,
			&state.activeReplacement,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect stale translation terminal projection: %w", err)
		}
		state.status = model.TranslationStatus(status)
		found = true
		return nil
	})
	if err != nil {
		return translationTerminalProjectionState{}, false, err
	}
	return state, found, nil
}

// TranslationTerminalReconcilerOptions wires the durable translation
// terminal-state repair loop. Construction is dormant; Runtime owns Start and
// Stop.
type TranslationTerminalReconcilerOptions struct {
	Pool           *pgxpool.Pool
	Projector      TranslationTerminalProjector
	LegacyAttempts linktranslation.LegacyAttemptResolver
	Interval       time.Duration
	BatchSize      int
	RoundTimeout   time.Duration
	MissingAfter   time.Duration
	Logger         *slog.Logger
	Metrics        *observability.Metrics
}

// TranslationTerminalReconcileResult summarizes one bounded reconciliation
// round. Failed items remain nonterminal and are retried in later rounds.
type TranslationTerminalReconcileResult struct {
	Scanned    int
	Reconciled int
	Missing    int
	Invalid    int
	Stale      int
	Failed     int
}

type TranslationTerminalReconciler struct {
	store          translationTerminalStore
	projector      TranslationTerminalProjector
	legacyAttempts linktranslation.LegacyAttemptResolver
	interval       time.Duration
	batchSize      int
	roundTimeout   time.Duration
	missingAfter   time.Duration
	logger         *slog.Logger
	metrics        *observability.Metrics
	runGate        chan struct{}

	terminalCycleActive  bool
	terminalCursor       translationTerminalCursor
	terminalCycleEnd     translationTerminalCursor
	terminalObservations translationTerminalObservationTracker
	terminalRetries      []translationTerminalJob
	terminalRetryIDs     map[int64]struct{}

	missingCycleActive bool
	missingCutoff      time.Time
	missingCursor      translationMissingCursor

	lifecycle backgroundLoop
}

func NewTranslationTerminalReconciler(
	opts TranslationTerminalReconcilerOptions,
) (*TranslationTerminalReconciler, error) {
	if opts.Pool == nil {
		return nil, errors.New("translation terminal reconciler: pool is required")
	}
	if opts.Projector == nil {
		return nil, errors.New("translation terminal reconciler: projector is required")
	}
	if opts.LegacyAttempts == nil {
		return nil, errors.New("translation terminal reconciler: legacy attempt resolver is required")
	}
	return newTranslationTerminalReconciler(
		&pgTranslationTerminalStore{db: opts.Pool},
		opts.Projector,
		opts.LegacyAttempts,
		opts.Interval,
		opts.BatchSize,
		opts.RoundTimeout,
		opts.MissingAfter,
		opts.Logger,
		opts.Metrics,
	), nil
}

func newTranslationTerminalReconciler(
	store translationTerminalStore,
	projector TranslationTerminalProjector,
	legacyAttempts linktranslation.LegacyAttemptResolver,
	interval time.Duration,
	batchSize int,
	roundTimeout time.Duration,
	missingAfter time.Duration,
	logger *slog.Logger,
	metrics *observability.Metrics,
) *TranslationTerminalReconciler {
	if interval <= 0 {
		interval = defaultTranslationTerminalReconcileInterval
	}
	if batchSize <= 0 {
		batchSize = defaultTranslationTerminalReconcileBatch
	}
	if roundTimeout <= 0 {
		roundTimeout = defaultTranslationTerminalRoundTimeout
	}
	if missingAfter <= 0 {
		missingAfter = defaultTranslationMissingJobAfter
	}
	runGate := make(chan struct{}, 1)
	runGate <- struct{}{}
	return &TranslationTerminalReconciler{
		store:            store,
		projector:        projector,
		legacyAttempts:   legacyAttempts,
		interval:         interval,
		batchSize:        batchSize,
		roundTimeout:     roundTimeout,
		missingAfter:     missingAfter,
		logger:           logger,
		metrics:          metrics,
		runGate:          runGate,
		terminalRetryIDs: make(map[int64]struct{}),
		lifecycle:        newBackgroundLoop("translation_terminal_reconciler", logger),
	}
}

func (r *TranslationTerminalReconciler) Start(ctx context.Context) error {
	if r == nil || r.store == nil || r.projector == nil || r.legacyAttempts == nil {
		return errors.New("translation terminal reconciler is not configured")
	}
	return r.lifecycle.Start(ctx, r.loop)
}

func (r *TranslationTerminalReconciler) loop(ctx context.Context) {
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

func (r *TranslationTerminalReconciler) runIteration(ctx context.Context) {
	roundCtx, cancel := context.WithTimeout(ctx, r.roundTimeout)
	result, err := r.RunOnce(roundCtx)
	cancel()
	if err != nil && !errors.Is(err, context.Canceled) && r.logger != nil {
		r.logger.Warn("translation terminal reconciliation incomplete",
			"scanned", result.Scanned,
			"reconciled", result.Reconciled,
			"missing", result.Missing,
			"invalid", result.Invalid,
			"stale", result.Stale,
			"failed", result.Failed,
			"error", observability.SafeError(err),
		)
	}
}

// RunOnce resumes bounded terminal and missing-job cycles across rounds. The
// terminal cycle freezes a visible high-water mark, advances to it even while
// newer terminals arrive, then wraps to zero. The wrap discovers a River
// transaction whose finalized_at was assigned before it committed and was
// therefore invisible when an earlier cycle passed its timestamp. The missing
// cycle similarly freezes its cutoff while retaining the current keyset cursor
// across round deadlines.
func (r *TranslationTerminalReconciler) RunOnce(
	ctx context.Context,
) (TranslationTerminalReconcileResult, error) {
	var result TranslationTerminalReconcileResult
	if r == nil || r.store == nil || r.projector == nil || r.legacyAttempts == nil {
		return result, errors.New("translation terminal reconciler is not configured")
	}
	roundCtx, cancelRound := context.WithTimeout(ctx, r.roundTimeout)
	defer cancelRound()
	select {
	case <-roundCtx.Done():
		return result, roundCtx.Err()
	case <-r.runGate:
	}
	defer func() { r.runGate <- struct{}{} }()

	terminalCtx, cancelTerminal := translationTerminalPassContext(roundCtx)
	terminalErr := r.reconcileTerminalJobs(terminalCtx, &result)
	cancelTerminal()
	missingErr := r.reconcileMissingJobs(roundCtx, &result)
	return result, errors.Join(terminalErr, missingErr)
}

func translationTerminalPassContext(ctx context.Context) (context.Context, context.CancelFunc) {
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

func (r *TranslationTerminalReconciler) reconcileTerminalJobs(
	ctx context.Context,
	result *TranslationTerminalReconcileResult,
) error {
	var passErr error
	var retriedTerminalIDs map[int64]struct{}
	if len(r.terminalRetries) > 0 {
		retryCtx, cancelRetry := translationTerminalPassContext(ctx)
		retriedTerminalIDs, passErr = r.retryTerminalJobs(retryCtx, result)
		cancelRetry()
	}
	cycleReady, err := r.ensureTerminalCycle(ctx)
	if err != nil {
		r.observeOutcome("failed")
		return errors.Join(passErr, err)
	}
	if !cycleReady {
		return passErr
	}
	for {
		jobs, err := r.store.ListTerminalJobs(ctx, r.terminalCursor, r.batchSize)
		if err != nil {
			r.observeOutcome("failed")
			return errors.Join(passErr, err)
		}
		if len(jobs) == 0 {
			r.completeTerminalCycle()
			return passErr
		}
		reachedEnd := false
		for _, job := range jobs {
			if err := ctx.Err(); err != nil {
				return errors.Join(passErr, err)
			}
			if !terminalCursorAfter(job, r.terminalCursor) {
				r.observeOutcome("failed")
				return errors.Join(passErr, errors.New("translation terminal store returned non-increasing cursor"))
			}
			if terminalJobAfterCursor(job, r.terminalCycleEnd) {
				r.completeTerminalCycle()
				return passErr
			}
			r.terminalCursor = terminalCursorForJob(job)
			r.terminalObservations.record(job)
			if err := r.reconcileTerminalHistoryJob(ctx, job, retriedTerminalIDs, result); err != nil {
				passErr = errors.Join(passErr, err)
			}
			if terminalCursorEqual(r.terminalCursor, r.terminalCycleEnd) {
				reachedEnd = true
			}
		}
		if reachedEnd {
			r.completeTerminalCycle()
			return passErr
		}
		if len(jobs) < r.batchSize {
			r.completeTerminalCycle()
			return passErr
		}
	}
}

func (r *TranslationTerminalReconciler) reconcileTerminalHistoryJob(
	ctx context.Context,
	job translationTerminalJob,
	retriedTerminalIDs map[int64]struct{},
	result *TranslationTerminalReconcileResult,
) error {
	if _, retried := retriedTerminalIDs[job.riverJobID]; retried {
		return nil
	}
	result.Scanned++
	if err := r.reconcileTerminalJob(ctx, job, r.shouldObserveTerminalStale(job), result); err != nil {
		result.Failed++
		r.observeOutcome("failed")
		// Only lane overflow needs the prefix-rescan escape hatch. Jobs the lane
		// still holds are retried next round anyway, and forcing a rescan for
		// them would re-observe every settled stale item in the prefix once per
		// round for the whole outage.
		if !r.enqueueTerminalRetry(job) {
			r.terminalObservations.recordUnfinished(job)
		}
		return err
	}
	r.forgetTerminalRetry(job.riverJobID)
	return nil
}

func (r *TranslationTerminalReconciler) ensureTerminalCycle(ctx context.Context) (bool, error) {
	if r.terminalCycleActive {
		return true, nil
	}
	end, ok, err := r.store.LatestTerminalCursor(ctx)
	if err != nil || !ok {
		return false, err
	}
	r.terminalCursor = translationTerminalCursor{}
	r.terminalCycleEnd = end
	r.terminalObservations.startCycle(end)
	r.terminalCycleActive = true
	return true, nil
}

func (r *TranslationTerminalReconciler) completeTerminalCycle() {
	r.terminalObservations.completeCycle(r.terminalCycleEnd)
	r.terminalCycleActive = false
	r.terminalCursor = translationTerminalCursor{}
	r.terminalCycleEnd = translationTerminalCursor{}
}

// retryTerminalJobs drains one full rotation of the bounded retry lane and
// returns the set of River job ids it handled, so the enclosing history walk can
// skip them instead of projecting the same item twice in one round. It owns that
// set rather than filling a caller-supplied map: a nil map would panic on the
// unconditional insert below, and the signature should not depend on the caller
// remembering to allocate.
func (r *TranslationTerminalReconciler) retryTerminalJobs(
	ctx context.Context,
	result *TranslationTerminalReconcileResult,
) (map[int64]struct{}, error) {
	if len(r.terminalRetries) == 0 {
		return nil, nil
	}
	attempts := min(r.batchSize, len(r.terminalRetries))
	retriedTerminalIDs := make(map[int64]struct{}, attempts)
	var passErr error
	for range attempts {
		if err := ctx.Err(); err != nil {
			return retriedTerminalIDs, errors.Join(passErr, err)
		}
		job := r.terminalRetries[0]
		r.terminalRetries = r.terminalRetries[1:]
		delete(r.terminalRetryIDs, job.riverJobID)
		retriedTerminalIDs[job.riverJobID] = struct{}{}
		result.Scanned++
		// A retry represents an item whose projection or stale classification did
		// not finish. Complete that classification even if the enclosing history
		// cycle already advanced its observability high-water mark.
		if err := r.reconcileTerminalJob(ctx, job, true, result); err != nil {
			result.Failed++
			r.observeOutcome("failed")
			// Re-entering the lane it was just dequeued from is the normal case;
			// only a rotation that finds the lane already full loses the item and
			// needs the next cycle to walk that prefix again.
			if !r.enqueueTerminalRetry(job) {
				r.terminalObservations.recordUnfinished(job)
			}
			passErr = errors.Join(passErr, err)
		}
	}
	return retriedTerminalIDs, passErr
}

// enqueueTerminalRetry reports whether the job is now held in the bounded retry
// lane. A false return means the caller must arrange rediscovery some other way,
// because nothing else remembers this unfinished item.
func (r *TranslationTerminalReconciler) enqueueTerminalRetry(job translationTerminalJob) bool {
	if _, exists := r.terminalRetryIDs[job.riverJobID]; exists {
		return true
	}
	// A full retry lane must not grow with infinite River history during a
	// persistent projection outage. Jobs that do not enter this fast lane are
	// still rediscovered when the finite high-water cycle wraps.
	if len(r.terminalRetries) >= r.batchSize {
		return false
	}
	r.terminalRetryIDs[job.riverJobID] = struct{}{}
	r.terminalRetries = append(r.terminalRetries, job)
	return true
}

func (r *TranslationTerminalReconciler) forgetTerminalRetry(riverJobID int64) {
	if _, exists := r.terminalRetryIDs[riverJobID]; !exists {
		return
	}
	delete(r.terminalRetryIDs, riverJobID)
	for index, job := range r.terminalRetries {
		if job.riverJobID != riverJobID {
			continue
		}
		copy(r.terminalRetries[index:], r.terminalRetries[index+1:])
		r.terminalRetries[len(r.terminalRetries)-1] = translationTerminalJob{}
		r.terminalRetries = r.terminalRetries[:len(r.terminalRetries)-1]
		return
	}
}

func (r *TranslationTerminalReconciler) shouldObserveTerminalStale(job translationTerminalJob) bool {
	return r.terminalObservations.shouldObserve(job)
}

func (t *translationTerminalObservationTracker) startCycle(end translationTerminalCursor) {
	t.cycleFull = newTranslationTerminalHistoryAccumulator()
	t.cyclePrefix = newTranslationTerminalHistoryAccumulator()
	t.cycleCompareThrough = t.baselineThrough
	if terminalCursorAfterCursor(t.cycleCompareThrough, end) {
		t.cycleCompareThrough = end
	}
	t.cycleForceThrough = t.forceNextThrough
	t.forceNextThrough = translationTerminalCursor{}
}

func (t *translationTerminalObservationTracker) record(job translationTerminalJob) {
	t.cycleFull.record(job)
	if t.baseline.valid && !terminalCursorAfter(job, t.cycleCompareThrough) {
		t.cyclePrefix.record(job)
	}
}

func (t *translationTerminalObservationTracker) completeCycle(end translationTerminalCursor) {
	if t.baseline.valid && !t.cyclePrefix.fingerprint().equal(t.baseline) {
		t.forceNextThrough = laterTerminalCursor(t.forceNextThrough, t.cycleCompareThrough)
	}
	t.forceNextThrough = laterTerminalCursor(t.forceNextThrough, t.unfinishedThrough)
	t.baseline = t.cycleFull.fingerprint()
	t.baselineThrough = end
	if terminalCursorAfterCursor(end, t.observedThrough) {
		t.observedThrough = end
	}
	t.cycleFull = translationTerminalHistoryAccumulator{}
	t.cyclePrefix = translationTerminalHistoryAccumulator{}
	t.cycleCompareThrough = translationTerminalCursor{}
	t.cycleForceThrough = translationTerminalCursor{}
	t.unfinishedThrough = translationTerminalCursor{}
}

func (t *translationTerminalObservationTracker) recordUnfinished(job translationTerminalJob) {
	t.unfinishedThrough = laterTerminalCursor(t.unfinishedThrough, terminalCursorForJob(job))
}

func (t *translationTerminalObservationTracker) shouldObserve(job translationTerminalJob) bool {
	if terminalCursorAfter(job, t.observedThrough) {
		return true
	}
	return !terminalCursorZero(t.cycleForceThrough) &&
		!terminalCursorAfter(job, t.cycleForceThrough)
}

func newTranslationTerminalHistoryAccumulator() translationTerminalHistoryAccumulator {
	return translationTerminalHistoryAccumulator{digest: sha256.New()}
}

func (a *translationTerminalHistoryAccumulator) record(job translationTerminalJob) {
	var numeric [32]byte
	a.writeBytes(strconv.AppendInt(numeric[:0], job.riverJobID, 10))
	var timestamp [64]byte
	a.writeBytes(job.finalizedAt.UTC().AppendFormat(timestamp[:0], time.RFC3339Nano))
	a.writeBytes([]byte(job.kind))
	a.writeBytes([]byte(job.state))
	a.writeBytes(job.encodedArgs)
	a.jobCount++
}

func (a *translationTerminalHistoryAccumulator) writeBytes(value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = a.digest.Write(length[:])
	_, _ = a.digest.Write(value)
}

func (a translationTerminalHistoryAccumulator) fingerprint() translationTerminalHistoryFingerprint {
	fingerprint := translationTerminalHistoryFingerprint{
		jobCount: a.jobCount,
		valid:    true,
	}
	sum := a.digest.Sum(fingerprint.digest[:0])
	copy(fingerprint.digest[:], sum)
	return fingerprint
}

func (f translationTerminalHistoryFingerprint) equal(other translationTerminalHistoryFingerprint) bool {
	return f.valid == other.valid && f.jobCount == other.jobCount && f.digest == other.digest
}

func laterTerminalCursor(left, right translationTerminalCursor) translationTerminalCursor {
	if terminalCursorAfterCursor(right, left) {
		return right
	}
	return left
}

func terminalCursorAfter(job translationTerminalJob, cursor translationTerminalCursor) bool {
	return job.finalizedAt.After(cursor.finalizedAt) ||
		(job.finalizedAt.Equal(cursor.finalizedAt) && job.riverJobID > cursor.riverJobID)
}

func terminalJobAfterCursor(job translationTerminalJob, cursor translationTerminalCursor) bool {
	return terminalCursorAfter(job, cursor)
}

func terminalCursorAfterCursor(left, right translationTerminalCursor) bool {
	return left.finalizedAt.After(right.finalizedAt) ||
		(left.finalizedAt.Equal(right.finalizedAt) && left.riverJobID > right.riverJobID)
}

func terminalCursorForJob(job translationTerminalJob) translationTerminalCursor {
	return translationTerminalCursor{finalizedAt: job.finalizedAt, riverJobID: job.riverJobID}
}

func terminalCursorEqual(left, right translationTerminalCursor) bool {
	return left.riverJobID == right.riverJobID && left.finalizedAt.Equal(right.finalizedAt)
}

func terminalCursorZero(cursor translationTerminalCursor) bool {
	return cursor.riverJobID == 0 && cursor.finalizedAt.IsZero()
}

func (r *TranslationTerminalReconciler) reconcileTerminalJob(
	ctx context.Context,
	job translationTerminalJob,
	observeStale bool,
	result *TranslationTerminalReconcileResult,
) error {
	code, ok := translationTerminalCodeForState(job.state)
	if !ok {
		result.Invalid++
		r.observeRejection(model.TranslationAttemptRejectionSourceStatusMismatch)
		r.logRejected(ctx, job, "", model.TranslationAttemptRejectionSourceStatusMismatch)
		return nil
	}
	resolution, err := r.resolveTerminalAttempt(ctx, job)
	if err != nil {
		return err
	}
	if resolution.Rejected() {
		result.Invalid++
		r.observeRejection(resolution.Rejection)
		r.logRejected(ctx, job, code, resolution.Rejection)
		return nil
	}
	return r.project(ctx, resolution.Attempt, code, false, observeStale, result)
}

func translationTerminalCodeForState(state rivertype.JobState) (TranslationTerminalCode, bool) {
	switch state {
	case rivertype.JobStateCancelled:
		return TranslationTerminalJobCancelled, true
	case rivertype.JobStateDiscarded:
		return TranslationTerminalJobDiscarded, true
	case rivertype.JobStateCompleted:
		return TranslationTerminalProjectionMissing, true
	default:
		return "", false
	}
}

func (r *TranslationTerminalReconciler) resolveTerminalAttempt(
	ctx context.Context,
	job translationTerminalJob,
) (model.TranslationAttemptResolution, error) {
	row := &rivertype.JobRow{
		ID:          job.riverJobID,
		Kind:        job.kind,
		EncodedArgs: job.encodedArgs,
	}
	switch job.kind {
	case model.TranslationJobKindV2:
		return translationAttemptFromV2Job(row), nil
	case model.TranslationJobKindLegacy:
		var args linktranslation.LegacyJobArgs
		if err := json.Unmarshal(job.encodedArgs, &args); err != nil {
			// A malformed durable item is rejected and scanning continues; it is
			// not a transient round failure that should starve later River IDs.
			return rejectedTranslationAttemptResolution(model.TranslationAttemptRejectionMalformedArgs), nil //nolint:nilerr // reason: malformed job args are an observable item rejection
		}
		if args.TranslationID == uuid.Nil {
			return rejectedTranslationAttemptResolution(model.TranslationAttemptRejectionMissingTranslationID), nil
		}
		resolution, err := linktranslation.ResolveLegacyAttempt(
			ctx,
			r.legacyAttempts,
			args,
			job.riverJobID,
		)
		if err != nil {
			return model.TranslationAttemptResolution{}, fmt.Errorf("resolve legacy terminal translation job %d: %w", job.riverJobID, err)
		}
		return resolution, nil
	default:
		return rejectedTranslationAttemptResolution(model.TranslationAttemptRejectionKindMismatch), nil
	}
}

func (r *TranslationTerminalReconciler) reconcileMissingJobs(
	ctx context.Context,
	result *TranslationTerminalReconcileResult,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.missingCycleActive {
		r.missingCycleActive = true
		r.missingCutoff = time.Now().Add(-r.missingAfter)
		r.missingCursor = translationMissingCursor{}
	}
	var passErr error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(passErr, err)
		}
		attempts, err := r.store.ListMissingAttempts(
			ctx,
			r.missingCutoff,
			r.missingCursor,
			r.batchSize,
		)
		if err != nil {
			r.observeOutcome("failed")
			return errors.Join(passErr, err)
		}
		if len(attempts) == 0 {
			r.missingCycleActive = false
			r.missingCutoff = time.Time{}
			r.missingCursor = translationMissingCursor{}
			return passErr
		}
		for _, item := range attempts {
			if err := ctx.Err(); err != nil {
				return errors.Join(passErr, err)
			}
			if !missingCursorAfter(item, r.missingCursor) {
				r.observeOutcome("failed")
				return errors.Join(passErr, errors.New("translation missing-job store returned non-increasing cursor"))
			}
			r.missingCursor = translationMissingCursor{
				updatedAt:     item.updatedAt,
				translationID: item.attempt.TranslationID,
			}
			result.Scanned++
			if err := r.project(ctx, item.attempt, TranslationTerminalJobMissing, true, true, result); err != nil {
				result.Failed++
				r.observeOutcome("failed")
				passErr = errors.Join(passErr, err)
			}
		}
		if len(attempts) < r.batchSize {
			r.missingCycleActive = false
			r.missingCutoff = time.Time{}
			r.missingCursor = translationMissingCursor{}
			return passErr
		}
	}
}

func missingCursorAfter(item translationMissingAttempt, cursor translationMissingCursor) bool {
	return item.updatedAt.After(cursor.updatedAt) ||
		(item.updatedAt.Equal(cursor.updatedAt) && item.attempt.TranslationID.String() > cursor.translationID.String())
}

func (r *TranslationTerminalReconciler) project(
	ctx context.Context,
	attempt model.TranslationAttempt,
	code TranslationTerminalCode,
	missing bool,
	observeStale bool,
	result *TranslationTerminalReconcileResult,
) error {
	var (
		applied bool
		err     error
	)
	if missing {
		applied, err = r.projector.FailMissing(ctx, attempt, string(code))
	} else {
		applied, err = r.projector.Fail(ctx, attempt, string(code))
	}
	if err != nil {
		return fmt.Errorf("project translation terminal %d as %s: %w", attempt.RiverJobID, code, err)
	}
	if !applied {
		if observeStale {
			reason, err := r.classifyStaleProjection(ctx, attempt, missing, code)
			if err != nil {
				return err
			}
			if reason == translationTerminalAlreadyProjected {
				r.observeOutcome(reason)
				return nil
			}
			if reason == "" {
				return nil
			}
			result.Stale++
			outcome := "stale_" + reason
			r.observeOutcome(outcome)
			r.logStale(ctx, attempt, code, reason, outcome)
		}
		return nil
	}
	result.Reconciled++
	r.observeOutcome(string(code))
	if missing {
		result.Missing++
		r.logMissingReconciled(ctx, attempt, code)
	}
	return nil
}

func (r *TranslationTerminalReconciler) classifyStaleProjection(
	ctx context.Context,
	attempt model.TranslationAttempt,
	missing bool,
	code TranslationTerminalCode,
) (string, error) {
	inspector, ok := r.store.(translationTerminalStaleInspector)
	if !ok {
		return model.TranslationAttemptRejectionNotCurrent.String(), nil
	}
	state, found, err := inspector.InspectTerminalProjection(ctx, attempt, missing)
	if err != nil {
		return "", fmt.Errorf(
			"classify stale translation terminal %d: %w",
			attempt.RiverJobID,
			err,
		)
	}
	if !found {
		return model.TranslationAttemptRejectionTranslationNotFound.String(), nil
	}
	if state.status != model.TranslationStatusPending && state.status != model.TranslationStatusProcessing {
		if reason := terminalProjectionIdentityMismatch(state, attempt); reason != "" {
			return reason, nil
		}
		if terminalProjectionAlreadyApplied(state, code) {
			return translationTerminalAlreadyProjected, nil
		}
		return model.TranslationAttemptRejectionSourceStatusMismatch.String(), nil
	}
	if state.currentRiverJobID == nil || *state.currentRiverJobID != attempt.RiverJobID {
		return model.TranslationAttemptRejectionNotCurrent.String(), nil
	}
	if reason := terminalProjectionIdentityMismatch(state, attempt); reason != "" {
		return reason, nil
	}
	if missing && state.activeReplacement {
		return translationTerminalStaleActiveReplacement, nil
	}
	// The conditional update and this diagnostic read are deliberately separate.
	// If the row changes again between them, fail closed to the generic current
	// owner rejection instead of asserting a stale reason we can no longer prove.
	return model.TranslationAttemptRejectionNotCurrent.String(), nil
}

func terminalProjectionIdentityMismatch(
	state translationTerminalProjectionState,
	attempt model.TranslationAttempt,
) string {
	if state.attemptGeneration != attempt.AttemptGeneration {
		return model.TranslationAttemptRejectionGenerationMismatch.String()
	}
	if state.sourceHash != attempt.SourceHash ||
		!sameTranslationSourceRevision(state.sourceContentRevision, attempt.SourceContentRevision) {
		return model.TranslationAttemptRejectionIdentityMismatch.String()
	}
	return ""
}

func terminalProjectionAlreadyApplied(
	state translationTerminalProjectionState,
	code TranslationTerminalCode,
) bool {
	if state.currentRiverJobID != nil {
		return false
	}
	if code == TranslationTerminalProjectionMissing && state.status == model.TranslationStatusDone {
		return true
	}
	if state.status != model.TranslationStatusFailed {
		return false
	}
	if state.errorMsg != nil && translationTerminalCodeKnown(*state.errorMsg) {
		return *state.errorMsg == string(code)
	}
	// Ordinary cancellation/discard handlers project the established
	// user-facing failure messages before River commits its terminal state.
	// Those messages intentionally differ from the reconciler's stable codes.
	return code == TranslationTerminalJobCancelled || code == TranslationTerminalJobDiscarded
}

func translationTerminalCodeKnown(value string) bool {
	switch TranslationTerminalCode(value) {
	case TranslationTerminalJobCancelled,
		TranslationTerminalJobDiscarded,
		TranslationTerminalProjectionMissing,
		TranslationTerminalJobMissing:
		return true
	default:
		return false
	}
}

func sameTranslationSourceRevision(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *TranslationTerminalReconciler) logMissingReconciled(
	ctx context.Context,
	attempt model.TranslationAttempt,
	code TranslationTerminalCode,
) {
	if r.logger == nil {
		return
	}
	r.logger.InfoContext(ctx, "translation missing job reconciled",
		"terminal_code", code,
		"translation_id", attempt.TranslationID.String(),
		"river_job_id", attempt.RiverJobID,
		"attempt_generation", attempt.AttemptGeneration,
	)
}

func (r *TranslationTerminalReconciler) observeRejection(reason model.TranslationAttemptRejectionReason) {
	r.observeOutcome("rejected_" + reason.String())
}

func (r *TranslationTerminalReconciler) observeOutcome(outcome string) {
	if r.metrics == nil || r.metrics.TranslationTerminalOutcomesTotal == nil {
		return
	}
	r.metrics.TranslationTerminalOutcomesTotal.WithLabelValues(outcome).Inc()
}

func (r *TranslationTerminalReconciler) logStale(
	ctx context.Context,
	attempt model.TranslationAttempt,
	code TranslationTerminalCode,
	reason string,
	outcome string,
) {
	if r.logger == nil {
		return
	}
	r.logger.WarnContext(ctx, "translation terminal reconciliation rejected",
		"reason", reason,
		"outcome", outcome,
		"terminal_code", code,
		"translation_id", attempt.TranslationID.String(),
		"river_job_id", attempt.RiverJobID,
		"attempt_generation", attempt.AttemptGeneration,
	)
}

func (r *TranslationTerminalReconciler) logRejected(
	ctx context.Context,
	job translationTerminalJob,
	code TranslationTerminalCode,
	reason model.TranslationAttemptRejectionReason,
) {
	if r.logger == nil {
		return
	}
	attrs := []any{
		"reason", reason.String(),
		"river_job_id", job.riverJobID,
		"kind", job.kind,
	}
	if code != "" {
		attrs = append(attrs, "terminal_code", string(code))
	}
	attrs = append(attrs, translationTerminalRejectedIdentityAttrs(job)...)
	r.logger.WarnContext(ctx, "translation terminal reconciliation rejected", attrs...)
}

func translationTerminalRejectedIdentityAttrs(job translationTerminalJob) []any {
	var (
		translationID     uuid.UUID
		attemptGeneration int64
	)
	switch job.kind {
	case model.TranslationJobKindV2:
		var args linktranslation.JobArgs
		if json.Unmarshal(job.encodedArgs, &args) != nil {
			return nil
		}
		translationID = args.TranslationID
		attemptGeneration = args.AttemptGeneration
	case model.TranslationJobKindLegacy:
		var args linktranslation.LegacyJobArgs
		if json.Unmarshal(job.encodedArgs, &args) != nil {
			return nil
		}
		translationID = args.TranslationID
		attemptGeneration = args.AttemptGeneration
	default:
		return nil
	}
	attrs := make([]any, 0, 4)
	if translationID != uuid.Nil {
		attrs = append(attrs, "translation_id", translationID.String())
	}
	if attemptGeneration != 0 {
		attrs = append(attrs, "attempt_generation", attemptGeneration)
	}
	return attrs
}

func (r *TranslationTerminalReconciler) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.lifecycle.Stop(ctx)
}
