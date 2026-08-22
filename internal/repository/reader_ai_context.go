package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/model"
)

// GetAIContext returns only bounded, published link context and a small live
// thought projection from the installed library.
func (r *PGXReaderVNextRepository) GetAIContext(ctx context.Context, linkID uuid.UUID) (*model.ReaderAIContext, error) {
	item := &model.ReaderAIContext{LinkID: linkID, Tags: []string{}, Thoughts: []model.ReaderAIThoughtContext{}}
	if err := r.db.QueryRow(ctx, `
		SELECT left(COALESCE(content,''),12000),left(COALESCE(summary,''),2000),COALESCE(tags,'{}')
		FROM links
		WHERE id=$1 AND deleted_at IS NULL`, linkID).Scan(&item.Content, &item.Summary, &item.Tags); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read AI link context: %w", err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT left(t.body,1000)
		FROM reader_thoughts t
		WHERE t.host_kind='link' AND t.host_id=$1 AND t.deleted=false
			AND NOT EXISTS (
				SELECT 1 FROM reader_thought_tombstones tt
				WHERE tt.thought_id=t.id
			)
		ORDER BY t.updated_at DESC,t.id DESC LIMIT 8`, linkID.String())
	if err != nil {
		return nil, fmt.Errorf("read AI thought context: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var thought model.ReaderAIThoughtContext
		if err := rows.Scan(&thought.Body); err != nil {
			return nil, fmt.Errorf("scan AI thought context: %w", err)
		}
		item.Thoughts = append(item.Thoughts, thought)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read AI thought context rows: %w", err)
	}
	return item, nil
}
