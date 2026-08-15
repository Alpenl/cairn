package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

// exportPagingStore is a cursor-paginating link store for the export tests. It
// honours filter.Limit + filter.After exactly like the production repo's cursor
// path: rows are ordered (created_at DESC, id DESC) and each call returns up to
// Limit rows strictly after the cursor. It also records every filter it saw so
// tests can assert the export requested status=done in cursor mode.
type exportPagingStore struct {
	repotest.BaseLinkStore
	rows    []model.Link
	filters []repository.ListLinksFilter
	err     error
}

func (s *exportPagingStore) ListDone(_ context.Context, filter repository.ListLinksFilter) ([]model.Link, int, error) {
	s.filters = append(s.filters, filter)
	if s.err != nil {
		return nil, 0, s.err
	}

	ordered := append([]model.Link(nil), s.rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.After(ordered[j].CreatedAt)
		}
		return ordered[i].ID.String() > ordered[j].ID.String()
	})

	start := 0
	if filter.After != nil {
		for idx, l := range ordered {
			if l.ID == filter.After.ID {
				start = idx + 1
				break
			}
		}
	}
	end := start + filter.Limit
	if end > len(ordered) {
		end = len(ordered)
	}
	page := ordered[start:end]
	return append([]model.Link(nil), page...), 0, nil
}

func exportTestLink(i int, now time.Time) model.Link {
	title := "Title " + uuidShort(i)
	return model.Link{
		ID:          uuid.New(),
		URL:         "https://example.com/" + uuidShort(i),
		Title:       &title,
		Status:      model.LinkStatusDone,
		CreatedAt:   now.Add(-time.Duration(i) * time.Second),
		UpdatedAt:   now,
		FetcherType: ptr("basic"),
	}
}

func uuidShort(i int) string {
	const digits = "0123456789abcdef"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%16]}, b...)
		i /= 16
	}
	return string(b)
}

func ptr(s string) *string { return &s }

func decodeExport(t *testing.T, body []byte) []dto.LinkResponse {
	t.Helper()
	var items []dto.LinkResponse
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("export body is not a valid JSON array: %v\nbody=%s", err, body)
	}
	return items
}

// TestExportStreamsAllDoneLinksSingleBatch: a handful of done links serialize as
// a valid JSON array with every link present.
func TestExportStreamsAllDoneLinksSingleBatch(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	rows := make([]model.Link, 3)
	for i := range rows {
		rows[i] = exportTestLink(i, now)
	}
	store := &exportPagingStore{rows: rows}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	var buf bytes.Buffer
	if err := svc.Export(context.Background(), &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	items := decodeExport(t, buf.Bytes())
	if len(items) != 3 {
		t.Fatalf("exported %d items, want 3", len(items))
	}

	// Every export must request status=done in cursor mode.
	if len(store.filters) == 0 {
		t.Fatal("ListDone was never called")
	}
	f := store.filters[0]
	if !f.Cursor || len(f.Statuses) != 1 || f.Statuses[0] != string(model.LinkStatusDone) {
		t.Fatalf("first export filter = %+v, want cursor + status=done", f)
	}
}

// TestExportPaginatesAcrossBatchesAndTerminates: with more rows than one batch,
// the export advances the cursor, returns every row exactly once, and stops
// (does not loop forever).
func TestExportPaginatesAcrossBatchesAndTerminates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	const n = exportBatchSize + 7 // forces a second (short) batch
	rows := make([]model.Link, n)
	for i := range rows {
		rows[i] = exportTestLink(i, now)
	}
	store := &exportPagingStore{rows: rows}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	var buf bytes.Buffer
	if err := svc.Export(context.Background(), &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	items := decodeExport(t, buf.Bytes())
	if len(items) != n {
		t.Fatalf("exported %d items, want %d (every row exactly once)", len(items), n)
	}
	// Two batches: a full one (exportBatchSize) then the short remainder. The
	// short batch terminates the loop without an extra empty round trip.
	if len(store.filters) != 2 {
		t.Fatalf("ListDone calls = %d, want 2 (full batch + short batch)", len(store.filters))
	}
	if store.filters[0].After != nil {
		t.Fatalf("first batch must start at the stream head (After=nil); got %+v", store.filters[0].After)
	}
	if store.filters[1].After == nil {
		t.Fatalf("second batch must carry an advanced cursor; got After=nil")
	}

	// No duplicates across batches.
	seen := make(map[string]struct{}, n)
	for _, it := range items {
		if _, dup := seen[it.ID]; dup {
			t.Fatalf("link %s exported twice across batches", it.ID)
		}
		seen[it.ID] = struct{}{}
	}
}

// TestExportEmptyKnowledgeBase: zero done links still produce a valid empty JSON
// array.
func TestExportEmptyKnowledgeBase(t *testing.T) {
	t.Parallel()
	store := &exportPagingStore{}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	var buf bytes.Buffer
	if err := svc.Export(context.Background(), &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if buf.String() != "[]" {
		t.Fatalf("empty export = %q, want []", buf.String())
	}
}

func TestExportWithCountMatchesStreamedRows(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	rows := []model.Link{
		exportTestLink(1, now),
		exportTestLink(2, now),
		exportTestLink(3, now),
	}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: &exportPagingStore{rows: rows}})

	var buf bytes.Buffer
	count, err := svc.ExportWithCount(context.Background(), &buf)
	if err != nil {
		t.Fatalf("ExportWithCount: %v", err)
	}
	if got := len(decodeExport(t, buf.Bytes())); got != count {
		t.Fatalf("streamed rows = %d, returned count = %d", got, count)
	}
}

// TestExportPropagatesFirstBatchError: a DB error on the first batch (before any
// byte is written past "[") is returned so the handler can still log it.
func TestExportPropagatesFirstBatchError(t *testing.T) {
	t.Parallel()
	store := &exportPagingStore{err: errors.New("db down")}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	var buf bytes.Buffer
	err := svc.Export(context.Background(), &buf)
	if err == nil {
		t.Fatal("Export must surface the first-batch DB error")
	}
}
