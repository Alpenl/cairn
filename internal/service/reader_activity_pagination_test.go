package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerActivityPageStore struct {
	repository.ReaderVNextStore
	items []model.ReaderActivity
	calls []model.ReaderActivityQuery
}

func (s *readerActivityPageStore) ListActivity(_ context.Context, query model.ReaderActivityQuery) (model.ReaderActivityPage, error) {
	s.calls = append(s.calls, query)
	start := 0
	if query.After != nil {
		for index, item := range s.items {
			if item.LastAt.Equal(query.After.LastAt) && item.Kind == query.After.Kind &&
				item.NormalizedKey == query.After.NormalizedKey && item.Key == query.After.Key {
				start = index + 1
				break
			}
		}
	}
	end := start + query.Limit
	if end > len(s.items) {
		end = len(s.items)
	}
	return model.ReaderActivityPage{
		Items:   append([]model.ReaderActivity(nil), s.items[start:end]...),
		HasMore: end < len(s.items),
	}, nil
}

func TestReaderActivityPaginatesBeyondOneHundredWithoutDuplicates(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &readerActivityPageStore{items: make([]model.ReaderActivity, 0, 102)}
	for index := 0; index < 102; index++ {
		key := fmt.Sprintf("tag-%03d", index)
		store.items = append(store.items, model.ReaderActivity{
			Kind: "tag", Key: key, NormalizedKey: key, LastAt: when,
		})
	}
	service := NewReaderVNextService(store, nil, ReaderVNextServiceOptions{CursorSigningKey: "activity-test-key"})
	ctx := context.Background()

	first, err := service.Activity(ctx, "tag", "", 100)
	if err != nil {
		t.Fatalf("Activity(first) error = %v", err)
	}
	if len(first.Tags) != 100 || first.Tags[0].Tag != "tag-000" || first.Tags[99].Tag != "tag-099" || first.NextCursor == "" {
		t.Fatalf("first page = %#v, want tag-000..tag-099 plus next_cursor", first)
	}

	second, err := service.Activity(ctx, "tag", first.NextCursor, 100)
	if err != nil {
		t.Fatalf("Activity(second) error = %v", err)
	}
	if len(second.Tags) != 2 || second.Tags[0].Tag != "tag-100" || second.Tags[1].Tag != "tag-101" || second.NextCursor != "" {
		t.Fatalf("second page = %#v, want tag-100..tag-101 and no next_cursor", second)
	}
	seen := make(map[string]struct{}, 102)
	for _, page := range []dto.ReaderActivityResponse{first, second} {
		for _, item := range page.Tags {
			tag := item.Tag
			if _, duplicate := seen[tag]; duplicate {
				t.Fatalf("duplicate activity tag %q across pages", tag)
			}
			seen[tag] = struct{}{}
		}
	}
	if len(seen) != 102 {
		t.Fatalf("unique activity tags = %d, want 102", len(seen))
	}
}

func TestReaderActivityCursorRejectsTamperingAndCrossQueryReuse(t *testing.T) {
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &readerActivityPageStore{items: []model.ReaderActivity{
		{Kind: "tag", Key: "alpha", NormalizedKey: "alpha", LastAt: when},
		{Kind: "tag", Key: "beta", NormalizedKey: "beta", LastAt: when},
	}}
	service := NewReaderVNextService(store, nil, ReaderVNextServiceOptions{CursorSigningKey: "activity-test-key"})
	ctx := context.Background()

	first, err := service.Activity(ctx, "tag", "", 1)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("Activity(first) = %#v, %v; want cursor", first, err)
	}
	tampered := first.NextCursor[:len(first.NextCursor)-1] + "x"
	for name, tc := range map[string]struct {
		ctx    context.Context
		kind   string
		cursor string
	}{
		"tampered":   {ctx: ctx, kind: "tag", cursor: tampered},
		"other kind": {ctx: ctx, kind: "domain", cursor: first.NextCursor},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.Activity(tc.ctx, tc.kind, tc.cursor, 1)
			var carrier httperr.StatusCarrier
			if !errors.As(err, &carrier) || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Fatalf("Activity() error = %v, want stable 422", err)
			}
			coder, ok := carrier.(httperr.ErrorCoder)
			if !ok || coder.HTTPErrorCode() != httperr.CodeInvalidCursor {
				t.Fatalf("Activity() error = %v, want %q", err, httperr.CodeInvalidCursor)
			}
		})
	}
}
