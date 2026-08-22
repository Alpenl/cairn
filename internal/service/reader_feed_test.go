package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
)

type readerFeedStoreStub struct {
	ReaderLibraryStore
	page            *model.ReaderFeedPage
	mode            string
	after           string
	filteredSources []string
	limit           int
	listCalls       int
	feedbackItemKey string
	feedbackAction  string
	feedbackCalls   int
	err             error
}

func (s *readerFeedStoreStub) ListFeedWithSources(_ context.Context, mode, after string, sources []string, limit int) (*model.ReaderFeedPage, error) {
	s.listCalls++
	s.mode = mode
	s.after = after
	s.filteredSources = append([]string(nil), sources...)
	s.limit = limit
	return s.page, s.err
}

func (s *readerFeedStoreStub) FeedbackFeed(_ context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	s.feedbackCalls++
	s.feedbackItemKey = itemKey
	s.feedbackAction = action
	feedback := model.ReaderFeedFeedback{ItemKey: itemKey, Action: action}
	if action == "save" {
		linkID := uuid.New()
		feedback.LinkID = &linkID
	}
	return feedback, nil
}

func TestReaderFeedServiceMapsLivePage(t *testing.T) {
	linkID, inboxID, feedItemID, linkedID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	publishedAt := createdAt.Add(time.Hour)
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{
		Mode:       "recommended",
		NextCursor: "live-cursor",
		Items: []model.ReaderFeedItem{
			{Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID, URL: "https://reading.test", CreatedAt: createdAt},
			{Key: "inbox:" + inboxID.String(), Source: "inbox", InboxID: &inboxID, URL: "https://inbox.test", CreatedAt: createdAt},
			{Key: "subscription:" + feedItemID.String(), Source: "subscription", FeedItemID: &feedItemID, LinkID: &linkedID, URL: "https://feed.test", PublishedAt: &publishedAt, CreatedAt: createdAt},
		},
	}}

	response, err := newReaderTestFeatureSet(readerTestStores(store), nil).FeedWithSources(
		context.Background(),
		"recommended",
		"cursor-before",
		[]string{"subscription, saved", "subscription"},
		30,
	)
	if err != nil {
		t.Fatalf("FeedWithSources() error = %v", err)
	}
	if response.Mode != "recommended" || response.NextCursor != "live-cursor" || len(response.Items) != 3 {
		t.Fatalf("response = %#v", response)
	}
	if store.mode != "recommended" || store.after != "cursor-before" || store.limit != 30 ||
		len(store.filteredSources) != 2 || store.filteredSources[0] != "reading" || store.filteredSources[1] != "subscription" {
		t.Fatalf("store request = mode %q after %q sources %v limit %d", store.mode, store.after, store.filteredSources, store.limit)
	}
	if got := response.Items[0].ResourceIdentity(); got != "link:"+linkID.String() {
		t.Fatalf("reading resource_key = %q", got)
	}
	if got := response.Items[1].ResourceIdentity(); got != "inbox:"+inboxID.String() {
		t.Fatalf("inbox resource_key = %q", got)
	}
	if got := response.Items[2].ResourceIdentity(); got != "link:"+linkedID.String() {
		t.Fatalf("subscription resource_key = %q", got)
	}
	if !response.Items[2].VisibleEventAt().Equal(publishedAt) {
		t.Fatalf("subscription event_at = %s, want %s", response.Items[2].VisibleEventAt(), publishedAt)
	}
}

func TestReaderFeedServiceRejectsInvalidRequestBeforeStore(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		sources []string
	}{
		{name: "mode", mode: "ranking-v2"},
		{name: "source", mode: "recommended", sources: []string{"archive"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &readerFeedStoreStub{}
			_, err := newReaderTestFeatureSet(readerTestStores(store), nil).FeedWithSources(context.Background(), test.mode, "", test.sources, 30)
			if err == nil || store.listCalls != 0 {
				t.Fatalf("error = %v, list calls = %d", err, store.listCalls)
			}
		})
	}
}

func TestReaderFeedServiceRejectsNilPage(t *testing.T) {
	_, err := newReaderTestFeatureSet(readerTestStores(&readerFeedStoreStub{}), nil).FeedWithSources(context.Background(), "", "", nil, 30)
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("error = %v, want 500", err)
	}
}

func TestReaderFeedServiceUsesCanonicalFeedbackIdentity(t *testing.T) {
	itemKey := "subscription:" + uuid.NewString()
	store := &readerFeedStoreStub{}
	response, err := newReaderTestFeatureSet(readerTestStores(store), nil).FeedbackFeed(context.Background(), "  "+itemKey+"  ", "save")
	if err != nil || response.LinkID == nil {
		t.Fatalf("FeedbackFeed() error = %v", err)
	}
	if store.feedbackItemKey != itemKey || store.feedbackAction != "save" {
		t.Fatalf("feedback command = (%q, %q)", store.feedbackItemKey, store.feedbackAction)
	}
}

func TestReaderFeedServiceRejectsNonSubscriptionSave(t *testing.T) {
	for _, source := range []string{"link", "inbox"} {
		store := &readerFeedStoreStub{}
		_, err := newReaderTestFeatureSet(readerTestStores(store), nil).FeedbackFeed(context.Background(), source+":"+uuid.NewString(), "save")
		if err == nil || store.feedbackCalls != 0 {
			t.Fatalf("source %s: error = %v, feedback calls = %d", source, err, store.feedbackCalls)
		}
	}
}

func TestReaderFeedServiceRejectsRemovedRecommendationFeedback(t *testing.T) {
	store := &readerFeedStoreStub{}
	_, err := newReaderTestFeatureSet(readerTestStores(store), nil).FeedbackFeed(
		context.Background(),
		"subscription:"+uuid.NewString(),
		"not_interested",
	)
	if err == nil || store.feedbackCalls != 0 {
		t.Fatalf("error = %v, feedback calls = %d", err, store.feedbackCalls)
	}
}
