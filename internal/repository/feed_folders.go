package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/model"
)

func (r *PGXFeedRepository) ListFolders(ctx context.Context) ([]model.FeedFolder, error) {
	rows, err := r.db.Query(ctx, `SELECT id, name, created_at, updated_at
		FROM feed_folders ORDER BY lower(name), id`)
	if err != nil {
		return nil, fmt.Errorf("list feed folders: %w", err)
	}
	defer rows.Close()
	folders := make([]model.FeedFolder, 0)
	for rows.Next() {
		var folder model.FeedFolder
		if err := rows.Scan(&folder.ID, &folder.Name, &folder.CreatedAt, &folder.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan feed folder: %w", err)
		}
		folders = append(folders, folder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feed folders: %w", err)
	}
	return folders, nil
}

func (r *PGXFeedRepository) CreateFolder(ctx context.Context, name string) (model.FeedFolder, error) {
	var folder model.FeedFolder
	err := r.db.QueryRow(ctx, `INSERT INTO feed_folders (name)
		VALUES ($1)
		ON CONFLICT (lower(name)) DO UPDATE SET
			name = EXCLUDED.name, updated_at = now()
		RETURNING id, name, created_at, updated_at`, strings.TrimSpace(name)).Scan(
		&folder.ID, &folder.Name, &folder.CreatedAt, &folder.UpdatedAt,
	)
	if err != nil {
		return model.FeedFolder{}, fmt.Errorf("create feed folder: %w", err)
	}
	return folder, nil
}

func (r *PGXFeedRepository) UpdateFolder(ctx context.Context, id uuid.UUID, name string) (model.FeedFolder, error) {
	var folder model.FeedFolder
	err := r.db.QueryRow(ctx, `UPDATE feed_folders SET name = $2, updated_at = now()
		WHERE id = $1
		RETURNING id, name, created_at, updated_at`, id, strings.TrimSpace(name)).Scan(
		&folder.ID, &folder.Name, &folder.CreatedAt, &folder.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.FeedFolder{}, ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return model.FeedFolder{}, fmt.Errorf("update feed folder: %w", ErrFeedFolderNameConflict)
	}
	if err != nil {
		return model.FeedFolder{}, fmt.Errorf("update feed folder: %w", err)
	}
	return folder, nil
}

// DeleteFolder relies on ON DELETE SET NULL, so deleting organization never
// unsubscribes a source.
func (r *PGXFeedRepository) DeleteFolder(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM feed_folders WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete feed folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
