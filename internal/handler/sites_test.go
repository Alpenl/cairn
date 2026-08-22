package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

type siteHandlerFake struct {
	list    dto.PaginatedSitesResponse
	get     dto.SiteDetailResponse
	err     error
	view    string
	tags    string
	cutoff  string
	page    int
	limit   int
	id      string
	update  dto.SiteUpdateRequest
	entry   dto.SiteEntryUpdateRequest
	ifMatch string
	entryID string
	count   string
}

func (f *siteHandlerFake) List(_ context.Context, view, tags, cutoff string, page, limit int) (dto.PaginatedSitesResponse, error) {
	f.view, f.tags, f.cutoff, f.page, f.limit = view, tags, cutoff, page, limit
	return f.list, f.err
}
func (f *siteHandlerFake) Get(_ context.Context, id string) (dto.SiteDetailResponse, error) {
	f.id = id
	return f.get, f.err
}
func (f *siteHandlerFake) Update(_ context.Context, id, ifMatch string, update dto.SiteUpdateRequest) (dto.SiteDetailResponse, error) {
	f.id, f.ifMatch, f.update = id, ifMatch, update
	return f.get, f.err
}
func (f *siteHandlerFake) UpdateEntry(_ context.Context, siteID, entryID, ifMatch string, update dto.SiteEntryUpdateRequest) (dto.SiteDetailResponse, error) {
	f.id, f.entryID, f.ifMatch, f.entry = siteID, entryID, ifMatch, update
	return f.get, f.err
}
func (f *siteHandlerFake) SetPrimaryEntry(_ context.Context, siteID, entryID, ifMatch string) (dto.SiteDetailResponse, error) {
	f.id, f.entryID, f.ifMatch = siteID, entryID, ifMatch
	return f.get, f.err
}
func (f *siteHandlerFake) DeleteEntry(_ context.Context, siteID, entryID, ifMatch string) (dto.SiteEntryDeleteResponse, error) {
	f.id, f.entryID, f.ifMatch = siteID, entryID, ifMatch
	return dto.SiteEntryDeleteResponse{}, f.err
}
func (f *siteHandlerFake) Delete(_ context.Context, siteID, ifMatch, count string) error {
	f.id, f.ifMatch, f.count = siteID, ifMatch, count
	return f.err
}

func TestSiteRoutesSupportQueryParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &siteHandlerFake{list: dto.PaginatedSitesResponse{Page: 2, Limit: 20}}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{Sites: svc}))
	path := "/api/sites?view=recent&tags=go,tools&recent_cutoff=2026-07-01T00%3A00%3A00Z&page=2&limit=20"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s status = %d body=%s", path, recorder.Code, recorder.Body.String())
	}
	if svc.view != "recent" || svc.tags != "go,tools" || svc.cutoff != "2026-07-01T00:00:00Z" || svc.page != 2 || svc.limit != 20 {
		t.Fatalf("query mapping = %#v", svc)
	}
}

func TestSiteEntryManagementRoutesForwardRevisionAndIdentifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &siteHandlerFake{}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{Sites: svc, SiteManagement: svc}))
	siteID := "11111111-1111-1111-1111-111111111111"
	entryID := "22222222-2222-2222-2222-222222222222"
	requests := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPatch, "/api/sites/" + siteID + "/entries/" + entryID, `{"name":"Docs"}`, http.StatusOK},
		{http.MethodPost, "/api/sites/" + siteID + "/entries/" + entryID + "/set-primary", "", http.StatusOK},
		{http.MethodDelete, "/api/sites/" + siteID + "/entries/" + entryID, "", http.StatusOK},
		{http.MethodDelete, "/api/sites/" + siteID + "?confirm_entry_count=2", "", http.StatusNoContent},
	}
	for _, request := range requests {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
		req.Header.Set("If-Match", `"7"`)
		if request.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		router.ServeHTTP(recorder, req)
		if recorder.Code != request.want {
			t.Fatalf("%s %s status=%d body=%s", request.method, request.path, recorder.Code, recorder.Body.String())
		}
	}
	if svc.id != siteID || svc.ifMatch != `"7"` || svc.count != "2" {
		t.Fatalf("management route mapping = %#v", svc)
	}
}

func TestSiteRoutesReturnUnavailableWhenServiceIsNotWired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sites", nil))
	if recorder.Code != http.StatusServiceUnavailable || !contains(recorder.Body.String(), "site_library_unavailable") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSiteRoutesPropagateServiceErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{Sites: &siteHandlerFake{err: httperr.NewWithCode(http.StatusNotFound, httperr.CodeSiteNotFound, "site not found")}}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/sites/11111111-1111-1111-1111-111111111111", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSiteUpdateRouteForwardsIfMatchAndBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &siteHandlerFake{}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{Sites: svc, SiteManagement: svc}))
	id := "11111111-1111-1111-1111-111111111111"
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/sites/"+id, strings.NewReader(`{"name":"Example","pinned":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"3"`)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if svc.id != id || svc.ifMatch != `"3"` || svc.update.Name == nil || *svc.update.Name != "Example" || svc.update.Pinned == nil || !*svc.update.Pinned {
		t.Fatalf("update mapping = %#v", svc)
	}
}

func contains(text, part string) bool {
	return len(part) == 0 || (len(text) >= len(part) && index(text, part) >= 0)
}
func index(text, part string) int {
	for i := 0; i+len(part) <= len(text); i++ {
		if text[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}
