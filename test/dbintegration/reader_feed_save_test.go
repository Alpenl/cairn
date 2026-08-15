package dbintegration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestReaderFeedSavePostgresAssociationLifecycle exercises the persistent
// association rather than a Feed snapshot. It deliberately covers the cases
// that cannot be made trustworthy with mocks: canonical identity races,
// creator ownership, and the link lifecycle trigger that tombstones thoughts.
func TestReaderFeedSavePostgresAssociationLifecycle(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	const canonicalURL = "https://feed-save.example.test/article?chapter=1"
	creatorItem := seedReaderFeedSaveItem(t, pool, canonicalURL, "creator")
	reuserItem := seedReaderFeedSaveItem(t, pool, canonicalURL, "reuser")

	// Repeated and concurrent saves of one feed item must converge on one
	// association and one created reading link.
	const workers = 8
	start := make(chan struct{})
	results := make(chan model.ReaderFeedFeedback, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := repo.FeedbackFeed(ctx, "subscription:"+creatorItem.String(), "save")
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent save: %v", err)
	}
	var creatorLink uuid.UUID
	for result := range results {
		if !result.Saved || result.Association == nil || !result.Association.CreatedLink {
			t.Fatalf("concurrent save result = %#v, want created association", result)
		}
		if creatorLink == uuid.Nil {
			creatorLink = result.Association.LinkID
		} else if result.Association.LinkID != creatorLink {
			t.Fatalf("concurrent link = %s, want %s", result.Association.LinkID, creatorLink)
		}
	}
	if creatorLink == uuid.Nil {
		t.Fatal("concurrent saves returned no link")
	}
	if countReaderFeedSaves(t, pool, creatorItem, creatorLink) != 1 || countLiveReaderLinks(t, pool, canonicalURL) != 1 {
		t.Fatal("repeated save did not converge to one association and one live link")
	}

	// A second feed item reuses the same link but is never made its creator.
	reused, err := repo.FeedbackFeed(ctx, "subscription:"+reuserItem.String(), "save")
	if err != nil || reused.Association == nil {
		t.Fatalf("save reuser = %#v, %v", reused, err)
	}
	if reused.Association.LinkID != creatorLink || reused.Association.CreatedLink {
		t.Fatalf("reused association = %#v, want link %s with created_link=false", reused.Association, creatorLink)
	}
	if countReaderFeedSaves(t, pool, uuid.Nil, creatorLink) != 2 {
		t.Fatal("expected two active associations for the canonical link")
	}

	// Removing the non-creator association cannot trash the shared link. Saving
	// it again is idempotent reuse, after which the creator's final unsave is
	// the only operation that transitions the link to trash.
	if result, err := repo.FeedbackFeed(ctx, "subscription:"+reuserItem.String(), "unsave"); err != nil || result.Association == nil || result.Saved {
		t.Fatalf("unsave reuser = %#v, %v", result, err)
	}
	assertReaderFeedLinkLive(t, pool, creatorLink, true)
	if result, err := repo.FeedbackFeed(ctx, "subscription:"+reuserItem.String(), "save"); err != nil || result.Association == nil || result.Association.CreatedLink {
		t.Fatalf("repeat reuser save = %#v, %v", result, err)
	}
	if result, err := repo.FeedbackFeed(ctx, "subscription:"+reuserItem.String(), "unsave"); err != nil || result.Association == nil {
		t.Fatalf("final reuser unsave = %#v, %v", result, err)
	}
	if result, err := repo.FeedbackFeed(ctx, "subscription:"+creatorItem.String(), "unsave"); err != nil || result.Association == nil || !result.Association.CreatedLink {
		t.Fatalf("creator final unsave = %#v, %v", result, err)
	}
	assertReaderFeedLinkLive(t, pool, creatorLink, false)

	// A created link carrying reader data follows the ordinary #53 lifecycle:
	// unsave produces a trash host/tombstone and a Feed re-save restores the same
	// Link and re-anchors the thought without losing progress or note body.
	lifecycleItem := seedReaderFeedSaveItem(t, pool, "https://feed-save.example.test/lifecycle", "lifecycle")
	created, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "save")
	if err != nil || created.Association == nil || !created.Association.CreatedLink {
		t.Fatalf("save lifecycle item = %#v, %v", created, err)
	}
	lifecycleLink := created.Association.LinkID
	if _, err := pool.Exec(t.Context(), `UPDATE links SET content=$2,content_document=$2,updated_at=NOW() WHERE id=$1`, lifecycleLink, "preserve this thought"); err != nil {
		t.Fatalf("seed lifecycle link content: %v", err)
	}
	progress := float32(0.4)
	if _, err := repo.PatchEngagement(ctx, model.ReaderEngagementPatch{LinkID: lifecycleLink, Progress: &progress}); err != nil {
		t.Fatalf("set lifecycle progress: %v", err)
	}
	thoughtID := "feed-save-thought-" + uuid.NewString()
	_, err = repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{{
		OpID:          "feed-save-op-" + uuid.NewString(),
		DeviceID:      "feed-save-test",
		LogicalClock:  1,
		OperationKind: "add",
		AnnotationID:  thoughtID,
		HostKind:      "link",
		HostID:        lifecycleLink.String(),
		Target:        readerVNextJSON(t, map[string]any{"kind": "saved-content", "host_id": lifecycleLink.String(), "version": map[string]any{"content_revision": 1}}),
		Payload:       readerVNextJSON(t, map[string]any{"body": "preserve this thought", "quote": map[string]any{"exact": "preserve"}, "source": "user"}),
	}})
	if err != nil {
		t.Fatalf("append lifecycle thought: %v", err)
	}
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "unsave"); err != nil {
		t.Fatalf("unsave lifecycle item: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, lifecycleLink, false)
	assertReaderThoughtLifecycle(t, repo, ctx, thoughtID, false, "tombstone")
	restored, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "save")
	if err != nil || restored.Association == nil || restored.Association.LinkID != lifecycleLink || restored.Association.CreatedLink {
		t.Fatalf("re-save feed-created link = %#v, %v", restored, err)
	}
	assertReaderFeedLinkLive(t, pool, lifecycleLink, true)
	assertReaderThoughtLifecycle(t, repo, ctx, thoughtID, false, "active")
	engagement, err := repo.GetEngagement(ctx, lifecycleLink)
	if err != nil || engagement.Progress != progress {
		t.Fatalf("restored engagement = %#v, %v; want progress %v", engagement, err, progress)
	}
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "unsave"); err != nil {
		t.Fatalf("trash lifecycle item before explicit restore: %v", err)
	}
	manuallyRestored, err := repo.RestoreHost(ctx, model.ReaderHostLink, lifecycleLink)
	if err != nil || !manuallyRestored.Changed {
		t.Fatalf("explicit RestoreHost() = %#v, %v", manuallyRestored, err)
	}
	assertReaderThoughtLifecycle(t, repo, ctx, thoughtID, false, "active")
	var feedManaged bool
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, lifecycleLink).Scan(&feedManaged); err != nil {
		t.Fatalf("read explicit restore ownership: %v", err)
	}
	if feedManaged {
		t.Fatal("explicitly restored Link retained Feed-exclusive ownership")
	}
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "save"); err != nil {
		t.Fatalf("save explicitly restored Link: %v", err)
	}
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+lifecycleItem.String(), "unsave"); err != nil {
		t.Fatalf("unsave explicitly restored Link: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, lifecycleLink, true)
}

func TestReaderFeedSavePostgresUnsaveOrderAndConcurrencyConverge(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	t.Run("creator first", func(t *testing.T) {
		const rawURL = "https://feed-save.example.test/creator-first"
		creator := seedReaderFeedSaveItem(t, pool, rawURL, "creator-first-a")
		follower := seedReaderFeedSaveItem(t, pool, rawURL, "creator-first-b")
		first, err := repo.FeedbackFeed(ctx, "subscription:"+creator.String(), "save")
		if err != nil || first.Association == nil || !first.Association.CreatedLink {
			t.Fatalf("save creator = %#v, %v", first, err)
		}
		linkID := first.Association.LinkID
		if _, err := repo.FeedbackFeed(ctx, "subscription:"+follower.String(), "save"); err != nil {
			t.Fatalf("save follower: %v", err)
		}
		if _, err := repo.FeedbackFeed(ctx, "subscription:"+creator.String(), "unsave"); err != nil {
			t.Fatalf("unsave creator first: %v", err)
		}
		assertReaderFeedLinkLive(t, pool, linkID, true)
		if _, err := repo.FeedbackFeed(ctx, "subscription:"+follower.String(), "unsave"); err != nil {
			t.Fatalf("unsave follower last: %v", err)
		}
		assertReaderFeedLinkLive(t, pool, linkID, false)
	})

	t.Run("concurrent last claims", func(t *testing.T) {
		const rawURL = "https://feed-save.example.test/concurrent-unsave"
		items := []uuid.UUID{
			seedReaderFeedSaveItem(t, pool, rawURL, "concurrent-unsave-a"),
			seedReaderFeedSaveItem(t, pool, rawURL, "concurrent-unsave-b"),
			seedReaderFeedSaveItem(t, pool, rawURL, "concurrent-unsave-c"),
		}
		var linkID uuid.UUID
		for _, itemID := range items {
			result, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "save")
			if err != nil || result.Association == nil {
				t.Fatalf("save concurrent fixture = %#v, %v", result, err)
			}
			if linkID == uuid.Nil {
				linkID = result.Association.LinkID
			} else if result.Association.LinkID != linkID {
				t.Fatalf("association link = %s, want %s", result.Association.LinkID, linkID)
			}
		}
		start := make(chan struct{})
		errs := make(chan error, len(items))
		var wait sync.WaitGroup
		for _, itemID := range items {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "unsave")
				errs <- err
			}()
		}
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent unsave: %v", err)
			}
		}
		if got := countReaderFeedSaves(t, pool, uuid.Nil, linkID); got != 0 {
			t.Fatalf("remaining Feed claims = %d, want 0", got)
		}
		assertReaderFeedLinkLive(t, pool, linkID, false)
	})
}

func TestReaderFeedSavePostgresIndependentLinkSurvivesLastUnsave(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	rawURL := "https://feed-save.example.test/independent"
	independent := seedReaderVNextSavedLink(t, pool, rawURL, "Independent", "body", "summary")
	itemID := seedReaderFeedSaveItem(t, pool, rawURL, "independent-feed")
	saved, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "save")
	if err != nil || saved.Association == nil || saved.Association.LinkID != independent || saved.Association.CreatedLink {
		t.Fatalf("save independent link = %#v, %v", saved, err)
	}
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "unsave"); err != nil {
		t.Fatalf("unsave independent link: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, independent, true)
}

func TestReaderFeedSavePostgresResaveRestoreRollsBackWithAssociation(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	itemID := seedReaderFeedSaveItem(t, pool, "https://feed-save.example.test/restore-rollback", "restore-rollback")
	created, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "save")
	if err != nil || created.Association == nil {
		t.Fatalf("seed Feed save = %#v, %v", created, err)
	}
	linkID := created.Association.LinkID
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "unsave"); err != nil {
		t.Fatalf("seed Feed trash: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_reader_feed_save_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected reader_feed_saves failure';
		END;
		$$;
		CREATE TRIGGER fail_reader_feed_save_insert
		BEFORE INSERT ON reader_feed_saves
		FOR EACH ROW EXECUTE FUNCTION fail_reader_feed_save_insert()`); err != nil {
		t.Fatalf("install Feed save failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_reader_feed_save_insert ON reader_feed_saves; DROP FUNCTION IF EXISTS fail_reader_feed_save_insert()`)
	})

	if _, err := repo.FeedbackFeed(ctx, "subscription:"+itemID.String(), "save"); err == nil {
		t.Fatal("re-save with injected association failure succeeded")
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)
	if got := countReaderFeedSaves(t, pool, uuid.Nil, linkID); got != 0 {
		t.Fatalf("Feed claims after rollback = %d, want 0", got)
	}
}

func seedReaderFeedSaveItem(t *testing.T, pool *pgxpool.Pool, url, suffix string) uuid.UUID {
	t.Helper()
	subscriptionID, itemID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO feed_subscriptions (id,url,title) VALUES ($1,$2,$3)`, subscriptionID, "https://feed-save.example.test/"+suffix+".xml", "Feed "+suffix); err != nil {
		t.Fatalf("seed subscription %s: %v", suffix, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO feed_items (id,subscription_id,external_id,url,title,summary) VALUES ($1,$2,$3,$4,$5,$6)`, itemID, subscriptionID, suffix, url, "Item "+suffix, "Summary "+suffix); err != nil {
		t.Fatalf("seed feed item %s: %v", suffix, err)
	}
	return itemID
}

func countReaderFeedSaves(t *testing.T, pool *pgxpool.Pool, itemID, linkID uuid.UUID) int {
	t.Helper()
	query := `SELECT count(*) FROM reader_feed_saves WHERE link_id=$1`
	args := []any{linkID}
	if itemID != uuid.Nil {
		query += ` AND feed_item_id=$2`
		args = append(args, itemID)
	}
	var count int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count feed saves: %v", err)
	}
	return count
}

func countLiveReaderLinks(t *testing.T, pool *pgxpool.Pool, sourceKey string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM links WHERE source_key=$1 AND deleted_at IS NULL`, sourceKey).Scan(&count); err != nil {
		t.Fatalf("count live reader links: %v", err)
	}
	return count
}

func assertReaderFeedLinkLive(t *testing.T, pool *pgxpool.Pool, linkID uuid.UUID, wantLive bool) {
	t.Helper()
	var deleted bool
	if err := pool.QueryRow(t.Context(), `SELECT deleted_at IS NOT NULL FROM links WHERE id=$1`, linkID).Scan(&deleted); err != nil {
		t.Fatalf("read feed-save link state: %v", err)
	}
	if deleted == wantLive {
		t.Fatalf("feed-save link deleted=%v, want live=%v", deleted, wantLive)
	}
}
