package dbintegration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestReaderConfirmInboxInsertsSavedLinkForNewURL covers ConfirmInbox on a URL
// that has no saved link yet, i.e. the branch that actually INSERTs into
// `links`.
//
// The existing cross-surface chain test seeds a saved link at the same URL
// before confirming, so it only ever exercises the ON CONFLICT / find-existing
// path. That left the INSERT itself unexecuted by any real database, and it
// shipped omitting `first_collected_at` — a NOT NULL column with no default —
// which made the whole confirm flow return 500 in production:
//
//	null value in column "first_collected_at" of relation "links"
//	violates not-null constraint (SQLSTATE 23502)
//
// EXPLAIN cannot catch this class: the statement parses and plans fine, the
// constraint only fires on a real write. Only an insert against a real table
// proves it.
func TestReaderConfirmInboxInsertsSavedLinkForNewURL(t *testing.T) {
	pool := StartPostgres(t)

	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)

	// Deliberately no seeded saved link at this URL: ConfirmInbox must create
	// one from scratch.
	inboxID := seedReaderVNextInbox(t, pool,
		"https://reader-vnext.example/confirm-fresh", "Fresh capture", "Fresh capture body", "Fresh capture summary")

	linkID, err := reader.ConfirmInbox(ctx, inboxID)
	if err != nil {
		t.Fatalf("ConfirmInbox on a URL with no existing link: %v", err)
	}
	if linkID == (uuid.UUID{}) {
		t.Fatal("ConfirmInbox returned a zero link id for a fresh URL")
	}

	var url string
	var firstCollectedAt, createdAt any
	if err := pool.QueryRow(t.Context(),
		`SELECT url, first_collected_at, created_at FROM links WHERE id=$1`, linkID,
	).Scan(&url, &firstCollectedAt, &createdAt); err != nil {
		t.Fatalf("read the link ConfirmInbox created: %v", err)
	}
	if url != "https://reader-vnext.example/confirm-fresh" {
		t.Fatalf("created link url = %q, want the inbox URL", url)
	}
	// first_collected_at is NOT NULL, so a missing column would have failed the
	// insert above; assert it anyway so the column cannot be quietly dropped
	// back out of the statement if the constraint is ever relaxed.
	if firstCollectedAt == nil {
		t.Fatal("created link has a null first_collected_at")
	}

	// Confirming again must return the same link rather than inserting a
	// duplicate: the ON CONFLICT branch has to keep working alongside the fix.
	again, err := reader.ConfirmInbox(ctx, inboxID)
	if err != nil {
		t.Fatalf("ConfirmInbox idempotent retry: %v", err)
	}
	if again != linkID {
		t.Fatalf("ConfirmInbox retry returned %v, want the stable link %s", again, linkID)
	}
}

func TestReaderConfirmInboxRestoresAndAdoptsFeedManagedTrashLink(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	rawURL := "https://reader-vnext.example/feed-trash-confirm"
	feedItemID := seedReaderFeedSaveItem(t, pool, rawURL, "feed-trash-confirm")
	saved, err := reader.FeedbackFeed(ctx, "subscription:"+feedItemID.String(), "save")
	if err != nil || saved.Association == nil {
		t.Fatalf("seed Feed save = %#v, %v", saved, err)
	}
	linkID := saved.Association.LinkID
	if _, err := reader.FeedbackFeed(ctx, "subscription:"+feedItemID.String(), "unsave"); err != nil {
		t.Fatalf("seed Feed trash: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)

	inboxID := seedReaderVNextInbox(t, pool, rawURL, "Adopt feed Trash", "Inbox-owned body", "Inbox summary")
	confirmed, err := reader.ConfirmInbox(ctx, inboxID)
	if err != nil || confirmed != linkID {
		t.Fatalf("ConfirmInbox() = %s, %v; want %s", confirmed, err, linkID)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	var feedManaged bool
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, linkID).Scan(&feedManaged); err != nil {
		t.Fatalf("read adopted ownership: %v", err)
	}
	if feedManaged {
		t.Fatal("Inbox-confirmed Link retained Feed-exclusive ownership")
	}

	// A later Feed claim can reuse the adopted Link, but releasing that claim
	// must not trash the independently-owned Library record.
	if _, err := reader.FeedbackFeed(ctx, "subscription:"+feedItemID.String(), "save"); err != nil {
		t.Fatalf("save adopted Link from Feed: %v", err)
	}
	if _, err := reader.FeedbackFeed(ctx, "subscription:"+feedItemID.String(), "unsave"); err != nil {
		t.Fatalf("unsave adopted Link from Feed: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
}

func TestReaderFeedResaveAndInboxConfirmSerializeTrashRestore(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reader := repository.NewPGXReaderVNextRepository(pool)
	rawURL := "https://reader-vnext.example/concurrent-trash-restore"
	originalItem := seedReaderFeedSaveItem(t, pool, rawURL, "concurrent-trash-original")
	saved, err := reader.FeedbackFeed(ctx, "subscription:"+originalItem.String(), "save")
	if err != nil || saved.Association == nil {
		t.Fatalf("seed Feed save = %#v, %v", saved, err)
	}
	linkID := saved.Association.LinkID
	if _, err := reader.FeedbackFeed(ctx, "subscription:"+originalItem.String(), "unsave"); err != nil {
		t.Fatalf("seed Feed trash: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)

	resaveItem := seedReaderFeedSaveItem(t, pool, rawURL, "concurrent-trash-resave")
	inboxID := seedReaderVNextInbox(t, pool, rawURL, "Concurrent adoption", "Inbox body", "Inbox summary")
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Trash restore blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if err := blocker.QueryRow(ctx, `SELECT id FROM links WHERE id=$1 FOR UPDATE`, linkID).Scan(&linkID); err != nil {
		t.Fatalf("lock Trash Link: %v", err)
	}

	feedApplication := "feed_trash_restore_" + uuid.NewString()
	inboxApplication := "inbox_trash_restore_" + uuid.NewString()
	feedPool := openNamedPool(t, feedApplication)
	inboxPool := openNamedPool(t, inboxApplication)
	feedRepo := repository.NewPGXReaderVNextRepository(feedPool)
	inboxRepo := repository.NewPGXReaderVNextRepository(inboxPool)
	type feedOutcome struct {
		result model.ReaderFeedFeedback
		err    error
	}
	feedDone := make(chan feedOutcome, 1)
	go func() {
		result, callErr := feedRepo.FeedbackFeed(ctx, "subscription:"+resaveItem.String(), "save")
		feedDone <- feedOutcome{result: result, err: callErr}
	}()
	waitForPostgresLock(t, ctx, pool, feedApplication)

	inboxDone := make(chan struct {
		linkID uuid.UUID
		err    error
	}, 1)
	go func() {
		confirmed, callErr := inboxRepo.ConfirmInbox(ctx, inboxID)
		inboxDone <- struct {
			linkID uuid.UUID
			err    error
		}{linkID: confirmed, err: callErr}
	}()

	// Inbox may wait on the shared representation prelock rather than the
	// canonical advisory lock, but either wait proves it cannot pass the Feed
	// transaction and observe a half-restored Link.
	waitForPostgresLock(t, ctx, pool, inboxApplication)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release Trash restore blocker: %v", err)
	}

	feed := <-feedDone
	if feed.err != nil || feed.result.Association == nil || feed.result.Association.LinkID != linkID {
		t.Fatalf("concurrent Feed restore = %#v, %v; want %s", feed.result, feed.err, linkID)
	}
	inbox := <-inboxDone
	if inbox.err != nil || inbox.linkID != linkID {
		t.Fatalf("concurrent Inbox restore = %s, %v; want %s", inbox.linkID, inbox.err, linkID)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	var links, associations int
	var feedManaged bool
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE source_key=$1`, rawURL).Scan(&links); err != nil {
		t.Fatalf("count canonical Links: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reader_feed_saves WHERE link_id=$1`, linkID).Scan(&associations); err != nil {
		t.Fatalf("count restored Feed associations: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, linkID).Scan(&feedManaged); err != nil {
		t.Fatalf("read concurrent restore ownership: %v", err)
	}
	if links != 1 || associations != 1 || feedManaged {
		t.Fatalf("concurrent restore state = links:%d associations:%d feed_managed:%v, want 1/1/false", links, associations, feedManaged)
	}
}

func TestReaderBulkConfirmRestoresTrashLinksAtomically(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	confirmations := make([]model.ReaderInboxBulkConfirmation, 0, 2)
	wantLinks := make(map[uuid.UUID]uuid.UUID, 2)
	for index, suffix := range []string{"a", "b"} {
		rawURL := "https://reader-vnext.example/bulk-trash-" + suffix
		feedItem := seedReaderFeedSaveItem(t, pool, rawURL, "bulk-trash-"+suffix)
		saved, err := reader.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "save")
		if err != nil || saved.Association == nil {
			t.Fatalf("seed bulk Feed save %d = %#v, %v", index, saved, err)
		}
		if _, err := reader.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "unsave"); err != nil {
			t.Fatalf("seed bulk Feed trash %d: %v", index, err)
		}
		inboxID := seedReaderVNextInbox(t, pool, rawURL, "Bulk restore "+suffix, "Inbox body "+suffix, "Inbox summary")
		confirmations = append(confirmations, model.ReaderInboxBulkConfirmation{ID: inboxID})
		wantLinks[inboxID] = saved.Association.LinkID
	}

	results, err := reader.BulkConfirmInbox(ctx, confirmations)
	if err != nil {
		t.Fatalf("BulkConfirmInbox(): %v", err)
	}
	if len(results) != len(confirmations) {
		t.Fatalf("bulk confirmation results = %d, want %d", len(results), len(confirmations))
	}
	for _, result := range results {
		want := wantLinks[result.ID]
		if result.Status != "confirmed" || result.LinkID == nil || *result.LinkID != want {
			t.Fatalf("bulk confirmation result = %#v, want restored Link %s", result, want)
		}
		assertReaderFeedLinkLive(t, pool, want, true)
	}
}

func TestReaderConfirmTrashRestoreRollsBackOnCategoryFailure(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	rawURL := "https://reader-vnext.example/trash-confirm-rollback"
	feedItem := seedReaderFeedSaveItem(t, pool, rawURL, "trash-confirm-rollback")
	saved, err := reader.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "save")
	if err != nil || saved.Association == nil {
		t.Fatalf("seed Feed save = %#v, %v", saved, err)
	}
	linkID := saved.Association.LinkID
	if _, err := reader.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "unsave"); err != nil {
		t.Fatalf("seed Feed trash: %v", err)
	}
	inboxID := seedReaderVNextInbox(t, pool, rawURL, "Rollback adoption", "Inbox-owned body", "Inbox summary")
	category, err := reader.CreateCategory(ctx, "rollback category")
	if err != nil {
		t.Fatalf("create rollback category: %v", err)
	}
	if err := reader.SetCategoryMembership(ctx, category.ID, "inbox", inboxID.String(), true); err != nil {
		t.Fatalf("seed Inbox category: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_trash_confirm_category_move() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected Trash confirmation category failure'; END;
		$$;
		CREATE TRIGGER fail_trash_confirm_category_move
		BEFORE UPDATE ON reader_categorizables
		FOR EACH ROW EXECUTE FUNCTION fail_trash_confirm_category_move()`); err != nil {
		t.Fatalf("install Trash confirmation failure trigger: %v", err)
	}
	if _, err := reader.ConfirmInbox(ctx, inboxID); err == nil {
		t.Fatal("ConfirmInbox() with injected category failure succeeded")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_trash_confirm_category_move ON reader_categorizables`); err != nil {
		t.Fatalf("drop Trash confirmation failure trigger: %v", err)
	}

	assertReaderFeedLinkLive(t, pool, linkID, false)
	var status, content string
	var feedManaged bool
	if err := pool.QueryRow(ctx, `SELECT status FROM reader_inbox WHERE id=$1`, inboxID).Scan(&status); err != nil {
		t.Fatalf("read rolled-back Inbox status: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(content_document,content,''),feed_managed FROM links WHERE id=$1`, linkID).Scan(&content, &feedManaged); err != nil {
		t.Fatalf("read rolled-back Trash Link: %v", err)
	}
	if status != "pending" || !feedManaged || content == "Inbox-owned body" {
		t.Fatalf("rolled-back confirmation = status:%s feed_managed:%v content:%q", status, feedManaged, content)
	}
}
