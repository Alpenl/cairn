package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	feedremote "webtag/internal/feed"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type feedStoreStub struct {
	FeedStore
	listOverviewFn       func(context.Context, string) (model.FeedSubscriptionsResponse, error)
	createSubscriptionFn func(context.Context, string, *uuid.UUID, bool, string) (model.FeedSubscription, error)
	getSubscriptionFn    func(context.Context, uuid.UUID) (model.FeedSubscription, bool, error)
	getItemFn            func(context.Context, uuid.UUID, bool) (model.FeedItem, bool, error)
	associateFn          func(context.Context, uuid.UUID, uuid.UUID) error
	completeRefreshFn    func(context.Context, repository.FeedRefreshSuccess) (int, error)
	failRefreshFn        func(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time, string) error
	createFolderFn       func(context.Context, string) (model.FeedFolder, error)
}

func (s *feedStoreStub) ListOverview(ctx context.Context, rawURL string) (model.FeedSubscriptionsResponse, error) {
	return s.listOverviewFn(ctx, rawURL)
}

func (s *feedStoreStub) CreateSubscription(ctx context.Context, rawURL string, folderID *uuid.UUID, setFolder bool, title string) (model.FeedSubscription, error) {
	return s.createSubscriptionFn(ctx, rawURL, folderID, setFolder, title)
}

func (s *feedStoreStub) GetSubscription(ctx context.Context, id uuid.UUID) (model.FeedSubscription, bool, error) {
	return s.getSubscriptionFn(ctx, id)
}

func (s *feedStoreStub) GetItem(ctx context.Context, id uuid.UUID, markRead bool) (model.FeedItem, bool, error) {
	return s.getItemFn(ctx, id, markRead)
}

func (s *feedStoreStub) AssociateItemLink(ctx context.Context, itemID, linkID uuid.UUID) error {
	return s.associateFn(ctx, itemID, linkID)
}

func (s *feedStoreStub) CompleteRefresh(ctx context.Context, success repository.FeedRefreshSuccess) (int, error) {
	return s.completeRefreshFn(ctx, success)
}

func (s *feedStoreStub) FailRefresh(ctx context.Context, subscriptionID, claimToken uuid.UUID, now, next time.Time, message string) error {
	return s.failRefreshFn(ctx, subscriptionID, claimToken, now, next, message)
}

func (s *feedStoreStub) CreateFolder(ctx context.Context, name string) (model.FeedFolder, error) {
	return s.createFolderFn(ctx, name)
}

type feedRemoteStub struct {
	FeedRemote
	discoverFn func(context.Context, string) ([]model.FeedCandidate, error)
	fetchFn    func(context.Context, string, feedremote.ConditionalHeaders) (feedremote.RemoteResult, model.ParsedFeed, error)
}

func (s *feedRemoteStub) Discover(ctx context.Context, rawURL string) ([]model.FeedCandidate, error) {
	return s.discoverFn(ctx, rawURL)
}

func (s *feedRemoteStub) FetchAndParse(ctx context.Context, rawURL string, headers feedremote.ConditionalHeaders) (feedremote.RemoteResult, model.ParsedFeed, error) {
	return s.fetchFn(ctx, rawURL, headers)
}

type feedAnalyzerStub struct {
	request  RSSIngestRequest
	response dto.SubmitResponse
}

type recordingFeedLocker struct {
	keys       []string
	batchCalls int
}

func (l *recordingFeedLocker) WithURL(ctx context.Context, key string, fn func(context.Context) error) error {
	l.keys = append(l.keys, key)
	return fn(ctx)
}

func (l *recordingFeedLocker) WithURLs(ctx context.Context, keys []string, fn func(context.Context) error) error {
	l.batchCalls++
	l.keys = append(l.keys, keys...)
	return fn(ctx)
}

func (s *feedAnalyzerStub) AnalyzeRSS(_ context.Context, request RSSIngestRequest) (dto.SubmitResponse, error) {
	s.request = request
	return s.response, nil
}

func TestFeedServiceSubscribeIsFastAndSchedulesThroughPersistence(t *testing.T) {
	t.Parallel()
	var gotURL string
	var gotSetFolder bool
	store := &feedStoreStub{createSubscriptionFn: func(_ context.Context, rawURL string, _ *uuid.UUID, setFolder bool, _ string) (model.FeedSubscription, error) {
		gotURL, gotSetFolder = rawURL, setFolder
		return model.FeedSubscription{URL: rawURL, FeedURL: rawURL, Active: true}, nil
	}}
	service := NewFeedService(FeedServiceOptions{Store: store})
	result, err := service.Subscribe(context.Background(), "https://example.com/feed.xml#ignored", nil, false)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if gotURL != "https://example.com/feed.xml" || result.URL != gotURL {
		t.Fatalf("canonical URL got=%q result=%q", gotURL, result.URL)
	}
	if gotSetFolder {
		t.Fatal("omitted folder_id was treated as explicit ungrouped")
	}
}

func TestFeedServiceListSubscriptionsIsPureRead(t *testing.T) {
	t.Parallel()
	store := &feedStoreStub{
		listOverviewFn: func(context.Context, string) (model.FeedSubscriptionsResponse, error) {
			return model.FeedSubscriptionsResponse{}, nil
		},
	}
	locker := &recordingFeedLocker{}
	service := NewFeedService(FeedServiceOptions{Store: store, Locker: locker})
	if _, err := service.ListSubscriptions(context.Background(), ""); err != nil {
		t.Fatalf("ListSubscriptions() error = %v", err)
	}
	if len(locker.keys) != 0 {
		t.Fatalf("pure GET acquired mutation locks = %#v", locker.keys)
	}
}

func TestFeedServiceListSubscriptionsDerivesRefreshingFromInjectedClock(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	now := deadline.Add(-time.Minute)
	store := &feedStoreStub{
		listOverviewFn: func(context.Context, string) (model.FeedSubscriptionsResponse, error) {
			return model.FeedSubscriptionsResponse{Subscriptions: []model.FeedSubscription{{
				ID:                uuid.New(),
				Refreshing:        false,
				RefreshClaimUntil: &deadline,
			}}}, nil
		},
	}
	service := NewFeedService(FeedServiceOptions{Store: store, Now: func() time.Time { return now }})

	before, err := service.ListSubscriptions(t.Context(), "")
	if err != nil {
		t.Fatalf("ListSubscriptions(before deadline): %v", err)
	}
	if !before.Subscriptions[0].Refreshing {
		t.Fatal("Refreshing before deadline = false, want true")
	}

	now = deadline.Add(time.Nanosecond)
	after, err := service.ListSubscriptions(t.Context(), "")
	if err != nil {
		t.Fatalf("ListSubscriptions(after deadline): %v", err)
	}
	if after.Subscriptions[0].Refreshing {
		t.Fatal("Refreshing after deadline = true, want false")
	}
}

func TestFeedServiceAnalyzeUsesLongFeedContent(t *testing.T) {
	t.Parallel()
	itemID, subscriptionID, linkID := uuid.New(), uuid.New(), uuid.New()
	content := make([]byte, 0, 400)
	for len(content) < 320 {
		content = append(content, []byte("useful feed content ")...)
	}
	plain := string(content)
	html := "<p>" + plain + "</p>"
	linked := false
	store := &feedStoreStub{
		getSubscriptionFn: func(_ context.Context, id uuid.UUID) (model.FeedSubscription, bool, error) {
			return model.FeedSubscription{ID: id, URL: "https://example.com/feed.xml"}, true, nil
		},
		getItemFn: func(_ context.Context, id uuid.UUID, _ bool) (model.FeedItem, bool, error) {
			item := model.FeedItem{ID: id, SubscriptionID: subscriptionID, ExternalID: "guid-1", URL: "https://example.com/post", Title: "Post", Content: &plain, ContentHTML: &html}
			if linked {
				item.LinkID = &linkID
			}
			return item, true, nil
		},
		associateFn: func(_ context.Context, gotItemID, gotLinkID uuid.UUID) error {
			if gotItemID != itemID || gotLinkID != linkID {
				t.Fatalf("AssociateItemLink(%s,%s)", gotItemID, gotLinkID)
			}
			linked = true
			return nil
		},
	}
	analyzer := &feedAnalyzerStub{response: dto.SubmitResponse{LinkID: linkID.String(), Status: "pending"}}
	service := NewFeedService(FeedServiceOptions{Store: store, Analyzer: analyzer})
	item, submission, err := service.AnalyzeItem(context.Background(), itemID.String())
	if err != nil {
		t.Fatalf("AnalyzeItem() error = %v", err)
	}
	if !analyzer.request.UseFeedContent || analyzer.request.Text != plain || analyzer.request.HTML != html {
		t.Fatalf("analyzer request did not preserve feed body: %#v", analyzer.request)
	}
	if analyzer.request.ExternalID != "guid-1" || analyzer.request.FeedURL != "https://example.com/feed.xml" {
		t.Fatalf("analyzer request lost RSS provenance: %#v", analyzer.request)
	}
	if submission.LinkID != linkID.String() || item.LinkID == nil || *item.LinkID != linkID {
		t.Fatalf("analysis response item=%#v submission=%#v", item, submission)
	}
}

func TestFeedServiceRefreshFailureUsesExponentialBackoff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	claim := uuid.New()
	subscription := model.FeedSubscription{ID: uuid.New(), URL: "https://example.com/feed", FailureCount: 2, RefreshClaimToken: &claim}
	var gotNext time.Time
	var gotMessage string
	store := &feedStoreStub{failRefreshFn: func(_ context.Context, _, _ uuid.UUID, _, next time.Time, message string) error {
		gotNext, gotMessage = next, message
		return nil
	}}
	remote := &feedRemoteStub{fetchFn: func(context.Context, string, feedremote.ConditionalHeaders) (feedremote.RemoteResult, model.ParsedFeed, error) {
		return feedremote.RemoteResult{}, model.ParsedFeed{}, errors.New("feed upstream returned HTTP 403")
	}}
	service := NewFeedService(FeedServiceOptions{Store: store, Remote: remote, Now: func() time.Time { return now }})
	if err := service.RefreshClaimed(context.Background(), subscription); err != nil {
		t.Fatalf("RefreshClaimed() error = %v", err)
	}
	if want := now.Add(4 * time.Hour); !gotNext.Equal(want) {
		t.Fatalf("next = %s, want %s", gotNext, want)
	}
	if gotMessage != "feed upstream returned HTTP 403" {
		t.Fatalf("failure message = %q", gotMessage)
	}
}

func TestSafeFeedRefreshErrorDescribesFeedItemLimit(t *testing.T) {
	t.Parallel()
	if got := safeFeedRefreshError(errors.Join(feedremote.ErrFeedItemLimitExceeded)); got != feedremote.ErrFeedItemLimitExceeded.Error() {
		t.Fatalf("safeFeedRefreshError() = %q, want %q", got, feedremote.ErrFeedItemLimitExceeded.Error())
	}
}

func TestSafeFeedRefreshErrorDescribesUnsupportedDocument(t *testing.T) {
	t.Parallel()
	for _, err := range []error{feedremote.ErrUnsupportedFeedDocument, feedremote.ErrMalformedFeedDocument} {
		if got := safeFeedRefreshError(errors.Join(err)); got != "feed content is not valid RSS, Atom, or RDF" {
			t.Fatalf("safeFeedRefreshError(%v) = %q", err, got)
		}
	}
}

func TestFeedServiceRefresh304PreservesInitialFlag(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	claim := uuid.New()
	subscription := model.FeedSubscription{ID: uuid.New(), URL: "https://example.com/feed", RefreshClaimToken: &claim}
	var completed repository.FeedRefreshSuccess
	store := &feedStoreStub{completeRefreshFn: func(_ context.Context, success repository.FeedRefreshSuccess) (int, error) {
		completed = success
		return 0, nil
	}}
	remote := &feedRemoteStub{fetchFn: func(context.Context, string, feedremote.ConditionalHeaders) (feedremote.RemoteResult, model.ParsedFeed, error) {
		return feedremote.RemoteResult{NotModified: true}, model.ParsedFeed{}, nil
	}}
	service := NewFeedService(FeedServiceOptions{Store: store, Remote: remote, Now: func() time.Time { return now }})
	if err := service.RefreshClaimed(context.Background(), subscription); err != nil {
		t.Fatalf("RefreshClaimed() error = %v", err)
	}
	if !completed.NotModified || !completed.Initial || !completed.Now.Equal(now) {
		t.Fatalf("CompleteRefresh = %#v", completed)
	}
}

func TestFeedFailureBackoffCapsAt24Hours(t *testing.T) {
	t.Parallel()
	wants := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for index, want := range wants {
		if got := feedFailureBackoff(index + 1); got != want {
			t.Fatalf("feedFailureBackoff(%d) = %s, want %s", index+1, got, want)
		}
	}
}

func TestFeedFolderNameConflictMapsTo409(t *testing.T) {
	t.Parallel()
	err := mapFeedFolderError(repository.ErrFeedFolderNameConflict)
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != 409 {
		t.Fatalf("mapped error = %v, carrier=%v", err, ok)
	}
	var coder httperr.ErrorCoder
	if !errors.As(err, &coder) || coder.HTTPErrorCode() != "feed_folder_name_conflict" {
		t.Fatalf("mapped code = %v", coder)
	}
}
