package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
)

func TestReaderFeedRecommendedKeysetSurvivesBoundaryDeletion(t *testing.T) {
	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	items := []model.ReaderFeedItem{
		{Key: "link:a", Source: "reading", Score: 100, CreatedAt: base.Add(4 * time.Hour)},
		{Key: "link:b", Source: "reading", Score: 90, CreatedAt: base.Add(3 * time.Hour)},
		{Key: "link:c", Source: "reading", Score: 90, CreatedAt: base.Add(2 * time.Hour)},
		{Key: "link:d", Source: "reading", Score: 80, CreatedAt: base.Add(time.Hour)},
	}
	sortReaderFeedItems(items, "recommended")
	first := readerFeedPage(items, "recommended", []string{"reading"}, readerFeedCursor{}, 2)
	if got := feedPageKeys(first); !equalFeedKeys(got, []string{"link:a", "link:b"}) || first.NextCursor == "" {
		t.Fatalf("first page = keys %v cursor %q", got, first.NextCursor)
	}
	cursor, err := feedCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	// A live write before the boundary and deletion of the boundary row must not
	// shift the next page back to offset zero or require the old row to exist.
	items = []model.ReaderFeedItem{
		{Key: "link:new", Source: "reading", Score: 200, CreatedAt: base.Add(5 * time.Hour)},
		items[0],
		items[2],
		items[3],
	}
	sortReaderFeedItems(items, "recommended")
	second := readerFeedPage(items, "recommended", []string{"reading"}, cursor, 10)
	if got := feedPageKeys(second); !equalFeedKeys(got, []string{"link:c", "link:d"}) {
		t.Fatalf("second page keys = %v", got)
	}
}

func TestReaderFeedChronologicalCursorUsesUniqueFinalKey(t *testing.T) {
	eventAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	linkID := uuid.New()
	items := []model.ReaderFeedItem{
		{Key: "subscription:b", Source: "subscription", URL: "https://b.test", LinkID: &linkID, Score: 1, PublishedAt: &eventAt, CreatedAt: eventAt},
		{Key: "subscription:a", Source: "subscription", URL: "https://a.test", LinkID: &linkID, Score: 1, PublishedAt: &eventAt, CreatedAt: eventAt},
		{Key: "link:z", Source: "reading", URL: "https://z.test", Score: 1, CreatedAt: eventAt.Add(-time.Hour)},
	}
	sortReaderFeedItems(items, "chronological")
	first := readerFeedPage(items, "chronological", nil, readerFeedCursor{}, 1)
	cursor, err := feedCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	second := readerFeedPage(items, "chronological", nil, cursor, 1)
	if got := feedPageKeys(first); !equalFeedKeys(got, []string{"subscription:a"}) {
		t.Fatalf("first page keys = %v", got)
	}
	if got := feedPageKeys(second); !equalFeedKeys(got, []string{"subscription:b"}) {
		t.Fatalf("second page keys = %v", got)
	}
}

func TestReaderFeedCursorBindsModeAndSources(t *testing.T) {
	item := model.ReaderFeedItem{
		Key:       "link:a",
		Source:    "reading",
		Score:     90,
		CreatedAt: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
	}
	cursor := makeFeedCursor("recommended", []string{"reading"}, item)
	repo := &PGXReaderVNextRepository{}

	if _, err := repo.ListFeedWithSources(context.Background(), "chronological", cursor, []string{"reading"}, 10); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("mode mismatch error = %v", err)
	}
	if _, err := repo.ListFeedWithSources(context.Background(), "recommended", cursor, []string{"inbox"}, 10); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("source mismatch error = %v", err)
	}
}

func TestReaderFeedSourceNormalizationIsCanonical(t *testing.T) {
	got, err := normalizeRepositoryFeedSources([]string{"subscription, saved", "pending", "subscription"})
	if err != nil {
		t.Fatalf("normalize sources: %v", err)
	}
	want := []string{"inbox", "reading", "subscription"}
	if !equalFeedKeys(got, want) {
		t.Fatalf("normalized sources = %v, want %v", got, want)
	}
	if _, err := normalizeRepositoryFeedSources([]string{"archive"}); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("unknown source error = %v", err)
	}
}

func TestReaderFeedCursorRejectsMalformedWire(t *testing.T) {
	for _, raw := range []string{"not-base64", "e30"} {
		if _, err := feedCursor(raw); !errors.Is(err, ErrInvalidReaderCursor) {
			t.Fatalf("feedCursor(%q) error = %v", raw, err)
		}
	}
}

func feedPageKeys(page *model.ReaderFeedPage) []string {
	keys := make([]string, len(page.Items))
	for index, item := range page.Items {
		keys[index] = item.Key
	}
	return keys
}

func equalFeedKeys(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
