package repository

import "webtag/internal/model"

const (
	readerFeedPendingConfirmationWeight = 100
	readerFeedSavedLibraryWeight        = 70
	readerFeedSubscriptionRecentWeight  = 40
	readerFeedUnreadWeight              = 20
	readerFeedReadLaterWeight           = 10
)

func scoreReaderFeedItem(item model.ReaderFeedItem) model.ReaderFeedItem {
	switch item.Source {
	case "inbox":
		item.Score = readerFeedPendingConfirmationWeight
	case "reading":
		item.Score = readerFeedSavedLibraryWeight
	case "subscription":
		item.Score = readerFeedSubscriptionRecentWeight
	default:
		item.Score = 0
	}
	if !item.Read {
		item.Score += readerFeedUnreadWeight
	}
	if item.ReadLater {
		item.Score += readerFeedReadLaterWeight
	}
	return item
}

func scoreReaderFeedItems(items []model.ReaderFeedItem) []model.ReaderFeedItem {
	scored := make([]model.ReaderFeedItem, len(items))
	for index, item := range items {
		scored[index] = scoreReaderFeedItem(item)
	}
	return scored
}
