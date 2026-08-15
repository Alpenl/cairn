package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
	"webtag/internal/alloc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/jsonx"
	"webtag/internal/model"
)

const retainedOrdinaryFeedItems = 100

const (
	feedEndBoundary       = "webtag:internal:end-of-feed:v1"
	maxFeedItemsProcessed = 1000
)

func (r *PGXFeedRepository) ScheduleRefresh(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE feed_subscriptions SET
		next_fetch_at = LEAST(next_fetch_at, now()), updated_at = now()
		WHERE id = $1 AND active`, id)
	if err != nil {
		return fmt.Errorf("schedule feed refresh: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PGXFeedRepository) ScheduleAllRefreshes(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `UPDATE feed_subscriptions SET
		next_fetch_at = LEAST(next_fetch_at, now()), updated_at = now()
		WHERE active`)
	if err != nil {
		return 0, fmt.Errorf("schedule all feed refreshes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ClaimDue atomically leases installation-owned rows with SKIP LOCKED.
func (r *PGXFeedRepository) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]model.FeedSubscription, error) {
	if r == nil || r.scheduler == nil {
		return nil, fmt.Errorf("feed scheduler database is not configured")
	}
	tx, err := r.scheduler.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin feed scheduler claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	leaseSeconds := int(lease.Seconds())
	if leaseSeconds < 1 {
		leaseSeconds = 120
	}
	// 只挡负值，**不动 0**：`LIMIT 0` 是合法的 no-op（认领 0 行），而
	// `limit < 1 → 1` 会把「什么都别做」静默变成「认领并租约锁住 1 条订阅」
	// ——ClaimDue 是 UPDATE ... RETURNING，那是真写操作。
	//
	// 注意 make 的 panic 风险已由下面的 alloc.Hint 消除（Hint(负数)==0），
	// 这里挡的是 Postgres 侧：负 LIMIT 会直接报错。
	if limit < 0 {
		limit = 0
	}
	rows, err := tx.Query(ctx, `WITH due AS (
		SELECT id FROM feed_subscriptions
		WHERE active AND next_fetch_at <= now()
			AND (refresh_claimed_until IS NULL OR refresh_claimed_until <= now())
		ORDER BY next_fetch_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT $1
	)
	UPDATE feed_subscriptions s SET
		refresh_claim_token = gen_random_uuid(),
		refresh_claimed_until = now() + make_interval(secs => $2)
	FROM due WHERE s.id = due.id
		RETURNING s.id, COALESCE(s.canonical_url, s.url), s.site_url, s.title, s.description,
		s.folder_id, s.active, s.last_fetched_at, s.last_success_at,
		s.next_fetch_at, s.last_error, s.failure_count, COALESCE(s.etag, ''),
		COALESCE(s.last_modified, ''), COALESCE(s.sync_boundary_external_id, ''), s.refresh_claim_token, s.refresh_claimed_until,
		s.created_at, s.updated_at`, limit, leaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("claim due feed subscriptions: %w", err)
	}
	claimed := make([]model.FeedSubscription, 0, alloc.Hint(limit))
	for rows.Next() {
		var subscription model.FeedSubscription
		if err := rows.Scan(&subscription.ID, &subscription.URL, &subscription.SiteURL, &subscription.Title,
			&subscription.Description, &subscription.FolderID, &subscription.Active,
			&subscription.LastFetchedAt, &subscription.LastSuccessAt,
			&subscription.NextFetchAt, &subscription.LastError,
			&subscription.FailureCount, &subscription.ETag,
			&subscription.LastModified, &subscription.SyncBoundaryID, &subscription.RefreshClaimToken,
			&subscription.RefreshClaimUntil, &subscription.CreatedAt,
			&subscription.UpdatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claimed feed subscription: %w", err)
		}
		subscription.FeedURL = subscription.URL
		subscription.FetchError = subscription.LastError
		subscription.Refreshing = true
		claimed = append(claimed, subscription)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate claimed feed subscriptions: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit feed scheduler claim: %w", err)
	}
	return claimed, nil
}

func (r *PGXFeedRepository) CompleteRefresh(ctx context.Context, success FeedRefreshSuccess) (int, error) {
	inserted := 0
	err := database.WithTx(ctx, r.tx, func(tx pgx.Tx) error {
		if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
			return err
		}
		var txErr error
		inserted, txErr = completeFeedRefreshTx(ctx, tx, success)
		return txErr
	})
	return inserted, err
}

type feedRefreshState struct {
	etag         string
	lastModified string
	boundary     string
}

func completeFeedRefreshTx(ctx context.Context, tx pgx.Tx, success FeedRefreshSuccess) (int, error) {
	state, err := lockFeedRefreshState(ctx, tx, success)
	if err != nil {
		return 0, err
	}
	inserted, boundary, err := persistFeedRefreshItems(ctx, tx, success, state.boundary)
	if err != nil {
		return 0, err
	}
	if err := trimOrdinaryFeedItems(ctx, tx, success.SubscriptionID); err != nil {
		return inserted, err
	}
	if err := finalizeFeedRefresh(ctx, tx, success, state, boundary); err != nil {
		return inserted, err
	}
	return inserted, nil
}

func lockFeedRefreshState(ctx context.Context, tx pgx.Tx, success FeedRefreshSuccess) (feedRefreshState, error) {
	var state feedRefreshState
	err := tx.QueryRow(ctx, `SELECT COALESCE(etag, ''), COALESCE(last_modified, ''),
		COALESCE(sync_boundary_external_id, '')
		FROM feed_subscriptions
		WHERE id = $1 AND refresh_claim_token = $2
		FOR UPDATE`, success.SubscriptionID, success.ClaimToken).
		Scan(&state.etag, &state.lastModified, &state.boundary)
	if errors.Is(err, pgx.ErrNoRows) {
		return feedRefreshState{}, ErrFeedRefreshClaimLost
	}
	if err != nil {
		return feedRefreshState{}, fmt.Errorf("lock feed refresh claim: %w", err)
	}
	return state, nil
}

func persistFeedRefreshItems(ctx context.Context, tx pgx.Tx, success FeedRefreshSuccess, boundary string) (int, string, error) {
	if success.NotModified {
		return 0, boundary, nil
	}
	feedItems := success.Parsed.Items
	if len(feedItems) > maxFeedItemsProcessed {
		feedItems = feedItems[:maxFeedItemsProcessed]
	}
	known, err := knownFeedExternalIDs(ctx, tx, success.SubscriptionID, feedItems)
	if err != nil {
		return 0, boundary, err
	}
	selected, nextBoundary := selectFeedRefreshItems(feedItems, known, boundary, success.Initial)
	upserts := feedRefreshUpserts(feedItems, selected, known)
	if err := upsertFeedItems(ctx, tx, success.SubscriptionID, upserts); err != nil {
		return 0, boundary, err
	}
	return len(selected), nextBoundary, nil
}

// Existing entries are refreshed from publisher edits but do not count
// against the new-item budget. Unknown rows after the historical boundary are
// intentionally ignored to avoid backfilling old history.
func feedRefreshUpserts(feedItems, selected []model.FeedItem, known map[string]struct{}) []model.FeedItem {
	upserts := append([]model.FeedItem(nil), selected...)
	seen := make(map[string]struct{}, len(feedItems))
	for _, item := range selected {
		seen[item.ExternalID] = struct{}{}
	}
	for _, item := range feedItems {
		if _, exists := known[item.ExternalID]; !exists {
			continue
		}
		if _, duplicate := seen[item.ExternalID]; duplicate {
			continue
		}
		seen[item.ExternalID] = struct{}{}
		upserts = append(upserts, item)
	}
	return upserts
}

// Run retention on every successful refresh, including 304. An item that was
// protected while unread/starred/later is trimmed on the next success after
// the user clears that state.
func trimOrdinaryFeedItems(ctx context.Context, tx pgx.Tx, subscriptionID uuid.UUID) error {
	// reader_feed_saves 的外键是 ON DELETE CASCADE，所以裁掉一条仍被保存的 item
	// 会连同 save 一起消失，把 Link 变成"feed_managed 却无 save"的伪孤儿。保存路径
	// 已经回填 feed_items.link_id，这里再显式排除有 save 的行作为纵深防御：即便将来
	// 某条写入路径漏了回填，保留策略也不会吃掉用户保存的内容。
	_, err := tx.Exec(ctx, `DELETE FROM feed_items fi
		WHERE fi.subscription_id = $1
			AND fi.read_at IS NOT NULL AND NOT fi.starred
			AND NOT fi.read_later AND fi.link_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM reader_feed_saves s WHERE s.feed_item_id = fi.id)
			AND fi.id NOT IN (
				SELECT kept.id FROM feed_items kept
				WHERE kept.subscription_id = $1
					AND kept.read_at IS NOT NULL AND NOT kept.starred
					AND NOT kept.read_later AND kept.link_id IS NULL
					AND NOT EXISTS (SELECT 1 FROM reader_feed_saves s WHERE s.feed_item_id = kept.id)
				ORDER BY COALESCE(kept.published_at, kept.created_at) DESC, kept.id DESC
				LIMIT $2
			)`, subscriptionID, retainedOrdinaryFeedItems)
	if err != nil {
		return fmt.Errorf("trim old feed items: %w", err)
	}
	return nil
}

func finalizeFeedRefresh(ctx context.Context, tx pgx.Tx, success FeedRefreshSuccess, state feedRefreshState, boundary string) error {
	etag := firstNonEmpty(success.ETag, state.etag)
	lastModified := firstNonEmpty(success.LastModified, state.lastModified)
	nextFetch := success.Now.Add(time.Hour)
	command, err := tx.Exec(ctx, `UPDATE feed_subscriptions SET
		site_url = CASE WHEN $3 = '' THEN site_url ELSE $3 END,
		title = CASE WHEN $4 = '' THEN title ELSE $4 END,
		description = CASE WHEN $5 = '' THEN description ELSE $5 END,
		etag = NULLIF($6, ''), last_modified = NULLIF($7, ''),
		last_fetched_at = $8, last_success_at = $8,
		next_fetch_at = $9, last_error = NULL, failure_count = 0,
		sync_boundary_external_id = CASE WHEN $10 = '' THEN sync_boundary_external_id ELSE $10 END,
		refresh_claim_token = NULL, refresh_claimed_until = NULL,
		updated_at = $8
		WHERE id = $1 AND refresh_claim_token = $2`,
		success.SubscriptionID, success.ClaimToken,
		success.Parsed.SiteURL, success.Parsed.Title, success.Parsed.Description,
		etag, lastModified, success.Now, nextFetch, boundary)
	if err != nil {
		return fmt.Errorf("complete feed refresh: %w", err)
	}
	if command.RowsAffected() == 0 {
		return ErrFeedRefreshClaimLost
	}
	return nil
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

type feedItemUpsert struct {
	ExternalID  string     `json:"external_id"`
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	Author      *string    `json:"author"`
	Summary     *string    `json:"summary"`
	Content     *string    `json:"content_text"`
	ContentHTML *string    `json:"content_html"`
	PublishedAt *time.Time `json:"published_at"`
}

func upsertFeedItems(ctx context.Context, tx pgx.Tx, subscriptionID uuid.UUID, items []model.FeedItem) error {
	if len(items) == 0 {
		return nil
	}
	payload := make([]feedItemUpsert, 0, len(items))
	for _, item := range items {
		payload = append(payload, feedItemUpsert{
			ExternalID: item.ExternalID, URL: item.URL, Title: item.Title,
			Author: item.Author, Summary: item.Summary, Content: item.Content,
			ContentHTML: item.ContentHTML, PublishedAt: item.PublishedAt,
		})
	}
	encoded, err := jsonx.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode feed item batch: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO feed_items
		(subscription_id, external_id, url, title, author,
		 summary, content_text, content_html, published_at)
		SELECT $1, item.external_id, item.url, item.title, item.author,
			item.summary, item.content_text, item.content_html, item.published_at
		FROM jsonb_to_recordset($2::jsonb) AS item(
			external_id text, url text, title text, author text, summary text,
			content_text text, content_html text, published_at timestamptz)
		ON CONFLICT (subscription_id, external_id) DO UPDATE SET
			url = EXCLUDED.url, title = EXCLUDED.title,
			author = EXCLUDED.author, summary = EXCLUDED.summary,
			content_text = EXCLUDED.content_text,
			content_html = EXCLUDED.content_html,
			published_at = EXCLUDED.published_at,
			updated_at = now()`, subscriptionID, string(encoded))
	if err != nil {
		return fmt.Errorf("batch upsert feed items: %w", err)
	}
	return nil
}

func knownFeedExternalIDs(ctx context.Context, tx pgx.Tx, subscriptionID uuid.UUID, items []model.FeedItem) (map[string]struct{}, error) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.ExternalID != "" {
			ids = append(ids, item.ExternalID)
		}
	}
	known := make(map[string]struct{})
	if len(ids) == 0 {
		return known, nil
	}
	rows, err := tx.Query(ctx, `SELECT external_id FROM feed_items
		WHERE subscription_id = $1 AND external_id = ANY($2::text[])`, subscriptionID, ids)
	if err != nil {
		return nil, fmt.Errorf("query known feed item identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan known feed item identity: %w", err)
		}
		known[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate known feed item identities: %w", err)
	}
	return known, nil
}

// selectFeedRefreshItems maintains a durable historical boundary. Initial
// sync imports 50 and pins the feed head. Later syncs consume only unseen
// entries before that boundary. If more than 100 arrived, the old boundary is
// retained so subsequent refreshes drain the remainder instead of mistaking
// the newly inserted head for the end of new work.
func selectFeedRefreshItems(items []model.FeedItem, known map[string]struct{}, boundary string, initial bool) ([]model.FeedItem, string) {
	if len(items) == 0 {
		if initial && boundary == "" {
			return nil, feedEndBoundary
		}
		return nil, boundary
	}
	head := items[0].ExternalID
	if initial {
		return selectInitialFeedItems(items), head
	}
	boundary = resolveFeedBoundary(items, known, boundary)
	unseen := unseenFeedItems(items, known, boundary)
	if len(unseen) > 100 {
		return unseen[:100], boundary
	}
	// We consumed every unseen item in the current window. Advance the durable
	// boundary whether the old marker was reached or has fallen out of a
	// truncated feed; on the latter path every currently visible item is known.
	return unseen, head
}

func selectInitialFeedItems(items []model.FeedItem) []model.FeedItem {
	selected := make([]model.FeedItem, 0, min(len(items), 50))
	seen := make(map[string]struct{}, cap(selected))
	for _, item := range items {
		if _, duplicate := seen[item.ExternalID]; duplicate {
			continue
		}
		seen[item.ExternalID] = struct{}{}
		selected = append(selected, item)
		if len(selected) == 50 {
			break
		}
	}
	return selected
}

func resolveFeedBoundary(items []model.FeedItem, known map[string]struct{}, boundary string) string {
	if boundary != "" {
		return boundary
	}
	for _, item := range items {
		if _, exists := known[item.ExternalID]; exists {
			return item.ExternalID
		}
	}
	return feedEndBoundary
}

func unseenFeedItems(items []model.FeedItem, known map[string]struct{}, boundary string) []model.FeedItem {
	unseen := make([]model.FeedItem, 0, min(len(items), 100))
	seen := make(map[string]struct{}, cap(unseen))
	for _, item := range items {
		if item.ExternalID == boundary {
			break
		}
		if _, exists := known[item.ExternalID]; exists {
			continue
		}
		if _, duplicate := seen[item.ExternalID]; duplicate {
			continue
		}
		seen[item.ExternalID] = struct{}{}
		unseen = append(unseen, item)
	}
	return unseen
}

func (r *PGXFeedRepository) FailRefresh(ctx context.Context, subscriptionID, claimToken uuid.UUID, now, nextFetch time.Time, publicError string) error {
	tag, err := r.db.Exec(ctx, `UPDATE feed_subscriptions SET
		last_fetched_at = $3, next_fetch_at = $4,
		last_error = $5, failure_count = failure_count + 1,
		refresh_claim_token = NULL, refresh_claimed_until = NULL,
		updated_at = $3
		WHERE id = $1 AND refresh_claim_token = $2`,
		subscriptionID, claimToken, now, nextFetch, publicError)
	if err != nil {
		return fmt.Errorf("record feed refresh failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFeedRefreshClaimLost
	}
	return nil
}
