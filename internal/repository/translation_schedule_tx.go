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

const (
	lockTranslationSourceTxSQL = `SELECT status, COALESCE(library_kind, ''), content_revision,
		content IS NOT NULL, COALESCE(content, ''),
		content_document IS NOT NULL, COALESCE(content_document, ''), content_format,
		summary IS NOT NULL, COALESCE(summary, '')
		FROM links WHERE id=$1 FOR UPDATE`

	// The targetless conflict clause covers both current partial identity
	// indexes without branching the statement by source domain.
	insertPendingTranslationTxSQL = `INSERT INTO link_translations (
		link_id, scope, block_key, start_offset, end_offset,
		source_text, source_format, target_language, source_hash,
		source_content_revision, status, attempt_generation, current_river_job_id
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', 1, NULL)
	ON CONFLICT DO NOTHING
	RETURNING ` + translationColumns

	advancePendingTranslationTxSQL = `UPDATE link_translations SET
		status = 'pending', translated_text = NULL, model = NULL,
		error_msg = NULL, attempt_generation = attempt_generation + 1,
		current_river_job_id = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING ` + translationColumns
)

// TranslationSourceSnapshot is the canonical source projection shared by the
// durable scheduler's locked write path and the translation list's read-only
// snapshot. ContentRevision versions only the saved body; Summary has its own
// hash identity. Lock ownership is defined by the method returning it.
type TranslationSourceSnapshot struct {
	Status          model.LinkStatus
	LibraryKind     *model.LibraryKind
	ContentRevision int64
	Content         *string
	ContentDocument *string
	ContentFormat   model.ContentFormat
	Summary         *string
}

// LockTranslationSourceTx locks the link root before source validation and
// translation-product mutation. The caller owns the surrounding database
// transaction and must keep this lock through River insertion and commit.
func (r *PGXTranslationRepository) LockTranslationSourceTx(
	ctx context.Context,
	tx database.Querier,
	linkID uuid.UUID,
) (*TranslationSourceSnapshot, error) {
	var source TranslationSourceSnapshot
	var libraryKind string
	var contentPresent, documentPresent, summaryPresent bool
	var content, document, summary string
	err := tx.QueryRow(
		ctx,
		lockTranslationSourceTxSQL,
		linkID,
	).Scan(
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
		return nil, fmt.Errorf("lock translation source: %w", err)
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

// FindTranslationIdentityTx returns and locks the logical product identity.
// Saved products use SourceContentRevision; summary products use SourceHash
// with a NULL revision.
func (r *PGXTranslationRepository) FindTranslationIdentityTx(
	ctx context.Context,
	tx database.Querier,
	params UpsertTranslationParams,
) (*model.LinkTranslation, error) {
	return findTranslationByIdentityForUpdate(ctx, tx, params)
}

// InsertPendingTranslationTx inserts generation one without depending on a
// particular database conflict target. inserted=false means the same logical
// identity won a race; the caller refetches it while still holding the link
// lock.
func (r *PGXTranslationRepository) InsertPendingTranslationTx(
	ctx context.Context,
	tx database.Querier,
	params UpsertTranslationParams,
) (*model.LinkTranslation, bool, error) {
	item, err := scanTranslation(tx.QueryRow(
		ctx,
		insertPendingTranslationTxSQL,
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
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("insert pending translation: %w", err)
	}
	return item, true, nil
}

// AdvancePendingTranslationTx starts the next RF6A attempt on a locked
// product. Source identity fields are intentionally absent from the SET list:
// a row must never be rewritten to masquerade as another document generation.
func (r *PGXTranslationRepository) AdvancePendingTranslationTx(
	ctx context.Context,
	tx database.Querier,
	translationID uuid.UUID,
) (*model.LinkTranslation, error) {
	item, err := scanTranslation(tx.QueryRow(
		ctx,
		advancePendingTranslationTxSQL,
		translationID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("advance pending translation: locked row disappeared")
	}
	if err != nil {
		return nil, fmt.Errorf("advance pending translation: %w", err)
	}
	return item, nil
}

// BindCurrentTranslationAttemptTx completes the immutable RF6A attempt
// identity after the River row has been inserted in the same transaction.
func (r *PGXTranslationRepository) BindCurrentTranslationAttemptTx(
	ctx context.Context,
	tx database.Querier,
	seed model.TranslationAttemptSeed,
	riverJobID int64,
) (*model.LinkTranslation, error) {
	return bindCurrentTranslationAttempt(ctx, tx, seed, riverJobID)
}
