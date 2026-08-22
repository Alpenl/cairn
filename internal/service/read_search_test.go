package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

// searchFakeLinkStore records the ListDone filter and serves a configurable
// GetByURL result so the q=/url= routing can be asserted without a database.
type searchFakeLinkStore struct {
	repotest.BaseLinkStore
	listResult   []model.Link
	listTotal    int
	lastFilter   *repository.ListLinksFilter
	byURL        map[string]*model.Link
	lastURLQuery string
}

func (s *searchFakeLinkStore) ListDone(_ context.Context, filter repository.ListLinksFilter) ([]model.Link, int, error) {
	s.lastFilter = &filter
	return append([]model.Link(nil), s.listResult...), s.listTotal, nil
}

func (s *searchFakeLinkStore) GetDetailByURL(_ context.Context, url string) (*repository.LinkDetailProjection, error) {
	s.lastURLQuery = url
	if s.byURL == nil {
		return nil, nil
	}
	return detailForTest(s.byURL[url]), nil
}

func doneLink(id uuid.UUID) model.Link {
	return model.Link{ID: id, URL: "https://example.com/" + id.String(), Status: model.LinkStatusDone}
}

func TestSearchSetsKeywordFilter(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	store := &searchFakeLinkStore{listResult: []model.Link{doneLink(id)}, listTotal: 1}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := svc.List(context.Background(), dto.ListLinksRequest{Query: "vector database", Page: 3, After: "ignored"})
	if err != nil {
		t.Fatalf("List q=: %v", err)
	}
	if store.lastFilter == nil || store.lastFilter.Query == nil || *store.lastFilter.Query != "vector database" {
		t.Fatalf("filter.Query not set to trimmed query: %+v", store.lastFilter)
	}
	if store.lastFilter.Limit != searchResponseLimit {
		t.Fatalf("filter.Limit = %d, want %d", store.lastFilter.Limit, searchResponseLimit)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.NextCursor != nil || resp.Page != 0 {
		t.Fatalf("unexpected response shape: total=%d items=%d page=%d cursor=%v", resp.Total, len(resp.Items), resp.Page, resp.NextCursor)
	}
}

// TestBlankQueryFallsThroughToList: a whitespace-only q is treated as not
// supplied — the normal offset list path runs (Query nil, Page honoured).
func TestBlankQueryFallsThroughToList(t *testing.T) {
	t.Parallel()
	store := &searchFakeLinkStore{}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	if _, err := svc.List(context.Background(), dto.ListLinksRequest{Query: "   ", Page: 2, Limit: 20}); err != nil {
		t.Fatalf("List blank q=: %v", err)
	}
	if store.lastFilter == nil || store.lastFilter.Query != nil {
		t.Fatalf("blank q must not enter hybrid mode: %+v", store.lastFilter)
	}
	if store.lastFilter.Offset != 20 {
		t.Fatalf("normal list path should compute offset; got %d", store.lastFilter.Offset)
	}
}

// TestOverlongQueryReturns422: a query past maxListQueryLen returns 422 with the
// query_too_long slug and never touches the store.
func TestOverlongQueryReturns422(t *testing.T) {
	t.Parallel()
	store := &searchFakeLinkStore{}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	long := make([]rune, maxListQueryLen+1)
	for i := range long {
		long[i] = 'a'
	}
	_, err := svc.List(context.Background(), dto.ListLinksRequest{Query: string(long)})
	var he *problem.Error
	if !errors.As(err, &he) || problemHTTPStatus(he) != http.StatusUnprocessableEntity || he.Code() != httperr.CodeQueryTooLong {
		t.Fatalf("want 422 query_too_long, got %v", err)
	}
	if store.lastFilter != nil {
		t.Fatal("overlong q must not reach the store")
	}
}

// TestURLTakesPrecedenceOverQuery: when both url= and q= are supplied the url=
// existence check wins (GetByURL called, ListDone not).
func TestURLTakesPrecedenceOverQuery(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	link := doneLink(id)
	store := &searchFakeLinkStore{byURL: map[string]*model.Link{"https://github.com/astral-sh/uv": &link}}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := svc.List(context.Background(), dto.ListLinksRequest{URL: "https://github.com/astral-sh/uv", Query: "should be ignored"})
	if err != nil {
		t.Fatalf("List url=: %v", err)
	}
	if store.lastURLQuery != "https://github.com/astral-sh/uv" {
		t.Fatalf("GetByURL queried %q", store.lastURLQuery)
	}
	if store.lastFilter != nil {
		t.Fatal("url= must short-circuit before ListDone")
	}
	if len(resp.Items) != 1 || resp.Total != 1 {
		t.Fatalf("url= hit should return exactly 1 item; got %d total=%d", len(resp.Items), resp.Total)
	}
}

// TestURLMissReturnsEmpty: a url= miss returns an empty (non-nil) items array,
// total 0.
func TestURLMissReturnsEmpty(t *testing.T) {
	t.Parallel()
	store := &searchFakeLinkStore{byURL: map[string]*model.Link{}}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := svc.List(context.Background(), dto.ListLinksRequest{URL: "https://example.com/not-stored"})
	if err != nil {
		t.Fatalf("List url= miss: %v", err)
	}
	if resp.Items == nil || len(resp.Items) != 0 || resp.Total != 0 {
		t.Fatalf("url= miss should return empty items / total 0; got %+v", resp)
	}
}

// TestURLInvalidReturns422: a malformed url= surfaces the validateURL 422
// rather than silently returning empty.
func TestURLInvalidReturns422(t *testing.T) {
	t.Parallel()
	store := &searchFakeLinkStore{}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	_, err := svc.List(context.Background(), dto.ListLinksRequest{URL: "not-a-url"})
	var he *problem.Error
	if !errors.As(err, &he) || problemHTTPStatus(he) != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for invalid url, got %v", err)
	}
}

// TestSearchFilterRejectsBadContentType: filters that combine with q= are
// validated — a bad content_type still 422s in search mode.
func TestSearchFilterRejectsBadContentType(t *testing.T) {
	t.Parallel()
	store := &searchFakeLinkStore{}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store})

	_, err := svc.List(context.Background(), dto.ListLinksRequest{Query: "rust", ContentType: "bogus"})
	var he *problem.Error
	if !errors.As(err, &he) || problemHTTPStatus(he) != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for bad content_type in q= mode, got %v", err)
	}
	if store.lastFilter != nil {
		t.Fatal("validation failure must not reach the store")
	}
}
