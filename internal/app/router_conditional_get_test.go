package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/representation"
	"webtag/internal/service"
)

type conditionalGetRevisionProbe struct {
	identityCalls  int
	routeCalls     int
	componentCalls map[string]int
}

func (p *conditionalGetRevisionProbe) Current(_ context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	base := representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	}
	switch components.Key() {
	case "":
		p.identityCalls++
	default:
		p.routeCalls++
		if p.componentCalls == nil {
			p.componentCalls = map[string]int{}
		}
		p.componentCalls[components.Key()]++
		for _, name := range components.Names() {
			base.Components = append(base.Components, representation.Component{Name: name, Revision: 1})
		}
	}
	return base, nil
}

type conditionalGetLinkReadProbe struct {
	smokeLinkReadService
	listCalls int
	getCalls  int
	getErr    error
}

func (p *conditionalGetLinkReadProbe) List(_ context.Context, request dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	p.listCalls++
	return dto.PaginatedLinksResponse{Total: len(request.Domain)}, nil
}

func (p *conditionalGetLinkReadProbe) GetWithContent(context.Context, string, bool) (dto.LinkResponse, error) {
	p.getCalls++
	return dto.LinkResponse{}, p.getErr
}

type conditionalGetTagStoreProbe struct {
	listCalls   int
	scopedCalls int
	lastScope   string
}

func (*conditionalGetTagStoreProbe) ListDistinct(context.Context) ([]string, error) {
	return nil, nil
}

func (p *conditionalGetTagStoreProbe) ListCounts(context.Context) ([]repository.TagCount, error) {
	p.listCalls++
	return nil, nil
}

func (p *conditionalGetTagStoreProbe) ListScopedCounts(_ context.Context, scope string) ([]repository.ScopedTagCount, error) {
	p.scopedCalls++
	p.lastScope = scope
	return nil, nil
}

type conditionalGetTreeProbe struct {
	smokeTreeService
	calls       int
	scopedCalls int
	lastScope   string
}

func (p *conditionalGetTreeProbe) Get(_ context.Context, domain string) (dto.TreeResponse, error) {
	p.calls++
	return dto.TreeResponse{Total: len(domain)}, nil
}

func (p *conditionalGetTreeProbe) ListDomainsScoped(_ context.Context, scope string) (dto.DomainTreeSummaryEnvelope, error) {
	normalized, ok := model.NormalizeOptionalLibraryKind(scope)
	if !ok {
		return dto.DomainTreeSummaryEnvelope{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "library_kind must be reading or site")
	}
	p.scopedCalls++
	p.lastScope = string(normalized)
	return dto.DomainTreeSummaryEnvelope{Total: len(normalized)}, nil
}

type conditionalGetFeedProbe struct {
	subscriptionCalls int
	itemCalls         int
}

func (p *conditionalGetFeedProbe) ListSubscriptions(context.Context, string) (model.FeedSubscriptionsResponse, error) {
	p.subscriptionCalls++
	return model.FeedSubscriptionsResponse{}, nil
}

func (p *conditionalGetFeedProbe) ListItems(context.Context, service.FeedItemFilter) (model.PaginatedFeedItems, error) {
	p.itemCalls++
	return model.PaginatedFeedItems{}, nil
}

func (*conditionalGetFeedProbe) Discover(context.Context, string) (model.FeedDiscoveryResponse, error) {
	return model.FeedDiscoveryResponse{}, nil
}

func (*conditionalGetFeedProbe) Subscribe(context.Context, string, *uuid.UUID, bool) (model.FeedSubscription, error) {
	return model.FeedSubscription{}, nil
}

func (*conditionalGetFeedProbe) UpdateSubscription(context.Context, string, service.FeedSubscriptionUpdateCommand) (model.FeedSubscription, error) {
	return model.FeedSubscription{}, nil
}

func (*conditionalGetFeedProbe) Unsubscribe(context.Context, string) error { return nil }

func (*conditionalGetFeedProbe) ScheduleRefresh(context.Context, string) error { return nil }

func (*conditionalGetFeedProbe) ScheduleAllRefreshes(context.Context) (int64, error) {
	return 0, nil
}

func (*conditionalGetFeedProbe) GetItem(context.Context, string) (model.FeedItem, error) {
	return model.FeedItem{}, nil
}

func (*conditionalGetFeedProbe) UpdateItemState(context.Context, string, service.FeedItemStateCommand) (model.FeedItem, error) {
	return model.FeedItem{}, nil
}

func (*conditionalGetFeedProbe) MarkItemsRead(context.Context, service.FeedItemFilter) (int64, error) {
	return 0, nil
}

func (*conditionalGetFeedProbe) AnalyzeItem(context.Context, string) (model.FeedItem, dto.SubmitResponse, error) {
	return model.FeedItem{}, dto.SubmitResponse{}, nil
}

func (*conditionalGetFeedProbe) CreateFolder(context.Context, string) (model.FeedFolder, error) {
	return model.FeedFolder{}, nil
}

func (*conditionalGetFeedProbe) UpdateFolder(context.Context, string, string) (model.FeedFolder, error) {
	return model.FeedFolder{}, nil
}

func (*conditionalGetFeedProbe) DeleteFolder(context.Context, string) error { return nil }

func (*conditionalGetFeedProbe) ExportOPML(context.Context) ([]byte, error) { return nil, nil }

func (*conditionalGetFeedProbe) ImportOPML(context.Context, []byte) (model.OPMLImportResponse, error) {
	return model.OPMLImportResponse{}, nil
}

type conditionalGetSiteProbe struct {
	listCalls int
}

func (p *conditionalGetSiteProbe) List(context.Context, string, string, string, int, int) (dto.PaginatedSitesResponse, error) {
	p.listCalls++
	return dto.PaginatedSitesResponse{}, nil
}

func (*conditionalGetSiteProbe) Get(context.Context, string) (dto.SiteDetailResponse, error) {
	return dto.SiteDetailResponse{}, nil
}

type conditionalGetRouterProbes struct {
	revisions *conditionalGetRevisionProbe
	links     *conditionalGetLinkReadProbe
	tags      *conditionalGetTagStoreProbe
	tree      *conditionalGetTreeProbe
	feeds     *conditionalGetFeedProbe
	sites     *conditionalGetSiteProbe
}

func newConditionalGetProductionRouter(getErr error) (*conditionalGetRouterProbes, http.Handler) {
	probes := &conditionalGetRouterProbes{
		revisions: &conditionalGetRevisionProbe{},
		links:     &conditionalGetLinkReadProbe{getErr: getErr},
		tags:      &conditionalGetTagStoreProbe{},
		tree:      &conditionalGetTreeProbe{},
		feeds:     &conditionalGetFeedProbe{},
		sites:     &conditionalGetSiteProbe{},
	}
	deps := smokeDeps()
	deps.LinksRead = probes.links
	deps.Tags = service.NewTagReadService(probes.tags, nil)
	deps.Tree = probes.tree
	deps.Feeds = probes.feeds
	deps.Sites = probes.sites
	return probes, NewRouterWithDependencies(deps, nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "dev",
		ExtensionAPIToken:       "conditional-get-production-fixture",
		ConditionalGetRevisions: probes.revisions,
	})
}

func conditionalRequest(router http.Handler, path string, validator string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer conditional-get-production-fixture")
	req.Header.Set("If-None-Match", validator)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestConditionalGetProductionPolicyRunsHandlersForStaleValidators(t *testing.T) {
	probes, router := newConditionalGetProductionRouter(nil)
	paths := []struct {
		path      string
		cacheable bool
	}{
		{path: "/api/links", cacheable: true},
		{path: "/api/links/known", cacheable: true},
		{path: "/api/tags", cacheable: true},
		{path: "/api/tree", cacheable: true},
		{path: "/api/subscriptions"},
		{path: "/api/feed-items", cacheable: true},
		{path: "/api/sites", cacheable: true},
	}

	for _, tc := range paths {
		rec := conditionalRequest(router, tc.path, `"stale"`)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.path, rec.Code)
		}
		if tag := rec.Header().Get("ETag"); (tag != "") != tc.cacheable {
			t.Errorf("%s: ETag = %q, cacheable = %v", tc.path, tag, tc.cacheable)
		}
	}

	for name, got := range map[string]int{
		"links list":    probes.links.listCalls,
		"link detail":   probes.links.getCalls,
		"tags":          probes.tags.listCalls,
		"tree":          probes.tree.calls,
		"subscriptions": probes.feeds.subscriptionCalls,
		"feed items":    probes.feeds.itemCalls,
		"sites":         probes.sites.listCalls,
	} {
		if got != 1 {
			t.Errorf("%s handler calls = %d, want 1", name, got)
		}
	}
	if probes.revisions.identityCalls != len(paths) {
		t.Fatalf("identity revision reads = %d, want %d", probes.revisions.identityCalls, len(paths))
	}
	if probes.revisions.routeCalls != len(paths)-1 {
		t.Fatalf("route revision reads = %d, want %d", probes.revisions.routeCalls, len(paths)-1)
	}
}

func TestConditionalGetProductionPolicyPreservesHandlerSemantics(t *testing.T) {
	linkNotFound := httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	probes, router := newConditionalGetProductionRouter(linkNotFound)

	cases := []struct {
		path    string
		want    int
		wantTag bool
	}{
		{path: "/api/links/missing", want: http.StatusNotFound},
		{path: "/api/tags?library_kind=invalid", want: http.StatusUnprocessableEntity},
		{path: "/api/tree?view=domains&library_kind=all", want: http.StatusUnprocessableEntity},
		{path: "/api/tags?library_kind=%20SITE%20", want: http.StatusNotModified, wantTag: true},
		{path: "/api/links?q=semantic", want: http.StatusOK},
	}
	for _, tc := range cases {
		rec := conditionalRequest(router, tc.path, "*")
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.path, rec.Code, tc.want)
		}
		if tag := rec.Header().Get("ETag"); (tag != "") != tc.wantTag {
			t.Errorf("%s: ETag = %q, wantTag = %v", tc.path, tag, tc.wantTag)
		}
	}
	if probes.links.getCalls != 1 {
		t.Fatalf("link detail calls = %d, want 1", probes.links.getCalls)
	}
	if probes.links.listCalls != 1 {
		t.Fatalf("link list calls = %d, want 1", probes.links.listCalls)
	}
	if probes.tags.scopedCalls != 1 || probes.tags.lastScope != "site" {
		t.Fatalf("normalized scoped tag calls = %d scope = %q, want 1/site", probes.tags.scopedCalls, probes.tags.lastScope)
	}
	if probes.tree.calls != 0 || probes.tree.scopedCalls != 0 {
		t.Fatalf("invalid tree scope called aggregate service tree=%d scoped=%d", probes.tree.calls, probes.tree.scopedCalls)
	}
	if probes.revisions.identityCalls != len(cases) {
		t.Fatalf("identity revision reads = %d, want %d", probes.revisions.identityCalls, len(cases))
	}
	if probes.revisions.routeCalls != 2 {
		t.Fatalf("route revision reads = %d, want 2", probes.revisions.routeCalls)
	}
}
