package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

const lockLinkForRestoreSQL = `SELECT status,deleted_at,COALESCE(content_document,content,''),content_revision
	FROM links WHERE id=$1 FOR UPDATE`

type linkRestoreResult struct {
	status  model.LinkStatus
	changed bool
}

// restoreLinkOn is the transaction-bound Link restore primitive used by the
// public Trash command and by canonical identity reusers. Callers own the
// surrounding transaction, so restoring the Link and re-anchoring its Thoughts
// commit or roll back together.
func restoreLinkOn(ctx context.Context, db database.Querier, id uuid.UUID) (linkRestoreResult, error) {
	var (
		result    linkRestoreResult
		deletedAt pgtype.Timestamptz
		body      string
		revision  int64
	)
	if err := db.QueryRow(ctx, lockLinkForRestoreSQL, id).Scan(
		&result.status,
		&deletedAt,
		&body,
		&revision,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return linkRestoreResult{}, ErrNotFound
		}
		return linkRestoreResult{}, fmt.Errorf("lock link for restore: %w", err)
	}
	if deletedAt.Valid {
		if tag, err := db.Exec(ctx, `UPDATE links SET deleted_at=NULL,updated_at=NOW() WHERE id=$1 AND deleted_at IS NOT NULL`, id); err != nil {
			return linkRestoreResult{}, fmt.Errorf("restore link: %w", err)
		} else if tag.RowsAffected() == 0 {
			return linkRestoreResult{}, ErrRevisionConflict
		}
		reader := NewPGXReaderVNextRepository(db)
		if err := reader.restoreReaderHostThoughts(ctx, db, model.ReaderHostLink, id, body, revision); err != nil {
			return linkRestoreResult{}, err
		}
		result.changed = true
	}
	return result, nil
}

// restoreLinkLifecycleOn extends the shared Link/Thought restore with the
// durable parse lifecycle required by Reader-owned restore entry points. A
// deleted in-flight Link has no runnable attempt after deletion, so restoring
// it must create and enqueue one replacement in the same transaction.
func (r *PGXReaderVNextRepository) restoreLinkLifecycleOn(
	ctx context.Context,
	db database.Querier,
	id uuid.UUID,
) (linkRestoreResult, error) {
	result, err := restoreLinkOn(ctx, db, id)
	if err != nil || !result.changed || (result.status != model.LinkStatusPending && result.status != model.LinkStatusProcessing) {
		return result, err
	}
	if r.linkLifecycleQueue == nil {
		return linkRestoreResult{}, errors.New("restore in-flight Link: lifecycle queue is not configured")
	}
	tx, ok := db.(pgx.Tx)
	if !ok {
		return linkRestoreResult{}, errors.New("restore in-flight Link: transaction-bound lifecycle queue requires pgx.Tx")
	}
	if err := r.linkLifecycleQueue.CancelAllActiveTx(ctx, tx, id); err != nil {
		return linkRestoreResult{}, fmt.Errorf("cancel restored Link work: %w", err)
	}
	if _, err := tx.Exec(ctx, terminalizeDeletedTranslationAttemptsSQL, id); err != nil {
		return linkRestoreResult{}, fmt.Errorf("terminalize restored Link translation attempts: %w", err)
	}
	attempt, err := restartParseAttemptOn(ctx, tx, id)
	if err != nil {
		return linkRestoreResult{}, err
	}
	if err := r.linkLifecycleQueue.EnqueueTx(ctx, tx, attempt); err != nil {
		return linkRestoreResult{}, fmt.Errorf("enqueue restored Link parse attempt: %w", err)
	}
	return result, nil
}
