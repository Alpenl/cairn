package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

const libraryReviewColumns = "id, kind, link_id, site_id, payload, status, revision, created_at, resolved_at"

type PGXLibraryReviewRepository struct{ db database.Querier }

func NewPGXLibraryReviewRepository(db database.Querier) *PGXLibraryReviewRepository {
	return &PGXLibraryReviewRepository{db: db}
}

func (r *PGXLibraryReviewRepository) ListLibraryReviews(ctx context.Context, p ListLibraryReviewsParams) ([]model.LibraryReviewItem, error) {
	if p.Limit < 1 || p.Limit > 100 || p.Offset < 0 {
		return nil, fmt.Errorf("invalid library review page")
	}
	where := "TRUE"
	args := []any{}
	if p.Status != nil {
		where += fmt.Sprintf(" AND status=$%d", len(args)+1)
		args = append(args, *p.Status)
	}
	if p.Kind != nil {
		where += fmt.Sprintf(" AND kind=$%d", len(args)+1)
		args = append(args, *p.Kind)
	}
	args = append(args, p.Limit, p.Offset)
	query := "SELECT " + libraryReviewColumns + " FROM library_review_items WHERE " + where + fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list library reviews: %w", err)
	}
	defer rows.Close()
	out := []model.LibraryReviewItem{}
	for rows.Next() {
		item, err := scanLibraryReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate library reviews: %w", err)
	}
	return out, nil
}

func (r *PGXLibraryReviewRepository) ResolveLibraryReview(ctx context.Context, p ResolveLibraryReviewParams) (*model.LibraryReviewItem, error) {
	row := r.db.QueryRow(ctx, "UPDATE library_review_items SET status=$1, revision=revision+1, resolved_at=NOW() WHERE id=$2 AND revision=$3 AND status='pending' RETURNING "+libraryReviewColumns, p.Status, p.ID, p.Revision)
	item, err := scanLibraryReview(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("resolve library review: %w", err)
	}
	return &item, nil
}

func scanLibraryReview(row interface{ Scan(...any) error }) (model.LibraryReviewItem, error) {
	var item model.LibraryReviewItem
	var linkID, siteID pgtype.UUID
	var resolvedAt pgtype.Timestamptz
	err := row.Scan(&item.ID, &item.Kind, &linkID, &siteID, &item.Payload, &item.Status, &item.Revision, &item.CreatedAt, &resolvedAt)
	item.LinkID, item.SiteID, item.ResolvedAt = uuidPointer(linkID), uuidPointer(siteID), timePointer(resolvedAt)
	return item, err
}

var _ LibraryReviewStore = (*PGXLibraryReviewRepository)(nil)
