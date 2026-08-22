package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func TestCapabilitiesExposeAdditiveCollectionSupport(t *testing.T) {
	response := requestCapabilities(t, false, nil)
	if !response.LibraryKinds || !response.SiteLibrary || !response.SiteManagement || !response.SiteAdvancedManagement {
		t.Fatalf("website capabilities = %+v, want all enabled", response)
	}
	if len(response.ArchiveVersions) != 1 || response.ArchiveVersions[0] != 2 {
		t.Fatalf("ArchiveVersions = %v, want [2]", response.ArchiveVersions)
	}
}

func TestCapabilitiesReaderTotalOffOverridesSnapshot(t *testing.T) {
	response := requestCapabilities(t, false, &dto.ReaderCapabilitiesResponse{
		Annotations: true, Notes: true, Inbox: true, Todos: true,
		Engagement: true, Home: true, Feed: true, AI: true,
		RelatedTags: true, Activity: true, History: true,
	})

	if response.ReaderVNext {
		t.Fatal("ReaderVNext = true, want false")
	}
	assertReaderCapabilities(t, response.Reader, false)
}

func TestCapabilitiesPreserveDisabledIndividualReaderCapabilities(t *testing.T) {
	response := requestCapabilities(t, true, &dto.ReaderCapabilitiesResponse{
		Annotations: true, Notes: true, Inbox: true, Todos: true,
		Engagement: true, Home: true, Feed: true, AI: false,
		RelatedTags: false, Activity: true, History: true,
	})

	if !response.ReaderVNext {
		t.Fatal("ReaderVNext = false, want true")
	}
	if response.Reader.AI || response.Reader.RelatedTags {
		t.Fatalf("disabled capabilities were enabled: %+v", response.Reader)
	}
	if !response.Reader.Annotations || !response.Reader.Notes || !response.Reader.Inbox || !response.Reader.Todos || !response.Reader.Engagement || !response.Reader.Home || !response.Reader.Feed || !response.Reader.Activity || !response.Reader.History {
		t.Fatalf("enabled capabilities were lost: %+v", response.Reader)
	}
}

func TestCapabilitiesFailClosedWhenReaderSnapshotIsMissing(t *testing.T) {
	response := requestCapabilities(t, true, nil)

	if !response.ReaderVNext {
		t.Fatal("ReaderVNext = false, want true")
	}
	assertReaderCapabilities(t, response.Reader, false)
}

func requestCapabilities(t *testing.T, readerVNext bool, readerCapabilities *dto.ReaderCapabilitiesResponse) dto.CapabilitiesResponse {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/capabilities", capabilities(readerVNext, readerCapabilities))
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
	if response.Annotations != want || response.Notes != want || response.Inbox != want || response.Todos != want || response.Engagement != want || response.Home != want || response.Feed != want || response.AI != want || response.RelatedTags != want || response.Activity != want || response.History != want {
		t.Fatalf("Reader capabilities = %+v, want all %v", response, want)
	}
}
