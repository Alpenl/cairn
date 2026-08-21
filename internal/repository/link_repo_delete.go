package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
)

const lockLinkForDeleteSQL = `SELECT id
	FROM links
	WHERE id = $1
	FOR UPDATE`

const terminalizeDeletedTranslationAttemptsSQL = `UPDATE link_translations
	SET status='failed',error_msg='link_deleted',current_river_job_id=NULL,updated_at=NOW()
	WHERE link_id=$1 AND status IN ('pending','processing')`

// DeleteLifecycle serializes soft deletion with requeue/parse state writes in a
// repository-owned transaction. Product callers use durablework.LinkCommands
// so active River jobs are cancelled between the lock and state-change steps.
func (r *PGXLinkRepository) DeleteLifecycle(ctx context.Context, id uuid.UUID) error {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin link delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockedID, err := r.LockLinkForDeleteTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.DeleteLockedLinkTx(ctx, tx, lockedID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit link delete tx: %w", err)
	}
	return nil
}

// LockLinkForDeleteTx establishes the canonical delete lock. The durable adapter
// must keep this tx open, cancel active River jobs, then call
// DeleteLockedLinkTx with the returned ID.
func (r *PGXLinkRepository) LockLinkForDeleteTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (uuid.UUID, error) {
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, lockLinkForDeleteSQL, id).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("lock link for delete: %w", err)
	}
	return lockedID, nil
}

// DeleteLockedLinkTx deletes a link already locked by LockLinkForDeleteTx.
// Keeping the mutation separate is what gives durablework a real seam for the
// queue cancellation that must commit or roll back with the business row. A
// previously trashed row remains a successful no-op.
func (r *PGXLinkRepository) DeleteLockedLinkTx(ctx context.Context, tx pgx.Tx, lockedID uuid.UUID) error {
	return terminalizeAndDeleteLockedLinkOn(ctx, tx, lockedID)
}

func terminalizeAndDeleteLockedLinkOn(ctx context.Context, db database.Querier, lockedID uuid.UUID) error {
	if _, err := db.Exec(ctx, terminalizeDeletedTranslationAttemptsSQL, lockedID); err != nil {
		return fmt.Errorf("terminalize deleted link translation attempts: %w", err)
	}
	tag, err := db.Exec(ctx, deleteLinkSQL, lockedID)
	if err != nil {
		return fmt.Errorf("soft delete link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// The delete above fires the trigger that tombstones this Link's Thoughts.
	// Their TODO projections have to retire in the same transaction, otherwise
	// they outlive their source with no read left to notice.
	return NewPGXReaderVNextRepository(db).replaceLinkThoughtTodoProjectionsOn(ctx, db, lockedID)
}
