package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestRefreshActivityUsesReadingCollectionEventsAndCleansStaleRowsAtomically(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*library_kind='reading'.*status='done'.*reader_tag_activity.*ON CONFLICT.*DELETE FROM reader_tag_activity.*NOT EXISTS").
		WillReturnResult(pgxmock.NewResult("WITH", 1))
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*library_kind='reading'.*status='done'.*reader_domain_activity.*ON CONFLICT.*DELETE FROM reader_domain_activity.*NOT EXISTS").
		WillReturnResult(pgxmock.NewResult("WITH", 1))
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	if err := repo.RefreshActivity(context.Background()); err != nil {
		t.Fatalf("RefreshActivity() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestRefreshActivityRollsBackWhenDomainProjectionFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*library_kind='reading'.*status='done'.*reader_tag_activity").
		WillReturnResult(pgxmock.NewResult("WITH", 1))
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*library_kind='reading'.*status='done'.*reader_domain_activity").
		WillReturnError(context.Canceled)
	mock.ExpectRollback()

	repo := NewPGXReaderVNextRepository(mock)
	if err := repo.RefreshActivity(context.Background()); err == nil {
		t.Fatal("RefreshActivity() error = nil, want rollback error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestRefreshActivityRollsBackWhenActivityFenceFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	fenceErr := errors.New("activity fence unavailable")
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(hashtextextended\\('reader-activity',0\\)\\)").
		WillReturnError(fenceErr)
	mock.ExpectRollback()

	err = NewPGXReaderVNextRepository(mock).RefreshActivity(context.Background())
	if !errors.Is(err, fenceErr) {
		t.Fatalf("RefreshActivity() error = %v, want activity fence error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListActivityUsesInstallationScopeStableSeekAndLimitPlusOne(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	newest := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT kind,activity_key,last_at,normalized_key.*reader_tag_activity.*ORDER BY last_at DESC.*LIMIT \\$1").
		WithArgs(3).
		WillReturnRows(mock.NewRows([]string{"kind", "activity_key", "last_at", "normalized_key"}).
			AddRow("tag", "Alpha", newest, "alpha").
			AddRow("tag", "beta", newest, "beta").
			AddRow("tag", "third", newest.Add(-time.Hour), "third"))

	page, err := NewPGXReaderVNextRepository(mock).ListActivity(
		context.Background(),
		model.ReaderActivityQuery{Kind: model.ReaderActivityKindTag, Limit: 2},
	)
	if err != nil {
		t.Fatalf("ListActivity() error = %v", err)
	}
	if !page.HasMore || len(page.Items) != 2 || page.Items[0].Key != "Alpha" || page.Items[0].NormalizedKey != "alpha" || page.Items[1].Key != "beta" {
		t.Fatalf("page = %#v, want stable two-row page with has_more", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func expectReaderActivityFence(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(hashtextextended\\('reader-activity',0\\)\\)").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}
