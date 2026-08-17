package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// readerHomeTestKey is a package-private context key type. An anonymous
// struct{} would collide with any other package doing the same thing.
type readerHomeTestKey struct{}

type readerHomeAggregateStoreStub struct {
	repository.ReaderVNextStore
	aggregate repository.ReaderHomeAggregate
	err       error
	calls     int
	ctx       context.Context
}

func (s *readerHomeAggregateStoreStub) LoadHomeAggregate(ctx context.Context) (repository.ReaderHomeAggregate, error) {
	s.calls++
	s.ctx = ctx
	return s.aggregate, s.err
}

func TestHomeAggregateMapsOneAuthoritativeResultToHomeDTO(t *testing.T) {
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	todoID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	updatedAt := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	dueAt := updatedAt.Add(24 * time.Hour)
	completedAt := updatedAt.Add(-30 * time.Minute)
	store := &readerHomeAggregateStoreStub{aggregate: repository.ReaderHomeAggregate{
		Freshness: repository.ReaderHomeFreshnessFresh,
		Counts: map[string]int{
			"pending": 2,
			"todos":   3,
			"reading": 7,
		},
		ContinueReading: []model.ReaderFeedItem{{
			Key:         "link:" + linkID.String(),
			Source:      "reading",
			Title:       "Continue this",
			Summary:     "A saved article",
			URL:         "https://example.com/article",
			LinkID:      &linkID,
			ReasonCode:  model.ReaderFeedReasonContinueReading,
			ReasonText:  "已读 42%，继续阅读",
			PublishedAt: &updatedAt,
			CreatedAt:   updatedAt,
		}},
		RecentThoughts: []model.ReaderThought{{
			ID:           "thought-1",
			HostKind:     "link",
			HostID:       linkID.String(),
			Body:         "Keep the snapshot boundary small.",
			Source:       "user",
			LastSequence: 4,
			CreatedAt:    updatedAt,
			UpdatedAt:    updatedAt,
		}},
		Todos: []model.ReaderTodo{{
			ID:          todoID,
			Text:        "Review the aggregate",
			DueAt:       &dueAt,
			Done:        true,
			OriginKind:  "standalone",
			CompletedAt: &completedAt,
			CreatedAt:   updatedAt,
			UpdatedAt:   updatedAt,
		}},
	}}
	service := NewReaderVNextService(store, nil)
	service.now = func() time.Time { return updatedAt }
	ctx := context.WithValue(context.Background(), readerHomeTestKey{}, "home-test")

	got, err := service.HomeAggregate(ctx)
	if err != nil {
		t.Fatalf("HomeAggregate() error = %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("LoadHomeAggregate() calls = %d, want 1", store.calls)
	}
	if store.ctx != ctx {
		t.Fatal("HomeAggregate() did not forward the request context")
	}
	if got.Today != "2026-08-10" {
		t.Fatalf("Today = %q, want 2026-08-10", got.Today)
	}
	if got.Summary != "2026-08-10：收件箱 2 条，待办 3 项" {
		t.Fatalf("Summary = %q", got.Summary)
	}
	if got.Counts["reading"] != 7 || got.Counts["pending"] != 2 {
		t.Fatalf("Counts = %#v", got.Counts)
	}
	if got.Stale {
		t.Fatal("transactional aggregate should not be marked stale")
	}
	if got.Freshness != "fresh" || got.Partial {
		t.Fatalf("freshness state = (%q, partial=%v), want fresh/false", got.Freshness, got.Partial)
	}
	if len(got.ContinueReading) != 1 || got.ContinueReading[0].LinkID == nil || *got.ContinueReading[0].LinkID != linkID.String() {
		t.Fatalf("ContinueReading = %#v", got.ContinueReading)
	}
	if len(got.RecentThoughts) != 1 || got.RecentThoughts[0].ID != "thought-1" {
		t.Fatalf("RecentThoughts = %#v", got.RecentThoughts)
	}
	if len(got.Todos) != 1 || got.Todos[0].ID != todoID.String() {
		t.Fatalf("Todos = %#v", got.Todos)
	}
	if got.Todos[0].DueAt == nil || !got.Todos[0].DueAt.Equal(dueAt) || !got.Todos[0].Done || got.Todos[0].CompletedAt == nil || !got.Todos[0].CompletedAt.Equal(completedAt) {
		t.Fatalf("TODO lifecycle = %#v, want due/done/completed_at preserved", got.Todos[0])
	}

	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal Home response: %v", err)
	}
	var envelope struct {
		Freshness string `json:"freshness"`
		Partial   bool   `json:"partial"`
		Stale     bool   `json:"stale"`
		Todos     []struct {
			DueAt       *time.Time `json:"due_at"`
			CompletedAt *time.Time `json:"completed_at"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("unmarshal Home response: %v", err)
	}
	if envelope.Freshness != "fresh" || envelope.Partial || envelope.Stale || len(envelope.Todos) != 1 || envelope.Todos[0].DueAt == nil || envelope.Todos[0].CompletedAt == nil {
		t.Fatalf("Home wire state/lifecycle = %#v, want explicit fresh and TODO timestamps", envelope)
	}
}

// TestHomeAggregateEmitsContinueReadingAsAnUnscoredReason pins the wire shape a
// Reader client validates. continue_reading explains the card without being a
// ranking signal, so it must never claim a contribution nor appear inside the
// fixed-width score evidence — the two places a client checks column by column.
func TestHomeAggregateEmitsContinueReadingAsAnUnscoredReason(t *testing.T) {
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	updatedAt := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	store := &readerHomeAggregateStoreStub{aggregate: repository.ReaderHomeAggregate{
		Freshness: repository.ReaderHomeFreshnessFresh,
		Counts:    map[string]int{"pending": 0, "todos": 0},
		ContinueReading: []model.ReaderFeedItem{{
			Key:         "link:" + linkID.String(),
			Source:      "reading",
			Title:       "Continue this",
			Summary:     "A saved article",
			URL:         "https://example.com/article",
			LinkID:      &linkID,
			ReasonCode:  model.ReaderFeedReasonContinueReading,
			ReasonText:  "已读 42%，继续阅读",
			PublishedAt: &updatedAt,
			CreatedAt:   updatedAt,
		}},
	}}
	service := NewReaderVNextService(store, nil)
	service.now = func() time.Time { return updatedAt }

	got, err := service.HomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("HomeAggregate() error = %v", err)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal Home response: %v", err)
	}
	var envelope struct {
		ContinueReading []struct {
			ReasonCode          string         `json:"reason_code"`
			ReasonParams        map[string]any `json:"reason_params"`
			ReasonContribution  int            `json:"reason_contribution"`
			Score               int            `json:"score"`
			ScoreContributions  map[string]int `json:"score_contributions"`
			EnabledScoreSignals []string       `json:"enabled_score_signals"`
		} `json:"continue_reading"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("unmarshal Home response: %v", err)
	}
	if len(envelope.ContinueReading) != 1 {
		t.Fatalf("continue_reading = %#v, want one card", envelope.ContinueReading)
	}
	card := envelope.ContinueReading[0]
	if card.ReasonCode != string(model.ReaderFeedReasonContinueReading) {
		t.Fatalf("reason_code = %q, want %q", card.ReasonCode, model.ReaderFeedReasonContinueReading)
	}
	if len(card.ReasonParams) != 0 || card.ReasonContribution != 0 || card.Score != 0 || len(card.EnabledScoreSignals) != 0 {
		t.Fatalf("unscored card = %#v, want empty params, zero contribution and no enabled signals", card)
	}
	if len(card.ScoreContributions) != 6 {
		t.Fatalf("score_contributions keys = %#v, want the six ranking signals", card.ScoreContributions)
	}
	if _, ok := card.ScoreContributions[string(model.ReaderFeedReasonContinueReading)]; ok {
		t.Fatalf("score_contributions = %#v, want no continue_reading column", card.ScoreContributions)
	}
	for signal, contribution := range card.ScoreContributions {
		if contribution != 0 {
			t.Fatalf("score_contributions[%q] = %d, want 0 for an unscored card", signal, contribution)
		}
	}
}

func TestHomeAggregateMapsPartialAndStaleWithoutConflatingThem(t *testing.T) {
	cases := []struct {
		name          string
		freshness     repository.ReaderHomeFreshness
		wantFreshness string
		wantPartial   bool
		wantStale     bool
	}{
		{name: "fresh", freshness: repository.ReaderHomeFreshnessFresh, wantFreshness: "fresh"},
		{name: "partial", freshness: repository.ReaderHomeFreshness("partial"), wantFreshness: "partial", wantPartial: true},
		{name: "stale", freshness: repository.ReaderHomeFreshnessStale, wantFreshness: "stale", wantStale: true},
		{name: "zero_legacy", freshness: "", wantFreshness: "partial", wantPartial: true},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := &readerHomeAggregateStoreStub{aggregate: repository.ReaderHomeAggregate{
				Freshness:       tt.freshness,
				Counts:          map[string]int{"pending": 1, "todos": 2},
				ContinueReading: []model.ReaderFeedItem{},
				RecentThoughts:  []model.ReaderThought{},
				Todos:           []model.ReaderTodo{},
			}}
			service := NewReaderVNextService(store, nil)

			got, err := service.HomeAggregate(context.Background())
			if err != nil {
				t.Fatalf("HomeAggregate() error = %v", err)
			}
			if got.Freshness != tt.wantFreshness || got.Partial != tt.wantPartial || got.Stale != tt.wantStale {
				t.Fatalf("wire state = (freshness=%q, partial=%v, stale=%v), want (%q, %v, %v)", got.Freshness, got.Partial, got.Stale, tt.wantFreshness, tt.wantPartial, tt.wantStale)
			}
		})
	}
}

func TestHomeAggregateMapsProjectedTodoAfterHostWriteback(t *testing.T) {
	todoID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	updatedAt := time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC)
	dueAt := updatedAt.Add(48 * time.Hour)
	completedAt := updatedAt.Add(-time.Minute)
	hostKind, hostID := "thought", "thought-1"
	store := &readerHomeAggregateStoreStub{aggregate: repository.ReaderHomeAggregate{
		Freshness: repository.ReaderHomeFreshnessFresh,
		Counts:    map[string]int{"pending": 0, "todos": 1},
		Todos: []model.ReaderTodo{{
			ID:             todoID,
			Text:           "Projected after writeback",
			DueAt:          &dueAt,
			Done:           true,
			OriginKind:     "thought",
			OriginHostKind: &hostKind,
			OriginHostID:   &hostID,
			OriginRef:      json.RawMessage(`{"block_ref":"task:writeback","occurrence":2}`),
			HostRevision:   9,
			CompletedAt:    &completedAt,
			CreatedAt:      updatedAt,
			UpdatedAt:      updatedAt,
		}},
		ContinueReading: []model.ReaderFeedItem{},
		RecentThoughts:  []model.ReaderThought{},
	}}
	service := NewReaderVNextService(store, nil)

	got, err := service.HomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("HomeAggregate() error = %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("LoadHomeAggregate() calls = %d, want 1", store.calls)
	}
	if len(got.Todos) != 1 {
		t.Fatalf("Todos = %#v, want one projected row", got.Todos)
	}
	todo := got.Todos[0]
	if todo.OriginKind != "thought" || todo.OriginHostID == nil || *todo.OriginHostID != hostID || todo.HostRevision != 9 {
		t.Fatalf("projected TODO origin = %#v, want stable host identity and revision", todo)
	}
	if todo.DueAt == nil || !todo.DueAt.Equal(dueAt) || !todo.Done || todo.CompletedAt == nil || !todo.CompletedAt.Equal(completedAt) {
		t.Fatalf("projected TODO lifecycle = %#v, want due/done/completed_at from aggregate", todo)
	}
}

func TestHomeAggregatePreservesKnownLegacyFieldsWithoutInventingMissingSections(t *testing.T) {
	updatedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &readerHomeAggregateStoreStub{aggregate: repository.ReaderHomeAggregate{
		// Freshness is intentionally zero: this is the shape returned by a
		// legacy Reader aggregate proxy that predates the freshness contract.
		Counts:          map[string]int{"pending": 2, "todos": 4},
		ContinueReading: []model.ReaderFeedItem{},
	}}
	service := NewReaderVNextService(store, nil)
	service.now = func() time.Time { return updatedAt }

	got, err := service.HomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("HomeAggregate() error = %v", err)
	}
	if got.Freshness != "partial" || !got.Partial || got.Stale {
		t.Fatalf("legacy freshness state = %#v, want partial/partial/not-stale", got)
	}
	if got.Summary != "2026-08-10：收件箱 2 条，待办 4 项" {
		t.Fatalf("legacy known summary = %q", got.Summary)
	}
	if got.Counts["pending"] != 2 || got.Counts["todos"] != 4 {
		t.Fatalf("legacy known counts = %#v", got.Counts)
	}
	if got.ContinueReading == nil || got.RecentThoughts != nil || got.Todos != nil {
		t.Fatalf("legacy section projection = %#v, want preserve known empty section and omit missing sections", got)
	}
}

func TestHomeAggregateDoesNotInventSummaryOrSectionsForUnverifiedResult(t *testing.T) {
	store := &readerHomeAggregateStoreStub{}
	service := NewReaderVNextService(store, nil)
	service.now = func() time.Time { return time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC) }

	got, err := service.HomeAggregate(context.Background())
	if err != nil {
		t.Fatalf("HomeAggregate() error = %v", err)
	}
	if got.Freshness != "partial" || !got.Partial || got.Stale {
		t.Fatalf("unverified wire state = %#v, want partial/partial/not-stale", got)
	}
	if got.Summary != "" || got.Counts != nil || got.ContinueReading != nil || got.RecentThoughts != nil || got.Todos != nil {
		t.Fatalf("unverified Home response invented data: %#v", got)
	}
}

func TestHomeAggregateDoesNotReturnPartialDTOOnRepositoryError(t *testing.T) {
	wantErr := errors.New("home counts unavailable")
	store := &readerHomeAggregateStoreStub{err: wantErr}
	service := NewReaderVNextService(store, nil)

	got, err := service.HomeAggregate(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HomeAggregate() error = %v, want %v", err, wantErr)
	}
	if got.Today != "" || got.Summary != "" || got.Counts != nil || got.ContinueReading != nil || got.RecentThoughts != nil || got.Todos != nil || got.Freshness != "" || got.Partial || got.Stale {
		t.Fatalf("HomeAggregate() returned partial DTO: %#v", got)
	}
}

func TestHomeAggregateRejectsStoresWithoutAuthoritativeSeam(t *testing.T) {
	service := NewReaderVNextService(nil, nil)

	got, err := service.HomeAggregate(context.Background())
	if !errors.Is(err, ErrReaderHomeAggregateUnavailable) {
		t.Fatalf("HomeAggregate() error = %v, want ErrReaderHomeAggregateUnavailable", err)
	}
	if got.Today != "" || got.Summary != "" || got.Counts != nil || got.ContinueReading != nil || got.RecentThoughts != nil || got.Todos != nil || got.Freshness != "" || got.Partial || got.Stale {
		t.Fatalf("HomeAggregate() = %#v, want zero response", got)
	}
}
