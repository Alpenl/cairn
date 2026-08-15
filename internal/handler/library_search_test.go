package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

type librarySearchHandlerFake struct {
	query        string
	readingLimit int
	siteLimit    int
	thoughtLimit int
	thoughtAfter string
	response     dto.GroupedSearchResponse
	err          error
}

func (f *librarySearchHandlerFake) Search(_ context.Context, query string, readingLimit, siteLimit, thoughtLimit int, thoughtAfter string) (dto.GroupedSearchResponse, error) {
	f.query, f.readingLimit, f.siteLimit, f.thoughtLimit, f.thoughtAfter = query, readingLimit, siteLimit, thoughtLimit, thoughtAfter
	return f.response, f.err
}

func TestGroupedSearchHandlerForwardsParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &librarySearchHandlerFake{response: dto.GroupedSearchResponse{Reading: dto.LibrarySearchGroup{Items: []dto.LinkResponse{}}, Sites: dto.SiteSearchGroup{Items: []dto.SiteSearchResultResponse{}}}}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{LibrarySearch: svc}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=docs&reading_limit=4&site_limit=6&thought_limit=20&thought_after=opaque-page", nil))
	if rec.Code != http.StatusOK || svc.query != "docs" || svc.readingLimit != 4 || svc.siteLimit != 6 || svc.thoughtLimit != 20 || svc.thoughtAfter != "opaque-page" {
		t.Fatalf("status=%d call=%#v", rec.Code, svc)
	}
	var body dto.GroupedSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Reading.Items == nil || body.Sites.Items == nil {
		t.Fatalf("response body = %q, err=%v", rec.Body.String(), err)
	}
}

func TestGroupedSearchHandlerWritesServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &librarySearchHandlerFake{err: httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeQueryTooLong, "search query is too long")}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{LibrarySearch: svc}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=too-long", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}
