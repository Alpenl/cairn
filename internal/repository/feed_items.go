package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"webtag/internal/alloc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/model"
)

const feedItemSelectColumns = `fi.id, fi.subscription_id, s.title, fi.external_id, fi.title, fi.url,
	fi.author, fi.summary, fi.content_text, fi.content_html, fi.published_at,
	fi.created_at, fi.read_at, fi.starred, fi.read_later, fi.link_id, l.status`

func (r *PGXFeedRepository) ListItems(ctx context.Context, filter FeedItemFilter) (model.PaginatedFeedItems, error) {
	where, args := buildFeedItemWhere(filter)
	var total int
	if err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM feed_items fi
		JOIN feed_subscriptions s ON s.id = fi.subscription_id
		WHERE `+where, args...).Scan(&total); err != nil {
		return model.PaginatedFeedItems{}, fmt.Errorf("count feed items: %w", err)
	}

	offset := (filter.Page - 1) * filter.Limit
	queryArgs := append(append([]any(nil), args...), filter.Limit, offset)
	rows, err := r.db.Query(ctx, `SELECT `+feedItemSelectColumns+`
		FROM feed_items fi
		JOIN feed_subscriptions s ON s.id = fi.subscription_id
		LEFT JOIN links l ON l.id = fi.link_id
		WHERE `+where+`
		ORDER BY COALESCE(fi.published_at, fi.created_at) DESC, fi.id DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), queryArgs...)
	if err != nil {
		return model.PaginatedFeedItems{}, fmt.Errorf("list feed items: %w", err)
	}
	defer rows.Close()
	items := make([]model.FeedItem, 0, alloc.Hint(filter.Limit))
	for rows.Next() {
		item, scanErr := scanFeedItem(rows)
		if scanErr != nil {
			return model.PaginatedFeedItems{}, fmt.Errorf("scan feed item: %w", scanErr)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return model.PaginatedFeedItems{}, fmt.Errorf("iterate feed items: %w", err)
	}
	return model.PaginatedFeedItems{Items: items, Total: total, Page: filter.Page, Limit: filter.Limit}, nil
}

func (r *PGXFeedRepository) GetItem(ctx context.Context, id uuid.UUID, markRead bool) (model.FeedItem, bool, error) {
	return r.getItem(ctx, id, markRead, false)
}

func (r *PGXFeedRepository) getItem(ctx context.Context, id uuid.UUID, markRead, includeHidden bool) (model.FeedItem, bool, error) {
	if markRead {
		if _, err := r.db.Exec(ctx, `UPDATE feed_items SET read_at = COALESCE(read_at, now()), updated_at = now()
			WHERE id = $1 AND ($2 OR EXISTS (
				SELECT 1 FROM feed_subscriptions s WHERE s.id = subscription_id
					AND (s.active OR feed_items.starred OR feed_items.read_later)
			))`, id, includeHidden); err != nil {
			return model.FeedItem{}, false, fmt.Errorf("mark opened feed item read: %w", err)
		}
	}
	row := r.db.QueryRow(ctx, `SELECT `+feedItemSelectColumns+`
		FROM feed_items fi
		JOIN feed_subscriptions s ON s.id = fi.subscription_id
		LEFT JOIN links l ON l.id = fi.link_id
		WHERE fi.id = $1
			AND ($2 OR s.active OR fi.starred OR fi.read_later)`, id, includeHidden)
	item, err := scanFeedItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.FeedItem{}, false, nil
	}
	if err != nil {
		return model.FeedItem{}, false, fmt.Errorf("get feed item: %w", err)
	}
	return item, true, nil
}

func (r *PGXFeedRepository) UpdateItemState(ctx context.Context, id uuid.UUID, patch FeedItemStatePatch) (model.FeedItem, error) {
	tag, err := r.db.Exec(ctx, `UPDATE feed_items SET
		read_at = CASE WHEN $2::boolean IS NULL THEN read_at
			WHEN $2 THEN COALESCE(read_at, now()) ELSE NULL END,
		starred = COALESCE($3::boolean, starred),
		read_later = COALESCE($4::boolean, read_later),
		updated_at = now()
		WHERE id = $1 AND EXISTS (
			SELECT 1 FROM feed_subscriptions s WHERE s.id = subscription_id
				AND (s.active OR feed_items.starred OR feed_items.read_later)
		)`, id, patch.Read, patch.Starred, patch.ReadLater)
	if err != nil {
		return model.FeedItem{}, fmt.Errorf("update feed item state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.FeedItem{}, ErrNotFound
	}
	item, found, err := r.getItem(ctx, id, false, true)
	if err != nil {
		return model.FeedItem{}, err
	}
	if !found {
		return model.FeedItem{}, ErrNotFound
	}
	return item, nil
}

func (r *PGXFeedRepository) MarkItemsRead(ctx context.Context, filter FeedItemFilter) (int64, error) {
	where, args := buildFeedItemWhere(filter)
	tag, err := r.db.Exec(ctx, `UPDATE feed_items fi SET read_at = COALESCE(fi.read_at, now()), updated_at = now()
		FROM feed_subscriptions s
		WHERE s.id = fi.subscription_id AND `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("mark feed items read: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *PGXFeedRepository) AssociateItemLink(ctx context.Context, itemID, linkID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE feed_items fi SET link_id = $2, updated_at = now()
		WHERE fi.id = $1
		AND EXISTS (SELECT 1 FROM links l WHERE l.id = $2)`, itemID, linkID)
	if err != nil {
		return fmt.Errorf("associate feed item link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func buildFeedItemWhere(filter FeedItemFilter) (string, []any) {
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 3)
	switch filter.View {
	case "unread":
		clauses = append(clauses, "s.active AND fi.read_at IS NULL")
	case "starred":
		clauses = append(clauses, "fi.starred")
	case "later":
		clauses = append(clauses, "fi.read_later")
	default:
		clauses = append(clauses, "s.active")
	}
	if filter.SubscriptionID != nil {
		args = append(args, *filter.SubscriptionID)
		clauses = append(clauses, fmt.Sprintf("fi.subscription_id = $%d", len(args)))
	}
	if filter.FolderID != nil {
		args = append(args, *filter.FolderID)
		clauses = append(clauses, fmt.Sprintf("s.folder_id = $%d", len(args)))
	} else if filter.Ungrouped {
		clauses = append(clauses, "s.folder_id IS NULL")
	}
	if query := strings.TrimSpace(filter.Query); query != "" {
		args = append(args, "%"+query+"%")
		placeholder := fmt.Sprintf("$%d", len(args))
		clauses = append(clauses, `(fi.title ILIKE `+placeholder+` OR COALESCE(fi.author, '') ILIKE `+placeholder+
			` OR COALESCE(fi.content_text, '') ILIKE `+placeholder+` OR s.title ILIKE `+placeholder+`)`)
	}
	return strings.Join(clauses, " AND "), args
}

func scanFeedItem(row interface{ Scan(...any) error }) (model.FeedItem, error) {
	var item model.FeedItem
	if err := row.Scan(&item.ID, &item.SubscriptionID, &item.SubscriptionTitle, &item.ExternalID, &item.Title, &item.URL,
		&item.Author, &item.Summary, &item.Content, &item.ContentHTML,
		&item.PublishedAt, &item.CreatedAt, &item.ReadAt, &item.Starred,
		&item.ReadLater, &item.LinkID, &item.AnalysisStatus); err != nil {
		return model.FeedItem{}, err
	}
	item.Read = item.ReadAt != nil
	return item, nil
}
