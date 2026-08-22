package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/errsafe"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestNewLinkReadServiceRequiresLinks(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("NewLinkReadService() did not panic without Links")
		}
	}()
	NewLinkReadService(LinkReadServiceOptions{})
}

func TestLinkReadServiceListNormalizesFiltersAndMapsItems(t *testing.T) {
	t.Parallel()

	linkID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	parentID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	now := time.Unix(0, 0).UTC()
	title := "Fetched title"
	summary := "Summary"
	domain := "example.com"
	contentType := "article"
	pathDepth := 2
	fetcherType := "basic"
	errorMsg := "unused"

	store := &readFakeLinkStore{
		listDoneResult: []model.Link{
			{
				ID:              linkID,
				URL:             "https://example.com/post",
				Title:           &title,
				Summary:         &summary,
				Tags:            []string{"Go", "AI"},
				FetcherType:     &fetcherType,
				IsLowConfidence: true,
				Status:          model.LinkStatusDone,
				ErrorMsg:        &errorMsg,
				Domain:          &domain,
				ContentType:     &contentType,
				PathDepth:       &pathDepth,
				ParentID:        &parentID,
				CreatedAt:       now,
				UpdatedAt:       now,
			},
		},
		listDoneTotal: 17,
	}

	service := NewLinkReadService(LinkReadServiceOptions{Links: store})
	got, err := service.List(context.Background(), dto.ListLinksRequest{
		Tags:        " Go, AI ,, ",
		Domain:      "example.com",
		ContentType: "article",
		Page:        2,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if store.lastListFilter == nil {
		t.Fatal("ListDone() filter was not recorded")
	}
	if len(store.lastListFilter.Tags) != 2 || store.lastListFilter.Tags[0] != "Go" || store.lastListFilter.Tags[1] != "AI" {
		t.Fatalf("filter tags = %#v, want [Go AI]", store.lastListFilter.Tags)
	}
	if store.lastListFilter.Domain == nil || *store.lastListFilter.Domain != "example.com" {
		t.Fatalf("filter domain = %#v, want example.com", store.lastListFilter.Domain)
	}
	if store.lastListFilter.ContentType == nil || *store.lastListFilter.ContentType != "article" {
		t.Fatalf("filter content type = %#v, want article", store.lastListFilter.ContentType)
	}
	if store.lastListFilter.Offset != 10 || store.lastListFilter.Limit != 10 {
		t.Fatalf("filter pagination = %#v, want offset=10 limit=10", *store.lastListFilter)
	}

	if got.Total != 17 || got.Page != 2 || got.Limit != 10 {
		t.Fatalf("pagination = %#v, want total=17 page=2 limit=10", got)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(got.Items))
	}
	if got.Items[0].ID != linkID.String() || got.Items[0].ParentID == nil || *got.Items[0].ParentID != parentID.String() {
		t.Fatalf("mapped item = %#v, want ids preserved", got.Items[0])
	}
	if got.Items[0].LowConfidenceReason == nil || *got.Items[0].LowConfidenceReason == "" {
		t.Fatalf("low confidence reason = %#v, want derived reason", got.Items[0].LowConfidenceReason)
	}
	if got.Items[0].ErrorCategory != nil {
		t.Fatalf("error category = %#v, want nil for done link (omitted)", got.Items[0].ErrorCategory)
	}
}

func TestLinkToResponseNormalizesMissingTagsToEmptyArray(t *testing.T) {
	t.Parallel()

	response := linkToResponse(model.Link{Tags: nil})
	if response.Tags == nil {
		t.Fatal("linkToResponse() tags = nil, want a non-nil empty array")
	}
	if len(response.Tags) != 0 {
		t.Fatalf("linkToResponse() tags = %#v, want []", response.Tags)
	}
}

func TestDeriveLowConfidenceReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link model.Link
		want *string
	}{
		{
			name: "search fallback reason",
			link: model.Link{
				IsLowConfidence: true,
				FetcherType:     stringPtr("basic+search"),
			},
			want: stringPtr("search_fallback"),
		},
		{
			name: "thin content reason",
			link: model.Link{
				IsLowConfidence: true,
				FetcherType:     stringPtr("basic+thin"),
			},
			want: stringPtr("thin_content"),
		},
		{
			name: "generic fetch quality reason",
			link: model.Link{
				IsLowConfidence: true,
				FetcherType:     stringPtr("basic"),
				ErrorMsg:        stringPtr("fetch_quality"),
			},
			want: stringPtr("fetch_quality"),
		},
		{
			name: "persisted title quality reason",
			link: model.Link{
				IsLowConfidence: true,
				ErrorMsg:        stringPtr("title_quality"),
			},
			want: stringPtr("title_quality"),
		},
		{
			name: "unknown reason without fetcher type",
			link: model.Link{
				IsLowConfidence: true,
			},
			want: stringPtr("unknown"),
		},
		{
			name: "no reason when confidence is normal",
			link: model.Link{
				IsLowConfidence: false,
				FetcherType:     stringPtr("basic+search"),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := deriveLowConfidenceReason(tt.link)
			if !equalStringPointers(got, tt.want) {
				t.Fatalf("deriveLowConfidenceReason() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestClassifyErrorMessageAndStatusAwareDerivation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *string
		want string
	}{
		{
			name: "none for empty",
			msg:  nil,
			want: "none",
		},
		{
			name: "upstream http",
			msg:  stringPtr("analyzer call failed: status=502 body=temporary upstream failure"),
			want: "upstream_http",
		},
		{
			name: "timeout",
			msg:  stringPtr("context deadline exceeded"),
			want: "timeout",
		},
		{
			name: "network",
			msg:  stringPtr("dial tcp: lookup example.com: no such host"),
			want: "network",
		},
		{
			name: "unsafe target",
			msg:  stringPtr("analyzer call failed: Post \"http://127.0.0.1:8080/chat/completions\": unsafe target host: 127.0.0.1"),
			want: "unsafe_target",
		},
		{
			name: "content type",
			msg:  stringPtr("analyzer response content-type is not JSON: text/html; charset=utf-8"),
			want: "content_type",
		},
		{
			name: "response too large",
			msg:  stringPtr("fetch https://example.com/file.pdf: PDF exceeds size limit"),
			want: "response_too_large",
		},
		{
			name: "parse",
			msg:  stringPtr("invalid character 'x' looking for beginning of value"),
			want: "parse",
		},
		{
			name: "unknown",
			msg:  stringPtr("unexpected processing failure"),
			want: "unknown",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := errsafe.ClassifyPersisted(derefOrEmpty(tt.msg)); got != tt.want {
				t.Fatalf("errsafe.ClassifyPersisted() = %q, want %q", got, tt.want)
			}
		})
	}

	// Done state collapses the category to "" so the response wrapper
	// (stringPtr → omitempty) drops the field instead of writing a
	// literal "none" with no information.
	doneErr := stringPtr("analyzer call failed: status=502 body=stale error")
	if got := deriveLinkErrorCategory(model.Link{
		Status:   model.LinkStatusDone,
		ErrorMsg: doneErr,
	}); got != "" {
		t.Fatalf("deriveLinkErrorCategory(done) = %q, want empty (omitted)", got)
	}
}

func TestTagReadServiceListsCountsAndMapsResponse(t *testing.T) {
	t.Parallel()

	store := &readFakeTagStore{
		counts: []repository.TagCount{
			{Tag: "Go", Count: 3},
			{Tag: "AI", Count: 2},
		},
	}
	service := NewTagReadService(store)

	got, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := []dto.TagCountResponse{
		{Tag: "Go", Count: 3},
		{Tag: "AI", Count: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("len(tags) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestTagReadServiceReturnsEmptyArrayWhenNoTagsExist(t *testing.T) {
	t.Parallel()

	service := NewTagReadService(&readFakeTagStore{counts: nil})
	got, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got == nil {
		t.Fatal("List() = nil, want a non-nil empty array")
	}
	if len(got) != 0 {
		t.Fatalf("List() = %#v, want []", got)
	}
}

func TestTagReadServiceListsScopedCountsWithSeparateSiteCardinality(t *testing.T) {
	store := &readFakeTagStore{scoped: []repository.ScopedTagCount{{Tag: "whiteboard", Count: 5, ReadingCount: 2, SiteCount: 3}}}
	got, err := NewTagReadService(store).ListScoped(context.Background(), "all")
	if err != nil {
		t.Fatalf("ListScoped() error = %v", err)
	}
	if store.scope != "all" || len(got) != 1 || got[0].Count != 5 || got[0].ReadingCount == nil || *got[0].ReadingCount != 2 || got[0].SiteCount == nil || *got[0].SiteCount != 3 {
		t.Fatalf("ListScoped() = %#v", got)
	}
}

func TestTreeReadServiceBuildsNestedResponse(t *testing.T) {
	t.Parallel()

	parentID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	childID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	now := time.Unix(0, 0).UTC()
	domain := "example.com"

	store := &readFakeTreeStore{
		visible: []model.Link{
			{
				ID:        parentID,
				URL:       "https://example.com/",
				Status:    model.LinkStatusDone,
				Domain:    &domain,
				CreatedAt: now,
				UpdatedAt: now,
			},
			{
				ID:        childID,
				URL:       "https://example.com/post",
				Status:    model.LinkStatusDone,
				Domain:    &domain,
				ParentID:  &parentID,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	service := NewTreeReadService(store)
	got, err := service.Get(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if store.lastDomain == nil || *store.lastDomain != "example.com" {
		t.Fatalf("domain filter = %#v, want example.com", store.lastDomain)
	}
	if got.Total != 2 || len(got.Nodes) != 1 {
		t.Fatalf("tree response = %#v, want single root and total=2", got)
	}
	if len(got.Nodes[0].Children) != 1 || got.Nodes[0].Children[0].ID != childID.String() {
		t.Fatalf("tree children = %#v, want child %s", got.Nodes[0].Children, childID)
	}
}

func TestTreeReadServiceListDomainsSummarizesCounts(t *testing.T) {
	t.Parallel()

	store := &readFakeTreeStore{
		domainSummary: repository.DomainTreeSummarySet{
			Domains: []repository.DomainTreeSummary{
				{Domain: "example.com", Count: 3},
				{Domain: "news.ycombinator.com", Count: 2},
			},
			Total: 6,
		},
	}

	service := NewTreeReadService(store)
	got, err := service.ListDomains(context.Background())
	if err != nil {
		t.Fatalf("ListDomains() error = %v", err)
	}
	if got.Total != 6 || len(got.Domains) != 2 {
		t.Fatalf("got = %#v, want 2 domains and total 6", got)
	}
	if got.Domains[0].Domain != "example.com" || got.Domains[0].Count != 3 {
		t.Fatalf("got.Domains[0] = %#v, want example.com/3", got.Domains[0])
	}
}

func TestTreeReadServiceListDomainsScopedNormalizesAndRejectsInvalidScope(t *testing.T) {
	t.Parallel()

	store := &readFakeTreeStore{
		domainSummary: repository.DomainTreeSummarySet{
			Domains: []repository.DomainTreeSummary{{Domain: "reading.example", Count: 1}},
			Total:   2,
		},
	}
	service := NewTreeReadService(store)

	got, err := service.ListDomainsScoped(context.Background(), "  READING ")
	if err != nil {
		t.Fatalf("ListDomainsScoped(reading) error = %v", err)
	}
	if store.lastLibraryKind != model.LibraryKindReading {
		t.Fatalf("repository scope = %q, want reading", store.lastLibraryKind)
	}
	if got.LibraryKind == nil || *got.LibraryKind != string(model.LibraryKindReading) || got.Total != 2 {
		t.Fatalf("scoped response = %#v, want reading scope and total 2", got)
	}

	_, err = service.ListDomainsScoped(context.Background(), "all")
	httpErr, ok := httperr.As(err)
	if !ok || httpErr.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("invalid scope error = %#v, want stable 422", err)
	}
}

func equalStringPointers(got, want *string) bool {
	switch {
	case got == nil || want == nil:
		return got == nil && want == nil
	default:
		return *got == *want
	}
}

type readFakeLinkStore struct {
	repotest.BaseLinkStore
	byID           map[uuid.UUID]*model.Link
	listDoneResult []model.Link
	listDoneTotal  int
	listDoneErr    error
	lastListFilter *repository.ListLinksFilter
	deleteErr      error
	deletedIDs     []uuid.UUID
}

func (s *readFakeLinkStore) GetByID(_ context.Context, id uuid.UUID) (*model.Link, error) {
	if s.byID == nil {
		return nil, nil
	}
	return s.byID[id], nil
}

func (s *readFakeLinkStore) GetDetailByID(_ context.Context, id uuid.UUID) (*repository.LinkDetailProjection, error) {
	if s.byID == nil {
		return nil, nil
	}
	return detailForTest(s.byID[id]), nil
}

func (s *readFakeLinkStore) GetLifecycleByID(_ context.Context, id uuid.UUID) (*repository.LinkLifecycleProjection, error) {
	if s.byID == nil {
		return nil, nil
	}
	return lifecycleForTest(s.byID[id]), nil
}

func (s *readFakeLinkStore) ListDone(_ context.Context, filter repository.ListLinksFilter) ([]model.Link, int, error) {
	s.lastListFilter = &filter
	return append([]model.Link(nil), s.listDoneResult...), s.listDoneTotal, s.listDoneErr
}

func (s *readFakeLinkStore) Delete(_ context.Context, id uuid.UUID) error {
	s.deletedIDs = append(s.deletedIDs, id)
	return s.deleteErr
}

type readFakeTagStore struct {
	counts []repository.TagCount
	scoped []repository.ScopedTagCount
	scope  string
}

func (s *readFakeTagStore) ListDistinct(context.Context) ([]string, error) {
	return nil, nil
}

func (s *readFakeTagStore) ListCounts(context.Context) ([]repository.TagCount, error) {
	return append([]repository.TagCount(nil), s.counts...), nil
}

func (s *readFakeTagStore) ListScopedCounts(_ context.Context, scope string) ([]repository.ScopedTagCount, error) {
	s.scope = scope
	return append([]repository.ScopedTagCount(nil), s.scoped...), nil
}

type readFakeTreeStore struct {
	repotest.BaseTreeStore
	lastDomain      *string
	lastLibraryKind model.LibraryKind
	visible         []model.Link
	domainSummary   repository.DomainTreeSummarySet
}

func (s *readFakeTreeStore) ListVisible(_ context.Context, domain *string) ([]model.Link, error) {
	s.lastDomain = domain
	return append([]model.Link(nil), s.visible...), nil
}

func (s *readFakeTreeStore) ListDomains(context.Context) (repository.DomainTreeSummarySet, error) {
	return s.domainSummary, nil
}

func (s *readFakeTreeStore) ListDomainsScoped(_ context.Context, kind model.LibraryKind) (repository.DomainTreeSummarySet, error) {
	s.lastLibraryKind = kind
	return s.domainSummary, nil
}

// TestLinkReadServiceListRejectsAbusiveFilters pins the input bounds we
// rely on to keep listLinks from forwarding unchecked URL query strings
// straight to PG. Each subtest exercises one rejected shape.
func TestLinkReadServiceListRejectsAbusiveFilters(t *testing.T) {
	t.Parallel()

	tooManyTags := strings.Repeat("a,", maxListTagFilters+1)
	tooManyTags = strings.TrimRight(tooManyTags, ",")

	cases := []struct {
		name string
		req  dto.ListLinksRequest
		want string
	}{
		{
			name: "too many tag filters",
			req:  dto.ListLinksRequest{Tags: tooManyTags},
			want: "too many tag filters",
		},
		{
			name: "tag filter too long",
			req:  dto.ListLinksRequest{Tags: strings.Repeat("x", maxListTagFilterLen+1)},
			want: "tag filter too long",
		},
		{
			name: "unknown content_type",
			req:  dto.ListLinksRequest{ContentType: "podcast"},
			want: "unsupported content_type filter",
		},
		{
			name: "domain too long",
			req:  dto.ListLinksRequest{Domain: strings.Repeat("a", maxListDomainLen+1)},
			want: "domain filter too long",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := NewLinkReadService(LinkReadServiceOptions{Links: &readFakeLinkStore{}})
			_, err := service.List(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("List() error = nil, want %q", tc.want)
			}
			var statusErr *problem.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("List() error = %T, want *problem.Error", err)
			}
			if problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", problemHTTPStatus(statusErr))
			}
			if statusErr.Message() != tc.want {
				t.Fatalf("message = %q, want %q", statusErr.Message(), tc.want)
			}
		})
	}
}

func TestLinkReadServiceListRejectsInvalidCreatedRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  dto.ListLinksRequest
	}{
		{
			name: "missing lower bound",
			req:  dto.ListLinksRequest{CreatedBefore: "2026-08-11T16:00:00Z"},
		},
		{
			name: "missing upper bound",
			req:  dto.ListLinksRequest{CreatedFrom: "2026-08-10T16:00:00Z"},
		},
		{
			name: "malformed lower bound",
			req: dto.ListLinksRequest{
				CreatedFrom:   "2026-08-11",
				CreatedBefore: "2026-08-11T16:00:00Z",
			},
		},
		{
			name: "malformed upper bound",
			req: dto.ListLinksRequest{
				CreatedFrom:   "2026-08-10T16:00:00Z",
				CreatedBefore: "tomorrow",
			},
		},
		{
			name: "empty range",
			req: dto.ListLinksRequest{
				CreatedFrom:   "2026-08-10T16:00:00Z",
				CreatedBefore: "2026-08-10T16:00:00Z",
			},
		},
		{
			name: "reversed range",
			req: dto.ListLinksRequest{
				CreatedFrom:   "2026-08-11T16:00:00Z",
				CreatedBefore: "2026-08-10T16:00:00Z",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &readFakeLinkStore{}
			service := NewLinkReadService(LinkReadServiceOptions{Links: store})

			_, err := service.List(context.Background(), tc.req)
			if err == nil {
				t.Fatal("List() error = nil, want stable 422")
			}
			var statusErr *problem.Error
			if !errors.As(err, &statusErr) {
				t.Fatalf("List() error = %T, want *problem.Error", err)
			}
			if problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", problemHTTPStatus(statusErr))
			}
			if statusErr.Code() != "invalid_created_range" {
				t.Fatalf("error_code = %q, want invalid_created_range", statusErr.Code())
			}
			if store.lastListFilter != nil {
				t.Fatal("invalid range reached repository")
			}
		})
	}
}

// TestSplitAndValidateStatuses is the table-driven contract for the
// ?status= query parameter: all saved links on empty, whitelist validation
// with 400 on illegal tokens, de-duplication, and the count cap.
func TestSplitAndValidateStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		want      []string
		wantErr   bool
		wantMsg   string
		wantSlug  string
		httpState int
	}{
		{name: "empty includes every saved link", raw: "", want: []string{"pending", "processing", "failed", "done"}},
		{name: "whitespace includes every saved link", raw: "   ", want: []string{"pending", "processing", "failed", "done"}},
		{name: "commas only include every saved link", raw: ",, ,", want: []string{"pending", "processing", "failed", "done"}},
		{name: "single done", raw: "done", want: []string{"done"}},
		{name: "single pending", raw: "pending", want: []string{"pending"}},
		{name: "single processing", raw: "processing", want: []string{"processing"}},
		{name: "single failed", raw: "failed", want: []string{"failed"}},
		{name: "extension trio", raw: "pending,processing,failed", want: []string{"pending", "processing", "failed"}},
		{name: "all four", raw: "pending,processing,failed,done", want: []string{"pending", "processing", "failed", "done"}},
		{name: "trims whitespace", raw: " pending , failed ", want: []string{"pending", "failed"}},
		{name: "dedupes repeats preserving first-seen order", raw: "failed,failed,pending,failed", want: []string{"failed", "pending"}},
		{
			name:      "illegal token rejected with 400",
			raw:       "faild",
			wantErr:   true,
			wantMsg:   "unsupported status filter: faild (allowed: pending, processing, failed, done)",
			wantSlug:  httperr.CodeUnsupportedStatusFilter,
			httpState: http.StatusBadRequest,
		},
		{
			name:      "unknown state not allowed",
			raw:       "queued",
			wantErr:   true,
			wantMsg:   "unsupported status filter: queued (allowed: pending, processing, failed, done)",
			wantSlug:  httperr.CodeUnsupportedStatusFilter,
			httpState: http.StatusBadRequest,
		},
		{
			name:      "one bad token among valid ones rejected",
			raw:       "pending,bogus,done",
			wantErr:   true,
			wantMsg:   "unsupported status filter: bogus (allowed: pending, processing, failed, done)",
			wantSlug:  httperr.CodeUnsupportedStatusFilter,
			httpState: http.StatusBadRequest,
		},
		{
			name:      "too many tokens rejected",
			raw:       strings.TrimRight(strings.Repeat("done,", maxListStatusFilters+1), ","),
			wantErr:   true,
			wantMsg:   "too many status filters",
			wantSlug:  httperr.CodeUnsupportedStatusFilter,
			httpState: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := splitAndValidateStatuses(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitAndValidateStatuses(%q) error = nil, want %q", tc.raw, tc.wantMsg)
				}
				var statusErr *problem.Error
				if !errors.As(err, &statusErr) {
					t.Fatalf("error = %T, want *problem.Error", err)
				}
				if problemHTTPStatus(statusErr) != tc.httpState {
					t.Fatalf("status = %d, want %d", problemHTTPStatus(statusErr), tc.httpState)
				}
				if statusErr.Message() != tc.wantMsg {
					t.Fatalf("message = %q, want %q", statusErr.Message(), tc.wantMsg)
				}
				if statusErr.Code() != tc.wantSlug {
					t.Fatalf("slug = %q, want %q", statusErr.Code(), tc.wantSlug)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitAndValidateStatuses(%q) error = %v, want nil", tc.raw, err)
			}
			if !equalStringSlices(got, tc.want) {
				t.Fatalf("splitAndValidateStatuses(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestLinkReadServiceListPropagatesStatusFilter pins that the ?status=
// set flows through to the repository filter alongside the existing
// orthogonal filters, and that the non-done statuses returned by the
// repo are mapped with their status / error fields intact (the field
// the extension's "processing / failed" partitions read).
func TestLinkReadServiceListPropagatesStatusFilter(t *testing.T) {
	t.Parallel()

	pendingID := uuid.MustParse("aaaaaaaa-1111-1111-1111-111111111111")
	failedID := uuid.MustParse("bbbbbbbb-2222-2222-2222-222222222222")
	now := time.Unix(0, 0).UTC()
	failErr := "analyzer call failed: status=502 body=temporary upstream failure"

	store := &readFakeLinkStore{
		listDoneResult: []model.Link{
			{ID: pendingID, URL: "https://example.com/a", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
			{ID: failedID, URL: "https://example.com/b", Status: model.LinkStatusFailed, ErrorMsg: &failErr, CreatedAt: now, UpdatedAt: now},
		},
		listDoneTotal: 2,
	}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	got, err := service.List(context.Background(), dto.ListLinksRequest{
		Status:      "pending,processing,failed",
		Domain:      "example.com",
		ContentType: "article",
		Page:        1,
		Limit:       20,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if store.lastListFilter == nil {
		t.Fatal("ListDone() filter was not recorded")
	}
	wantStatuses := []string{"pending", "processing", "failed"}
	if !equalStringSlices(store.lastListFilter.Statuses, wantStatuses) {
		t.Fatalf("filter.Statuses = %#v, want %#v", store.lastListFilter.Statuses, wantStatuses)
	}
	// Orthogonal filters must still be combined.
	if store.lastListFilter.Domain == nil || *store.lastListFilter.Domain != "example.com" {
		t.Fatalf("filter.Domain = %#v, want example.com", store.lastListFilter.Domain)
	}
	if store.lastListFilter.ContentType == nil || *store.lastListFilter.ContentType != "article" {
		t.Fatalf("filter.ContentType = %#v, want article", store.lastListFilter.ContentType)
	}

	if len(got.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(got.Items))
	}
	if got.Items[0].Status != string(model.LinkStatusPending) {
		t.Fatalf("items[0].Status = %q, want pending", got.Items[0].Status)
	}
	failed := got.Items[1]
	if failed.Status != string(model.LinkStatusFailed) {
		t.Fatalf("items[1].Status = %q, want failed", failed.Status)
	}
	if failed.ErrorCategory == nil || *failed.ErrorCategory != "upstream_http" {
		t.Fatalf("items[1].ErrorCategory = %#v, want upstream_http", failed.ErrorCategory)
	}
	if failed.ErrorMsg == nil || *failed.ErrorMsg != failErr {
		t.Fatalf("items[1].ErrorMsg = %#v, want surfaced", failed.ErrorMsg)
	}
}

func TestLinkReadServiceListCombinesCreatedRangeWithReadingFiltersAndCursor(t *testing.T) {
	t.Parallel()

	store := &readFakeLinkStore{}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	_, err := service.List(context.Background(), dto.ListLinksRequest{
		CreatedFrom:   "2026-08-10T16:00:00Z",
		CreatedBefore: "2026-08-11T16:00:00Z",
		LibraryKind:   "reading",
		Status:        "done",
		Tags:          "go,ai",
		Domain:        "example.com",
		Cursor:        true,
		Limit:         30,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	filter := store.lastListFilter
	if filter == nil {
		t.Fatal("ListDone() filter was not recorded")
	}
	if filter.CreatedFrom == nil || !filter.CreatedFrom.Equal(time.Date(2026, 8, 10, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("filter.CreatedFrom = %v, want 2026-08-10T16:00:00Z", filter.CreatedFrom)
	}
	if filter.CreatedBefore == nil || !filter.CreatedBefore.Equal(time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("filter.CreatedBefore = %v, want 2026-08-11T16:00:00Z", filter.CreatedBefore)
	}
	if filter.LibraryKind == nil || *filter.LibraryKind != model.LibraryKindReading {
		t.Fatalf("filter.LibraryKind = %v, want reading", filter.LibraryKind)
	}
	if !equalStringSlices(filter.Statuses, []string{"done"}) || !equalStringSlices(filter.Tags, []string{"go", "ai"}) {
		t.Fatalf("filter status/tags = %#v/%#v", filter.Statuses, filter.Tags)
	}
	if filter.Domain == nil || *filter.Domain != "example.com" {
		t.Fatalf("filter.Domain = %v, want example.com", filter.Domain)
	}
	if !filter.Cursor {
		t.Fatal("filter.Cursor = false, want true")
	}
}

// TestLinkReadServiceListDefaultsToDoneReading preserves the legacy reader
// view: a plain list request must not mix website cards or active work into
// the reading stream. Operational callers opt into those states with status.
func TestLinkReadServiceListDefaultsToDoneReading(t *testing.T) {
	t.Parallel()

	store := &readFakeLinkStore{listDoneTotal: 0}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	_, err := service.List(context.Background(), dto.ListLinksRequest{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if store.lastListFilter == nil {
		t.Fatal("ListDone() was not called")
	}
	want := []string{"done"}
	if !equalStringSlices(store.lastListFilter.Statuses, want) {
		t.Fatalf("filter.Statuses = %#v, want %#v", store.lastListFilter.Statuses, want)
	}
	if store.lastListFilter.LibraryKind == nil || *store.lastListFilter.LibraryKind != model.LibraryKindReading {
		t.Fatalf("filter.LibraryKind = %v, want reading", store.lastListFilter.LibraryKind)
	}
}

func TestLinkReadServiceListAllowsExplicitSiteAndActivityViews(t *testing.T) {
	t.Parallel()
	store := &readFakeLinkStore{listDoneTotal: 0}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})
	_, err := service.List(context.Background(), dto.ListLinksRequest{LibraryKind: "site", Status: "done", Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("site list: %v", err)
	}
	if store.lastListFilter.LibraryKind == nil || *store.lastListFilter.LibraryKind != model.LibraryKindSite {
		t.Fatalf("site filter = %#v", store.lastListFilter)
	}
	_, err = service.List(context.Background(), dto.ListLinksRequest{Status: "pending,failed", Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("activity list: %v", err)
	}
	if store.lastListFilter.LibraryKind != nil {
		t.Fatalf("activity filter unexpectedly constrained kind: %#v", store.lastListFilter)
	}
	if !equalStringSlices(store.lastListFilter.Statuses, []string{"pending", "failed"}) {
		t.Fatalf("activity statuses = %#v", store.lastListFilter.Statuses)
	}
}

// TestLinkReadServiceListRejectsIllegalStatus is the end-to-end 400 path
// through the service entry point (not just the helper).
func TestLinkReadServiceListRejectsIllegalStatus(t *testing.T) {
	t.Parallel()

	service := NewLinkReadService(LinkReadServiceOptions{Links: &readFakeLinkStore{}})
	_, err := service.List(context.Background(), dto.ListLinksRequest{Status: "pending,bogus"})
	if err == nil {
		t.Fatal("List() error = nil, want 400 for illegal status")
	}
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T, want *problem.Error", err)
	}
	if problemHTTPStatus(statusErr) != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", problemHTTPStatus(statusErr))
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLinkReadServiceListAcceptsValidFilters is a happy-path control so
// the rejection cases above cannot regress into "always 422".
func TestLinkReadServiceListAcceptsValidFilters(t *testing.T) {
	t.Parallel()

	store := &readFakeLinkStore{listDoneTotal: 0}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	_, err := service.List(context.Background(), dto.ListLinksRequest{
		Tags:        "go,ai",
		ContentType: "article",
		Domain:      "example.com",
		Page:        1,
		Limit:       20,
	})
	if err != nil {
		t.Fatalf("List() error = %v, want nil", err)
	}
	if store.lastListFilter == nil {
		t.Fatal("ListDone() was not called")
	}
	if len(store.lastListFilter.Tags) != 2 {
		t.Fatalf("tag count = %d, want 2", len(store.lastListFilter.Tags))
	}
}

// TestLinkReadServiceListCursorModeRoundTrips confirms the cursor token
// emitted on a full page can be decoded back into a filter.After and
// that NextCursor is omitted on the last page (partial response).
func TestLinkReadServiceListCursorModeRoundTrips(t *testing.T) {
	t.Parallel()

	id1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	id2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	t1 := time.Unix(1700000000, 0).UTC()
	t2 := t1.Add(-time.Hour)

	store := &readFakeLinkStore{
		listDoneResult: []model.Link{
			{ID: id1, URL: "https://example.com/1", Tags: []string{"go"}, Status: model.LinkStatusDone, CreatedAt: t1, UpdatedAt: t1},
			{ID: id2, URL: "https://example.com/2", Tags: []string{"go"}, Status: model.LinkStatusDone, CreatedAt: t2, UpdatedAt: t2},
		},
	}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := service.List(context.Background(), dto.ListLinksRequest{Limit: 2, Cursor: true})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.NextCursor == nil {
		t.Fatal("first page did not produce NextCursor on full result")
	}
	if resp.Total != 0 || resp.Page != 0 {
		t.Fatalf("cursor-mode Total/Page = (%d/%d), want (0/0)", resp.Total, resp.Page)
	}

	// Second call: feed the cursor back. The fake returns a partial
	// page so NextCursor must be omitted.
	store.listDoneResult = []model.Link{
		{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), URL: "https://example.com/3", Tags: []string{"go"}, Status: model.LinkStatusDone, CreatedAt: t2.Add(-time.Hour), UpdatedAt: t2.Add(-time.Hour)},
	}
	resp2, err := service.List(context.Background(), dto.ListLinksRequest{Limit: 2, Cursor: true, After: *resp.NextCursor})
	if err != nil {
		t.Fatalf("List() second page error = %v", err)
	}
	if resp2.NextCursor != nil {
		t.Fatalf("partial page emitted NextCursor = %v", *resp2.NextCursor)
	}
	if store.lastListFilter == nil || store.lastListFilter.After == nil {
		t.Fatal("cursor mode did not propagate filter.After")
	}
	if !store.lastListFilter.After.CreatedAt.Equal(t2) || store.lastListFilter.After.ID != id2 {
		t.Fatalf("decoded cursor = (%v, %v), want (%v, %v)", store.lastListFilter.After.CreatedAt, store.lastListFilter.After.ID, t2, id2)
	}
	if store.lastListFilter.Offset != 0 {
		t.Fatalf("cursor mode used Offset = %d, want 0", store.lastListFilter.Offset)
	}
}

func TestLinkReadServiceListCursorModeSkipsCountAndUsesDefaultFirstPage(t *testing.T) {
	t.Parallel()

	store := &readFakeLinkStore{
		listDoneResult: []model.Link{
			{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), URL: "https://example.com/1", Status: model.LinkStatusDone, CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000000, 0).UTC()},
		},
	}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := service.List(context.Background(), dto.ListLinksRequest{Limit: 10, Cursor: true})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.Total != 0 || resp.Page != 0 {
		t.Fatalf("cursor-mode Total/Page = (%d/%d), want (0/0)", resp.Total, resp.Page)
	}
	if store.lastListFilter == nil || !store.lastListFilter.Cursor {
		t.Fatal("cursor mode was not propagated to repository")
	}
	if store.lastListFilter.After != nil {
		t.Fatalf("cursor mode first page should leave After nil, got %#v", store.lastListFilter.After)
	}
}

func TestLinkReadServiceListCursorModeFirstPageStillAppliesDefaultLimit(t *testing.T) {
	t.Parallel()

	store := &readFakeLinkStore{
		listDoneResult: []model.Link{},
	}
	service := NewLinkReadService(LinkReadServiceOptions{Links: store})

	resp, err := service.List(context.Background(), dto.ListLinksRequest{Cursor: true, After: "", Limit: 0})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if resp.Limit != 20 {
		t.Fatalf("cursor-mode default limit = %d, want 20", resp.Limit)
	}
	if store.lastListFilter == nil || store.lastListFilter.Limit != 20 {
		t.Fatalf("repository limit = %#v, want 20", store.lastListFilter)
	}
}

func TestLinkReadServiceListRejectsCursorAndPageCombination(t *testing.T) {
	t.Parallel()

	service := NewLinkReadService(LinkReadServiceOptions{Links: &readFakeLinkStore{}})
	_, err := service.List(context.Background(), dto.ListLinksRequest{
		Page:   3,
		Limit:  20,
		Cursor: true,
		After:  service.encodeListCursor(time.Now(), uuid.New()),
	})
	if err == nil {
		t.Fatal("List() error = nil, want 422 for page+after combination")
	}
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) || problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want 422", err)
	}
}

func TestLinkReadServiceListRejectsMalformedCursor(t *testing.T) {
	t.Parallel()

	service := NewLinkReadService(LinkReadServiceOptions{Links: &readFakeLinkStore{}})
	_, err := service.List(context.Background(), dto.ListLinksRequest{
		Limit:  20,
		Cursor: true,
		After:  "not-a-real-cursor",
	})
	if err == nil {
		t.Fatal("List() error = nil, want 422 for malformed cursor")
	}
	var statusErr *problem.Error
	if !errors.As(err, &statusErr) || problemHTTPStatus(statusErr) != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want 422", err)
	}
}

// TestLinkReadServiceCursorSignedRoundTrip 覆盖 Wave 9 MED M5：开启
// CursorSigningKey 后，encodeListCursor 产生的 token 必须能被自身解码
// 还原成同一对 (CreatedAt, ID)，且不带签名段（旧明文格式）或被篡改的
// token 都应该被 decodeListCursor 拒掉。
func TestLinkReadServiceCursorSignedRoundTrip(t *testing.T) {
	t.Parallel()

	service := NewLinkReadService(LinkReadServiceOptions{
		Links:            &readFakeLinkStore{},
		CursorSigningKey: "wave9-cursor-test-key",
	})

	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	when := time.Unix(1_700_000_000, 0).UTC()

	token := service.encodeListCursor(when, id)
	cursor, err := service.decodeListCursor(token)
	if err != nil {
		t.Fatalf("signed cursor failed to decode: %v", err)
	}
	if !cursor.CreatedAt.Equal(when) || cursor.ID != id {
		t.Fatalf("decoded cursor = (%v, %v), want (%v, %v)", cursor.CreatedAt, cursor.ID, when, id)
	}

	// 把签名段干掉重新 base64 → 解码应当因为缺少签名失败。
	rawNoSig := strconv.FormatInt(when.UTC().UnixNano(), 10) + ":" + id.String()
	tamperedNoSig := base64.RawURLEncoding.EncodeToString([]byte(rawNoSig))
	if _, err := service.decodeListCursor(tamperedNoSig); err == nil {
		t.Fatal("decode of unsigned token succeeded on signed service; want error")
	}

	// 篡改最后一个十六进制字符（同长度），应当因为签名不匹配失败。
	tampered := token[:len(token)-1] + invertHexChar(token[len(token)-1])
	if _, err := service.decodeListCursor(tampered); err == nil {
		t.Fatal("decode of tampered signature succeeded; want error")
	}

	// 关键键不同时不能解码同一个 token。
	other := NewLinkReadService(LinkReadServiceOptions{
		Links:            &readFakeLinkStore{},
		CursorSigningKey: "different-key",
	})
	if _, err := other.decodeListCursor(token); err == nil {
		t.Fatal("cross-key decode succeeded; want signature mismatch")
	}
}

// invertHexChar 把一个 lowercase hex 字符替换成另一个 lowercase hex 字符，
// 用来构造长度相同但内容不同的篡改 token。
func invertHexChar(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}
