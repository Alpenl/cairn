package repository

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"webtag/internal/model"
)

func TestScoreReaderFeedItemKeepsLegacyWeightsAndUsesWinningContribution(t *testing.T) {
	createdAt := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name              string
		item              model.ReaderFeedItem
		wantScore         int
		wantReason        model.ReaderFeedReasonCode
		wantContribution  int
		wantContributions model.ReaderFeedScoreContributions
		wantText          string
	}{
		{
			name:              "pending unread read later",
			item:              model.ReaderFeedItem{Source: "inbox", Read: false, ReadLater: true, CreatedAt: createdAt},
			wantScore:         130,
			wantReason:        model.ReaderFeedReasonPendingConfirmation,
			wantContribution:  100,
			wantContributions: model.ReaderFeedScoreContributions{PendingConfirmation: 100, Unread: 20, ReadLater: 10},
			wantText:          "收件箱采集",
		},
		{
			name:              "saved unread read later",
			item:              model.ReaderFeedItem{Source: "reading", Read: false, ReadLater: true, CreatedAt: createdAt},
			wantScore:         100,
			wantReason:        model.ReaderFeedReasonSavedLibrary,
			wantContribution:  70,
			wantContributions: model.ReaderFeedScoreContributions{SavedLibrary: 70, Unread: 20, ReadLater: 10},
			wantText:          "已保存到资料库",
		},
		{
			name:              "subscription unread read later",
			item:              model.ReaderFeedItem{Source: "subscription", Read: false, ReadLater: true, CreatedAt: createdAt},
			wantScore:         70,
			wantReason:        model.ReaderFeedReasonSubscriptionRecent,
			wantContribution:  40,
			wantContributions: model.ReaderFeedScoreContributions{SubscriptionRecent: 40, Unread: 20, ReadLater: 10},
			wantText:          "订阅更新",
		},
		{
			name:              "unknown source unread",
			item:              model.ReaderFeedItem{Source: "unknown", Read: false, CreatedAt: createdAt},
			wantScore:         20,
			wantReason:        model.ReaderFeedReasonUnread,
			wantContribution:  20,
			wantContributions: model.ReaderFeedScoreContributions{Unread: 20},
			wantText:          "尚未阅读",
		},
		{
			name:              "unknown source read later",
			item:              model.ReaderFeedItem{Source: "unknown", Read: true, ReadLater: true, CreatedAt: createdAt},
			wantScore:         10,
			wantReason:        model.ReaderFeedReasonReadLater,
			wantContribution:  10,
			wantContributions: model.ReaderFeedScoreContributions{ReadLater: 10},
			wantText:          "已加入稍后读",
		},
		{
			name:              "no positive contribution",
			item:              model.ReaderFeedItem{Source: "unknown", Read: true, CreatedAt: createdAt},
			wantScore:         0,
			wantReason:        model.ReaderFeedReasonChronologicalFallback,
			wantContribution:  0,
			wantContributions: model.ReaderFeedScoreContributions{},
			wantText:          "按时间排序",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scored, err := scoreReaderFeedItem(test.item)
			if err != nil {
				t.Fatalf("scoreReaderFeedItem() error = %v", err)
			}
			if scored.Score != test.wantScore || scored.ReasonCode != test.wantReason || scored.ReasonContribution != test.wantContribution {
				t.Fatalf("scored item = score %d reason %q contribution %d, want %d %q %d", scored.Score, scored.ReasonCode, scored.ReasonContribution, test.wantScore, test.wantReason, test.wantContribution)
			}
			if scored.ScoreContributions != test.wantContributions {
				t.Fatalf("score contributions = %#v, want %#v", scored.ScoreContributions, test.wantContributions)
			}
			if scored.ReasonText != test.wantText {
				t.Fatalf("legacy reason_text = %q, want tuple-derived %q", scored.ReasonText, test.wantText)
			}
			if len(scored.EnabledScoreSignals) == 0 {
				t.Fatal("enabled score signals are empty")
			}
		})
	}
}

func TestReaderFeedSnapshotPagesPreserveFrozenScoreAndReasonTuple(t *testing.T) {
	createdAt := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading", "subscription"}, []model.ReaderFeedItem{
		{Key: "link:one", Source: "reading", Read: false, CreatedAt: createdAt},
		{Key: "subscription:two", Source: "subscription", Read: false, CreatedAt: createdAt.Add(-time.Minute)},
	})
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}
	_, _, items, capabilities, sections, sources, envelope, err := unmarshalReaderFeedSnapshotDetails(raw)
	if err != nil || !envelope || len(items) != 2 {
		t.Fatalf("unmarshal snapshot = items %#v envelope %v error %v", items, envelope, err)
	}
	state := readerFeedSnapshotState{
		SnapshotID:   "snapshot-frozen",
		Mode:         "recommended",
		Items:        items,
		Capabilities: capabilities,
		Sections:     sections,
		Sources:      sources,
	}
	first, err := readerFeedPage(state, readerFeedCursor{Offset: 0}, 1)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	second, err := readerFeedPage(state, readerFeedCursor{Offset: 1}, 1)
	if err != nil {
		t.Fatalf("second page error = %v", err)
	}
	if first.Items[0].Score != 90 || first.Items[0].ReasonCode != model.ReaderFeedReasonSavedLibrary || first.Items[0].ReasonContribution != 70 {
		t.Fatalf("first page frozen evidence = %#v", first.Items[0])
	}
	if second.Items[0].Score != 60 || second.Items[0].ReasonCode != model.ReaderFeedReasonSubscriptionRecent || second.Items[0].ReasonContribution != 40 {
		t.Fatalf("second page frozen evidence = %#v", second.Items[0])
	}
	if second.Items[0].ReasonParams.Source == nil || *second.Items[0].ReasonParams.Source != "subscription" || second.Items[0].ReasonText != "订阅更新" {
		t.Fatalf("second page frozen tuple = %#v", second.Items[0])
	}
}

func TestReaderFeedSnapshotFreezesScoringEvidence(t *testing.T) {
	createdAt := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	raw, err := marshalReaderFeedSnapshot("recommended", []string{"reading"}, []model.ReaderFeedItem{{
		Key:       "link:score-fixture",
		Source:    "reading",
		URL:       "https://example.com/scored",
		Read:      false,
		ReadLater: true,
		CreatedAt: createdAt,
	}})
	if err != nil {
		t.Fatalf("marshalReaderFeedSnapshot() error = %v", err)
	}

	var envelope readerFeedSnapshotEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if envelope.Version != 2 || len(envelope.Items) != 1 {
		t.Fatalf("snapshot envelope = version %d items %d, want version 2 with one item", envelope.Version, len(envelope.Items))
	}
	item := envelope.Items[0].Item
	if item.Score != 100 || item.ScoreContributions.SavedLibrary != 70 || item.ScoreContributions.Unread != 20 || item.ScoreContributions.ReadLater != 10 {
		t.Fatalf("frozen score evidence = score %d contributions %#v", item.Score, item.ScoreContributions)
	}
	if item.ScoreContributions.PendingConfirmation != 0 || item.ScoreContributions.SubscriptionRecent != 0 || item.ScoreContributions.ChronologicalFallback != 0 {
		t.Fatalf("disabled/fallback contributions = %#v", item.ScoreContributions)
	}
	if item.ReasonCode != model.ReaderFeedReasonSavedLibrary || item.ReasonParams.Source == nil || *item.ReasonParams.Source != "reading" || item.ReasonContribution != 70 || item.ReasonText != "已保存到资料库" {
		t.Fatalf("frozen reason tuple = code %q params %#v contribution %d text %q", item.ReasonCode, item.ReasonParams, item.ReasonContribution, item.ReasonText)
	}

	_, _, restored, envelopeFormat, err := unmarshalReaderFeedSnapshot(raw)
	if err != nil {
		t.Fatalf("unmarshalReaderFeedSnapshot() error = %v", err)
	}
	if !envelopeFormat || len(restored) != 1 || restored[0].Score != item.Score || restored[0].ReasonCode != item.ReasonCode || restored[0].ReasonText != item.ReasonText {
		t.Fatalf("restored score evidence = %#v", restored)
	}
}

func TestReaderFeedSnapshotRejectsMalformedWinningReasonBeforeWrite(t *testing.T) {
	item, err := scoreReaderFeedItem(model.ReaderFeedItem{
		Key:       "link:malformed-reason",
		Source:    "reading",
		CreatedAt: time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("scoreReaderFeedItem() error = %v", err)
	}
	item.ReasonParams = model.ReaderFeedReasonParams{}

	_, err = marshalReaderFeedSnapshot("recommended", []string{"reading"}, []model.ReaderFeedItem{item})
	if !errors.Is(err, ErrInvalidReaderFeedReason) {
		t.Fatalf("marshal malformed reason error = %v, want ErrInvalidReaderFeedReason", err)
	}
}

func TestReaderFeedRecommendationGoldenKeepsCandidatesWeightsPrecisionAndOrder(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	items := []model.ReaderFeedItem{
		{Key: "subscription-unread", Source: "subscription", Read: false, CreatedAt: base.Add(5 * time.Minute)},
		{Key: "reading-read", Source: "reading", Read: true, CreatedAt: base.Add(4 * time.Minute)},
		{Key: "inbox-read", Source: "inbox", Read: true, CreatedAt: base.Add(3 * time.Minute)},
		{Key: "reading-unread-later", Source: "reading", Read: false, ReadLater: true, CreatedAt: base.Add(2 * time.Minute)},
		{Key: "inbox-unread-later", Source: "inbox", Read: false, ReadLater: true, CreatedAt: base.Add(time.Minute)},
		{Key: "subscription-read", Source: "subscription", Read: true, CreatedAt: base},
	}

	scored, err := scoreReaderFeedItems(items)
	if err != nil {
		t.Fatalf("scoreReaderFeedItems() error = %v", err)
	}
	if len(scored) != len(items) {
		t.Fatalf("candidate count = %d, want %d", len(scored), len(items))
	}
	sortReaderFeedItems(scored, "recommended")
	want := []struct {
		key   string
		score int
	}{
		{"inbox-unread-later", 130},
		{"inbox-read", 100},
		{"reading-unread-later", 100},
		{"reading-read", 70},
		{"subscription-unread", 60},
		{"subscription-read", 40},
	}
	for index, expected := range want {
		if scored[index].Key != expected.key || scored[index].Score != expected.score {
			t.Fatalf("rank %d = (%q, %d), want (%q, %d)", index, scored[index].Key, scored[index].Score, expected.key, expected.score)
		}
	}
}

func TestSelectReaderFeedReasonUsesFrozenPriorityForEveryTie(t *testing.T) {
	priority := []model.ReaderFeedReasonCode{
		model.ReaderFeedReasonPendingConfirmation,
		model.ReaderFeedReasonSavedLibrary,
		model.ReaderFeedReasonSubscriptionRecent,
		model.ReaderFeedReasonUnread,
		model.ReaderFeedReasonReadLater,
		model.ReaderFeedReasonChronologicalFallback,
	}
	for higherIndex, higher := range priority {
		for lowerIndex := higherIndex + 1; lowerIndex < len(priority); lowerIndex++ {
			lower := priority[lowerIndex]
			t.Run(string(higher)+"_before_"+string(lower), func(t *testing.T) {
				winner, err := selectReaderFeedReason([]readerFeedSignalContribution{
					{code: lower, enabled: true, contribution: 10, params: validReaderFeedReasonParams(lower)},
					{code: higher, enabled: true, contribution: 10, params: validReaderFeedReasonParams(higher)},
				})
				if err != nil {
					t.Fatalf("selectReaderFeedReason() error = %v", err)
				}
				if winner.code != higher {
					t.Fatalf("winner = %q, want %q", winner.code, higher)
				}
			})
		}
	}

	winner, err := selectReaderFeedReason([]readerFeedSignalContribution{
		{code: model.ReaderFeedReasonReadLater, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonReadLater)},
		{code: model.ReaderFeedReasonSubscriptionRecent, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonSubscriptionRecent)},
		{code: model.ReaderFeedReasonPendingConfirmation, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonPendingConfirmation)},
		{code: model.ReaderFeedReasonChronologicalFallback, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonChronologicalFallback)},
		{code: model.ReaderFeedReasonUnread, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonUnread)},
		{code: model.ReaderFeedReasonSavedLibrary, enabled: true, contribution: 10, params: validReaderFeedReasonParams(model.ReaderFeedReasonSavedLibrary)},
	})
	if err != nil || winner.code != model.ReaderFeedReasonPendingConfirmation {
		t.Fatalf("multi-signal winner = %q, %v; want %q", winner.code, err, model.ReaderFeedReasonPendingConfirmation)
	}
}

func TestSelectReaderFeedReasonRejectsIneligibleSignalsAndMissingParams(t *testing.T) {
	winner, err := selectReaderFeedReason([]readerFeedSignalContribution{
		{code: model.ReaderFeedReasonSavedLibrary, enabled: false, contribution: 100, params: validReaderFeedReasonParams(model.ReaderFeedReasonSavedLibrary)},
		{code: model.ReaderFeedReasonUnread, enabled: true, contribution: 0, params: validReaderFeedReasonParams(model.ReaderFeedReasonUnread)},
		{code: model.ReaderFeedReasonReadLater, enabled: true, contribution: -10, params: validReaderFeedReasonParams(model.ReaderFeedReasonReadLater)},
		{code: model.ReaderFeedReasonChronologicalFallback, enabled: true, contribution: 0, params: validReaderFeedReasonParams(model.ReaderFeedReasonChronologicalFallback)},
	})
	if err != nil {
		t.Fatalf("selectReaderFeedReason() error = %v", err)
	}
	if winner.code != model.ReaderFeedReasonChronologicalFallback || winner.contribution != 0 {
		t.Fatalf("winner = %#v, want zero-contribution chronological fallback", winner)
	}

	_, err = selectReaderFeedReason([]readerFeedSignalContribution{
		{code: model.ReaderFeedReasonSavedLibrary, enabled: true, contribution: 70},
		{code: model.ReaderFeedReasonChronologicalFallback, enabled: true, params: validReaderFeedReasonParams(model.ReaderFeedReasonChronologicalFallback)},
	})
	if !errors.Is(err, ErrInvalidReaderFeedReason) {
		t.Fatalf("missing params error = %v, want ErrInvalidReaderFeedReason", err)
	}
}

func validReaderFeedReasonParams(code model.ReaderFeedReasonCode) model.ReaderFeedReasonParams {
	inbox, reading, subscription := "inbox", "reading", "subscription"
	unread, readLater := false, true
	createdAt := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)
	switch code {
	case model.ReaderFeedReasonPendingConfirmation:
		return model.ReaderFeedReasonParams{Source: &inbox}
	case model.ReaderFeedReasonSavedLibrary:
		return model.ReaderFeedReasonParams{Source: &reading}
	case model.ReaderFeedReasonSubscriptionRecent:
		return model.ReaderFeedReasonParams{Source: &subscription}
	case model.ReaderFeedReasonUnread:
		return model.ReaderFeedReasonParams{Read: &unread}
	case model.ReaderFeedReasonReadLater:
		return model.ReaderFeedReasonParams{ReadLater: &readLater}
	case model.ReaderFeedReasonChronologicalFallback:
		return model.ReaderFeedReasonParams{CreatedAt: &createdAt}
	default:
		return model.ReaderFeedReasonParams{}
	}
}
