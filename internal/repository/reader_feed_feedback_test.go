package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestFeedbackFeedValidatesAndCommitsLinkStateWithFeedback(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM links WHERE id=$1)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("(?s)INSERT INTO reader_feed_feedback").
		WithArgs("link:"+linkID.String(), "save").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.FeedbackFeed(context.Background(), "link:"+linkID.String(), "save"); err != nil {
		t.Fatalf("FeedbackFeed() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestFeedbackFeedDoesNotCreateEngagementForMissingLink(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM links WHERE id=$1)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectRollback()

	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.FeedbackFeed(context.Background(), "link:"+linkID.String(), "save"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FeedbackFeed() error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestFeedbackFeedRejectsMalformedItemBeforeTransaction(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.FeedbackFeed(context.Background(), "link:not-a-uuid", "save"); !errors.Is(err, ErrInvalidReaderFeedItem) {
		t.Fatalf("FeedbackFeed() error = %v, want ErrInvalidReaderFeedItem", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database operation: %v", err)
	}
}

func TestUnsaveSubscriptionTrashesLastFeedManagedClaimRegardlessOfCreator(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	itemID, linkID := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT link_id,created_link FROM reader_feed_saves").WithArgs(itemID).
		WillReturnRows(mock.NewRows([]string{"link_id", "created_link"}).AddRow(linkID, false))
	mock.ExpectQuery("SELECT feed_managed,status FROM links.*FOR UPDATE").WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"feed_managed", "status"}).AddRow(true, model.LinkStatusDone))
	mock.ExpectExec("DELETE FROM reader_feed_saves").WithArgs(itemID, linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec("UPDATE feed_items SET link_id=NULL").WithArgs(itemID, linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("SELECT EXISTS.*reader_feed_saves").WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedParseAttemptsSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(terminalizeDeletedTranslationAttemptsSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta(deleteLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectLinkThoughtTodoProjectionRefresh(mock, linkID)

	association, err := NewPGXReaderVNextRepository(mock).unsaveSubscriptionFeedItem(t.Context(), mock, itemID)
	if err != nil || association == nil || association.LinkID != linkID || association.CreatedLink {
		t.Fatalf("unsaveSubscriptionFeedItem() = %+v, %v", association, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUnsaveSubscriptionPreservesIndependentLibraryLink(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	itemID, linkID := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT link_id,created_link FROM reader_feed_saves").WithArgs(itemID).
		WillReturnRows(mock.NewRows([]string{"link_id", "created_link"}).AddRow(linkID, true))
	mock.ExpectQuery("SELECT feed_managed,status FROM links.*FOR UPDATE").WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"feed_managed", "status"}).AddRow(false, model.LinkStatusDone))
	mock.ExpectExec("DELETE FROM reader_feed_saves").WithArgs(itemID, linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec("UPDATE feed_items SET link_id=NULL").WithArgs(itemID, linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("SELECT EXISTS.*reader_feed_saves").WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	if _, err := NewPGXReaderVNextRepository(mock).unsaveSubscriptionFeedItem(t.Context(), mock, itemID); err != nil {
		t.Fatalf("unsaveSubscriptionFeedItem() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFeedbackFeedResaveRollsBackRestoreWhenAssociationWriteFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	itemID, linkID := uuid.New(), uuid.New()
	rawURL := "https://example.com/feed-restore"
	wantErr := errors.New("association write failed")
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery("SELECT EXISTS.*feed_items").WithArgs(itemID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("SELECT url,COALESCE.*FROM feed_items.*FOR UPDATE").WithArgs(itemID).
		WillReturnRows(mock.NewRows([]string{"url", "title", "summary"}).AddRow(rawURL, "Feed item", "Summary"))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs("canonical-link:" + rawURL).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery("SELECT feed_item_id,link_id,created_link FROM reader_feed_saves").WithArgs(itemID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(findInboxSavedLinkSQL)).WithArgs(rawURL).
		WillReturnRows(mock.NewRows([]string{"id", "trashed", "feed_managed"}).AddRow(linkID, true, true))
	mock.ExpectQuery(regexp.QuoteMeta(lockLinkForRestoreSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"status", "deleted_at", "body", "content_revision", "feed_managed"}).
			AddRow(model.LinkStatusDone, time.Now(), "saved body", int64(4), true))
	mock.ExpectExec("UPDATE links SET deleted_at=NULL").WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectEmptyLinkThoughtRestore(mock, linkID)
	mock.ExpectQuery("INSERT INTO reader_feed_saves").WithArgs(itemID, linkID, false).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	_, err = NewPGXReaderVNextRepository(mock).FeedbackFeed(t.Context(), "subscription:"+itemID.String(), "save")
	if !errors.Is(err, wantErr) {
		t.Fatalf("FeedbackFeed() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
