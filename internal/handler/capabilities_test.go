package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func TestCapabilitiesExposeAdditiveCollectionSupport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/capabilities", capabilities(WebsiteFeatures{LibraryKindAPI: true, SiteLibraryWrite: true, SiteAutoClassification: true, SiteAdvancedManagement: true}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	for _, key := range []string{"library_kinds", "site_library", "site_auto_classification", "site_management", "site_advanced_management", "archive_versions"} {
		if !strings.Contains(recorder.Body.String(), `"`+key+`"`) {
			t.Fatalf("response missing %s: %s", key, recorder.Body.String())
		}
	}
}

func TestCapabilitiesReflectDisabledWebsiteFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/capabilities", capabilities(WebsiteFeatures{}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	for _, fragment := range []string{`"library_kinds":false`, `"site_library":false`, `"site_auto_classification":false`, `"site_management":false`, `"site_advanced_management":false`, `"archive_versions":[1]`} {
		if !strings.Contains(recorder.Body.String(), fragment) {
			t.Fatalf("response missing %s: %s", fragment, recorder.Body.String())
		}
	}
}

func TestCapabilitiesSeparateSiteWriteFromAdvancedManagement(t *testing.T) {
	response := requestCapabilities(t, WebsiteFeatures{
		LibraryKindAPI:   true,
		SiteLibraryWrite: true,
	})

	if !response.SiteLibrary || !response.SiteManagement {
		t.Fatalf("site read/write capabilities = %+v, want enabled", response)
	}
	if response.SiteAdvancedManagement {
		t.Fatalf("SiteAdvancedManagement = true without the advanced feature")
	}
}

func TestCapabilitiesReaderTotalOffOverridesSnapshot(t *testing.T) {
	response := requestCapabilities(t, WebsiteFeatures{
		ReaderCapabilities: &dto.ReaderCapabilitiesResponse{
			Annotations: true, Notes: true, Inbox: true, Todos: true,
			Engagement: true, Home: true, Feed: true, AI: true,
			Semantic: true, Activity: true, History: true,
		},
	})

	if response.ReaderVNext {
		t.Fatal("ReaderVNext = true, want false")
	}
	assertReaderCapabilities(t, response.Reader, false)
}

func TestCapabilitiesPreserveDisabledIndividualReaderCapabilities(t *testing.T) {
	response := requestCapabilities(t, WebsiteFeatures{
		ReaderVNext: true,
		ReaderCapabilities: &dto.ReaderCapabilitiesResponse{
			Annotations: true, Notes: true, Inbox: true, Todos: true,
			Engagement: true, Home: true, Feed: true, AI: false,
			Semantic: false, Activity: true, History: true,
		},
	})

	if !response.ReaderVNext {
		t.Fatal("ReaderVNext = false, want true")
	}
	if response.Reader.AI || response.Reader.Semantic {
		t.Fatalf("disabled capabilities were enabled: %+v", response.Reader)
	}
	if !response.Reader.Annotations || !response.Reader.Notes || !response.Reader.Inbox || !response.Reader.Todos || !response.Reader.Engagement || !response.Reader.Home || !response.Reader.Feed || !response.Reader.Activity || !response.Reader.History {
		t.Fatalf("enabled capabilities were lost: %+v", response.Reader)
	}
}

func TestCapabilitiesFailClosedWhenReaderSnapshotIsMissing(t *testing.T) {
	response := requestCapabilities(t, WebsiteFeatures{ReaderVNext: true})

	if !response.ReaderVNext {
		t.Fatal("ReaderVNext = false, want true")
	}
	assertReaderCapabilities(t, response.Reader, false)
}

func requestCapabilities(t *testing.T, features WebsiteFeatures) dto.CapabilitiesResponse {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/capabilities", capabilities(features))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response dto.CapabilitiesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode capabilities: %v; body = %s", err, recorder.Body.String())
	}
	return response
}

func assertReaderCapabilities(t *testing.T, response dto.ReaderCapabilitiesResponse, want bool) {
	t.Helper()
	if response.Annotations != want || response.Notes != want || response.Inbox != want || response.Todos != want || response.Engagement != want || response.Home != want || response.Feed != want || response.AI != want || response.Semantic != want || response.Activity != want || response.History != want {
		t.Fatalf("Reader capabilities = %+v, want all %v", response, want)
	}
}
