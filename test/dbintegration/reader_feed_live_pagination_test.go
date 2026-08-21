package dbintegration

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestReaderFeedLiveCursorSurvivesBoundaryDeletionAndConcurrentInsert(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()
	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

	ids := make([]uuid.UUID, 4)
	for index := range ids {
		ids[index] = seedReaderFeedSaveItem(
			t,
			pool,
			"https://feed-live-pagination.example.test/item-"+string(rune('a'+index)),
			"live-pagination-"+string(rune('a'+index)),
		)
		if _, err := pool.Exec(ctx,
			`UPDATE feed_items SET created_at=$2,published_at=NULL WHERE id=$1`,
			ids[index], base.Add(time.Duration(4-index)*time.Hour),
		); err != nil {
			t.Fatalf("set feed item %d timestamp: %v", index, err)
		}
	}

	first, err := repo.ListFeedWithSources(ctx, "recommended", "", []string{"subscription"}, 2)
	if err != nil {
		t.Fatalf("ListFeedWithSources(first): %v", err)
	}
	wantFirst := []string{"subscription:" + ids[0].String(), "subscription:" + ids[1].String()}
	assertReaderFeedKeys(t, first.Items, wantFirst)
	if first.NextCursor == "" {
		t.Fatal("first page returned no cursor")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM feed_items WHERE id=$1`, ids[1]); err != nil {
		t.Fatalf("delete cursor boundary item: %v", err)
	}
	newID := seedReaderFeedSaveItem(
		t,
		pool,
		"https://feed-live-pagination.example.test/new",
		"live-pagination-new",
	)
	if _, err := pool.Exec(ctx,
		`UPDATE feed_items SET created_at=$2,published_at=NULL WHERE id=$1`,
		newID, base.Add(5*time.Hour),
	); err != nil {
		t.Fatalf("set concurrent feed item timestamp: %v", err)
	}

	second, err := repo.ListFeedWithSources(
		ctx,
		"recommended",
		first.NextCursor,
		[]string{"subscription"},
		10,
	)
	if err != nil {
		t.Fatalf("ListFeedWithSources(second): %v", err)
	}
	wantSecond := []string{"subscription:" + ids[2].String(), "subscription:" + ids[3].String()}
	assertReaderFeedKeys(t, second.Items, wantSecond)
	if second.NextCursor != "" {
		t.Fatalf("second page cursor = %q, want empty", second.NextCursor)
	}
}

func assertReaderFeedKeys(t *testing.T, items []model.ReaderFeedItem, want []string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("feed keys count = %d, want %d: %#v", len(items), len(want), items)
	}
	for index := range items {
		if items[index].Key != want[index] {
			t.Fatalf("feed key %d = %q, want %q", index, items[index].Key, want[index])
		}
	}
}
