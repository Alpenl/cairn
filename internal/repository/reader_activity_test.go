package repository

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestListActivityAggregatesReadingLinksWithStableSeekAndLimitPlusOne(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	newest := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT kind,activity_key,last_at,normalized_key.*unnest\\(l.tags\\).*library_kind='reading'.*status='done'.*deleted_at IS NULL.*GROUP BY source.tag.*ORDER BY last_at DESC.*LIMIT \\$1").
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
