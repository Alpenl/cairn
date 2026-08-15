package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

var (
	ErrFeedRefreshClaimLost   = errors.New("feed refresh claim is no longer owned")
	ErrFeedFolderNameConflict = errors.New("feed folder name already exists")
)

const feedSubscriptionColumns = `s.id, COALESCE(s.canonical_url, s.url), s.site_url, s.title, s.description,
	s.folder_id, s.active, s.last_fetched_at, s.last_success_at, s.next_fetch_at,
	s.last_error, s.failure_count, COALESCE(s.etag, ''), COALESCE(s.last_modified, ''),
	COALESCE(s.sync_boundary_external_id, ''), s.refresh_claim_token, s.refresh_claimed_until,
	s.created_at, s.updated_at`

type FeedItemFilter struct {
	View           string
	SubscriptionID *uuid.UUID
	FolderID       *uuid.UUID
	Ungrouped      bool
	Query          string
	Page           int
	Limit          int
}

type FeedItemStatePatch struct {
	Read      *bool
	Starred   *bool
	ReadLater *bool
}

type FeedSubscriptionPatch struct {
	FolderID  *uuid.UUID
	SetFolder bool
	Title     *string
}

type FeedRefreshSuccess struct {
	SubscriptionID uuid.UUID
	ClaimToken     uuid.UUID
	ETag           string
	LastModified   string
	Parsed         model.ParsedFeed
	NotModified    bool
	Initial        bool
	Now            time.Time
}

// PGXFeedRepository keeps the product query handle and a transaction beginner
// used by ClaimDue. In production they are backed by the same installation.
type PGXFeedRepository struct {
	db        database.Querier
	tx        database.TxBeginner
	scheduler database.TxBeginner
}

func NewPGXFeedRepository(db database.Querier, scheduler database.TxBeginner) *PGXFeedRepository {
	beginner, ok := db.(database.TxBeginner)
	if !ok {
		panic("repository: feed Querier must support transactions")
	}
	return &PGXFeedRepository{db: db, tx: beginner, scheduler: scheduler}
}

func (r *PGXFeedRepository) ListOverview(ctx context.Context, rawURL string) (model.FeedSubscriptionsResponse, error) {
	folders, err := r.ListFolders(ctx)
	if err != nil {
		return model.FeedSubscriptionsResponse{}, err
	}
	rows, err := r.db.Query(ctx, `SELECT `+feedSubscriptionColumns+`,
		COUNT(fi.id) FILTER (WHERE s.active OR fi.starred OR fi.read_later),
		COUNT(fi.id) FILTER (WHERE fi.read_at IS NULL AND s.active)
		FROM feed_subscriptions s
		LEFT JOIN feed_items fi ON fi.subscription_id = s.id
		WHERE s.active AND (
			$1 = '' OR s.canonical_url = $1 OR (
				s.canonical_url IS NULL AND s.url = $1
				AND NOT EXISTS (
					SELECT 1 FROM feed_subscriptions canonical
					WHERE canonical.canonical_url = $1
				)
			)
		)
		GROUP BY s.id
		ORDER BY lower(s.title), s.id`, strings.TrimSpace(rawURL))
	if err != nil {
		return model.FeedSubscriptionsResponse{}, fmt.Errorf("list feed subscriptions: %w", err)
	}
	defer rows.Close()
	subscriptions := make([]model.FeedSubscription, 0)
	for rows.Next() {
		subscription, scanErr := scanFeedSubscriptionWithCounts(rows)
		if scanErr != nil {
			return model.FeedSubscriptionsResponse{}, fmt.Errorf("scan feed subscription: %w", scanErr)
		}
		subscriptions = append(subscriptions, subscription)
	}
	if err := rows.Err(); err != nil {
		return model.FeedSubscriptionsResponse{}, fmt.Errorf("iterate feed subscriptions: %w", err)
	}

	var counts model.FeedCounts
	err = r.db.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE s.active),
		COUNT(*) FILTER (WHERE s.active AND fi.read_at IS NULL),
		COUNT(*) FILTER (WHERE fi.starred),
		COUNT(*) FILTER (WHERE fi.read_later)
		FROM feed_items fi
		JOIN feed_subscriptions s ON s.id = fi.subscription_id`).Scan(&counts.All, &counts.Unread, &counts.Starred, &counts.Later)
	if err != nil {
		return model.FeedSubscriptionsResponse{}, fmt.Errorf("count feed smart views: %w", err)
	}
	return model.FeedSubscriptionsResponse{Folders: folders, Subscriptions: subscriptions, Counts: counts}, nil
}

func (r *PGXFeedRepository) CreateSubscription(ctx context.Context, feedURL string, folderID *uuid.UUID, setFolder bool, title string) (model.FeedSubscription, error) {
	row := r.db.QueryRow(ctx, `WITH restored AS (
			UPDATE feed_subscriptions AS subscription SET
				folder_id = CASE WHEN $4 THEN $2 ELSE subscription.folder_id END,
				canonical_url = COALESCE(subscription.canonical_url, $1),
				active = true,
				next_fetch_at = now(),
				last_error = NULL,
				failure_count = 0,
				updated_at = now()
			WHERE (
				subscription.canonical_url = $1 OR (
					subscription.canonical_url IS NULL AND subscription.url = $1
					AND NOT EXISTS (
						SELECT 1 FROM feed_subscriptions canonical
						WHERE canonical.canonical_url = $1
					)
				)
			  )
			  AND ($2::uuid IS NULL OR EXISTS (
				SELECT 1 FROM feed_folders WHERE id = $2
			  ))
			RETURNING id, COALESCE(canonical_url, url), site_url, title, description, folder_id, active,
				last_fetched_at, last_success_at, next_fetch_at, last_error,
				failure_count, COALESCE(etag, ''), COALESCE(last_modified, ''), COALESCE(sync_boundary_external_id, ''), refresh_claim_token,
				refresh_claimed_until, created_at, updated_at
		), inserted AS (
			INSERT INTO feed_subscriptions
				(folder_id, url, canonical_url, title, active, next_fetch_at)
			SELECT $2, $1, $1, $3, true, now()
			WHERE NOT EXISTS (SELECT 1 FROM restored)
			  AND ($2::uuid IS NULL OR EXISTS (
				SELECT 1 FROM feed_folders WHERE id = $2
			  ))
			ON CONFLICT (canonical_url) WHERE canonical_url IS NOT NULL DO UPDATE SET
				folder_id = CASE WHEN $4 THEN EXCLUDED.folder_id ELSE feed_subscriptions.folder_id END,
				active = true,
				next_fetch_at = now(),
				last_error = NULL,
				failure_count = 0,
				updated_at = now()
			RETURNING id, COALESCE(canonical_url, url), site_url, title, description, folder_id, active,
				last_fetched_at, last_success_at, next_fetch_at, last_error,
				failure_count, COALESCE(etag, ''), COALESCE(last_modified, ''), COALESCE(sync_boundary_external_id, ''), refresh_claim_token,
				refresh_claimed_until, created_at, updated_at
		)
		SELECT * FROM restored
		UNION ALL
		SELECT * FROM inserted
		LIMIT 1`, feedURL, folderID, title, setFolder)
	subscription, err := scanFeedSubscription(row, 0, 0)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.FeedSubscription{}, ErrNotFound
	}
	if err != nil {
		return model.FeedSubscription{}, fmt.Errorf("create feed subscription: %w", err)
	}
	return subscription, nil
}

func (r *PGXFeedRepository) GetSubscription(ctx context.Context, id uuid.UUID) (model.FeedSubscription, bool, error) {
	row := r.db.QueryRow(ctx, `SELECT `+feedSubscriptionColumns+`,
		(SELECT COUNT(*) FROM feed_items fi WHERE fi.subscription_id = s.id AND (s.active OR fi.starred OR fi.read_later)),
		(SELECT COUNT(*) FROM feed_items fi WHERE fi.subscription_id = s.id AND fi.read_at IS NULL AND s.active)
		FROM feed_subscriptions s WHERE s.id = $1`, id)
	subscription, err := scanFeedSubscriptionWithCounts(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.FeedSubscription{}, false, nil
	}
	if err != nil {
		return model.FeedSubscription{}, false, fmt.Errorf("get feed subscription: %w", err)
	}
	return subscription, true, nil
}

func (r *PGXFeedRepository) UpdateSubscription(ctx context.Context, id uuid.UUID, patch FeedSubscriptionPatch) (model.FeedSubscription, error) {
	row := r.db.QueryRow(ctx, `UPDATE feed_subscriptions s SET
		folder_id = CASE WHEN $2 THEN $3 ELSE folder_id END,
		title = CASE WHEN $4::text IS NULL THEN title ELSE $4 END,
		updated_at = now()
	WHERE s.id = $1
		AND (NOT $2 OR $3::uuid IS NULL OR EXISTS (
			SELECT 1 FROM feed_folders f WHERE f.id = $3
		))
		RETURNING id, COALESCE(canonical_url, url), site_url, title, description, folder_id, active,
		last_fetched_at, last_success_at, next_fetch_at, last_error,
		failure_count, COALESCE(etag, ''), COALESCE(last_modified, ''), COALESCE(sync_boundary_external_id, ''), refresh_claim_token,
		refresh_claimed_until, created_at, updated_at`, id, patch.SetFolder, patch.FolderID, patch.Title)
	subscription, err := scanFeedSubscription(row, 0, 0)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.FeedSubscription{}, ErrNotFound
	}
	if err != nil {
		return model.FeedSubscription{}, fmt.Errorf("update feed subscription: %w", err)
	}
	return subscription, nil
}

func (r *PGXFeedRepository) SoftDeleteSubscription(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE feed_subscriptions SET
		active = false, refresh_claim_token = NULL, refresh_claimed_until = NULL,
		updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("soft delete feed subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanFeedSubscriptionWithCounts(row interface{ Scan(...any) error }) (model.FeedSubscription, error) {
	var subscription model.FeedSubscription
	var itemCount, unreadCount int
	err := scanFeedSubscriptionInto(row, &subscription, &itemCount, &unreadCount)
	if err != nil {
		return model.FeedSubscription{}, err
	}
	finishFeedSubscription(&subscription, itemCount, unreadCount)
	return subscription, nil
}

func scanFeedSubscription(row interface{ Scan(...any) error }, itemCount, unreadCount int) (model.FeedSubscription, error) {
	var subscription model.FeedSubscription
	if err := scanFeedSubscriptionInto(row, &subscription); err != nil {
		return model.FeedSubscription{}, err
	}
	finishFeedSubscription(&subscription, itemCount, unreadCount)
	return subscription, nil
}

func scanFeedSubscriptionInto(row interface{ Scan(...any) error }, subscription *model.FeedSubscription, trailing ...any) error {
	destinations := []any{
		&subscription.ID, &subscription.URL, &subscription.SiteURL,
		&subscription.Title, &subscription.Description, &subscription.FolderID,
		&subscription.Active, &subscription.LastFetchedAt, &subscription.LastSuccessAt,
		&subscription.NextFetchAt, &subscription.LastError, &subscription.FailureCount,
		&subscription.ETag, &subscription.LastModified, &subscription.SyncBoundaryID, &subscription.RefreshClaimToken,
		&subscription.RefreshClaimUntil, &subscription.CreatedAt, &subscription.UpdatedAt,
	}
	destinations = append(destinations, trailing...)
	return row.Scan(destinations...)
}

func finishFeedSubscription(subscription *model.FeedSubscription, itemCount, unreadCount int) {
	subscription.FeedURL = subscription.URL
	subscription.ItemCount = itemCount
	subscription.UnreadCount = unreadCount
	subscription.FetchError = subscription.LastError
	subscription.Refreshing = subscription.RefreshClaimUntil != nil && subscription.RefreshClaimUntil.After(time.Now())
}
