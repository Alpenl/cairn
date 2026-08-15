package repository

import (
	"fmt"
	"testing"

	"webtag/internal/model"
)

func TestSelectFeedRefreshItemsDoesNotBackfillInitialHistory(t *testing.T) {
	t.Parallel()
	items := feedItemsForTest(120, "old")
	initial, boundary := selectFeedRefreshItems(items, nil, "", true)
	if len(initial) != 50 || boundary != "old-000" {
		t.Fatalf("initial len=%d boundary=%q", len(initial), boundary)
	}
	known := make(map[string]struct{}, len(initial))
	for _, item := range initial {
		known[item.ExternalID] = struct{}{}
	}
	next, nextBoundary := selectFeedRefreshItems(items, known, boundary, false)
	if len(next) != 0 || nextBoundary != boundary {
		t.Fatalf("unchanged refresh backfilled len=%d boundary=%q", len(next), nextBoundary)
	}
}

func TestSelectFeedRefreshItemsImportsOnlyNewPrefix(t *testing.T) {
	t.Parallel()
	old := feedItemsForTest(50, "old")
	items := append(feedItemsForTest(3, "new"), old...)
	known := make(map[string]struct{}, len(old))
	for _, item := range old {
		known[item.ExternalID] = struct{}{}
	}
	selected, boundary := selectFeedRefreshItems(items, known, old[0].ExternalID, false)
	if len(selected) != 3 || selected[0].ExternalID != "new-000" || boundary != "new-000" {
		t.Fatalf("selected=%#v boundary=%q", selected, boundary)
	}
}

func TestSelectFeedRefreshItemsDrainsMoreThan100NewAcrossRefreshes(t *testing.T) {
	t.Parallel()
	old := feedItemsForTest(1, "old")
	newItems := feedItemsForTest(150, "new")
	items := append(append([]model.FeedItem(nil), newItems...), old...)
	known := map[string]struct{}{old[0].ExternalID: {}}
	first, boundary := selectFeedRefreshItems(items, known, old[0].ExternalID, false)
	if len(first) != 100 || boundary != old[0].ExternalID {
		t.Fatalf("first len=%d boundary=%q", len(first), boundary)
	}
	for _, item := range first {
		known[item.ExternalID] = struct{}{}
	}
	second, boundary := selectFeedRefreshItems(items, known, boundary, false)
	if len(second) != 50 || boundary != newItems[0].ExternalID {
		t.Fatalf("second len=%d boundary=%q", len(second), boundary)
	}
}

func TestSelectFeedRefreshItemsDrainsAfterInitiallyEmptyFeed(t *testing.T) {
	t.Parallel()
	_, boundary := selectFeedRefreshItems(nil, nil, "", true)
	if boundary != feedEndBoundary {
		t.Fatalf("empty initial boundary = %q", boundary)
	}
	items := feedItemsForTest(150, "new")
	known := make(map[string]struct{})
	first, boundary := selectFeedRefreshItems(items, known, boundary, false)
	if len(first) != 100 || boundary != feedEndBoundary {
		t.Fatalf("first len=%d boundary=%q", len(first), boundary)
	}
	for _, item := range first {
		known[item.ExternalID] = struct{}{}
	}
	second, boundary := selectFeedRefreshItems(items, known, boundary, false)
	if len(second) != 50 || boundary != items[0].ExternalID {
		t.Fatalf("second len=%d boundary=%q", len(second), boundary)
	}
}

func feedItemsForTest(count int, prefix string) []model.FeedItem {
	items := make([]model.FeedItem, count)
	for index := range items {
		items[index].ExternalID = fmt.Sprintf("%s-%03d", prefix, index)
	}
	return items
}
