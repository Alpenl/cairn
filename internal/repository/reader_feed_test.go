package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestReaderFeedSnapshotEnvelopePreservesResourceAndActionIdentity(t *testing.T) {
	linkID := uuid.New()
	inboxID := uuid.New()
	feedItemID := uuid.New()
	feedResourceID := uuid.New()
	items := []model.ReaderFeedItem{
		{Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID},
		{Key: "inbox:" + inboxID.String(), Source: "inbox", InboxID: &inboxID},
		{
			Key:        "subscription:" + feedItemID.String(),
			Source:     "subscription",
			LinkID:     &feedResourceID,
			FeedItemID: &feedItemID,
		},
	}

	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading", "subscription"}, items)
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	var wire readerFeedSnapshotEnvelope
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode snapshot envelope: %v", err)
	}
	if wire.Version != 2 || wire.Mode != "recommended" || len(wire.Items) != 3 {
		t.Fatalf("snapshot envelope = %#v", wire)
	}
	if len(wire.Capabilities) == 0 || len(wire.Sections) != 2 || len(wire.SourceMeta) != 2 {
		t.Fatalf("snapshot contract metadata = capabilities=%#v sections=%#v sources=%#v", wire.Capabilities, wire.Sections, wire.SourceMeta)
	}
	if wire.Items[0].ItemType != "reading" || wire.Items[0].Source != "reading" || wire.Items[0].SectionID != "reading" || wire.Items[0].DedupeKey == "" || len(wire.Items[0].Actions) == 0 {
		t.Fatalf("reading item contract metadata = %#v", wire.Items[0])
	}
	if wire.Items[0].ResourceKey != "link:"+linkID.String() || wire.Items[0].ActionKey != "link:"+linkID.String() {
		t.Fatalf("reading identities = resource=%q action=%q", wire.Items[0].ResourceKey, wire.Items[0].ActionKey)
	}
	if wire.Items[1].ResourceKey != "inbox:"+inboxID.String() || wire.Items[1].ActionKey != "inbox:"+inboxID.String() {
		t.Fatalf("inbox identities = resource=%q action=%q", wire.Items[1].ResourceKey, wire.Items[1].ActionKey)
	}
	if wire.Items[2].ResourceKey != "link:"+feedResourceID.String() || wire.Items[2].ActionKey != "subscription:"+feedItemID.String() {
		t.Fatalf("subscription identities = resource=%q action=%q", wire.Items[2].ResourceKey, wire.Items[2].ActionKey)
	}

	mode, sources, decoded, envelope, err := unmarshalReaderFeedSnapshot(raw)
	if err != nil {
		t.Fatalf("unmarshalReaderFeedSnapshot() error = %v", err)
	}
	if !envelope || mode != "recommended" || len(sources) != 2 || sources[0] != "reading" || sources[1] != "subscription" {
		t.Fatalf("decoded snapshot metadata = envelope=%v mode=%q sources=%#v", envelope, mode, sources)
	}
	if len(decoded) != 3 || decoded[2].Key != "subscription:"+feedItemID.String() || decoded[2].LinkID == nil || *decoded[2].LinkID != feedResourceID || decoded[2].FeedItemID == nil || *decoded[2].FeedItemID != feedItemID {
		t.Fatalf("decoded subscription identity = %#v", decoded[2])
	}
	_, _, detailedItems, capabilities, sections, sourceMeta, detailedEnvelope, err := unmarshalReaderFeedSnapshotDetails(raw)
	if err != nil {
		t.Fatalf("unmarshalReaderFeedSnapshotDetails() error = %v", err)
	}
	if !detailedEnvelope || len(capabilities) == 0 || len(sections) != 2 || len(sourceMeta) != 2 || len(detailedItems) != 3 {
		t.Fatalf("restored snapshot contract = envelope=%v capabilities=%#v sections=%#v sources=%#v items=%d", detailedEnvelope, capabilities, sections, sourceMeta, len(detailedItems))
	}
	if detailedItems[0].ActionIdentity() != wire.Items[0].ActionKey || detailedItems[0].ResourceIdentity() != wire.Items[0].ResourceKey {
		t.Fatalf("restored reading identities = resource=%q action=%q", detailedItems[0].ResourceIdentity(), detailedItems[0].ActionIdentity())
	}
}

func TestReaderFeedSnapshotCapabilityOffIsExplicitAndRestored(t *testing.T) {
	linkID := uuid.New()
	item := model.ReaderFeedItem{
		Key:         "link:" + linkID.String(),
		Source:      "reading",
		ResourceKey: "link:" + linkID.String(),
		ActionKey:   "link:" + linkID.String(),
		DedupeKey:   "url:https://example.com/off",
		SectionID:   "reading",
		Actions:     []string{},
		LinkID:      &linkID,
		URL:         "https://example.com/off",
	}
	raw, err := json.Marshal(readerFeedSnapshotEnvelope{
		Version:      1,
		Mode:         "recommended",
		Sources:      []string{"reading"},
		Capabilities: []string{},
		Sections:     []model.ReaderFeedSection{{ID: "reading", Source: "reading", Label: "收藏", Capabilities: []string{}}},
		SourceMeta:   []model.ReaderFeedSource{{ID: "reading", Label: "收藏", Enabled: false, Capabilities: []string{}}},
		Items: []readerFeedSnapshotItem{{
			ItemType: "reading", Source: "reading", SectionID: "reading",
			ResourceKey: item.ResourceKey, ActionKey: item.ActionKey, DedupeKey: item.DedupeKey,
			Actions: []string{}, Item: item,
		}},
	})
	if err != nil {
		t.Fatalf("marshal capability-off snapshot: %v", err)
	}

	_, sources, items, capabilities, sections, sourceMeta, envelope, err := unmarshalReaderFeedSnapshotDetails(raw)
	if err != nil {
		t.Fatalf("unmarshal capability-off snapshot: %v", err)
	}
	if !envelope || len(sources) != 1 || capabilities == nil || len(capabilities) != 0 {
		t.Fatalf("capability-off top-level metadata = envelope=%v sources=%#v capabilities=%#v", envelope, sources, capabilities)
	}
	if sections == nil || len(sections) != 1 || sections[0].Capabilities == nil || len(sections[0].Capabilities) != 0 {
		t.Fatalf("capability-off sections = %#v", sections)
	}
	if sourceMeta == nil || len(sourceMeta) != 1 || sourceMeta[0].Enabled || sourceMeta[0].Capabilities == nil || len(sourceMeta[0].Capabilities) != 0 {
		t.Fatalf("capability-off sources = %#v", sourceMeta)
	}
	if len(items) != 1 || items[0].Actions == nil || len(items[0].Actions) != 0 {
		t.Fatalf("capability-off item actions = %#v", items)
	}
}

func TestReaderFeedSnapshotMarshalPreservesCapabilityOffActions(t *testing.T) {
	linkID := uuid.New()
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, []model.ReaderFeedItem{{
		Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID, Actions: []string{},
	}})
	if err != nil {
		t.Fatalf("marshal capability-off snapshot: %v", err)
	}

	var wire readerFeedSnapshotEnvelope
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode capability-off snapshot: %v", err)
	}
	for _, capability := range wire.Capabilities {
		if capability == "actions" {
			t.Fatalf("capability-off snapshot advertises actions: %#v", wire.Capabilities)
		}
	}
	if len(wire.Items) != 1 || wire.Items[0].Actions == nil || len(wire.Items[0].Actions) != 0 {
		t.Fatalf("capability-off wire item actions = %#v", wire.Items)
	}
	if len(wire.Sections) != 1 || wire.Sections[0].Capabilities == nil || len(wire.Sections[0].Capabilities) != 0 {
		t.Fatalf("capability-off wire section capabilities = %#v", wire.Sections)
	}
	if len(wire.SourceMeta) != 1 || wire.SourceMeta[0].Capabilities == nil || len(wire.SourceMeta[0].Capabilities) != 0 {
		t.Fatalf("capability-off wire source capabilities = %#v", wire.SourceMeta)
	}

	_, _, items, capabilities, sections, sourceMeta, envelope, err := unmarshalReaderFeedSnapshotDetails(raw)
	if err != nil {
		t.Fatalf("unmarshal capability-off snapshot: %v", err)
	}
	if !envelope || len(capabilities) == 0 || len(items) != 1 || items[0].Actions == nil || len(items[0].Actions) != 0 {
		t.Fatalf("restored capability-off snapshot = envelope=%v capabilities=%#v items=%#v", envelope, capabilities, items)
	}
	if len(sections) != 1 || sections[0].Capabilities == nil || len(sections[0].Capabilities) != 0 || len(sourceMeta) != 1 || sourceMeta[0].Capabilities == nil || len(sourceMeta[0].Capabilities) != 0 {
		t.Fatalf("restored capability-off metadata = sections=%#v sources=%#v", sections, sourceMeta)
	}
}

func TestReaderFeedSnapshotRejectsConflictingResourceIdentity(t *testing.T) {
	itemID := uuid.New()
	otherID := uuid.New()
	raw, err := json.Marshal(readerFeedSnapshotEnvelope{
		Version: 1, Mode: "recommended", Sources: []string{"reading"},
		Capabilities: []string{}, Sections: []model.ReaderFeedSection{{ID: "reading", Source: "reading"}}, SourceMeta: []model.ReaderFeedSource{{ID: "reading"}},
		Items: []readerFeedSnapshotItem{{
			ItemType: "reading", Source: "reading", SectionID: "reading",
			ResourceKey: "link:" + otherID.String(), ActionKey: "link:" + itemID.String(), Actions: []string{},
			Item: model.ReaderFeedItem{Key: "link:" + itemID.String(), Source: "reading", LinkID: &itemID, Actions: []string{}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal conflicting snapshot: %v", err)
	}
	if _, _, _, _, _, _, _, err := unmarshalReaderFeedSnapshotDetails(raw); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("unmarshal conflicting resource identity error = %v, want ErrInvalidReaderCursor", err)
	}
}

func TestReaderFeedSnapshotLegacyArrayRemainsReadable(t *testing.T) {
	linkID := uuid.New()
	raw, err := json.Marshal([]model.ReaderFeedItem{{Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID}})
	if err != nil {
		t.Fatalf("marshal legacy snapshot: %v", err)
	}

	mode, sources, items, envelope, err := unmarshalReaderFeedSnapshot(raw)
	if err != nil {
		t.Fatalf("unmarshalReaderFeedSnapshot() error = %v", err)
	}
	if envelope || mode != "" || sources != nil || len(items) != 1 || items[0].Key != "link:"+linkID.String() {
		t.Fatalf("legacy snapshot = mode=%q sources=%#v envelope=%v items=%#v", mode, sources, envelope, items)
	}
}

func TestListFeedSnapshotRefreshBindsCursorAndSourceFilter(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.New()
	firstID, secondID := uuid.New(), uuid.New()
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, []model.ReaderFeedItem{
		{Key: "link:" + firstID.String(), Source: "reading", LinkID: &firstID},
		{Key: "link:" + secondID.String(), Source: "reading", LinkID: &secondID},
	})
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
		WithArgs(snapshotID).
		WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("recommended", raw))

	repo := NewPGXReaderVNextRepository(mock)
	page, err := repo.ListFeedWithSources(context.Background(), "", snapshotID.String(), makeFeedCursor(snapshotID.String(), 1), []string{"saved"}, 10)
	if err != nil {
		t.Fatalf("ListFeedWithSources() error = %v", err)
	}
	if page.SnapshotID != snapshotID.String() || page.Mode != "recommended" || len(page.Items) != 1 || page.Items[0].Key != "link:"+secondID.String() || page.NextCursor != "" {
		t.Fatalf("snapshot refresh page = %#v", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListFeedRejectsChangedSourceFilterForSnapshot(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.New()
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, nil)
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
		WithArgs(snapshotID).
		WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("recommended", raw))

	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.ListFeedWithSources(context.Background(), "recommended", snapshotID.String(), "", []string{"subscription"}, 10)
	if !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("ListFeedWithSources() error = %v, want ErrInvalidReaderCursor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListFeedRejectsChangedModeForSnapshot(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.New()
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, nil)
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
		WithArgs(snapshotID).
		WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("recommended", raw))

	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.ListFeedWithSources(context.Background(), "chronological", snapshotID.String(), "", []string{"reading"}, 10)
	if !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("ListFeedWithSources() error = %v, want ErrInvalidReaderCursor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListFeedRejectsCursorForDifferentSnapshotBeforeLookup(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.ListFeed(context.Background(), "recommended", uuid.NewString(), makeFeedCursor(uuid.NewString(), 0), 10)
	if !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("ListFeed() error = %v, want ErrInvalidReaderCursor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database query: %v", err)
	}
}

func TestListFeedReadsLegacySnapshotWithSnapshotCursor(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.New()
	firstID, secondID := uuid.New(), uuid.New()
	raw, err := json.Marshal([]model.ReaderFeedItem{
		{Key: "link:" + firstID.String(), Source: "reading", LinkID: &firstID},
		{Key: "link:" + secondID.String(), Source: "reading", LinkID: &secondID},
	})
	if err != nil {
		t.Fatalf("marshal legacy snapshot: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
		WithArgs(snapshotID).
		WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("chronological", raw))

	repo := NewPGXReaderVNextRepository(mock)
	page, err := repo.ListFeed(context.Background(), "chronological", snapshotID.String(), "", 1)
	if err != nil {
		t.Fatalf("ListFeed() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Key != "link:"+firstID.String() || page.NextCursor != makeChronologicalFeedCursor(snapshotID.String(), page.Items[0]) || page.SnapshotID != snapshotID.String() || page.Mode != "chronological" {
		t.Fatalf("legacy snapshot page = %#v", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestChronologicalFeedSortUsesVisibleEventAtAndResourceKey(t *testing.T) {
	sameEventAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	olderPublishedAt := sameEventAt.Add(-2 * time.Hour)
	items := []model.ReaderFeedItem{
		{Key: "subscription:late-capture", ResourceKey: "resource:d", PublishedAt: &olderPublishedAt, CreatedAt: sameEventAt.Add(4 * time.Hour)},
		{Key: "inbox:b", ResourceKey: "resource:b", CreatedAt: sameEventAt},
		{Key: "link:c", ResourceKey: "resource:c", PublishedAt: &sameEventAt, CreatedAt: sameEventAt.Add(-time.Hour)},
		{Key: "link:a", ResourceKey: "resource:a", CreatedAt: sameEventAt},
	}

	sortReaderFeedItems(items, "chronological")

	want := []string{"resource:a", "resource:b", "resource:c", "resource:d"}
	for index, resourceKey := range want {
		if got := items[index].ResourceIdentity(); got != resourceKey {
			t.Fatalf("items[%d].ResourceIdentity() = %q, want %q; items=%#v", index, got, resourceKey, items)
		}
	}
	if got := items[0].VisibleEventAt(); !got.Equal(sameEventAt) {
		t.Fatalf("created_at fallback = %s, want %s", got, sameEventAt)
	}
	if got := items[3].VisibleEventAt(); !got.Equal(olderPublishedAt) {
		t.Fatalf("published_at event = %s, want %s", got, olderPublishedAt)
	}
}

func TestChronologicalFeedCursorPagesThreeEqualTimePagesByCompleteTuple(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.NewString()
	eventAt := time.Date(2026, 8, 10, 12, 0, 0, 123456789, time.UTC)
	items := make([]model.ReaderFeedItem, 0, 6)
	for _, suffix := range []string{"f", "b", "e", "a", "d", "c"} {
		items = append(items, model.ReaderFeedItem{
			Key:         "resource:" + suffix,
			ResourceKey: "resource:" + suffix,
			CreatedAt:   eventAt,
		})
	}
	sortReaderFeedItems(items, "chronological")
	raw, err := marshalReaderFeedSnapshot("chronological", nil, items)
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	repo := NewPGXReaderVNextRepository(mock)

	var after string
	var seen []string
	for pageIndex := 0; pageIndex < 3; pageIndex++ {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
			WithArgs(uuid.MustParse(snapshotID)).
			WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("chronological", raw))
		page, err := repo.ListFeed(context.Background(), "chronological", snapshotID, after, 2)
		if err != nil {
			t.Fatalf("ListFeed(page=%d) error = %v", pageIndex+1, err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("page %d contains %d items, want 2", pageIndex+1, len(page.Items))
		}
		for _, item := range page.Items {
			seen = append(seen, item.ResourceIdentity())
		}
		if pageIndex == 2 {
			if page.NextCursor != "" {
				t.Fatalf("last page cursor = %q, want empty", page.NextCursor)
			}
			continue
		}
		decoded, err := feedCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("decode page %d cursor: %v", pageIndex+1, err)
		}
		last := page.Items[len(page.Items)-1]
		if !decoded.Chronological || decoded.SnapshotID != snapshotID || !decoded.EventAt.Equal(eventAt) || decoded.ResourceKey != last.ResourceIdentity() {
			t.Fatalf("page %d cursor = %#v, want tuple (%s, %q)", pageIndex+1, decoded, eventAt, last.ResourceIdentity())
		}
		after = page.NextCursor
	}

	want := []string{"resource:a", "resource:b", "resource:c", "resource:d", "resource:e", "resource:f"}
	if !slices.Equal(seen, want) {
		t.Fatalf("three-page resources = %#v, want %#v", seen, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
	state := readerFeedSnapshotState{SnapshotID: snapshotID, Mode: "chronological", Items: items}
	if _, err := readerFeedPage(state, readerFeedCursor{
		SnapshotID: snapshotID, EventAt: eventAt, ResourceKey: "resource:missing", Chronological: true,
	}, 2); !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("missing tuple cursor error = %v, want ErrInvalidReaderCursor", err)
	}
}

func TestListFeedSnapshotLookupUsesInstallationIdentity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	snapshotID := uuid.New()
	linkID := uuid.New()
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, []model.ReaderFeedItem{{
		Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID,
	}})
	if err != nil {
		t.Fatalf("marshal installation snapshot: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mode,items FROM reader_feed_snapshots WHERE id=$1 AND created_at > NOW() - INTERVAL '24 hours'")).
		WithArgs(snapshotID).
		WillReturnRows(mock.NewRows([]string{"mode", "items"}).AddRow("recommended", raw))

	repo := NewPGXReaderVNextRepository(mock)
	page, err := repo.ListFeedWithSources(context.Background(), "recommended", snapshotID.String(), "", []string{"reading"}, 10)
	if err != nil {
		t.Fatalf("ListFeed() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ActionIdentity() != "link:"+linkID.String() {
		t.Fatalf("installation snapshot page = %#v", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBuildFeedItemsUsesModeSpecificCandidateTieBreakers(t *testing.T) {
	t.Run("recommended keeps existing subscription id order", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool() error = %v", err)
		}
		defer mock.Close()
		mock.ExpectQuery("(?s)FROM feed_items fi.*ORDER BY COALESCE\\(fi\\.published_at,fi\\.created_at\\) DESC,fi\\.id DESC LIMIT 1000").
			WillReturnRows(mock.NewRows([]string{"id", "link_id", "url", "title", "summary", "published_at", "read", "read_later", "created_at"}))

		repo := NewPGXReaderVNextRepository(mock)
		if _, err := repo.buildFeedItems(context.Background(), []string{"subscription"}); err != nil {
			t.Fatalf("buildFeedItems() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("pgxmock expectations: %v", err)
		}
	})

	t.Run("chronological candidates use resource key ascending", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool() error = %v", err)
		}
		defer mock.Close()
		mock.ExpectQuery("(?s)FROM links l.*ORDER BY l\\.created_at DESC,l\\.id ASC LIMIT 1000").
			WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "created_at", "read", "read_later", "progress"}))
		mock.ExpectQuery("(?s)FROM reader_inbox inbox.*ORDER BY inbox\\.created_at DESC,inbox\\.id ASC LIMIT 200").
			WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "created_at"}))
		mock.ExpectQuery("(?s)FROM feed_items fi.*ORDER BY COALESCE\\(fi\\.published_at,fi\\.created_at\\) DESC,CASE WHEN COALESCE\\(fs\\.link_id,fi\\.link_id\\) IS NOT NULL THEN 'link:'.*END ASC LIMIT 1000").
			WillReturnRows(mock.NewRows([]string{"id", "link_id", "url", "title", "summary", "published_at", "read", "read_later", "created_at"}))

		repo := NewPGXReaderVNextRepository(mock)
		if _, err := repo.buildFeedItemsForMode(context.Background(), "chronological"); err != nil {
			t.Fatalf("buildFeedItemsForMode() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("pgxmock expectations: %v", err)
		}
	})
}

func TestBuildFeedItemsReadingRetainsReadCards(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT l\\.id,l\\.url.*COALESCE\\(l\\.library_kind,'reading'\\)='reading'.*").
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "created_at", "read", "read_later"}).
			AddRow(linkID, "https://example.com/reading", "Read card", "summary", createdAt, true, true))

	repo := NewPGXReaderVNextRepository(mock)
	items, err := repo.buildFeedItems(context.Background(), []string{"reading"})
	if err != nil {
		t.Fatalf("buildFeedItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Key != "link:"+linkID.String() || !items[0].Read || !items[0].ReadLater {
		t.Fatalf("reading feed items = %#v", items)
	}
	if items[0].ReasonCode != "" || items[0].ReasonText != "" {
		t.Fatalf("unscored reading item guessed a reason = code %q text %q", items[0].ReasonCode, items[0].ReasonText)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBuildFeedItemsSubscriptionRetainsResourceAndActionIdentity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	feedItemID := uuid.New()
	linkID := uuid.New()
	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	publishedAt := createdAt.Add(-time.Hour)
	mock.ExpectQuery("(?s)SELECT fi\\.id,COALESCE\\(fs\\.link_id,fi\\.link_id\\),fi\\.url.*FROM feed_items fi.*").
		WillReturnRows(mock.NewRows([]string{"id", "link_id", "url", "title", "summary", "published_at", "read", "read_later", "saved", "created_at"}).
			AddRow(feedItemID, linkID.String(), "https://example.com/subscription", "Subscription item", "summary", publishedAt, true, false, true, createdAt))

	repo := NewPGXReaderVNextRepository(mock)
	items, err := repo.buildFeedItems(context.Background(), []string{"subscription"})
	if err != nil {
		t.Fatalf("buildFeedItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0]
	if item.Key != "subscription:"+feedItemID.String() || item.Source != "subscription" || item.FeedItemID == nil || *item.FeedItemID != feedItemID || item.LinkID == nil || *item.LinkID != linkID {
		t.Fatalf("subscription identities = %#v", item)
	}
	if !item.Read || item.PublishedAt == nil || !item.PublishedAt.Equal(publishedAt) {
		t.Fatalf("subscription metadata = %#v", item)
	}
	if item.ReasonCode != "" || item.ReasonText != "" {
		t.Fatalf("unscored subscription item guessed a reason = code %q text %q", item.ReasonCode, item.ReasonText)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBuildFeedItemsSubscriptionFallsBackToCreatedAtWhenUnpublished(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	feedItemID := uuid.New()
	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT fi\\.id,COALESCE\\(fs\\.link_id,fi\\.link_id\\),fi\\.url.*FROM feed_items fi.*").
		WillReturnRows(mock.NewRows([]string{"id", "link_id", "url", "title", "summary", "published_at", "read", "read_later", "saved", "created_at"}).
			AddRow(feedItemID, nil, "https://example.com/unpublished", "Unpublished item", "summary", nil, false, false, false, createdAt))

	repo := NewPGXReaderVNextRepository(mock)
	items, err := repo.buildFeedItems(context.Background(), []string{"subscription"})
	if err != nil {
		t.Fatalf("buildFeedItems() error = %v", err)
	}
	if len(items) != 1 || items[0].PublishedAt != nil || !items[0].VisibleEventAt().Equal(createdAt) {
		t.Fatalf("unpublished subscription item = %#v, want event_at=%s", items, createdAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBuildFeedItemsDedupesSharedURLAndKeepsSavedReadState(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	feedItemID := uuid.New()
	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	sharedURL := "https://example.com/shared"
	mock.ExpectQuery("(?s)SELECT l\\.id,l\\.url.*COALESCE\\(l\\.library_kind,'reading'\\)='reading'.*").
		WillReturnRows(mock.NewRows([]string{"id", "url", "title", "summary", "created_at", "read", "read_later"}).
			AddRow(linkID, sharedURL, "Saved", "saved", createdAt, true, false))
	mock.ExpectQuery("(?s)SELECT fi\\.id,COALESCE\\(fs\\.link_id,fi\\.link_id\\),fi\\.url.*FROM feed_items fi.*").
		WillReturnRows(mock.NewRows([]string{"id", "link_id", "url", "title", "summary", "published_at", "read", "read_later", "saved", "created_at"}).
			AddRow(feedItemID, linkID.String(), sharedURL, "RSS", "rss", createdAt, false, true, true, createdAt))

	repo := NewPGXReaderVNextRepository(mock)
	items, err := repo.buildFeedItems(context.Background(), []string{"reading", "subscription"})
	if err != nil {
		t.Fatalf("buildFeedItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Key != "link:"+linkID.String() || !items[0].Read || items[0].ReadLater {
		t.Fatalf("deduped shared URL item = %#v", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestFeedbackFeedUsesInstallationActionIdentity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM links WHERE id=$1)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("(?s)INSERT INTO reader_feed_feedback").
		WithArgs("link:"+linkID.String(), "hide").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	if _, err := repo.FeedbackFeed(context.Background(), "link:"+linkID.String(), "hide"); err != nil {
		t.Fatalf("FeedbackFeed() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestFeedbackFeedRejectsNonCanonicalActionIdentityBeforeTransaction(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	for _, itemKey := range []string{
		"feed_item:" + uuid.NewString(),
		"link:" + uuid.NewString() + ":extra",
		"LINK:" + uuid.NewString(),
	} {
		if _, err := repo.FeedbackFeed(context.Background(), itemKey, "hide"); !errors.Is(err, ErrInvalidReaderFeedItem) {
			t.Fatalf("FeedbackFeed(%q) error = %v, want ErrInvalidReaderFeedItem", itemKey, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database operation: %v", err)
	}
}

func TestBulkUpdateInboxStatusRejectsUnsupportedStateBeforeTransaction(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	for _, status := range []string{"pending", "queued", ""} {
		if _, err := repo.BulkUpdateInboxStatus(context.Background(), []uuid.UUID{uuid.New()}, status); !errors.Is(err, ErrReaderInboxStateConflict) {
			t.Fatalf("BulkUpdateInboxStatus(%q) error = %v, want ErrReaderInboxStateConflict", status, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database operation: %v", err)
	}
}

func TestBulkConfirmInboxRollsBackWhenOnePendingItemCannotBeConfirmed(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	secondID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	firstURL := "https://example.com/pending-first"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	linkID := uuid.New()
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	lockQuery := regexp.QuoteMeta("SELECT " + readerInboxColumns + " FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")
	mock.ExpectQuery(lockQuery).
		WithArgs(firstID).
		WillReturnRows(mock.NewRows(readerFeedInboxColumnsForTest()).AddRow(readerFeedInboxRowForTest(firstID, "pending", firstURL, now)...))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WithArgs("canonical-link:" + firstURL).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(regexp.QuoteMeta(findInboxSavedLinkSQL)).
		WithArgs(firstURL).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO links.*ON CONFLICT \\(source_key\\) DO NOTHING.*RETURNING id").
		WithArgs(firstURL, "url", firstURL, "pending body", pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"tag"}, (*string)(nil), "plain").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	mock.ExpectExec("UPDATE links SET feed_managed=false").
		WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec("(?s)DELETE FROM reader_categorizables source").
		WithArgs(firstID.String(), linkID.String()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("(?s)UPDATE reader_categorizables").
		WithArgs(firstID.String(), linkID.String()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE reader_inbox SET status='confirmed',expiry_lease_id=NULL,expiry_lease_until=NULL,updated_at=NOW() WHERE id=$1 AND status='pending' AND deleted_at IS NULL")).
		WithArgs(firstID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(lockQuery).
		WithArgs(secondID).
		WillReturnRows(mock.NewRows(readerFeedInboxColumnsForTest()).AddRow(readerFeedInboxRowForTest(secondID, "discarded", "https://example.com/pending-second", now)...))
	mock.ExpectRollback()

	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.BulkConfirmInbox(context.Background(), []model.ReaderInboxBulkConfirmation{{ID: firstID}, {ID: secondID}})
	if !errors.Is(err, ErrReaderInboxStateConflict) {
		t.Fatalf("BulkConfirmInbox() error = %v, want ErrReaderInboxStateConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBulkConfirmInboxRejectsBlankTitleInsideAtomicTransaction(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	row := readerFeedInboxRowForTest(id, "pending", "https://example.com/blank-title", now)
	row[4] = " \t "
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerInboxColumns + " FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows(readerFeedInboxColumnsForTest()).AddRow(row...))
	mock.ExpectRollback()

	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.BulkConfirmInbox(context.Background(), []model.ReaderInboxBulkConfirmation{{ID: id}})
	if !errors.Is(err, ErrReaderInboxTitleRequired) {
		t.Fatalf("BulkConfirmInbox() error = %v, want ErrReaderInboxTitleRequired", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestBulkConfirmInboxRejectsStaleRevisionInsideAtomicTransaction(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	row := readerFeedInboxRowForTest(id, "pending", "https://example.com/stale", now)
	// Address the value by column name: a positional index silently points at
	// a different field the moment a column is added to the projection.
	row[readerFeedInboxColumnIndexForTest(t, "metadata_revision")] = int64(2)
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerInboxColumns + " FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows(readerFeedInboxColumnsForTest()).AddRow(row...))
	mock.ExpectRollback()

	expectedRevision := int64(1)
	repo := NewPGXReaderVNextRepository(mock)
	_, err = repo.BulkConfirmInbox(context.Background(), []model.ReaderInboxBulkConfirmation{{ID: id, ExpectedRevision: &expectedRevision}})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("BulkConfirmInbox() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func readerFeedInboxColumnsForTest() []string {
	return []string{
		"id", "url", "identity_key", "source_kind", "title", "body", "body_document", "body_format", "note", "summary", "suggested_tags", "proposal_signals", "proposal_status", "tags", "category_ids",
		"status", "metadata_revision", "job_id", "expires_at", "expired_at", "deleted_at", "created_at", "updated_at",
	}
}

func readerFeedInboxColumnIndexForTest(t *testing.T, column string) int {
	t.Helper()
	for index, name := range readerFeedInboxColumnsForTest() {
		if name == column {
			return index
		}
	}
	t.Fatalf("inbox test projection has no %q column", column)
	return -1
}

func readerFeedInboxRowForTest(id uuid.UUID, status, rawURL string, now time.Time) []any {
	return []any{
		id, rawURL, nil, "url", "Pending title", "pending body", nil, "plain", "", nil, []string{"suggested"}, []byte(`{}`), "pending", []string{"tag"}, []uuid.UUID{},
		status, int64(1), nil, nil, nil, nil, now.Add(-time.Hour), now,
	}
}

// TestConfirmInboxWritesTheCaptureDocumentAsTheLinkDocument pins the write that
// turned every confirmed browser capture into a wall of text: content and
// content_document used to receive the same flattened string under a hardcoded
// content_format='markdown'. They are two projections of one capture — the
// plain body and the Markdown converted from the captured HTML — and the format
// must describe what was actually written.
func TestConfirmInboxWritesTheCaptureDocumentAsTheLinkDocument(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	rawURL := "https://example.com/captured"
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	document := "# Guide\n\n- One\n- Two"
	linkID := uuid.New()
	row := readerFeedInboxRowForTest(id, "pending", rawURL, now)
	row[readerFeedInboxColumnIndexForTest(t, "body_document")] = document
	row[readerFeedInboxColumnIndexForTest(t, "body_format")] = "markdown"

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerInboxColumns + " FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(id).
		WillReturnRows(mock.NewRows(readerFeedInboxColumnsForTest()).AddRow(row...))
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WithArgs("canonical-link:" + rawURL).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(regexp.QuoteMeta(findInboxSavedLinkSQL)).
		WithArgs(rawURL).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO links.*ON CONFLICT \\(source_key\\) DO NOTHING.*RETURNING id").
		WithArgs(rawURL, "url", rawURL, "pending body", pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"tag"}, &document, "markdown").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(linkID))
	mock.ExpectExec("UPDATE links SET feed_managed=false").
		WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec("(?s)DELETE FROM reader_categorizables source").
		WithArgs(id.String(), linkID.String()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("(?s)UPDATE reader_categorizables").
		WithArgs(id.String(), linkID.String()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE reader_inbox SET status='confirmed'")).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	got, err := repo.ConfirmInbox(context.Background(), id)
	if err != nil {
		t.Fatalf("ConfirmInbox() error = %v", err)
	}
	if got != linkID {
		t.Fatalf("ConfirmInbox() link = %s, want %s", got, linkID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
