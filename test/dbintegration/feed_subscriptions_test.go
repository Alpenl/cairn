package dbintegration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestFeedSubscriptionSoftDeleteLifecycleClaimsAndRetention(t *testing.T) {
	pool := StartPostgres(t)
	feeds := repository.NewPGXFeedRepository(pool, pool)
	ctx := t.Context()

	subscription, err := feeds.CreateSubscription(ctx, "https://example.com/lifecycle.xml", nil, false, "lifecycle")
	if err != nil {
		t.Fatalf("CreateSubscription(lifecycle): %v", err)
	}
	ordinaryItem, starredItem := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO feed_items
		(id,subscription_id,external_id,url,title,read_at,starred)
		VALUES ($1,$2,'ordinary','https://example.com/ordinary','ordinary',NOW(),false),
		       ($3,$2,'starred','https://example.com/starred','starred',NOW(),true)`,
		ordinaryItem, subscription.ID, starredItem); err != nil {
		t.Fatalf("insert feed items: %v", err)
	}
	if err := feeds.SoftDeleteSubscription(ctx, subscription.ID); err != nil {
		t.Fatalf("SoftDeleteSubscription: %v", err)
	}
	if _, found, err := feeds.GetItem(ctx, ordinaryItem, false); err != nil || found {
		t.Fatalf("ordinary item after unsubscribe found=%v err=%v", found, err)
	}
	if _, found, err := feeds.GetItem(ctx, starredItem, false); err != nil || !found {
		t.Fatalf("starred item after unsubscribe found=%v err=%v", found, err)
	}
	if overview, err := feeds.ListOverview(ctx, subscription.URL); err != nil || len(overview.Subscriptions) != 0 {
		t.Fatalf("lookup after unsubscribe subscriptions=%d err=%v", len(overview.Subscriptions), err)
	}

	claimable, err := feeds.CreateSubscription(ctx, "https://example.com/claimable.xml", nil, false, "claimable")
	if err != nil {
		t.Fatalf("CreateSubscription(claimable): %v", err)
	}
	var wait sync.WaitGroup
	results := make(chan []uuid.UUID, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claimed, claimErr := feeds.ClaimDue(ctx, 1, 2*time.Minute)
			if claimErr != nil {
				t.Errorf("ClaimDue: %v", claimErr)
				return
			}
			ids := make([]uuid.UUID, 0, len(claimed))
			for _, item := range claimed {
				ids = append(ids, item.ID)
			}
			results <- ids
		}()
	}
	wait.Wait()
	close(results)
	claimedIDs := make([]uuid.UUID, 0, 1)
	for ids := range results {
		claimedIDs = append(claimedIDs, ids...)
	}
	if len(claimedIDs) != 1 || claimedIDs[0] != claimable.ID {
		t.Fatalf("concurrent claim IDs=%v, want [%s]", claimedIDs, claimable.ID)
	}

	retention, err := feeds.CreateSubscription(ctx, "https://example.com/retention.xml", nil, false, "retention")
	if err != nil {
		t.Fatalf("CreateSubscription(retention): %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO feed_items
		(subscription_id,external_id,url,title,published_at,read_at)
		SELECT $1::uuid,'ordinary-' || n,'https://example.com/o/' || n,'ordinary',NOW()-n*INTERVAL '1 second',NOW()
		FROM generate_series(1,120) n
		UNION ALL
		SELECT $1::uuid,'unread-' || n,'https://example.com/u/' || n,'unread',NOW()-n*INTERVAL '1 second',NULL
		FROM generate_series(1,120) n`, retention.ID); err != nil {
		t.Fatalf("insert retention fixtures: %v", err)
	}
	var savedItemID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM feed_items WHERE subscription_id=$1 AND external_id='ordinary-120'`, retention.ID).Scan(&savedItemID); err != nil {
		t.Fatalf("select saved retention fixture: %v", err)
	}
	savedLinkID := seedReaderVNextSavedLink(t, pool, "https://example.com/o/120", "Saved retention item", "body", "summary")
	if _, err := pool.Exec(ctx, `INSERT INTO reader_feed_saves (feed_item_id,link_id) VALUES ($1,$2)`, savedItemID, savedLinkID); err != nil {
		t.Fatalf("save retention fixture: %v", err)
	}
	claims, err := feeds.ClaimDue(ctx, 1, 2*time.Minute)
	if err != nil || len(claims) != 1 || claims[0].ID != retention.ID {
		t.Fatalf("retention claim=%#v err=%v", claims, err)
	}
	if _, err := feeds.CompleteRefresh(ctx, repository.FeedRefreshSuccess{
		SubscriptionID: retention.ID,
		ClaimToken:     *claims[0].RefreshClaimToken,
		Parsed:         model.ParsedFeed{},
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CompleteRefresh(retention): %v", err)
	}
	var ordinaryCount, unreadCount int
	if err := pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE read_at IS NOT NULL),
		COUNT(*) FILTER (WHERE read_at IS NULL)
		FROM feed_items WHERE subscription_id=$1`, retention.ID).Scan(&ordinaryCount, &unreadCount); err != nil {
		t.Fatalf("count retained items: %v", err)
	}
	if ordinaryCount != 101 || unreadCount != 120 {
		t.Fatalf("retention counts ordinary=%d unread=%d, want 101/120 including one Reader save", ordinaryCount, unreadCount)
	}
	var savedItemPresent bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_items WHERE id=$1 AND link_id IS NULL)`, savedItemID).Scan(&savedItemPresent); err != nil {
		t.Fatalf("read saved retention item: %v", err)
	}
	if !savedItemPresent {
		t.Fatal("retention removed Reader-saved item or conflated it with Analyze link_id")
	}
}

func TestFeedRefreshBatchUpsertPersistsAndUpdatesItems(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	feeds := repository.NewPGXFeedRepository(pool, pool)
	subscription, err := feeds.CreateSubscription(ctx, "https://example.com/batch.xml", nil, false, "batch")
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	claims, err := feeds.ClaimDue(ctx, 1, 2*time.Minute)
	if err != nil || len(claims) != 1 || claims[0].ID != subscription.ID {
		t.Fatalf("initial claim=%#v err=%v", claims, err)
	}
	stringPointer := func(value string) *string { return &value }
	now := time.Now().UTC().Truncate(time.Microsecond)
	firstPublished := now.Add(-2 * time.Hour)
	firstItems := []model.FeedItem{
		{
			ExternalID: "one", URL: "https://example.com/one", Title: "One",
			Author: stringPointer("author-one"), Summary: stringPointer("summary-one"),
			Content: stringPointer("content-one"), ContentHTML: stringPointer("<p>content-one</p>"),
			PublishedAt: &firstPublished,
		},
		{ExternalID: "two", URL: "https://example.com/two", Title: "Two"},
		{ExternalID: "three", URL: "https://example.com/three", Title: "Three"},
	}
	inserted, err := feeds.CompleteRefresh(ctx, repository.FeedRefreshSuccess{
		SubscriptionID: subscription.ID,
		ClaimToken:     *claims[0].RefreshClaimToken,
		Parsed:         model.ParsedFeed{Title: "Batch", Items: firstItems},
		Initial:        true,
		Now:            now,
	})
	if err != nil || inserted != len(firstItems) {
		t.Fatalf("initial CompleteRefresh inserted=%d err=%v", inserted, err)
	}

	if err := feeds.ScheduleRefresh(ctx, subscription.ID); err != nil {
		t.Fatalf("ScheduleRefresh: %v", err)
	}
	claims, err = feeds.ClaimDue(context.Background(), 1, 2*time.Minute)
	if err != nil || len(claims) != 1 || claims[0].ID != subscription.ID {
		t.Fatalf("update claim=%#v err=%v", claims, err)
	}
	updatedPublished := now.Add(time.Hour)
	updatedOne := model.FeedItem{
		ExternalID: "one", URL: "https://example.com/one-updated", Title: "One updated",
		Author: stringPointer("author-updated"), Summary: stringPointer("summary-updated"),
		Content: stringPointer("content-updated"), ContentHTML: stringPointer("<p>content-updated</p>"),
		PublishedAt: &updatedPublished,
	}
	newItem := model.FeedItem{ExternalID: "new", URL: "https://example.com/new", Title: "New"}
	inserted, err = feeds.CompleteRefresh(ctx, repository.FeedRefreshSuccess{
		SubscriptionID: subscription.ID,
		ClaimToken:     *claims[0].RefreshClaimToken,
		Parsed:         model.ParsedFeed{Title: "Batch", Items: []model.FeedItem{newItem, updatedOne, firstItems[1], firstItems[2]}},
		Now:            now.Add(time.Minute),
	})
	if err != nil || inserted != 1 {
		t.Fatalf("update CompleteRefresh inserted=%d err=%v", inserted, err)
	}

	var count int
	var gotURL, gotTitle, gotAuthor, gotSummary, gotContent, gotHTML string
	var gotPublished time.Time
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM feed_items WHERE subscription_id=$1`, subscription.ID).Scan(&count); err != nil {
		t.Fatalf("count batch-upserted items: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT url,title,author,summary,content_text,content_html,published_at
		FROM feed_items WHERE subscription_id=$1 AND external_id='one'`, subscription.ID).
		Scan(&gotURL, &gotTitle, &gotAuthor, &gotSummary, &gotContent, &gotHTML, &gotPublished); err != nil {
		t.Fatalf("query updated feed item: %v", err)
	}
	if count != 4 {
		t.Fatalf("feed item count=%d, want 4", count)
	}
	if gotURL != updatedOne.URL || gotTitle != updatedOne.Title || gotAuthor != *updatedOne.Author ||
		gotSummary != *updatedOne.Summary || gotContent != *updatedOne.Content || gotHTML != *updatedOne.ContentHTML ||
		!gotPublished.Equal(updatedPublished) {
		t.Fatalf("updated item=url %q title %q author %q summary %q content %q html %q published %s",
			gotURL, gotTitle, gotAuthor, gotSummary, gotContent, gotHTML, gotPublished)
	}
}
