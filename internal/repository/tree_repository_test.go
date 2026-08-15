package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestTreeRepositoryLookupByURLsOnlyReturnsDoneRows(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXTreeRepository(mock)
	urls := []string{"https://example.com/", "https://example.com/posts/"}
	doneURL := "https://example.com/"
	domain := "example.com"
	contentType := "listing"
	pathDepth := 0
	parentPath := "/"
	createdAt := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	doneID := uuid.MustParse("12345678-90ab-cdef-1234-567890abcdef")

	mock.ExpectQuery(regexp.QuoteMeta(
		lookupTreeLinksByURLsSQL,
	)).
		WithArgs(urls).
		WillReturnRows(
			mock.NewRows(linkColumns()).AddRow(
				doneID,
				doneURL,
				"url",
				doneURL,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				false,
				nil,
				model.LinkStatusDone,
				nil,
				nil,
				domain,
				contentType,
				"auto",
				"auto",
				"reading",
				"migration",
				false,
				nil,
				nil,
				nil,
				nil,
				nil,
				int64(1),
				int64(1),
				string(model.ContentSourceFetched),
				// PF6：content_revision 之后是 content_source / has_content / content_cjk_chars / content_words。
				false,
				0,
				0,
				createdAt,
				nil,
				nil,
				nil,
				pathDepth,
				parentPath,
				nil,
				createdAt,
				updatedAt,
			),
		)

	got, err := repo.LookupByURLs(context.Background(), urls)
	if err != nil {
		t.Fatalf("LookupByURLs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LookupByURLs() returned %d rows, want 1", len(got))
	}
	if got[doneURL] == nil || got[doneURL].ID != doneID {
		t.Fatalf("LookupByURLs()[%q] = %#v, want ID %s", doneURL, got[doneURL], doneID)
	}
	if _, ok := got["https://example.com/posts/"]; ok {
		t.Fatal("LookupByURLs() returned a non-done ancestor")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
