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

// ReaderLinkLifecycleChange is the durable work produced by a Reader-owned
// Link restore or trash transition. The repository owns the PostgreSQL state
// change; the caller owns River cancellation/enqueue inside the supplied
// transaction.
type ReaderLinkLifecycleChange struct {
	LinkID       uuid.UUID
	ParseAttempt *model.ParseAttempt
}

// ReaderLinkLifecycle applies durable Link work without making the repository
// retain or construct a queue. It is supplied per transaction by the
// durable-work adapter at the application boundary.
type ReaderLinkLifecycle func(context.Context, pgx.Tx, ReaderLinkLifecycleChange) error

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
	lifecycle ReaderLinkLifecycle,
) (linkRestoreResult, error) {
	result, err := restoreLinkOn(ctx, db, id)
	if err != nil || !result.changed || (result.status != model.LinkStatusPending && result.status != model.LinkStatusProcessing) {
		return result, err
	}
	if lifecycle == nil {
		return linkRestoreResult{}, errors.New("restore in-flight Link: lifecycle queue is not configured")
	}
	tx, ok := db.(pgx.Tx)
	if !ok {
		return linkRestoreResult{}, errors.New("restore in-flight Link: transaction-bound lifecycle queue requires pgx.Tx")
	}
	if _, err := tx.Exec(ctx, terminalizeDeletedTranslationAttemptsSQL, id); err != nil {
		return linkRestoreResult{}, fmt.Errorf("terminalize restored Link translation attempts: %w", err)
	}
	attempt, err := restartParseAttemptOn(ctx, tx, id)
	if err != nil {
		return linkRestoreResult{}, err
	}
	if err := lifecycle(ctx, tx, ReaderLinkLifecycleChange{LinkID: id, ParseAttempt: &attempt}); err != nil {
		return linkRestoreResult{}, fmt.Errorf("schedule restored Link work: %w", err)
	}
	return result, nil
}
