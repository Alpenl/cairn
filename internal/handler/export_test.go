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

	"webtag/internal/httperr"
	"webtag/internal/representation"
	"webtag/internal/service"
)

// TestGetExportStreamsArrayAndSetsHeaders verifies the export handler writes
// the service's streamed JSON array verbatim and sets the attachment +
// content-type headers with a dated filename.
func TestGetExportStreamsArrayAndSetsHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := `[{"id":"a","url":"https://x/1"},{"id":"b","url":"https://x/2"}]`
	svc := &fakeLinkService{exportPayload: payload}
	deps := linkFakeDeps(svc)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if svc.exportCalls != 1 {
		t.Fatalf("Export calls = %d, want 1", svc.exportCalls)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q, want streamed payload %q", rec.Body.String(), payload)
	}

	// Body must be a valid JSON array of objects.
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("export body is not a valid JSON array: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("decoded %d items, want 2", len(items))
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=") || !strings.Contains(cd, "webtag-export-") || !strings.HasSuffix(cd, `.json"`) {
		t.Fatalf("Content-Disposition = %q, want attachment with dated webtag-export-*.json filename", cd)
	}
}

// TestGetExportV1Alias confirms the /api/v1 alias also serves export (the
// route is registered under every apiRoutePrefixes prefix).
func TestGetExportV1Alias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{exportPayload: "[]"}
	deps := linkFakeDeps(svc)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/export", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("v1 export status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "[]" {
		t.Fatalf("v1 export body = %q, want []", rec.Body.String())
	}
}

func TestGetExportV2StreamsVersionedArchive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{exportPayload: "[]"}
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
		{path: "/api/v1/export/v2?sections=base", selection: service.ArchiveV2Selection{}},
		{path: "/api/export/v2?sections=base,thoughts", selection: service.ArchiveV2Selection{IncludeThoughts: true}},
		{path: "/api/export/v2?sections=base,notes", selection: service.ArchiveV2Selection{IncludeNotes: true}},
		{path: "/api/v1/export/v2?sections=base,thoughts,notes", selection: service.FullArchiveV2Selection()},
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

func TestGetExportV2PreflightsUnavailablePrivateReaderSections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	archive := service.NewArchiveV2Service(
		unavailableReaderArchiveLinks{},
		unavailableReaderArchiveSections{},
		unavailableReaderArchiveRules{},
	)
	deps := linkFakeDeps(&fakeLinkService{exportPayload: "[]"})
	deps.ArchiveV2 = archive
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	baseOnly := httptest.NewRecorder()
	router.ServeHTTP(baseOnly, archiveRequest("/api/export/v2?sections=base"))
	if baseOnly.Code != http.StatusOK {
		t.Fatalf("base-only status = %d, want 200; body=%s", baseOnly.Code, baseOnly.Body.String())
	}
	if baseOnly.Header().Get("Content-Disposition") == "" {
		t.Fatal("base-only archive did not install attachment header")
	}
	if !strings.Contains(baseOnly.Body.String(), `"manifest":`) {
		t.Fatalf("base-only archive did not stream a manifest: %s", baseOnly.Body.String())
	}

	for _, path := range []string{
		"/api/export/v2",
		"/api/export/v2?sections=base,thoughts",
		"/api/export/v2?sections=base,notes",
		"/api/export/v2?sections=base,thoughts,notes",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, archiveRequest(path))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503; body=%s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Disposition"); got != "" {
			t.Fatalf("%s Content-Disposition = %q, want no attachment header", path, got)
		}
		var body struct {
			Error struct {
				ErrorCode string `json:"error_code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s invalid error JSON: %v", path, err)
		}
		if body.Error.ErrorCode != httperr.CodeArchiveReaderUnavailable {
			t.Fatalf("%s error code = %q, want %q", path, body.Error.ErrorCode, httperr.CodeArchiveReaderUnavailable)
		}
	}
}

func TestGetExportV2RejectsEveryNonCanonicalSelectorBeforeStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	archive := &archiveV2Fake{payload: `{"must_not":"stream"}`}
	deps := linkFakeDeps(&fakeLinkService{exportPayload: "[]"})
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

func (f *archiveV2Fake) ValidateSelection(service.ArchiveV2Selection) error { return nil }

func (f *archiveV2Fake) ExportSelected(_ context.Context, w io.Writer, options service.ArchiveV2ExportOptions) error {
	f.calls = append(f.calls, options)
	_, err := io.WriteString(w, f.payload)
	return err
}

// These are intentionally real ArchiveV2Service dependencies with no Reader
// exporter. The private paths must fail before any of their stream methods run.
type unavailableReaderArchiveLinks struct{}

func (unavailableReaderArchiveLinks) Export(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "[]")
	return err
}

func (unavailableReaderArchiveLinks) ExportWithCount(_ context.Context, w io.Writer) (int, error) {
	_, err := io.WriteString(w, "[]")
	return 0, err
}

type unavailableReaderArchiveSections struct{}

func (unavailableReaderArchiveSections) StreamArchiveV2Section(_ context.Context, _ string, _ func([]byte) error) error {
	return nil
}

type unavailableReaderArchiveRules struct{}

func (unavailableReaderArchiveRules) StreamArchiveV2Rules(_ context.Context, _ func([]byte) error) error {
	return nil
}
