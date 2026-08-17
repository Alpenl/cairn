package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerFeedStoreStub struct {
	repository.ReaderVNextStore
	page            *model.ReaderFeedPage
	filteredSources []string
	feedbackItemKey string
	feedbackAction  string
	feedbackCalls   int
	err             error
}

func (s *readerFeedStoreStub) ListFeed(context.Context, string, string, string, int) (*model.ReaderFeedPage, error) {
	return s.page, s.err
}

func (s *readerFeedStoreStub) ListFeedWithSources(_ context.Context, _ string, _ string, _ string, sources []string, _ int) (*model.ReaderFeedPage, error) {
	s.filteredSources = append([]string(nil), sources...)
	return s.page, s.err
}

func (s *readerFeedStoreStub) FeedbackFeed(_ context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	s.feedbackCalls++
	s.feedbackItemKey = itemKey
	s.feedbackAction = action
	return model.ReaderFeedFeedback{ItemKey: itemKey, Action: action, Saved: action == "save"}, nil
}

func feedContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestReaderFeedServicePreservesUnionIdentityAndReason(t *testing.T) {
	linkID := uuid.New()
	inboxID := uuid.New()
	feedItemID := uuid.New()
	readingSource, inboxSource, subscriptionSource := "reading", "inbox", "subscription"
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{
		SnapshotID: "snapshot-1",
		Mode:       "recommended",
		Items: []model.ReaderFeedItem{
			{
				Key: "link:" + linkID.String(), Source: "reading", URL: "https://example.com/reading", LinkID: &linkID,
				Score: 100, ScoreContributions: model.ReaderFeedScoreContributions{SavedLibrary: 70, Unread: 20, ReadLater: 10},
				EnabledScoreSignals: []model.ReaderFeedScoreSignal{model.ReaderFeedSignalSavedLibrary, model.ReaderFeedSignalUnread, model.ReaderFeedSignalReadLater, model.ReaderFeedSignalChronologicalFallback},
				ReasonCode:          model.ReaderFeedReasonSavedLibrary, ReasonParams: model.ReaderFeedReasonParams{Source: &readingSource}, ReasonContribution: 70, ReasonText: "已保存到资料库",
			},
			{
				Key: "inbox:" + inboxID.String(), Source: "inbox", URL: "https://example.com/inbox", InboxID: &inboxID,
				Score: 120, ScoreContributions: model.ReaderFeedScoreContributions{PendingConfirmation: 100, Unread: 20},
				EnabledScoreSignals: []model.ReaderFeedScoreSignal{model.ReaderFeedSignalPendingConfirmation, model.ReaderFeedSignalUnread, model.ReaderFeedSignalReadLater, model.ReaderFeedSignalChronologicalFallback},
				ReasonCode:          model.ReaderFeedReasonPendingConfirmation, ReasonParams: model.ReaderFeedReasonParams{Source: &inboxSource}, ReasonContribution: 100, ReasonText: "收件箱采集",
			},
			{
				Key: "subscription:" + feedItemID.String(), Source: "subscription", URL: "https://example.com/subscription", FeedItemID: &feedItemID,
				Score: 60, ScoreContributions: model.ReaderFeedScoreContributions{SubscriptionRecent: 40, Unread: 20},
				EnabledScoreSignals: []model.ReaderFeedScoreSignal{model.ReaderFeedSignalSubscriptionRecent, model.ReaderFeedSignalUnread, model.ReaderFeedSignalReadLater, model.ReaderFeedSignalChronologicalFallback},
				ReasonCode:          model.ReaderFeedReasonSubscriptionRecent, ReasonParams: model.ReaderFeedReasonParams{Source: &subscriptionSource}, ReasonContribution: 40, ReasonText: "订阅更新",
			},
		},
	}}

	response, err := NewReaderVNextService(store, nil).Feed(context.Background(), "recommended", "", "", 30)
	if err != nil {
		t.Fatalf("Feed() error = %v", err)
	}
	if response.SnapshotID != "snapshot-1" || response.Mode != "recommended" || len(response.Items) != 3 {
		t.Fatalf("Feed() response = %#v", response)
	}
	if response.Items[0].Key != "link:"+linkID.String() || response.Items[0].LinkID == nil || *response.Items[0].LinkID != linkID.String() {
		t.Fatalf("saved link identity = %#v", response.Items[0])
	}
	if item := response.Items[0]; item.Score != 100 || item.ScoreContributions.SavedLibrary != 70 || item.ReasonCode != string(model.ReaderFeedReasonSavedLibrary) || item.ReasonParams.Source == nil || *item.ReasonParams.Source != "reading" || item.ReasonContribution != 70 {
		t.Fatalf("saved link score/reason evidence = %#v", item)
	}
	if response.Items[1].InboxID == nil || *response.Items[1].InboxID != inboxID.String() || response.Items[1].ReasonCode == "" || response.Items[1].ReasonText == "" {
		t.Fatalf("pending identity/reason = %#v", response.Items[1])
	}
	if response.Items[2].FeedItemID == nil || *response.Items[2].FeedItemID != feedItemID.String() || response.Items[2].ReasonCode != "subscription_recent" {
		t.Fatalf("subscription identity/reason = %#v", response.Items[2])
	}
	if len(response.Capabilities) == 0 || !feedContains(response.Capabilities, "snapshot") || !feedContains(response.Capabilities, "cursor") || !feedContains(response.Capabilities, "dedupe") || !feedContains(response.Capabilities, "reason") || !feedContains(response.Capabilities, "source_filter") || !feedContains(response.Capabilities, "inbox_batch") || !feedContains(response.Capabilities, "actions") {
		t.Fatalf("feed capabilities = %#v", response.Capabilities)
	}
	if len(response.Sections) != 3 || len(response.Sources) != 3 {
		t.Fatalf("section/source metadata lengths = (%d, %d), want (3, 3)", len(response.Sections), len(response.Sources))
	}
	for index, source := range []string{"inbox", "reading", "subscription"} {
		section := response.Sections[index]
		meta := response.Sources[index]
		if section.ID != source || section.Source != source || section.Count != 1 || section.Label == "" || len(section.Capabilities) == 0 {
			t.Fatalf("section[%d] = %#v", index, section)
		}
		if meta.ID != source || !meta.Enabled || meta.Count != 1 || meta.Label == "" || len(meta.Capabilities) == 0 {
			t.Fatalf("source[%d] = %#v", index, meta)
		}
	}
	for index, want := range []struct {
		itemType    string
		resourceKey string
		actionKey   string
		dedupeKey   string
		sectionID   string
	}{
		{itemType: "reading", resourceKey: "link:" + linkID.String(), actionKey: "link:" + linkID.String(), dedupeKey: "url:https://example.com/reading", sectionID: "reading"},
		{itemType: "inbox", resourceKey: "inbox:" + inboxID.String(), actionKey: "inbox:" + inboxID.String(), dedupeKey: "url:https://example.com/inbox", sectionID: "inbox"},
		{itemType: "subscription", resourceKey: "feed_item:" + feedItemID.String(), actionKey: "subscription:" + feedItemID.String(), dedupeKey: "url:https://example.com/subscription", sectionID: "subscription"},
	} {
		item := response.Items[index]
		if item.ItemType != want.itemType || item.ResourceKey != want.resourceKey || item.ActionKey != want.actionKey || item.DedupeKey != want.dedupeKey || item.SectionID != want.sectionID || len(item.Actions) == 0 {
			t.Fatalf("item[%d] identity/action wire = %#v, want %#v", index, item, want)
		}
	}
	if !feedContains(response.Items[0].Actions, "open_workspace") || feedContains(response.Items[0].Actions, "save") || feedContains(response.Items[0].Actions, "unsave") {
		t.Fatalf("reading actions = %#v", response.Items[0].Actions)
	}
	if !feedContains(response.Items[1].Actions, "confirm") || !feedContains(response.Items[1].Actions, "discard") || feedContains(response.Items[1].Actions, "save") {
		t.Fatalf("inbox actions = %#v", response.Items[1].Actions)
	}
	if !feedContains(response.Items[2].Actions, "save") || feedContains(response.Items[2].Actions, "unsave") {
		t.Fatalf("subscription actions = %#v", response.Items[2].Actions)
	}
}

func TestReaderFeedServiceMapsInvalidReasonToStableMachineError(t *testing.T) {
	service := NewReaderVNextService(&readerFeedStoreStub{err: repository.ErrInvalidReaderFeedReason}, nil)
	_, err := service.Feed(context.Background(), "recommended", "", "", 30)
	var carrier httperr.StatusCarrier
	var coder httperr.ErrorCoder
	if !errors.As(err, &carrier) || carrier.HTTPStatus() != http.StatusUnprocessableEntity ||
		!errors.As(err, &coder) || coder.HTTPErrorCode() != httperr.CodeInvalidFeedReason {
		t.Fatalf("Feed() error = %v, want 422 %q", err, httperr.CodeInvalidFeedReason)
	}
}

func TestReaderFeedServicePreservesRepositoryOrderAndPublishesVisibleEventAt(t *testing.T) {
	readingID := uuid.New()
	inboxID := uuid.New()
	feedItemID := uuid.New()
	createdAt := time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC)
	publishedAt := createdAt.Add(-6 * time.Hour)
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{
		SnapshotID: "snapshot-event-order",
		Mode:       "chronological",
		Items: []model.ReaderFeedItem{
			{Key: "subscription:" + feedItemID.String(), Source: "subscription", FeedItemID: &feedItemID, PublishedAt: &publishedAt, CreatedAt: createdAt},
			{Key: "link:" + readingID.String(), Source: "reading", LinkID: &readingID, CreatedAt: createdAt},
			{Key: "inbox:" + inboxID.String(), Source: "inbox", InboxID: &inboxID, CreatedAt: createdAt.Add(-time.Hour)},
		},
	}}

	response, err := NewReaderVNextService(store, nil).Feed(context.Background(), "chronological", "", "", 30)
	if err != nil {
		t.Fatalf("Feed() error = %v", err)
	}
	wantKeys := []string{
		"subscription:" + feedItemID.String(),
		"link:" + readingID.String(),
		"inbox:" + inboxID.String(),
	}
	for index, wantKey := range wantKeys {
		if response.Items[index].Key != wantKey {
			t.Fatalf("response.Items[%d].Key = %q, want %q", index, response.Items[index].Key, wantKey)
		}
	}
	if !response.Items[0].EventAt.Equal(publishedAt) || !response.Items[1].EventAt.Equal(createdAt) {
		t.Fatalf("event_at values = (%s, %s), want (%s, %s)", response.Items[0].EventAt, response.Items[1].EventAt, publishedAt, createdAt)
	}
}

func TestReaderFeedServiceNormalizesLegacyUnionIdentityBeforeDTO(t *testing.T) {
	linkID := uuid.New()
	inboxID := uuid.New()
	feedItemID := uuid.New()
	linkedResourceID := uuid.New()
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{
		SnapshotID: "snapshot-legacy",
		NextCursor: "cursor-next",
		Mode:       "chronological",
		Items: []model.ReaderFeedItem{
			// Older saved-link rows used the source alias and did not repeat the
			// ID pointer when the canonical action key was already present.
			{Key: "link:" + linkID.String(), Source: "saved", Read: true, ReasonCode: "reading_progress", ReasonText: "阅读进度"},
			// An old snapshot can carry only the action key for an Inbox item.
			{Key: "inbox:" + inboxID.String(), ReasonCode: "pending_confirmation", ReasonText: "收件箱采集"},
			// A subscription item keeps its RSS action identity and its linked
			// saved-resource identity separately.
			{Source: "subscription", FeedItemID: &feedItemID, LinkID: &linkedResourceID, ReadLater: true, ReasonCode: "subscription_recent", ReasonText: "订阅更新"},
		},
	}}

	response, err := NewReaderVNextService(store, nil).Feed(context.Background(), "chronological", "", "", 30)
	if err != nil {
		t.Fatalf("Feed() error = %v", err)
	}
	if response.SnapshotID != "snapshot-legacy" || response.NextCursor != "cursor-next" || response.Mode != "chronological" {
		t.Fatalf("snapshot envelope = %#v", response)
	}
	if len(response.Items) != 3 {
		t.Fatalf("len(response.Items) = %d, want 3", len(response.Items))
	}
	if item := response.Items[0]; item.Source != "reading" || item.Key != "link:"+linkID.String() || item.LinkID == nil || *item.LinkID != linkID.String() || !item.Read || item.ReasonCode != "reading_progress" {
		t.Fatalf("normalized saved-link item = %#v", item)
	}
	if item := response.Items[1]; item.Source != "inbox" || item.Key != "inbox:"+inboxID.String() || item.InboxID == nil || *item.InboxID != inboxID.String() || item.ReasonText == "" {
		t.Fatalf("normalized Inbox item = %#v", item)
	}
	if item := response.Items[2]; item.Source != "subscription" || item.Key != "subscription:"+feedItemID.String() || item.FeedItemID == nil || *item.FeedItemID != feedItemID.String() || item.LinkID == nil || *item.LinkID != linkedResourceID.String() || !item.ReadLater {
		t.Fatalf("normalized subscription item = %#v", item)
	}
}

func TestReaderFeedServicePreservesExplicitCapabilityOffAndMetadata(t *testing.T) {
	linkID := uuid.New()
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{
		Items: []model.ReaderFeedItem{{
			Key:         "link:" + linkID.String(),
			Source:      "reading",
			ResourceKey: "link:resource-identity",
			ActionKey:   "link:" + linkID.String(),
			DedupeKey:   "url:dedupe-identity",
			SectionID:   "custom-section",
			Actions:     []string{},
			LinkID:      &linkID,
		}},
		Capabilities: []string{},
		Sections:     []model.ReaderFeedSection{{ID: "custom-section", Source: "reading", Label: "Custom", Count: 1, Capabilities: []string{}}},
		Sources:      []model.ReaderFeedSource{{ID: "reading", Label: "Custom", Enabled: false, Count: 1, Capabilities: []string{}}},
	}}

	response, err := NewReaderVNextService(store, nil).FeedWithSources(context.Background(), "recommended", "", "", []string{"reading"}, 30)
	if err != nil {
		t.Fatalf("FeedWithSources() error = %v", err)
	}
	if response.Capabilities == nil || len(response.Capabilities) != 0 || len(response.Sections) != 1 || len(response.Sources) != 1 {
		t.Fatalf("explicit capability-off metadata = %#v", response)
	}
	if response.Sections[0].Label != "Custom" || response.Sections[0].Capabilities == nil || len(response.Sections[0].Capabilities) != 0 {
		t.Fatalf("explicit section metadata = %#v", response.Sections[0])
	}
	if response.Sources[0].Enabled || response.Sources[0].Capabilities == nil || len(response.Sources[0].Capabilities) != 0 {
		t.Fatalf("explicit source capability-off metadata = %#v", response.Sources[0])
	}
	item := response.Items[0]
	if item.ResourceKey != "link:resource-identity" || item.ActionKey != "link:"+linkID.String() || item.DedupeKey != "url:dedupe-identity" || item.SectionID != "custom-section" || item.Actions == nil || len(item.Actions) != 0 {
		t.Fatalf("explicit item capability-off metadata = %#v", item)
	}
}

func TestReaderFeedServiceDerivedMetadataPreservesItemCapabilityOff(t *testing.T) {
	linkID := uuid.New()
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{Items: []model.ReaderFeedItem{{
		Key:     "link:" + linkID.String(),
		Source:  "reading",
		Actions: []string{},
		LinkID:  &linkID,
	}}}}

	response, err := NewReaderVNextService(store, nil).FeedWithSources(context.Background(), "recommended", "", "", []string{"reading"}, 30)
	if err != nil {
		t.Fatalf("FeedWithSources() error = %v", err)
	}
	if feedContains(response.Capabilities, "actions") {
		t.Fatalf("derived capabilities incorrectly advertise actions: %#v", response.Capabilities)
	}
	if len(response.Sections) != 1 || len(response.Sources) != 1 {
		t.Fatalf("derived metadata = sections=%#v sources=%#v", response.Sections, response.Sources)
	}
	readingSection := response.Sections[0]
	readingSource := response.Sources[0]
	if readingSection.Source != "reading" || readingSection.Count != 1 || readingSection.Capabilities == nil || len(readingSection.Capabilities) != 0 {
		t.Fatalf("derived reading section capability-off = %#v", readingSection)
	}
	if readingSource.ID != "reading" || readingSource.Count != 1 || readingSource.Capabilities == nil || len(readingSource.Capabilities) != 0 {
		t.Fatalf("derived reading source capability-off = %#v", readingSource)
	}
	if response.Items[0].Actions == nil || len(response.Items[0].Actions) != 0 {
		t.Fatalf("derived item actions = %#v", response.Items[0].Actions)
	}
}

func TestReaderFeedServiceUsesActionKeyAsLegacyKeyFallback(t *testing.T) {
	feedItemID := uuid.New()
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{Items: []model.ReaderFeedItem{{
		Source:     "feed",
		ActionKey:  "subscription:" + feedItemID.String(),
		FeedItemID: &feedItemID,
	}}}}

	response, err := NewReaderVNextService(store, nil).Feed(context.Background(), "recommended", "", "", 30)
	if err != nil {
		t.Fatalf("Feed() error = %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Source != "subscription" || response.Items[0].Key != "subscription:"+feedItemID.String() || response.Items[0].ActionKey != "subscription:"+feedItemID.String() {
		t.Fatalf("action-key legacy fallback = %#v", response.Items)
	}
}

func TestReaderFeedServiceRejectsConflictingUnionIdentityBeforeStore(t *testing.T) {
	linkID := uuid.New()
	inboxID := uuid.New()
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{Items: []model.ReaderFeedItem{
		{Key: "link:" + linkID.String(), Source: "inbox", InboxID: &inboxID},
	}}}

	if _, err := NewReaderVNextService(store, nil).Feed(context.Background(), "recommended", "", "", 30); err == nil {
		t.Fatal("Feed() error = nil for conflicting union identity")
	}
}

func TestReaderFeedServiceNormalizesSourceFilterBeforeSnapshotStore(t *testing.T) {
	store := &readerFeedStoreStub{page: &model.ReaderFeedPage{SnapshotID: "snapshot-2", Mode: "recommended"}}
	_, err := NewReaderVNextService(store, nil).FeedWithSources(
		context.Background(), "recommended", "", "", []string{"subscription, saved", "subscription"}, 30,
	)
	if err != nil {
		t.Fatalf("FeedWithSources() error = %v", err)
	}
	if got, want := store.filteredSources, []string{"reading", "subscription"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("normalized source filter = %#v, want %#v", got, want)
	}
}

func TestReaderFeedServiceRejectsUnknownSourceBeforeStore(t *testing.T) {
	service := NewReaderVNextService(&readerFeedStoreStub{}, nil)
	if _, err := service.FeedWithSources(context.Background(), "recommended", "", "", []string{"archive"}, 30); err == nil {
		t.Fatal("FeedWithSources() error = nil for unknown source")
	}
}

func TestReaderFeedServiceUsesCanonicalActionIdentity(t *testing.T) {
	itemKey := "link:" + uuid.NewString()
	store := &readerFeedStoreStub{}
	if _, err := NewReaderVNextService(store, nil).FeedbackFeed(context.Background(), "  "+itemKey+"  ", "save"); err != nil {
		t.Fatalf("FeedbackFeed() error = %v", err)
	}
	if store.feedbackItemKey != itemKey || store.feedbackAction != "save" {
		t.Fatalf("feedback command = (%q, %q), want (%q, save)", store.feedbackItemKey, store.feedbackAction, itemKey)
	}
}

func TestReaderFeedServiceRejectsUnavailableInboxActionsBeforeStore(t *testing.T) {
	itemKey := "inbox:" + uuid.NewString()
	for _, action := range []string{"save", "unsave"} {
		store := &readerFeedStoreStub{}
		_, err := NewReaderVNextService(store, nil).FeedbackFeed(context.Background(), itemKey, action)
		if err == nil {
			t.Fatalf("FeedbackFeed(%q) error = nil, want source capability rejection", action)
		}
		if store.feedbackCalls != 0 {
			t.Fatalf("FeedbackFeed(%q) reached store for unavailable Inbox action", action)
		}
	}
}

func TestReaderFeedServiceRejectsNonCanonicalActionIdentityBeforeStore(t *testing.T) {
	service := NewReaderVNextService(&readerFeedStoreStub{}, nil)
	for _, itemKey := range []string{
		"feed_item:" + uuid.NewString(),
		"link:" + uuid.NewString() + ":extra",
		"link:not-a-uuid",
	} {
		store := &readerFeedStoreStub{}
		service.store = store
		if _, err := service.FeedbackFeed(context.Background(), itemKey, "hide"); err == nil {
			t.Fatalf("FeedbackFeed(%q) error = nil, want invalid action identity", itemKey)
		}
		if store.feedbackCalls != 0 {
			t.Fatalf("FeedbackFeed(%q) reached store for invalid action identity", itemKey)
		}
	}
}

func TestReaderFeedServiceRejectsInvalidModeBeforeStore(t *testing.T) {
	service := NewReaderVNextService(&readerFeedStoreStub{}, nil)
	if _, err := service.Feed(context.Background(), "ranking-v2", "", "", 30); err == nil {
		t.Fatal("Feed() error = nil for unknown mode")
	}

	// Keep the compile-time response contract visible in this focused test.
	var _ dto.ReaderFeedResponse
}
