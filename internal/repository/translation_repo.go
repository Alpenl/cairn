package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

const translationColumns = "id, link_id, scope, block_key, start_offset, end_offset, source_text, translated_text, source_format, target_language, source_hash, source_content_revision, status, model, error_msg, attempt_generation, created_at, updated_at"

const (
	getHashTranslationByIdentitySQL = `SELECT ` + translationColumns + `
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

	markTranslationProcessingSQL = `UPDATE link_translations
		SET status = 'processing', error_msg = NULL, updated_at = NOW()
		WHERE id = $1 AND attempt_generation = $2 AND source_hash = $3
			AND source_content_revision IS NOT DISTINCT FROM $4
			AND status IN ('pending', 'processing')
		RETURNING ` + translationColumns

	completeTranslationSQL = `UPDATE link_translations
		SET status = 'done', translated_text = $1, model = $2, error_msg = NULL, updated_at = NOW()
		WHERE id = $3 AND attempt_generation = $4 AND source_hash = $5
			AND source_content_revision IS NOT DISTINCT FROM $6
			AND status = 'processing'`

	failTranslationSQL = `UPDATE link_translations
		SET status = 'failed', error_msg = $1, updated_at = NOW()
		WHERE id = $2 AND attempt_generation = $3 AND source_hash = $4
			AND source_content_revision IS NOT DISTINCT FROM $5
			AND status IN ('pending', 'processing')`
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
	// SourceContentRevision is non-nil for saved-content identities. Summary
	// identities are keyed by SourceHash with a durable NULL revision.
	SourceContentRevision *int64
	Force                 bool
}

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
	query := getHashTranslationByIdentitySQL
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

func (r *PGXTranslationRepository) MarkProcessing(ctx context.Context, attempt model.TranslationAttempt) (*model.LinkTranslation, error) {
	item, err := scanTranslation(r.db.QueryRow(
		ctx,
		markTranslationProcessingSQL,
		attempt.TranslationID,
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
		attempt.AttemptGeneration,
		attempt.SourceHash,
		attempt.SourceContentRevision,
	)
	if err != nil {
		return false, fmt.Errorf("fail link translation: %w", err)
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
		&item.CreatedAt,
		&item.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &item, nil
}

var _ TranslationStore = (*PGXTranslationRepository)(nil)
