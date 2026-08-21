package repository

import (
	"testing"

	"webtag/internal/model"
)

func TestReaderFeedScoreUsesOnlyCurrentRankingInputs(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		read      bool
		readLater bool
		want      int
	}{
		{name: "pending unread", source: "inbox", want: 120},
		{name: "reading unread later", source: "reading", readLater: true, want: 100},
		{name: "reading read", source: "reading", read: true, want: 70},
		{name: "subscription unread", source: "subscription", want: 60},
		{name: "subscription read later", source: "subscription", read: true, readLater: true, want: 50},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := scoreReaderFeedItem(model.ReaderFeedItem{
				Source:    test.source,
				Read:      test.read,
				ReadLater: test.readLater,
			})
			if item.Score != test.want {
				t.Fatalf("score = %d, want %d", item.Score, test.want)
			}
		})
	}
}

func TestReaderFeedScoreItemsPreservesOrder(t *testing.T) {
	items := scoreReaderFeedItems([]model.ReaderFeedItem{
		{Key: "subscription:a", Source: "subscription"},
		{Key: "link:b", Source: "reading", Read: true},
	})
	if len(items) != 2 || items[0].Key != "subscription:a" || items[0].Score != 60 || items[1].Key != "link:b" || items[1].Score != 70 {
		t.Fatalf("scored items = %#v", items)
	}
}
