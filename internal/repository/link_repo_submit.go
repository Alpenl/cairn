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
	// A concrete capture selection replaces an unlocked partition. An explicit
	// user selection may also replace an existing lock; automatic sources cannot.
	requeueLinkCaptureSQL = `UPDATE links SET source_kind = $2, source_key = $3, input_title = $4, input_text = $5, input_html = $6, input_images = $7::jsonb, source_metadata = $8::jsonb, description = COALESCE($9, description),
		library_kind = CASE WHEN $10 IN ('reading','site') AND ($11 OR NOT library_kind_locked) THEN $10 ELSE library_kind END,
		library_kind_locked = CASE WHEN $10 IN ('reading','site') AND ($11 OR NOT library_kind_locked) THEN true ELSE library_kind_locked END,
		content = NULL, content_cjk_chars = 0, content_words = 0, content_document = NULL, content_format = 'plain', content_source = 'fetched', content_revision = content_revision + 1,
		payload_purge_due_at = CASE
			WHEN $10 = 'site' AND ($11 OR NOT library_kind_locked) THEN NOW() + INTERVAL '24 hours'
			WHEN $10 = 'reading' AND ($11 OR NOT library_kind_locked) THEN NULL
			WHEN library_kind = 'site' THEN NOW() + INTERVAL '24 hours'
			ELSE NULL
		END,
		payload_purged_at = CASE WHEN ($10 = 'site' AND ($11 OR NOT library_kind_locked)) OR library_kind = 'site' THEN NULL ELSE payload_purged_at END,
		status = 'pending', error_msg = NULL, parse_generation = parse_generation + 1, last_recollected_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL RETURNING parse_generation, metadata_revision`
	requeueLinkRefreshSQL  = "UPDATE links SET status = 'pending', error_msg = NULL, parse_generation = parse_generation + 1, last_recollected_at = NOW(), updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL RETURNING parse_generation, metadata_revision"
	adoptSubmittedLinkSQL  = "UPDATE links SET feed_managed=false,updated_at=NOW() WHERE id=$1 AND feed_managed=true"
	restartRestoredLinkSQL = "UPDATE links SET status='pending',error_msg=NULL,parse_generation=parse_generation+1,last_recollected_at=NOW(),updated_at=NOW() WHERE id=$1 RETURNING parse_generation,metadata_revision"
	lockLibraryKindSQL     = "SELECT status, library_kind_locked FROM links WHERE id = $1 AND deleted_at IS NULL FOR UPDATE"
	setLibraryKindSQL      = `UPDATE links SET
		library_kind = $2, library_kind_locked = true,
		payload_purge_due_at = CASE WHEN $2 = 'site' THEN COALESCE(payload_purge_due_at, NOW() + INTERVAL '24 hours') WHEN $2 = 'reading' THEN NULL ELSE payload_purge_due_at END,
		payload_purged_at = CASE WHEN $2 = 'site' THEN NULL ELSE payload_purged_at END,
		updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`

	// The canonical URL owner wins over legacy source-key or display-URL
	// matches. The selected row includes soft-deleted links because saving one
	// again restores that identity instead of creating a duplicate.
	selectSubmittedLinkSQL = `WITH canonical AS (
		SELECT link_id
		FROM link_url_identities
		WHERE normalized_url=$1 OR normalized_url=$2
		ORDER BY (normalized_url=$1) DESC, normalized_url ASC
		LIMIT 1
	)
	SELECT ` + linkSelectColumns + `, deleted_at IS NOT NULL
	FROM links
	WHERE source_key=$1 OR url=$2 OR id=(SELECT link_id FROM canonical)
	ORDER BY (id=(SELECT link_id FROM canonical)) DESC, (source_key=$1) DESC, id ASC
	LIMIT 1
	FOR UPDATE`
)

// RequeueExistingTx is the transaction-bound implementation used by the
// durablework adapter. It never commits and never calls infrastructure hooks;
// the adapter owns queue cancellation/insertion and the final commit decision.
func (r *PGXLinkRepository) RequeueExistingTx(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	capture *CreateLinkParams,
) (model.ParseAttempt, error) {
	inputImages, sourceMetadata, err := marshalRequeueCapture(capture)
	if err != nil {
		return model.ParseAttempt{}, err
	}
	return updateLinkForRequeue(ctx, tx, linkID, capture, inputImages, sourceMetadata)
}

// SetLibraryKindTx serializes a fixed capture selection with terminal parse
// completion. Automatic sources cannot replace an existing lock; explicit user
// input can.
func (r *PGXLinkRepository) SetLibraryKindTx(
	ctx context.Context,
	tx pgx.Tx,
	params SetLibraryKindParams,
) (SetLibraryKindResult, error) {
	if params.Kind != model.LibraryKindReading && params.Kind != model.LibraryKindSite {
		return SetLibraryKindResult{}, fmt.Errorf("set library kind: concrete kind is required")
	}
	var (
		status        model.LinkStatus
		currentLocked bool
	)
	if err := tx.QueryRow(ctx, lockLibraryKindSQL, params.ID).Scan(&status, &currentLocked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SetLibraryKindResult{}, ErrNotFound
		}
		return SetLibraryKindResult{}, fmt.Errorf("lock library kind: %w", err)
	}
	if currentLocked && !params.Override {
		return SetLibraryKindResult{Status: status}, nil
	}
	tag, err := tx.Exec(ctx, setLibraryKindSQL, params.ID, params.Kind)
	if err != nil {
		return SetLibraryKindResult{}, fmt.Errorf("set library kind: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return SetLibraryKindResult{}, ErrNotFound
	}
	return SetLibraryKindResult{Status: status}, nil
}

func marshalRequeueCapture(capture *CreateLinkParams) (inputImages, sourceMetadata any, err error) {
	if capture == nil {
		return nil, nil, nil
	}
	inputImages, err = marshalJSONB(capture.InputImages)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal requeue input images: %w", err)
	}
	sourceMetadata, err = marshalJSONB(capture.SourceMetadata)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal requeue source metadata: %w", err)
	}
	return inputImages, sourceMetadata, nil
}

func updateLinkForRequeue(
	ctx context.Context,
	tx pgx.Tx,
	linkID uuid.UUID,
	capture *CreateLinkParams,
	inputImages, sourceMetadata any,
) (model.ParseAttempt, error) {
	attempt := model.ParseAttempt{LinkID: linkID}
	if capture == nil {
		if err := tx.QueryRow(ctx, requeueLinkRefreshSQL, linkID).Scan(&attempt.Generation, &attempt.ExpectedMetadataRevision); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return model.ParseAttempt{}, ErrNotFound
			}
			return model.ParseAttempt{}, fmt.Errorf("mark existing link pending: %w", err)
		}
		return attempt, nil
	}

	updateErr := tx.QueryRow(
		ctx,
		requeueLinkCaptureSQL,
		linkID,
		defaultSourceKind(capture.SourceKind),
		defaultSourceKey(capture.SourceKey, capture.URL),
		capture.InputTitle,
		capture.InputText,
		capture.InputHTML,
		inputImages,
		sourceMetadata,
		capture.Description,
		capture.RequestedLibraryKind,
		capture.UserSelectedLibraryKind,
	).Scan(&attempt.Generation, &attempt.ExpectedMetadataRevision)
	if updateErr != nil {
		if errors.Is(updateErr, pgx.ErrNoRows) {
			return model.ParseAttempt{}, ErrNotFound
		}
		return model.ParseAttempt{}, fmt.Errorf("update existing capture for requeue: %w", updateErr)
	}
	return attempt, nil
}

// SubmitTx resolves one capture identity, restores an existing Trash row when
// necessary, or inserts one new Link. The caller owns the transaction and
// enqueues Attempt before commit.
func (r *PGXLinkRepository) SubmitTx(ctx context.Context, tx pgx.Tx, params CreateLinkParams) (LinkSubmitResult, error) {
	existing, trashed, err := querySubmittedLink(ctx, tx, params)
	if err != nil {
		return LinkSubmitResult{}, err
	}
	if existing != nil {
		return prepareSubmittedLink(ctx, tx, existing, trashed)
	}

	link, err := insertLinkWithSQLOn(ctx, tx, insertSubmittedLinkSQL, params)
	if err == nil {
		return LinkSubmitResult{
			Link: link,
			Attempt: &model.ParseAttempt{
				LinkID: link.ID, Generation: link.ParseGeneration,
				ExpectedMetadataRevision: link.MetadataRevision,
			},
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LinkSubmitResult{}, err
	}

	// A writer outside the URL-lock boundary can still win after the pre-read.
	// ON CONFLICT deliberately avoids rewriting it, then this one re-read
	// returns the committed identity without creating duplicate work.
	existing, trashed, err = querySubmittedLink(ctx, tx, params)
	if err != nil {
		return LinkSubmitResult{}, err
	}
	if existing == nil {
		return LinkSubmitResult{}, fmt.Errorf("submit link: conflict row is missing for source_key %q", defaultSourceKey(params.SourceKey, params.URL))
	}
	return prepareSubmittedLink(ctx, tx, existing, trashed)
}

func querySubmittedLink(ctx context.Context, tx pgx.Tx, params CreateLinkParams) (*model.Link, bool, error) {
	var (
		link    model.Link
		buf     linkScanBuffers
		trashed bool
	)
	fields := append(scanLinkFields(&link, &buf), &trashed)
	err := tx.QueryRow(ctx, selectSubmittedLinkSQL, defaultSourceKey(params.SourceKey, params.URL), params.URL).Scan(fields...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select submitted link: %w", err)
	}
	if err := applyLinkScanBuffers(&link, &buf); err != nil {
		return nil, false, fmt.Errorf("scan submitted link: %w", err)
	}
	return &link, trashed, nil
}

func prepareSubmittedLink(ctx context.Context, tx pgx.Tx, link *model.Link, trashed bool) (LinkSubmitResult, error) {
	if !trashed {
		if _, err := tx.Exec(ctx, adoptSubmittedLinkSQL, link.ID); err != nil {
			return LinkSubmitResult{}, fmt.Errorf("adopt submitted link: %w", err)
		}
		return LinkSubmitResult{Link: link}, nil
	}

	restored, err := restoreLinkOn(ctx, tx, link.ID)
	if err != nil {
		return LinkSubmitResult{}, err
	}
	if _, err := tx.Exec(ctx, adoptSubmittedLinkSQL, link.ID); err != nil {
		return LinkSubmitResult{}, fmt.Errorf("adopt restored submitted link: %w", err)
	}
	result := LinkSubmitResult{Link: link, Restored: restored.changed}
	if restored.status != model.LinkStatusPending && restored.status != model.LinkStatusProcessing {
		return result, nil
	}
	attempt, err := restartParseAttemptOn(ctx, tx, link.ID)
	if err != nil {
		return LinkSubmitResult{}, err
	}
	link.Status = model.LinkStatusPending
	result.Attempt = &attempt
	return result, nil
}

func restartParseAttemptOn(ctx context.Context, db database.Querier, linkID uuid.UUID) (model.ParseAttempt, error) {
	attempt := model.ParseAttempt{LinkID: linkID}
	if err := db.QueryRow(ctx, restartRestoredLinkSQL, linkID).Scan(&attempt.Generation, &attempt.ExpectedMetadataRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ParseAttempt{}, ErrNotFound
		}
		return model.ParseAttempt{}, fmt.Errorf("restart restored link: %w", err)
	}
	return attempt, nil
}
