package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/representation"
	"webtag/internal/service"
)

func TestLegacyGetExportIsNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(linkFakeDeps(&fakeLinkService{})))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("legacy export status = %d, want 404", rec.Code)
	}
}

func TestGetExportV2StreamsVersionedArchive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{}
	archive := &archiveV2Fake{payload: `{"schema_version":2,"links":[],"sites":[]}`}
	router := gin.New()
	deps := linkFakeDeps(svc)
	deps.ArchiveV2 = archive
	RegisterRoutes(router, withStubDeps(deps))
	for _, test := range []struct {
		path      string
		selection service.ArchiveV2Selection
	}{
		{path: "/api/export/v2", selection: service.FullArchiveV2Selection()},
		{path: "/api/export/v2?sections=base", selection: service.ArchiveV2Selection{}},
		{path: "/api/export/v2?sections=base,thoughts", selection: service.ArchiveV2Selection{IncludeThoughts: true}},
		{path: "/api/export/v2?sections=base,notes", selection: service.ArchiveV2Selection{IncludeNotes: true}},
		{path: "/api/export/v2?sections=base,thoughts,notes", selection: service.FullArchiveV2Selection()},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, archiveRequest(test.path))
		if rec.Code != http.StatusOK || rec.Body.String() != archive.payload {
			t.Fatalf("%s = %d %q", test.path, rec.Code, rec.Body.String())
		}
		if got := archive.calls[len(archive.calls)-1].Selection; got != test.selection {
			t.Fatalf("%s selection = %#v, want %#v", test.path, got, test.selection)
		}
		if got := archive.calls[len(archive.calls)-1].ClientDataNamespace; got != archiveTestNamespace {
			t.Fatalf("%s namespace = %q, want %q", test.path, got, archiveTestNamespace)
		}
	}
}

func TestGetExportV2RejectsEveryNonCanonicalSelectorBeforeStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	archive := &archiveV2Fake{payload: `{"must_not":"stream"}`}
	deps := linkFakeDeps(&fakeLinkService{})
	deps.ArchiveV2 = archive
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	for _, path := range []string{
		"/api/export/v2?sections=",
		"/api/export/v2?sections=thoughts",
		"/api/export/v2?sections=base,notes,thoughts",
		"/api/export/v2?sections=base,thoughts,thoughts",
		"/api/export/v2?sections=base,%20thoughts",
		"/api/export/v2?sections=BASE",
		"/api/export/v2?sections=base&sections=base,notes",
		"/api/export/v2?sections=base;sections=base,notes",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, archiveRequest(path))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d, want 422; body=%s", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Error struct {
				ErrorCode string `json:"error_code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s invalid error JSON: %v", path, err)
		}
		if body.Error.ErrorCode != "invalid_archive_sections" {
			t.Fatalf("%s error code = %q", path, body.Error.ErrorCode)
		}
		if strings.Contains(rec.Body.String(), "must_not") {
			t.Fatalf("%s started the archive stream", path)
		}
	}
	if len(archive.calls) != 0 {
		t.Fatalf("invalid selector called exporter %d times", len(archive.calls))
	}
}

const archiveTestNamespace = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func archiveRequest(path string) *http.Request {
	identity := representation.ClientIdentity{
		ClientDataNamespace:     archiveTestNamespace,
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	}
	ctx := representation.WithClientIdentity(context.Background(), identity)
	return httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
}

type archiveV2Fake struct {
	payload string
	calls   []service.ArchiveV2ExportOptions
}

func (f *archiveV2Fake) Export(_ context.Context, w io.Writer, options service.ArchiveV2ExportOptions) error {
	f.calls = append(f.calls, options)
	_, err := io.WriteString(w, f.payload)
	return err
}
