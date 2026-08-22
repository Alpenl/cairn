package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/urlidentity"
)

func scanReaderEngagement(row readerScanner) (*model.ReaderEngagement, error) {
	var out model.ReaderEngagement
	var lastOpened pgtype.Timestamptz
	if err := row.Scan(&out.LinkID, &out.Read, &out.Progress, &out.ReadLater, &lastOpened, &out.UpdatedAt); err != nil {
		return nil, err
	}
	if lastOpened.Valid {
		value := lastOpened.Time
		out.LastOpened = &value
	}
	return &out, nil
}

func (r *PGXReaderVNextRepository) GetEngagement(ctx context.Context, linkID uuid.UUID) (*model.ReaderEngagement, error) {
	var linkExists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, linkID).Scan(&linkExists); err != nil {
		return nil, fmt.Errorf("check engagement link: %w", err)
	}
	if !linkExists {
		return nil, ErrNotFound
	}
	item, err := scanReaderEngagement(r.db.QueryRow(ctx, `SELECT link_id,read,progress,read_later,last_opened,updated_at FROM reader_engagement WHERE link_id=$1`, linkID))
	if errors.Is(err, pgx.ErrNoRows) {
		return &model.ReaderEngagement{LinkID: linkID, Progress: 0}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get engagement: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) PatchEngagement(ctx context.Context, patch model.ReaderEngagementPatch) (*model.ReaderEngagement, error) {
	var linkExists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, patch.LinkID).Scan(&linkExists); err != nil {
		return nil, fmt.Errorf("check engagement link: %w", err)
	}
	if !linkExists {
		return nil, ErrNotFound
	}
	item, err := scanReaderEngagement(r.db.QueryRow(ctx, `
		INSERT INTO reader_engagement (link_id,read,progress,read_later,last_opened)
		VALUES ($1,COALESCE($2::boolean,false),COALESCE($3::real,0),COALESCE($4::boolean,false),CASE WHEN $2::boolean IS TRUE OR $3::real IS NOT NULL THEN NOW() ELSE NULL END)
		ON CONFLICT (link_id) DO UPDATE SET
			read=COALESCE($2::boolean,reader_engagement.read),progress=COALESCE($3::real,reader_engagement.progress),
			read_later=COALESCE($4::boolean,reader_engagement.read_later),
			last_opened=CASE WHEN $2::boolean IS TRUE OR $3::real IS NOT NULL THEN NOW() ELSE reader_engagement.last_opened END,
			updated_at=NOW()
		RETURNING link_id,read,progress,read_later,last_opened,updated_at`, patch.LinkID, patch.Read, patch.Progress, patch.ReadLater))
	if err != nil {
		return nil, fmt.Errorf("patch engagement: %w", err)
	}
	return item, nil
}

func (r *PGXReaderVNextRepository) HomeCounts(ctx context.Context) (map[string]int, error) {
	counts, err := homeCountsOn(ctx, r.db)
	if err != nil {
		return nil, fmt.Errorf("home counts: %w", err)
	}
	return counts, nil
}

type readerFeedCursor struct {
	Mode        string
	Sources     []string
	Score       int
	CreatedAt   time.Time
	EventAt     time.Time
	ResourceKey string
	Key         string
}

type readerFeedCursorWire struct {
	Version     int      `json:"version"`
	Mode        string   `json:"mode"`
	Sources     []string `json:"sources"`
	Score       *int     `json:"score,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	EventAt     string   `json:"event_at,omitempty"`
	ResourceKey string   `json:"resource_key,omitempty"`
	Key         string   `json:"key"`
}

func normalizeRepositoryFeedSources(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			source := strings.ToLower(strings.TrimSpace(part))
			if source == "" {
				continue
			}
			switch source {
			case "saved":
				source = "reading"
			case "pending":
				source = "inbox"
			case "reading", "inbox", "subscription":
			default:
				return nil, fmt.Errorf("%w: invalid feed source", ErrInvalidReaderCursor)
			}
			seen[source] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
}

func sameRepositoryFeedSources(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hasRepositoryFeedSource(sources []string, source string) bool {
	return len(sources) == 0 || slices.Contains(sources, source)
}

func feedCursor(raw string) (readerFeedCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return readerFeedCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor", ErrInvalidReaderCursor)
	}
	var wire readerFeedCursorWire
	if err := json.Unmarshal(decoded, &wire); err != nil || wire.Version != 1 {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor", ErrInvalidReaderCursor)
	}
	if wire.Mode != "recommended" && wire.Mode != "chronological" {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor mode", ErrInvalidReaderCursor)
	}
	sources, err := normalizeRepositoryFeedSources(wire.Sources)
	if err != nil || !sameRepositoryFeedSources(sources, wire.Sources) {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor sources", ErrInvalidReaderCursor)
	}
	if strings.TrimSpace(wire.Key) == "" || strings.TrimSpace(wire.Key) != wire.Key {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid feed cursor key", ErrInvalidReaderCursor)
	}
	cursor := readerFeedCursor{Mode: wire.Mode, Sources: sources, Key: wire.Key}
	return feedCursorPosition(wire, cursor)
}

func feedCursorPosition(wire readerFeedCursorWire, cursor readerFeedCursor) (readerFeedCursor, error) {
	if wire.Mode == "recommended" {
		if wire.Score == nil {
			return readerFeedCursor{}, fmt.Errorf("%w: invalid recommended feed cursor", ErrInvalidReaderCursor)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
		if err != nil {
			return readerFeedCursor{}, fmt.Errorf("%w: invalid recommended feed cursor", ErrInvalidReaderCursor)
		}
		cursor.Score = *wire.Score
		cursor.CreatedAt = createdAt
		return cursor, nil
	}
	eventAt, err := time.Parse(time.RFC3339Nano, wire.EventAt)
	if err != nil || strings.TrimSpace(wire.ResourceKey) == "" {
		return readerFeedCursor{}, fmt.Errorf("%w: invalid chronological feed cursor", ErrInvalidReaderCursor)
	}
	cursor.EventAt = eventAt
	cursor.ResourceKey = wire.ResourceKey
	return cursor, nil
}

func makeFeedCursor(mode string, sources []string, item model.ReaderFeedItem) string {
	wire := readerFeedCursorWire{
		Version: 1,
		Mode:    mode,
		Sources: append([]string{}, sources...),
		Key:     item.Key,
	}
	if mode == "chronological" {
		wire.EventAt = item.VisibleEventAt().Format(time.RFC3339Nano)
		wire.ResourceKey = item.ResourceIdentity()
	} else {
		score := item.Score
		wire.Score = &score
		wire.CreatedAt = item.CreatedAt.Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// sortReaderFeedItems orders the live merged set by the exact tuple encoded in
// its cursor. Key is the final tie-breaker in both modes.
func sortReaderFeedItems(items []model.ReaderFeedItem, mode string) {
	if mode == "chronological" {
		sort.SliceStable(items, func(i, j int) bool {
			leftEventAt, rightEventAt := items[i].VisibleEventAt(), items[j].VisibleEventAt()
			if !leftEventAt.Equal(rightEventAt) {
				return leftEventAt.After(rightEventAt)
			}
			leftResource, rightResource := items[i].ResourceIdentity(), items[j].ResourceIdentity()
			if leftResource != rightResource {
				return leftResource < rightResource
			}
			return items[i].Key < items[j].Key
		})
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].Key < items[j].Key
	})
}

func readerFeedItemAfterCursor(item model.ReaderFeedItem, cursor readerFeedCursor) bool {
	if cursor.Mode == "chronological" {
		eventAt := item.VisibleEventAt()
		if !eventAt.Equal(cursor.EventAt) {
			return eventAt.Before(cursor.EventAt)
		}
		resourceKey := item.ResourceIdentity()
		if resourceKey != cursor.ResourceKey {
			return resourceKey > cursor.ResourceKey
		}
		return item.Key > cursor.Key
	}
	if item.Score != cursor.Score {
		return item.Score < cursor.Score
	}
	if !item.CreatedAt.Equal(cursor.CreatedAt) {
		return item.CreatedAt.Before(cursor.CreatedAt)
	}
	return item.Key > cursor.Key
}

func readerFeedPage(items []model.ReaderFeedItem, mode string, sources []string, cursor readerFeedCursor, limit int) *model.ReaderFeedPage {
	start := 0
	if cursor.Mode != "" {
		start = sort.Search(len(items), func(index int) bool {
			return readerFeedItemAfterCursor(items[index], cursor)
		})
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = makeFeedCursor(mode, sources, items[end-1])
	}
	return &model.ReaderFeedPage{
		Items:      append([]model.ReaderFeedItem(nil), items[start:end]...),
		NextCursor: next,
		Mode:       mode,
	}
}

func (r *PGXReaderVNextRepository) ListFeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (*model.ReaderFeedPage, error) {
	if mode != "" && mode != "recommended" && mode != "chronological" {
		return nil, fmt.Errorf("%w: invalid feed mode", ErrInvalidReaderCursor)
	}
	mode = modeOrDefault(mode)
	normalizedSources, err := normalizeRepositoryFeedSources(sources)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	cursor, err := feedCursor(after)
	if err != nil {
		return nil, err
	}
	if cursor.Mode != "" && (cursor.Mode != mode || !sameRepositoryFeedSources(cursor.Sources, normalizedSources)) {
		return nil, fmt.Errorf("%w: feed cursor parameters changed", ErrInvalidReaderCursor)
	}
	items, err := r.buildFeedItemsForMode(ctx, mode, normalizedSources)
	if err != nil {
		return nil, err
	}
	items = scoreReaderFeedItems(items)
	sortReaderFeedItems(items, mode)
	return readerFeedPage(items, mode, normalizedSources, cursor, limit), nil
}

func modeOrDefault(mode string) string {
	if mode == "chronological" {
		return "chronological"
	}
	return "recommended"
}

func (r *PGXReaderVNextRepository) buildFeedItemsForMode(ctx context.Context, mode string, sourceFilters ...[]string) ([]model.ReaderFeedItem, error) {
	chronological := mode == "chronological"
	var sources []string
	if len(sourceFilters) > 0 {
		sources = sourceFilters[0]
	}
	items := make([]model.ReaderFeedItem, 0, 120)
	if hasRepositoryFeedSource(sources, "reading") {
		appended, err := r.appendReadingFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	if hasRepositoryFeedSource(sources, "inbox") {
		appended, err := r.appendInboxFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	if hasRepositoryFeedSource(sources, "subscription") {
		appended, err := r.appendSubscriptionFeedItems(ctx, items, chronological)
		if err != nil {
			return nil, err
		}
		items = appended
	}
	// A URL may be present both in the saved library and in RSS. Keep the first
	// source in the merge order, so Reading remains the canonical card.
	seenItems := make(map[string]struct{}, len(items))
	out := make([]model.ReaderFeedItem, 0, len(items))
	for _, item := range items {
		dedupeKey := item.DedupeIdentity()
		if dedupeKey != "" {
			if _, ok := seenItems[dedupeKey]; ok {
				continue
			}
			seenItems[dedupeKey] = struct{}{}
		}
		out = append(out, item)
	}
	return out, nil
}

// appendReadingFeedItems collects the saved-library slice of the feed. Hidden
// rows are filtered in SQL before ranking and dedupe.
func (r *PGXReaderVNextRepository) appendReadingFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "l.created_at DESC,l.id DESC"
	if chronological {
		orderBy = "l.created_at DESC,l.id ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT l.id,l.url,COALESCE(l.title,''),COALESCE(l.summary,''),l.created_at,
			COALESCE(e.read,false),COALESCE(e.read_later,false)
		FROM links l LEFT JOIN reader_engagement e ON e.link_id=l.id
		LEFT JOIN reader_feed_hides hidden ON hidden.item_key='link:'||l.id::text
		WHERE l.status='done' AND l.deleted_at IS NULL AND COALESCE(l.library_kind,'reading')='reading' AND hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 1000`)
	if err != nil {
		return nil, fmt.Errorf("feed links: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		if err := rows.Scan(&id, &item.URL, &item.Title, &item.Summary, &item.CreatedAt, &item.Read, &item.ReadLater); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "link:" + id.String()
		item.Source = "reading"
		item.LinkID = &id
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// appendInboxFeedItems collects the still-pending captures. Only pending rows
// belong in the feed: confirmed ones already surface through the reading slice
// and discarded ones must stay gone.
func (r *PGXReaderVNextRepository) appendInboxFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "inbox.created_at DESC,inbox.id DESC"
	if chronological {
		orderBy = "inbox.created_at DESC,inbox.id ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT inbox.id,inbox.url,COALESCE(inbox.title,''),COALESCE(inbox.summary,''),inbox.created_at
		FROM reader_inbox inbox
		LEFT JOIN reader_feed_hides hidden ON hidden.item_key='inbox:'||inbox.id::text
		WHERE inbox.status='pending' AND inbox.deleted_at IS NULL
			AND (inbox.expires_at IS NULL OR inbox.expires_at > NOW())
			AND hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 200`)
	if err != nil {
		return nil, fmt.Errorf("feed inbox: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		if err := rows.Scan(&id, &item.URL, &item.Title, &item.Summary, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "inbox:" + id.String()
		item.Source = "inbox"
		item.InboxID = &id
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// appendSubscriptionFeedItems collects the RSS slice. A feed item that was
// already saved carries its link id forward so the later dedupe pass can
// recognise it as the same resource as the saved-library entry.
func (r *PGXReaderVNextRepository) appendSubscriptionFeedItems(ctx context.Context, items []model.ReaderFeedItem, chronological bool) ([]model.ReaderFeedItem, error) {
	orderBy := "COALESCE(fi.published_at,fi.created_at) DESC,fi.id DESC"
	if chronological {
		orderBy = "COALESCE(fi.published_at,fi.created_at) DESC,CASE WHEN COALESCE(fs.link_id,fi.link_id) IS NOT NULL THEN 'link:'||COALESCE(fs.link_id,fi.link_id)::text ELSE 'feed_item:'||fi.id::text END ASC"
	}
	rows, err := r.db.Query(ctx, `
		SELECT fi.id,COALESCE(fs.link_id,fi.link_id),fi.url,COALESCE(fi.title,''),COALESCE(fi.summary,''),fi.published_at,
			(fi.read_at IS NOT NULL),fi.read_later,(fs.feed_item_id IS NOT NULL),fi.created_at
		FROM feed_items fi LEFT JOIN reader_feed_hides hidden ON hidden.item_key='subscription:'||fi.id::text
		LEFT JOIN reader_feed_saves fs ON fs.feed_item_id=fi.id
		WHERE hidden.item_key IS NULL
		ORDER BY `+orderBy+` LIMIT 1000`)
	if err != nil {
		return nil, fmt.Errorf("feed subscription items: %w", err)
	}
	for rows.Next() {
		var item model.ReaderFeedItem
		var id uuid.UUID
		var linkedID pgtype.UUID
		var published pgtype.Timestamptz
		if err := rows.Scan(&id, &linkedID, &item.URL, &item.Title, &item.Summary, &published, &item.Read, &item.ReadLater, &item.Saved, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		item.Key = "subscription:" + id.String()
		item.Source = "subscription"
		item.FeedItemID = &id
		if linkedID.Valid {
			value := uuid.UUID(linkedID.Bytes)
			item.LinkID = &value
		}
		if published.Valid {
			publishedAt := published.Time
			item.PublishedAt = &publishedAt
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *PGXReaderVNextRepository) FeedbackFeed(ctx context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	return r.feedbackFeed(ctx, itemKey, action, nil)
}

// FeedbackFeedTx applies feed feedback in a caller-owned transaction. The
// lifecycle callback is used only when a save restores, or an unsave trashes,
// a Link whose durable work must change atomically with the feed association.
func (r *PGXReaderVNextRepository) FeedbackFeedTx(
	ctx context.Context,
	tx pgx.Tx,
	itemKey string,
	action string,
	lifecycle ReaderLinkLifecycle,
) (model.ReaderFeedFeedback, error) {
	return r.feedbackFeedOn(ctx, tx, itemKey, action, lifecycle)
}

func (r *PGXReaderVNextRepository) feedbackFeed(
	ctx context.Context,
	itemKey string,
	action string,
	lifecycle ReaderLinkLifecycle,
) (model.ReaderFeedFeedback, error) {
	itemKey = strings.TrimSpace(itemKey)
	kind, _, err := parseReaderFeedItemKey(itemKey)
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	if err := validateReaderFeedAction(kind, action); err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	var result model.ReaderFeedFeedback
	err = r.withTx(ctx, func(db database.Querier) error {
		var err error
		result, err = r.feedbackFeedOn(ctx, db, itemKey, action, lifecycle)
		return err
	})
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	return result, nil
}

func (r *PGXReaderVNextRepository) feedbackFeedOn(
	ctx context.Context,
	db database.Querier,
	itemKey string,
	action string,
	lifecycle ReaderLinkLifecycle,
) (model.ReaderFeedFeedback, error) {
	itemKey = strings.TrimSpace(itemKey)
	kind, id, err := parseReaderFeedItemKey(itemKey)
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	if err := validateReaderFeedAction(kind, action); err != nil {
		return model.ReaderFeedFeedback{}, err
	}

	result := model.ReaderFeedFeedback{ItemKey: itemKey, Action: action}
	if err := ensureReaderFeedItem(ctx, db, kind, id); err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	if action == "save" && kind == "subscription" {
		linkID, err := r.saveSubscriptionFeedItem(ctx, db, id, lifecycle)
		if err != nil {
			return model.ReaderFeedFeedback{}, err
		}
		result.LinkID = &linkID
	}
	if action == "unsave" && kind == "subscription" {
		linkID, err := r.unsaveSubscriptionFeedItem(ctx, db, id, lifecycle)
		if err != nil {
			return model.ReaderFeedFeedback{}, err
		}
		result.LinkID = linkID
	}
	if action == "hide" {
		if _, err := db.Exec(ctx, `INSERT INTO reader_feed_hides (item_key) VALUES ($1) ON CONFLICT (item_key) DO UPDATE SET created_at=NOW()`, itemKey); err != nil {
			return model.ReaderFeedFeedback{}, fmt.Errorf("hide reader feed item: %w", err)
		}
	} else if _, err := db.Exec(ctx, `DELETE FROM reader_feed_hides WHERE item_key=$1`, itemKey); err != nil {
		return model.ReaderFeedFeedback{}, fmt.Errorf("clear reader feed hide: %w", err)
	}
	return result, nil
}

// saveSubscriptionFeedItem atomically reuses the canonical URL identity or
// creates one Feed-managed reading link, then records the save association.
func (r *PGXReaderVNextRepository) saveSubscriptionFeedItem(
	ctx context.Context,
	db database.Querier,
	feedItemID uuid.UUID,
	lifecycle ReaderLinkLifecycle,
) (uuid.UUID, error) {
	var url, title, summary string
	if err := db.QueryRow(ctx, `SELECT url,COALESCE(title,''),COALESCE(summary,'') FROM feed_items WHERE id=$1 FOR UPDATE`, feedItemID).Scan(&url, &title, &summary); err != nil {
		return uuid.Nil, ErrNotFound
	}
	identity, err := urlidentity.Normalize(url)
	if err != nil {
		return uuid.Nil, ErrInvalidReaderFeedItem
	}
	// All writers that create a link for this canonical identity share this
	// transaction lock, including distinct feed items saved concurrently.
	if err := lockCanonicalLinkIdentity(ctx, db, identity); err != nil {
		return uuid.Nil, err
	}
	var existingLinkID uuid.UUID
	err = db.QueryRow(ctx, `SELECT link_id FROM reader_feed_saves WHERE feed_item_id=$1`, feedItemID).Scan(&existingLinkID)
	if err == nil {
		if _, err := r.restoreLinkLifecycleOn(ctx, db, existingLinkID, lifecycle); err != nil {
			return uuid.Nil, err
		}
		// The association may have disappeared while this save waited for the
		// common Link lock behind a concurrent unsave.
		err = db.QueryRow(ctx, `SELECT link_id FROM reader_feed_saves WHERE feed_item_id=$1`, feedItemID).Scan(&existingLinkID)
		if err == nil {
			return existingLinkID, nil
		}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("read feed save association: %w", err)
	}
	matched, err := findCanonicalLink(ctx, db, identity)
	if err != nil {
		return uuid.Nil, err
	}
	var linkID uuid.UUID
	if matched == nil {
		err = db.QueryRow(ctx, `INSERT INTO links (url,source_kind,source_key,input_title,title,summary,tags,status,content,content_document,content_format,content_source,content_revision,library_kind,library_kind_locked,feed_managed,first_collected_at,created_at,updated_at) VALUES ($1,'subscription',$2,$3,$3,$4,'{}','done','',NULL,'markdown','user',1,'reading',true,true,NOW(),NOW(),NOW()) RETURNING id`, url, identity, title, summary).Scan(&linkID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("create subscription saved link: %w", err)
		}
	} else {
		linkID = *matched
		if _, err := r.restoreLinkLifecycleOn(ctx, db, linkID, lifecycle); err != nil {
			return uuid.Nil, err
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO reader_feed_saves (feed_item_id,link_id) VALUES ($1,$2)`, feedItemID, linkID); err != nil {
		return uuid.Nil, fmt.Errorf("write feed save association: %w", err)
	}
	return linkID, nil
}

func (r *PGXReaderVNextRepository) unsaveSubscriptionFeedItem(
	ctx context.Context,
	db database.Querier,
	feedItemID uuid.UUID,
	lifecycle ReaderLinkLifecycle,
) (*uuid.UUID, error) {
	var savedLink, analyzedLink pgtype.UUID
	err := db.QueryRow(ctx, `SELECT save.link_id,item.link_id
		FROM feed_items item LEFT JOIN reader_feed_saves save ON save.feed_item_id=item.id
		WHERE item.id=$1`, feedItemID).Scan(&savedLink, &analyzedLink)
	if err != nil {
		return nil, err
	}
	var visibleLinkID *uuid.UUID
	if analyzedLink.Valid {
		value := uuid.UUID(analyzedLink.Bytes)
		visibleLinkID = &value
	}
	if !savedLink.Valid {
		return visibleLinkID, nil
	}
	linkID := uuid.UUID(savedLink.Bytes)
	var (
		feedManaged bool
		linkStatus  model.LinkStatus
	)
	if err := db.QueryRow(ctx, `SELECT feed_managed,status FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&feedManaged, &linkStatus); err != nil {
		return nil, fmt.Errorf("lock feed save link: %w", err)
	}
	tag, err := db.Exec(ctx, `DELETE FROM reader_feed_saves WHERE feed_item_id=$1 AND link_id=$2`, feedItemID, linkID)
	if err != nil {
		return nil, fmt.Errorf("delete feed save association: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	var remaining bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reader_feed_saves WHERE link_id=$1)`, linkID).Scan(&remaining); err != nil {
		return nil, err
	}
	if !remaining && feedManaged {
		err = r.trashUnclaimedFeedManagedLinkOn(ctx, db, linkID, linkStatus, lifecycle)
	}
	if err != nil {
		return nil, err
	}
	return visibleLinkID, nil
}

func (r *PGXReaderVNextRepository) trashUnclaimedFeedManagedLinkOn(
	ctx context.Context,
	db database.Querier,
	linkID uuid.UUID,
	status model.LinkStatus,
	lifecycle ReaderLinkLifecycle,
) error {
	if lifecycle == nil {
		if status == model.LinkStatusPending || status == model.LinkStatusProcessing {
			return errors.New("trash Feed-managed in-flight Link: lifecycle queue is not configured")
		}
		return terminalizeAndDeleteLockedLinkOn(ctx, db, linkID)
	}
	tx, ok := db.(pgx.Tx)
	if !ok {
		return errors.New("trash Feed-managed Link: transaction-bound lifecycle queue requires pgx.Tx")
	}
	if err := lifecycle(ctx, tx, ReaderLinkLifecycleChange{LinkID: linkID}); err != nil {
		return fmt.Errorf("cancel Feed-managed Link work: %w", err)
	}
	return terminalizeAndDeleteLockedLinkOn(ctx, db, linkID)
}

// parseReaderFeedItemKey splits a "<kind>:<uuid>" feed key. The key is required
// to round-trip byte for byte, because it doubles as the primary key of the
// hide table: two spellings of the same UUID would otherwise hide different
// logical items.
func parseReaderFeedItemKey(itemKey string) (string, uuid.UUID, error) {
	kind, rawID, ok := strings.Cut(itemKey, ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	id, parseErr := uuid.Parse(rawID)
	if parseErr != nil {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	if itemKey != kind+":"+id.String() {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	if kind != "link" && kind != "subscription" && kind != "inbox" {
		return "", uuid.UUID{}, ErrInvalidReaderFeedItem
	}
	return kind, id, nil
}

// validateReaderFeedAction rejects actions the target kind cannot carry. Only
// Only subscription items can be saved. Hide is the single persisted negative
// action for every Feed source.
func validateReaderFeedAction(kind, action string) error {
	if action != "save" && action != "unsave" && action != "hide" {
		return ErrInvalidReaderFeedItem
	}
	if (action == "save" || action == "unsave") && kind != "subscription" {
		return ErrInvalidReaderFeedItem
	}
	return nil
}

func ensureReaderFeedItem(ctx context.Context, db database.Querier, kind string, kindID uuid.UUID) error {
	table := ""
	switch kind {
	case "link":
		table = "links"
	case "subscription":
		table = "feed_items"
	case "inbox":
		table = "reader_inbox"
	default:
		return ErrInvalidReaderFeedItem
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id=$1)`, kindID).Scan(&exists); err != nil {
		return fmt.Errorf("check reader feed item: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (r *PGXReaderVNextRepository) RelatedTags(ctx context.Context, linkID *uuid.UUID, limit int) ([]string, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	var tags []string
	if linkID != nil {
		if err := r.db.QueryRow(ctx, `SELECT COALESCE(tags,'{}') FROM links WHERE id=$1 AND status='done' AND deleted_at IS NULL`, *linkID).Scan(&tags); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("read related tag source: %w", err)
		}
	}
	return r.cooccurrenceRelatedTags(ctx, tags, limit)
}

// cooccurrenceRelatedTags returns tags that co-occur with the seed tags, or the
// installation's most-used tags when there is no seed.
func (r *PGXReaderVNextRepository) cooccurrenceRelatedTags(ctx context.Context, tags []string, limit int) ([]string, error) {
	var rows pgx.Rows
	var err error
	if len(tags) > 0 {
		rows, err = r.db.Query(ctx, `
				SELECT candidate FROM (
					SELECT unnest(l.tags) AS candidate, count(*) AS uses
					FROM links l WHERE l.status='done' AND l.deleted_at IS NULL AND l.tags && $1::text[]
					GROUP BY candidate
				) related
				-- The seed-tag exclusion lives in the outer WHERE, not in a
				-- HAVING on the aggregate: HAVING runs before SELECT output
				-- aliases exist, so it cannot name the candidate alias the way
				-- GROUP BY can, and it cannot repeat the expression either
				-- because unnest() is a set-returning function.
				WHERE candidate <> ALL($1::text[])
				ORDER BY uses DESC,candidate LIMIT $2`, tags, limit)
	} else {
		rows, err = r.db.Query(ctx, `SELECT tag FROM (SELECT unnest(tags) AS tag,count(*) AS uses FROM links WHERE status='done' AND deleted_at IS NULL AND tags IS NOT NULL GROUP BY tag ORDER BY uses DESC,tag LIMIT $1) related`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("related tags: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

const (
	readerTagActivityRows = `
		SELECT 'tag'::text AS kind, source.tag AS activity_key,
			MAX(source.event_at) AS last_at,
			lower(btrim(source.tag)) AS normalized_key
		FROM (
			SELECT GREATEST(l.created_at,l.first_collected_at,l.last_recollected_at) AS event_at,
				unnest(l.tags) AS tag
			FROM links l
			WHERE l.library_kind='reading' AND l.status='done'
				AND l.deleted_at IS NULL AND l.tags IS NOT NULL
		) source
		GROUP BY source.tag`
	readerDomainActivityRows = `
		SELECT 'domain'::text AS kind, l.domain AS activity_key,
			MAX(GREATEST(l.created_at,l.first_collected_at,l.last_recollected_at)) AS last_at,
			lower(btrim(l.domain)) AS normalized_key
		FROM links l
		WHERE l.library_kind='reading' AND l.status='done'
			AND l.deleted_at IS NULL AND l.domain IS NOT NULL AND l.domain <> ''
		GROUP BY l.domain`
)

func readerActivityRows(kind string) (string, error) {
	switch kind {
	case model.ReaderActivityKindAll:
		return readerTagActivityRows + " UNION ALL " + readerDomainActivityRows, nil
	case model.ReaderActivityKindTag:
		return readerTagActivityRows, nil
	case model.ReaderActivityKindDomain:
		return readerDomainActivityRows, nil
	default:
		return "", fmt.Errorf("%w: invalid activity kind", ErrInvalidReaderCursor)
	}
}

func (r *PGXReaderVNextRepository) ListActivity(ctx context.Context, query model.ReaderActivityQuery) (model.ReaderActivityPage, error) {
	if query.Limit <= 0 || query.Limit > 1000 {
		query.Limit = 100
	}
	base, err := readerActivityRows(query.Kind)
	if err != nil {
		return model.ReaderActivityPage{}, err
	}
	if query.After != nil && query.Kind != model.ReaderActivityKindAll && query.After.Kind != query.Kind {
		return model.ReaderActivityPage{}, fmt.Errorf("%w: activity cursor kind mismatch", ErrInvalidReaderCursor)
	}

	statement := `SELECT kind,activity_key,last_at,normalized_key FROM (` + base + `) activity`
	args := make([]any, 0, 5)
	if query.After != nil {
		statement += `
			WHERE last_at < $1
				OR (last_at = $1 AND (
					kind COLLATE "C" > $2 COLLATE "C"
					OR (kind = $2 AND (
						normalized_key COLLATE "C" > $3 COLLATE "C"
						OR (normalized_key = $3 AND activity_key COLLATE "C" > $4 COLLATE "C")
					))
				))`
		args = append(args, query.After.LastAt, query.After.Kind, query.After.NormalizedKey, query.After.Key)
	}
	args = append(args, query.Limit+1)
	statement += fmt.Sprintf(`
		ORDER BY last_at DESC, kind COLLATE "C" ASC, normalized_key COLLATE "C" ASC, activity_key COLLATE "C" ASC
		LIMIT $%d`, len(args))

	rows, err := r.db.Query(ctx, statement, args...)
	if err != nil {
		return model.ReaderActivityPage{}, err
	}
	defer rows.Close()
	items := make([]model.ReaderActivity, 0)
	for rows.Next() {
		var item model.ReaderActivity
		if err := rows.Scan(&item.Kind, &item.Key, &item.LastAt, &item.NormalizedKey); err != nil {
			return model.ReaderActivityPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return model.ReaderActivityPage{}, err
	}
	hasMore := len(items) > query.Limit
	if hasMore {
		items = items[:query.Limit]
	}
	return model.ReaderActivityPage{Items: items, HasMore: hasMore}, nil
}

func (r *PGXReaderVNextRepository) UpdateLinkMetadata(ctx context.Context, patch model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error) {
	var result model.ReaderLinkMetadataUpdate
	err := r.withTx(ctx, func(tx database.Querier) error {
		return r.updateLinkMetadata(ctx, tx, patch, &result)
	})
	if err != nil {
		return model.ReaderLinkMetadataUpdate{}, err
	}
	return result, nil
}

func (r *PGXReaderVNextRepository) updateLinkMetadata(ctx context.Context, tx database.Querier, patch model.ReaderLinkMetadataPatch, result *model.ReaderLinkMetadataUpdate) error {
	var (
		found        bool
		changed      bool
		tupleChanged bool
	)
	err := tx.QueryRow(ctx, `
			WITH target AS (
				SELECT id,title,summary,tags,metadata_revision
				FROM links
				WHERE id=$4 AND deleted_at IS NULL
					AND status='done' AND library_kind='reading' AND metadata_revision=$5
				FOR UPDATE
			), updated AS (
				UPDATE links AS link
				SET title=$1,
					summary=$2,
					tags=COALESCE($3::text[],'{}'::text[]),
					metadata_revision=link.metadata_revision+1,
					updated_at=NOW()
				FROM target
				WHERE link.id=target.id
					AND target.metadata_revision < $6
					AND (target.title IS DISTINCT FROM $1 OR
						target.summary IS DISTINCT FROM $2 OR
						target.tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[]))
				RETURNING link.metadata_revision
			)
			SELECT
				EXISTS (SELECT 1 FROM target),
				COALESCE((SELECT metadata_revision FROM updated), (SELECT metadata_revision FROM target), 0),
				COALESCE((SELECT tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[]) FROM target), false),
				EXISTS (SELECT 1 FROM updated),
				COALESCE((SELECT target.title IS DISTINCT FROM $1
					OR target.summary IS DISTINCT FROM $2
					OR target.tags IS DISTINCT FROM COALESCE($3::text[],'{}'::text[])
					FROM target), false)`,
		patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision,
	).Scan(&found, &result.MetadataRevision, &result.TagsChanged, &changed, &tupleChanged)
	if err != nil {
		return fmt.Errorf("update link metadata: %w", err)
	}
	if !found || tupleChanged && !changed {
		return ErrRevisionConflict
	}
	return nil
}
