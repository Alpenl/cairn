package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
)

const (
	lockRepresentationWriteGateSharedSQL    = `SELECT lock_representation_write_gate_shared()`
	lockRepresentationWriteGateExclusiveSQL = `SELECT lock_representation_write_gate_exclusive()`
	lockLibraryFeedRevisionsSQL             = `SELECT lock_library_feed_revisions()`
	lockLibraryGlobalRevisionsSQL           = `SELECT lock_library_global_revisions()`
)

func prelockRepresentationWriteGateShared(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, lockRepresentationWriteGateSharedSQL); err != nil {
		return fmt.Errorf("prelock shared representation write gate: %w", err)
	}
	return nil
}

func prelockRepresentationWriteGateExclusive(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, lockRepresentationWriteGateExclusiveSQL); err != nil {
		return fmt.Errorf("prelock exclusive representation write gate: %w", err)
	}
	return nil
}

func prelockLibraryFeedRevisions(ctx context.Context, db database.Querier) error {
	if _, err := db.Exec(ctx, lockLibraryFeedRevisionsSQL); err != nil {
		return fmt.Errorf("prelock library/feed representation revisions: %w", err)
	}
	return nil
}

func prelockLibraryGlobalRevisions(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, lockLibraryGlobalRevisionsSQL); err != nil {
		return fmt.Errorf("prelock library/global representation revisions: %w", err)
	}
	return nil
}
