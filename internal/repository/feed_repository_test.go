package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestBuildFeedItemWhereSupportsUngrouped(t *testing.T) {
	t.Parallel()
	where, args := buildFeedItemWhere(FeedItemFilter{View: "unread", Ungrouped: true, Query: "postgres"})
	for _, want := range []string{"s.active AND fi.read_at IS NULL", "s.folder_id IS NULL", "fi.title ILIKE $1"} {
		if !containsSQL(where, want) {
			t.Fatalf("where = %q, missing %q", where, want)
		}
	}
	if len(args) != 1 || args[0] != "%postgres%" {
		t.Fatalf("args = %#v", args)
	}
}

func TestGetFeedItemRequiresActiveOrPreservedSubscription(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repository := NewPGXFeedRepository(mock, mock)
	itemID := uuid.New()
	mock.ExpectQuery(`(?s)JOIN feed_subscriptions s.*\(\$2 OR s\.active OR fi\.starred OR fi\.read_later\)`).
		WithArgs(itemID, false).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	item, found, err := repository.GetItem(context.Background(), itemID, false)
	if err != nil {
		t.Fatalf("GetItem() error = %v", err)
	}
	if found || item.ID != uuid.Nil {
		t.Fatalf("GetItem() = (%#v,%v), want hidden", item, found)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateFeedFolderMapsUniqueViolationToStableSentinel(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repository := NewPGXFeedRepository(mock, mock)
	folderID := uuid.New()
	mock.ExpectQuery(`UPDATE feed_folders SET name`).
		WithArgs(folderID, "Duplicate").
		WillReturnError(&pgconn.PgError{Code: "23505"})

	_, err = repository.UpdateFolder(context.Background(), folderID, "Duplicate")
	if !errors.Is(err, ErrFeedFolderNameConflict) {
		t.Fatalf("UpdateFolder() error = %v, want ErrFeedFolderNameConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func containsSQL(query, fragment string) bool {
	return len(query) >= len(fragment) && (query == fragment || stringContains(query, fragment))
}

func stringContains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
