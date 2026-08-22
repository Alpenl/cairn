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

const updateLibraryClassificationForCompletionSQL = `UPDATE links
SET library_kind=$2,library_kind_locked=$3,updated_at=NOW()
WHERE id=$1 AND deleted_at IS NULL
  AND library_kind IS NOT DISTINCT FROM $4
  AND library_kind_locked=$5`

const selectLibrarySelectionChangedSQL = `SELECT
library_kind IS DISTINCT FROM $2 OR library_kind_locked <> $3
FROM links WHERE id=$1 AND deleted_at IS NULL`

func updateLibraryClassificationForCompletionOn(
	ctx context.Context,
	db database.Querier,
	params UpdateLibraryClassificationParams,
	expectedKind *model.LibraryKind,
	expectedLocked bool,
) error {
	if params.Kind != model.LibraryKindReading && params.Kind != model.LibraryKindSite {
		return fmt.Errorf("update library classification: invalid kind %q", params.Kind)
	}
	tag, err := db.Exec(
		ctx,
		updateLibraryClassificationForCompletionSQL,
		params.ID,
		params.Kind,
		params.Locked,
		expectedKind,
		expectedLocked,
	)
	if err != nil {
		return fmt.Errorf("update library classification for completion: %w", err)
	}
	if tag.RowsAffected() != 0 {
		return nil
	}

	return classifyLibrarySelectionMiss(ctx, db, params.ID, expectedKind, expectedLocked)
}

func classifyLibrarySelectionMiss(
	ctx context.Context,
	db database.Querier,
	linkID uuid.UUID,
	expectedKind *model.LibraryKind,
	expectedLocked bool,
) error {
	var changed bool
	err := db.QueryRow(ctx, selectLibrarySelectionChangedSQL, linkID, expectedKind, expectedLocked).Scan(&changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("update library classification for completion: inspect selection: %w", err)
	}
	if changed {
		return ErrLibrarySelectionChanged
	}
	return ErrNotFound
}
