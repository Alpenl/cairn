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

func TestLibraryReviewRepositoryListsInstallScopedPendingRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id, linkID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+libraryReviewColumns+" FROM library_review_items WHERE TRUE AND status=$1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3")).WithArgs(model.LibraryReviewStatusPending, 20, 0).WillReturnRows(pgxmock.NewRows([]string{"id", "kind", "link_id", "site_id", "payload", "status", "revision", "created_at", "resolved_at"}).AddRow(id.String(), string(model.LibraryReviewKindClassificationUncertain), linkID.String(), nil, []byte(`{"candidate":"site"}`), string(model.LibraryReviewStatusPending), int64(1), now, nil))
	status := model.LibraryReviewStatusPending
	got, err := NewPGXLibraryReviewRepository(mock).ListLibraryReviews(context.Background(), ListLibraryReviewsParams{Status: &status, Limit: 20})
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].LinkID == nil || *got[0].LinkID != linkID {
		t.Fatalf("ListLibraryReviews() = %#v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryReviewRepositoryResolveUsesPendingRevisionCAS(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id, now := uuid.New(), time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE library_review_items SET status=$1, revision=revision+1, resolved_at=NOW() WHERE id=$2 AND revision=$3 AND status='pending' RETURNING "+libraryReviewColumns)).WithArgs(model.LibraryReviewStatusDismissed, id, int64(4)).WillReturnRows(pgxmock.NewRows([]string{"id", "kind", "link_id", "site_id", "payload", "status", "revision", "created_at", "resolved_at"}).AddRow(id.String(), string(model.LibraryReviewKindNoteConflict), nil, nil, []byte(`{}`), string(model.LibraryReviewStatusDismissed), int64(5), now, now))
	got, err := NewPGXLibraryReviewRepository(mock).ResolveLibraryReview(context.Background(), ResolveLibraryReviewParams{ID: id, Revision: 4, Status: model.LibraryReviewStatusDismissed})
	if err != nil || got.Status != model.LibraryReviewStatusDismissed || got.Revision != 5 {
		t.Fatalf("ResolveLibraryReview() = %#v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
