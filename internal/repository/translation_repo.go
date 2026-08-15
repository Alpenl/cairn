package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

const translationColumns = "id, link_id, scope, block_key, start_offset, end_offset, source_text, translated_text, source_format, target_language, source_hash, source_content_revision, status, model, error_msg, attempt_generation, current_river_job_id, created_at, updated_at"

const legacyTranslationExplicitRecoveryMessage = "legacy translation attempt requires explicit recovery"

const (
	// The RF5A contract replaces the old global source-hash constraint with
	// two partial unique indexes: one for saved revisions and one for legacy
	// rows. A named ON CONFLICT target cannot match both schemas, so the
	// insert is intentionally targetless. SchedulePending already locks the
	// exact identity first; an existing eligible row is advanced explicitly
	// below, while a targetless insert handles both the expand and contract
	// schemas.
	upsertPendingTranslationSQL = `INSERT INTO link_translations (
		link_id, scope, block_key, start_offset, end_offset,
		source_text, source_format, target_language, source_hash,
		source_content_revision, status,
		attempt_generation, current_river_job_id
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', 1, NULL)
	ON CONFLICT DO NOTHING
	RETURNING ` + translationColumns

	advancePendingTranslationSQL = `UPDATE link_translations SET
		status = 'pending',
		translated_text = NULL,
		model = NULL,
		error_msg = NULL,
		attempt_generation = attempt_generation + 1,
		current_river_job_id = NULL,
		updated_at = NOW()
	WHERE id = $1 AND $2
		AND (
			status = 'failed'
			OR ($3 AND status = 'done')
			OR (status IN ('pending', 'processing')
				AND updated_at < NOW() - $4::interval)
		)
	RETURNING ` + translationColumns

	getLegacyTranslationByIdentitySQL = `SELECT ` + translationColumns + `
		FROM link_translations
		WHERE link_id = $1 AND scope = $2 AND block_key = $3
			AND start_offset = $4 AND end_offset = $5 AND source_hash = $6
			AND target_language = $7 AND source_content_revision IS NULL`
	getSavedTranslationByIdentitySQL = `SELECT ` + translationColumns + `
		FROM link_translations
		WHERE link_id = $1 AND scope = $2 AND block_key = $3
			AND start_offset = $4 AND end_offset = $5
			AND source_content_revision = $6 AND target_language = $7`

	listTranslationsByLinkSQL = `SELECT ` + translationColumns + `
		FROM link_translations WHERE link_id = $1
		ORDER BY (scope = 'full') DESC, updated_at DESC, id DESC`
	readTranslationListSourceSQL = `SELECT status, COALESCE(library_kind, ''), content_revision,
		content IS NOT NULL, COALESCE(content, ''),
		content_document IS NOT NULL, COALESCE(content_document, ''), content_format,
		summary IS NOT NULL, COALESCE(summary, '')
		FROM links WHERE id = $1`

	getTranslationByIDSQL = `SELECT ` + translationColumns + `
		FROM link_translations WHERE id = $1`
	lockTranslationByIDSQL = getTranslationByIDSQL + ` FOR UPDATE`

	summarizeActiveLegacyTranslationAttemptsSQL = `SELECT COUNT(*), COALESCE(MIN(id), 0)
		FROM river_job
		WHERE kind = $2
			AND args->>'translation_id' = $1
			AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`

	quarantineLegacyTranslationAttemptSQL = `UPDATE link_translations
		SET error_msg = $1, updated_at = NOW()
		WHERE id = $2 AND current_river_job_id IS NULL
			AND status IN ('pending', 'processing') AND error_msg IS DISTINCT FROM $1`

	markTranslationProcessingSQL = `UPDATE link_translations
		SET status = 'processing', error_msg = NULL, updated_at = NOW()
		WHERE id = $1 AND current_river_job_id = $2
			AND attempt_generation = $3 AND source_hash = $4
			AND source_content_revision IS NOT DISTINCT FROM $5
			AND status IN ('pending', 'processing')
		RETURNING ` + translationColumns

	bindCurrentTranslationAttemptSQL = `UPDATE link_translations
		SET current_river_job_id = $1
		WHERE id = $2 AND attempt_generation = $3
			AND status = 'pending' AND current_river_job_id IS NULL
		RETURNING ` + translationColumns

	bindCurrentLegacyTranslationAttemptSQL = `UPDATE link_translations
		SET current_river_job_id = $1
		WHERE id = $2 AND attempt_generation = $3
			AND status IN ('pending', 'processing') AND current_river_job_id IS NULL
		RETURNING ` + translationColumns

	completeTranslationSQL = `UPDATE link_translations
		SET status = 'done', translated_text = $1, model = $2, error_msg = NULL,
			current_river_job_id = NULL, updated_at = NOW()
		WHERE id = $3 AND current_river_job_id = $4
			AND attempt_generation = $5 AND source_hash = $6
			AND source_content_revision IS NOT DISTINCT FROM $7
			AND status = 'processing'`

	failTranslationSQL = `UPDATE link_translations
		SET status = 'failed', error_msg = $1, current_river_job_id = NULL, updated_at = NOW()
		WHERE id = $2 AND current_river_job_id = $3
			AND attempt_generation = $4 AND source_hash = $5
			AND source_content_revision IS NOT DISTINCT FROM $6
			AND status IN ('pending', 'processing')`

	failMissingTranslationSQL = `UPDATE link_translations AS candidate
		SET status = 'failed', error_msg = $1, current_river_job_id = NULL, updated_at = NOW()
		WHERE candidate.id = $2
			AND candidate.current_river_job_id = $3
			AND candidate.attempt_generation = $4 AND candidate.source_hash = $5
			AND candidate.source_content_revision IS NOT DISTINCT FROM $6
			AND candidate.status IN ('pending', 'processing')
			AND NOT EXISTS (
				SELECT 1 FROM river_job AS current_job
				WHERE current_job.id = candidate.current_river_job_id
			)
			AND NOT EXISTS (
				SELECT 1 FROM river_job AS active_job
				WHERE active_job.kind IN ($7, $8)
					AND active_job.state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
					AND active_job.args->>'translation_id' = candidate.id::text
					AND (
						(active_job.kind = $7
							AND active_job.args->>'attempt_generation' = candidate.attempt_generation::text
							AND active_job.args->>'source_hash' = candidate.source_hash
							AND active_job.args->>'source_content_revision'
								IS NOT DISTINCT FROM candidate.source_content_revision::text)
						OR
						(active_job.kind = $8
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
			)`
)

type UpsertTranslationParams struct {
	LinkID         uuid.UUID
	Scope          model.TranslationScope
	BlockKey       string
	StartOffset    int
	EndOffset      int
	SourceText     string
	SourceFormat   model.TranslationFormat
	TargetLanguage string
	SourceHash     string
	// SourceContentRevision is non-nil only for verified saved-content
	// identities. Summary and compatibility-window legacy identities remain
	// keyed by SourceHash with a durable NULL revision.
	SourceContentRevision *int64
	Force                 bool
	// AllowExistingReschedule is derived from the queue's immutable rollout
	// policy. PostgreSQL consumes it in the ON CONFLICT predicate so a request
	// that observed no row cannot race a compat-v1 initial attempt into a retry.
	AllowExistingReschedule bool
	// StallAfter 是在途行的停滞阈值：pending / processing 超过它未更新即可被
	// 重新排期。零值回落到 defaultTranslationStallAfter，避免历史调用方漏传时
	// 谓词退化成「永不重排」。
	StallAfter time.Duration
}

// defaultTranslationStallAfter 是 StallAfter 缺省时的兜底。
// 调用方（service 层）应按实际的 job 超时推导后显式传入。
const defaultTranslationStallAfter = 2 * time.Hour

type translationTxBeginner interface {
	database.TxBeginner
	database.TxOptionsBeginner
}

// TranslationListSnapshot carries the canonical source projection and all
// translation rows observed in one repeatable-read transaction.
// A nil *TranslationListSnapshot means the link was absent in that snapshot.
type TranslationListSnapshot struct {
	Source TranslationSourceSnapshot
	Items  []model.LinkTranslation
}

type PGXTranslationRepository struct {
	db database.Querier
	tx translationTxBeginner
}

func NewPGXTranslationRepository(db database.Querier) *PGXTranslationRepository {
	beginner, ok := db.(translationTxBeginner)
	if !ok {
		panic("repository: translation Querier must implement Begin and BeginTx")
	}
	return &PGXTranslationRepository{db: db, tx: beginner}
}

func (r *PGXTranslationRepository) FindByIdentity(ctx context.Context, params UpsertTranslationParams) (*model.LinkTranslation, error) {
	item, err := findTranslationByIdentity(ctx, r.db, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find link translation by identity: %w", err)
	}
	return item, nil
}

// SchedulePending returns scheduled=false when the same identity is already
// done or active. The conditional ON CONFLICT update is the concurrency gate:
// only one racing request can transition an absent/failed/done record to
// pending. When it does, hook runs before commit so the River job and state
// transition share one all-or-nothing transaction.
func (r *PGXTranslationRepository) SchedulePending(ctx context.Context, params UpsertTranslationParams, hook func(context.Context, pgx.Tx, TranslationScheduleCommand) (int64, error)) (*model.LinkTranslation, bool, error) {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin schedule translation tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	previous, err := findTranslationByIdentityForUpdate(ctx, tx, params)
	if err != nil {
		return nil, false, err
	}
	translation, scheduled, err := upsertPendingTranslation(ctx, tx, params, previous)
	if err != nil {
		return nil, false, err
	}
	if scheduled {
		if hook == nil {
			return nil, false, errors.New("schedule translation: River insert hook is required")
		}
		seed := TranslationAttemptSeed{
			TranslationID:         translation.ID,
			AttemptGeneration:     translation.AttemptGeneration,
			SourceHash:            translation.SourceHash,
			SourceContentRevision: translation.SourceContentRevision,
		}
		command := TranslationScheduleCommand{
			Seed:     seed,
			Previous: supersededTranslationAttempt(previous),
		}
		riverJobID, hookErr := hook(ctx, tx, command)
		if hookErr != nil {
			return nil, false, hookErr
		}
		if riverJobID <= 0 {
			return nil, false, fmt.Errorf("schedule translation: invalid River job ID %d", riverJobID)
		}
		translation, err = bindCurrentTranslationAttempt(ctx, tx, seed, riverJobID)
		if err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit schedule translation tx: %w", err)
	}
	return translation, scheduled, nil
}

func supersededTranslationAttempt(item *model.LinkTranslation) *model.TranslationAttempt {
	if item == nil || item.CurrentRiverJobID == nil || *item.CurrentRiverJobID <= 0 ||
		(item.Status != model.TranslationStatusPending && item.Status != model.TranslationStatusProcessing) {
		return nil
	}
	return &model.TranslationAttempt{
		TranslationID:         item.ID,
		AttemptGeneration:     item.AttemptGeneration,
		RiverJobID:            *item.CurrentRiverJobID,
		SourceHash:            item.SourceHash,
		SourceContentRevision: item.SourceContentRevision,
	}
}

func bindCurrentTranslationAttempt(
	ctx context.Context,
	db database.Querier,
	seed TranslationAttemptSeed,
	riverJobID int64,
) (*model.LinkTranslation, error) {
	item, err := scanTranslation(db.QueryRow(
		ctx,
		bindCurrentTranslationAttemptSQL,
		riverJobID,
		seed.TranslationID,
		seed.AttemptGeneration,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("bind current translation attempt: scheduled row no longer matches")
	}
	if err != nil {
		return nil, fmt.Errorf("bind current translation attempt: %w", err)
	}
	return item, nil
}

// translationStallInterval 把停滞阈值转成 PG interval 字面量。零值走兜底，
// 保证谓词永远拿到一个有意义的窗口。
func translationStallInterval(d time.Duration) string {
	if d <= 0 {
		d = defaultTranslationStallAfter
	}
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

func upsertPendingTranslation(
	ctx context.Context,
	db database.Querier,
	params UpsertTranslationParams,
	previous *model.LinkTranslation,
) (*model.LinkTranslation, bool, error) {
	row := db.QueryRow(
		ctx,
		upsertPendingTranslationSQL,
		params.LinkID,
		params.Scope,
		params.BlockKey,
		params.StartOffset,
		params.EndOffset,
		params.SourceText,
		params.SourceFormat,
		params.TargetLanguage,
		params.SourceHash,
		params.SourceContentRevision,
	)
	translation, err := scanTranslation(row)
	if err == nil {
		return translation, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("upsert link translation: %w", err)
	}

	if previous != nil && translationRescheduleEligible(previous, params, time.Now()) {
		row = db.QueryRow(
			ctx,
			advancePendingTranslationSQL,
			previous.ID,
			params.AllowExistingReschedule,
			params.Force,
			translationStallInterval(params.StallAfter),
		)
		translation, err = scanTranslation(row)
		if err == nil {
			return translation, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("advance link translation: %w", err)
		}
	}

	existing, findErr := findTranslationByIdentity(ctx, db, params)
	if findErr != nil {
		return nil, false, findErr
	}
	if existing == nil {
		return nil, false, errors.New("upsert link translation: conflict row disappeared")
	}
	return existing, false, nil
}

func translationRescheduleEligible(item *model.LinkTranslation, params UpsertTranslationParams, now time.Time) bool {
	if item == nil || !params.AllowExistingReschedule {
		return false
	}
	switch item.Status {
	case model.TranslationStatusFailed:
		return true
	case model.TranslationStatusDone:
		return params.Force
	case model.TranslationStatusPending, model.TranslationStatusProcessing:
		stallAfter := params.StallAfter
		if stallAfter <= 0 {
			stallAfter = defaultTranslationStallAfter
		}
		return !item.UpdatedAt.IsZero() && !now.Before(item.UpdatedAt.Add(stallAfter))
	default:
		return false
	}
}

func findTranslationByIdentity(ctx context.Context, db database.Querier, params UpsertTranslationParams) (*model.LinkTranslation, error) {
	return findTranslationByIdentityWithLock(ctx, db, params, false)
}

func findTranslationByIdentityForUpdate(ctx context.Context, db database.Querier, params UpsertTranslationParams) (*model.LinkTranslation, error) {
	return findTranslationByIdentityWithLock(ctx, db, params, true)
}

func findTranslationByIdentityWithLock(
	ctx context.Context,
	db database.Querier,
	params UpsertTranslationParams,
	forUpdate bool,
) (*model.LinkTranslation, error) {
	query := getLegacyTranslationByIdentitySQL
	identity := any(params.SourceHash)
	if params.SourceContentRevision != nil {
		query = getSavedTranslationByIdentitySQL
		identity = *params.SourceContentRevision
	}
	if forUpdate {
		query += ` FOR UPDATE`
	}
	item, err := scanTranslation(db.QueryRow(
		ctx,
		query,
		params.LinkID,
		params.Scope,
		params.BlockKey,
		params.StartOffset,
		params.EndOffset,
		identity,
		params.TargetLanguage,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find link translation by identity: %w", err)
	}
	return item, nil
}

func (r *PGXTranslationRepository) ListByLink(ctx context.Context, linkID uuid.UUID) ([]model.LinkTranslation, error) {
	return listTranslationsByLink(ctx, r.db, linkID)
}

// ReadListSnapshot atomically observes the canonical link source and its
// translation rows. Repeatable read is required because PostgreSQL's default
// read-committed mode takes a fresh snapshot for each statement and could pair
// generation N source fields with generation N+1 translation rows.
func (r *PGXTranslationRepository) ReadListSnapshot(
	ctx context.Context,
	linkID uuid.UUID,
) (*TranslationListSnapshot, error) {
	tx, err := r.tx.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin translation list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	source, err := readTranslationListSource(ctx, tx, linkID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, nil
	}
	items, err := listTranslationsByLink(ctx, tx, linkID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit translation list snapshot: %w", err)
	}
	return &TranslationListSnapshot{Source: *source, Items: items}, nil
}

func readTranslationListSource(
	ctx context.Context,
	db database.Querier,
	linkID uuid.UUID,
) (*TranslationSourceSnapshot, error) {
	var source TranslationSourceSnapshot
	var libraryKind string
	var contentPresent, documentPresent, summaryPresent bool
	var content, document, summary string
	err := db.QueryRow(ctx, readTranslationListSourceSQL, linkID).Scan(
		&source.Status,
		&libraryKind,
		&source.ContentRevision,
		&contentPresent,
		&content,
		&documentPresent,
		&document,
		&source.ContentFormat,
		&summaryPresent,
		&summary,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read translation list source: %w", err)
	}
	if libraryKind != "" {
		kind := model.LibraryKind(libraryKind)
		source.LibraryKind = &kind
	}
	if contentPresent {
		source.Content = &content
	}
	if documentPresent {
		source.ContentDocument = &document
	}
	if summaryPresent {
		source.Summary = &summary
	}
	return &source, nil
}

func listTranslationsByLink(
	ctx context.Context,
	db database.Querier,
	linkID uuid.UUID,
) ([]model.LinkTranslation, error) {
	rows, err := db.Query(ctx, listTranslationsByLinkSQL, linkID)
	if err != nil {
		return nil, fmt.Errorf("list link translations: %w", err)
	}
	defer rows.Close()
	items := make([]model.LinkTranslation, 0)
	for rows.Next() {
		item, scanErr := scanTranslation(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan link translation: %w", scanErr)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate link translations: %w", err)
	}
	return items, nil
}

func (r *PGXTranslationRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.LinkTranslation, error) {
	item, err := scanTranslation(r.db.QueryRow(ctx, getTranslationByIDSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link translation: %w", err)
	}
	return item, nil
}

// ProveCurrentLegacyAttempt returns the exact persisted owner for a legacy
// River job. During compat rollout it may bind a NULL current job left by a
// pre-RF6A API, but only after locking the product row and proving that the
// worker's job is the sole active legacy candidate.
func (r *PGXTranslationRepository) ProveCurrentLegacyAttempt(
	ctx context.Context,
	translationID uuid.UUID,
	riverJobID int64,
	attemptGeneration int64,
) (model.TranslationAttemptResolution, error) {
	if translationID == uuid.Nil || riverJobID <= 0 {
		return rejectedTranslationAttempt(model.TranslationAttemptRejectionInvalidIdentity), nil
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return model.TranslationAttemptResolution{}, fmt.Errorf("begin prove legacy translation attempt tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	item, err := scanTranslation(tx.QueryRow(ctx, lockTranslationByIDSQL, translationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectedTranslationAttempt(model.TranslationAttemptRejectionTranslationNotFound), nil
	}
	if err != nil {
		return model.TranslationAttemptResolution{}, fmt.Errorf("lock legacy translation attempt: %w", err)
	}
	if resolved, reason := classifyLockedLegacyAttempt(item, riverJobID, attemptGeneration); resolved {
		return classifiedTranslationAttempt(item, riverJobID, reason), nil
	}

	var activeCount, onlyActiveID int64
	if err := tx.QueryRow(
		ctx,
		summarizeActiveLegacyTranslationAttemptsSQL,
		translationID.String(),
		model.TranslationJobKindLegacy,
	).Scan(&activeCount, &onlyActiveID); err != nil {
		return model.TranslationAttemptResolution{}, fmt.Errorf("summarize active legacy translation attempts: %w", err)
	}
	if activeCount != 1 {
		if err := quarantineAmbiguousLegacyTranslationAttempt(ctx, tx, translationID); err != nil {
			return model.TranslationAttemptResolution{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.TranslationAttemptResolution{}, fmt.Errorf("commit ambiguous legacy translation attempt quarantine: %w", err)
		}
		return rejectedTranslationAttempt(model.TranslationAttemptRejectionAmbiguousCandidates), nil
	}
	if onlyActiveID != riverJobID {
		return rejectedTranslationAttempt(model.TranslationAttemptRejectionNotCurrent), nil
	}

	item, err = bindCurrentLegacyTranslationAttempt(ctx, tx, item, riverJobID)
	if err != nil {
		return model.TranslationAttemptResolution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.TranslationAttemptResolution{}, fmt.Errorf("commit prove legacy translation attempt tx: %w", err)
	}
	return resolvedTranslationAttempt(item, riverJobID), nil
}

func classifiedTranslationAttempt(
	item *model.LinkTranslation,
	riverJobID int64,
	reason model.TranslationAttemptRejectionReason,
) model.TranslationAttemptResolution {
	if reason != "" {
		return rejectedTranslationAttempt(reason)
	}
	return resolvedTranslationAttempt(item, riverJobID)
}

func rejectedTranslationAttempt(reason model.TranslationAttemptRejectionReason) model.TranslationAttemptResolution {
	return model.TranslationAttemptResolution{Rejection: reason}
}

func resolvedTranslationAttempt(item *model.LinkTranslation, riverJobID int64) model.TranslationAttemptResolution {
	return model.TranslationAttemptResolution{Attempt: model.TranslationAttempt{
		TranslationID:         item.ID,
		AttemptGeneration:     item.AttemptGeneration,
		RiverJobID:            riverJobID,
		SourceHash:            item.SourceHash,
		SourceContentRevision: item.SourceContentRevision,
	}}
}

func classifyLockedLegacyAttempt(
	item *model.LinkTranslation,
	riverJobID int64,
	attemptGeneration int64,
) (bool, model.TranslationAttemptRejectionReason) {
	if item.CurrentRiverJobID != nil {
		if *item.CurrentRiverJobID != riverJobID {
			return true, model.TranslationAttemptRejectionNotCurrent
		}
		if item.AttemptGeneration != attemptGeneration {
			return true, model.TranslationAttemptRejectionGenerationMismatch
		}
		return true, ""
	}
	if item.ErrorMsg != nil && *item.ErrorMsg == legacyTranslationExplicitRecoveryMessage {
		return true, model.TranslationAttemptRejectionExplicitRecoveryRequired
	}
	if item.AttemptGeneration != attemptGeneration {
		return true, model.TranslationAttemptRejectionGenerationMismatch
	}
	if item.Status != model.TranslationStatusPending && item.Status != model.TranslationStatusProcessing {
		return true, model.TranslationAttemptRejectionSourceStatusMismatch
	}
	return false, ""
}

func quarantineAmbiguousLegacyTranslationAttempt(
	ctx context.Context,
	tx pgx.Tx,
	translationID uuid.UUID,
) error {
	tag, err := tx.Exec(
		ctx,
		quarantineLegacyTranslationAttemptSQL,
		legacyTranslationExplicitRecoveryMessage,
		translationID,
	)
	if err != nil {
		return fmt.Errorf("quarantine ambiguous legacy translation attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("quarantine ambiguous legacy translation attempt: row no longer matches")
	}
	return nil
}

func bindCurrentLegacyTranslationAttempt(
	ctx context.Context,
	tx pgx.Tx,
	item *model.LinkTranslation,
	riverJobID int64,
) (*model.LinkTranslation, error) {
	bound, err := scanTranslation(tx.QueryRow(
		ctx,
		bindCurrentLegacyTranslationAttemptSQL,
		riverJobID,
		item.ID,
		item.AttemptGeneration,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("bind current legacy translation attempt: row no longer matches")
	}
	if err != nil {
		return nil, fmt.Errorf("bind current legacy translation attempt: %w", err)
	}
	return bound, nil
}

func (r *PGXTranslationRepository) MarkProcessing(ctx context.Context, attempt model.TranslationAttempt) (*model.LinkTranslation, error) {
	item, err := scanTranslation(r.db.QueryRow(
		ctx,
		markTranslationProcessingSQL,
		attempt.TranslationID,
		attempt.RiverJobID,
		attempt.AttemptGeneration,
		attempt.SourceHash,
		attempt.SourceContentRevision,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mark link translation processing: %w", err)
	}
	return item, nil
}

func (r *PGXTranslationRepository) Complete(ctx context.Context, attempt model.TranslationAttempt, translatedText, modelName string) (bool, error) {
	tag, err := r.db.Exec(
		ctx,
		completeTranslationSQL,
		translatedText,
		modelName,
		attempt.TranslationID,
		attempt.RiverJobID,
		attempt.AttemptGeneration,
		attempt.SourceHash,
		attempt.SourceContentRevision,
	)
	if err != nil {
		return false, fmt.Errorf("complete link translation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *PGXTranslationRepository) Fail(ctx context.Context, attempt model.TranslationAttempt, message string) (bool, error) {
	tag, err := r.db.Exec(
		ctx,
		failTranslationSQL,
		message,
		attempt.TranslationID,
		attempt.RiverJobID,
		attempt.AttemptGeneration,
		attempt.SourceHash,
		attempt.SourceContentRevision,
	)
	if err != nil {
		return false, fmt.Errorf("fail link translation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailMissing applies retention recovery only while the exact product attempt
// still has no active replacement. Repeating the active-job predicate in this
// final conditional write closes the discovery/projection check-write race.
func (r *PGXTranslationRepository) FailMissing(
	ctx context.Context,
	attempt model.TranslationAttempt,
	message string,
) (bool, error) {
	tag, err := r.db.Exec(
		ctx,
		failMissingTranslationSQL,
		message,
		attempt.TranslationID,
		attempt.RiverJobID,
		attempt.AttemptGeneration,
		attempt.SourceHash,
		attempt.SourceContentRevision,
		model.TranslationJobKindV2,
		model.TranslationJobKindLegacy,
	)
	if err != nil {
		return false, fmt.Errorf("fail link translation with missing current job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

type translationScanner interface {
	Scan(...any) error
}

func scanTranslation(row translationScanner) (*model.LinkTranslation, error) {
	var item model.LinkTranslation
	if err := row.Scan(
		&item.ID,
		&item.LinkID,
		&item.Scope,
		&item.BlockKey,
		&item.StartOffset,
		&item.EndOffset,
		&item.SourceText,
		&item.TranslatedText,
		&item.SourceFormat,
		&item.TargetLanguage,
		&item.SourceHash,
		&item.SourceContentRevision,
		&item.Status,
		&item.Model,
		&item.ErrorMsg,
		&item.AttemptGeneration,
		&item.CurrentRiverJobID,
		&item.CreatedAt,
		&item.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &item, nil
}

var _ TranslationStore = (*PGXTranslationRepository)(nil)
var _ TranslationListSnapshotReader = (*PGXTranslationRepository)(nil)
