package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
)

func TestPostLinksReturnsAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jobID := "job-1"
	deps := linkFakeDeps(&fakeLinkService{
		submitResponse: dto.SubmitResponse{JobID: &jobID, LinkID: "link-1", Status: "pending"},
	})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"url":"https://example.com/a","description":"sample"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	var got dto.SubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if got.JobID == nil || *got.JobID != "job-1" || got.LinkID != "link-1" || got.Status != "pending" {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestPostRefreshReturnsAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	refreshJobID := "job-r"
	deps := linkFakeDeps(&fakeLinkService{
		refreshResponse: dto.SubmitResponse{JobID: &refreshJobID, LinkID: "link-r", Status: "pending"},
	})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links/link-r/refresh", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestPostBatchReturnsAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	batchJobID := "job-1"
	deps := linkFakeDeps(&fakeLinkService{
		batchResponse: dto.BatchSubmitResponse{
			Results: []dto.BatchItemResponse{
				{Result: &dto.SubmitResponse{JobID: &batchJobID, LinkID: "link-1", Status: "pending"}},
			},
		},
	})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"items":[{"url":"https://example.com/a"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestPostBatchAllowsPerItemValidationFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jobID := "job-good"
	deps := linkFakeDeps(&fakeLinkService{
		batchResponse: dto.BatchSubmitResponse{Results: []dto.BatchItemResponse{
			{Error: "invalid url"},
			{Result: &dto.SubmitResponse{JobID: &jobID, LinkID: "link-good", Status: "pending"}},
		}},
	})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"items":[{"url":"not a url"},{"url":"https://example.com/good"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 partial-success envelope; body=%q", rec.Code, rec.Body.String())
	}
	var response dto.BatchSubmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	if len(response.Results) != 2 || response.Results[0].Error == "" || response.Results[1].Result == nil {
		t.Fatalf("batch response = %#v, want one item error followed by one success", response.Results)
	}
}

func TestPostIngestReturnsAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ingestJobID := "job-i"
	svc := &fakeLinkService{
		ingestResponse: dto.SubmitResponse{JobID: &ingestJobID, LinkID: "link-i", Status: "pending"},
	}
	deps := linkFakeDeps(svc)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"sources":[{"kind":"text","text":"captured text"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	if svc.ingestRequest == nil {
		t.Fatal("ingest request was not forwarded to service")
	}
	if len(svc.ingestRequest.Sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(svc.ingestRequest.Sources))
	}
	if svc.ingestRequest.Sources[0].Kind != "text" {
		t.Fatalf("source kind = %q, want text", svc.ingestRequest.Sources[0].Kind)
	}
}

func TestPostIngestForwardsInboxDestination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{
		ingestResponse: dto.SubmitResponse{
			InboxID:     "inbox-1",
			Destination: "inbox",
			Status:      "pending",
		},
	}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(linkFakeDeps(svc)))

	body := []byte(`{"destination":"inbox","sources":[{"kind":"browser_capture","url":"https://example.com/a","text":"captured"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if svc.ingestRequest == nil || svc.ingestRequest.Destination != "inbox" {
		if svc.ingestRequest == nil {
			t.Fatal("ingest request was not forwarded to service")
		}
		t.Fatalf("destination = %q, want inbox", svc.ingestRequest.Destination)
	}
}

func TestPostIngestForwardsSiteDestination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{
		ingestResponse: dto.SubmitResponse{
			LinkID:      "site-link-1",
			Destination: "site",
			Status:      "pending",
		},
	}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(linkFakeDeps(svc)))

	body := []byte(`{"destination":"site","sources":[{"kind":"browser_capture","url":"https://example.com/site","text":"captured"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if svc.ingestRequest == nil || svc.ingestRequest.Destination != "site" {
		t.Fatalf("ingest request = %+v, want site destination", svc.ingestRequest)
	}
}

func TestPostIngestRejectsOversizedRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	// /api/ingest retains a 4 MiB cap for generic multi-source clients. A
	// payload that exceeds it must still be rejected with 413; first-party
	// browser_capture stays well below this with its 512 KiB text budget.
	oversizedText := strings.Repeat("x", (4<<20)+1)
	body := []byte(`{"sources":[{"kind":"text","text":"` + oversizedText + `"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestPostBatchRejectsOversizedItemCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{
		batchErr: httperr.New(http.StatusUnprocessableEntity, "batch items exceed limit"),
	})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := `{"items":[{"url":"https://example.com/a"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links/batch", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

// TestGetLinkContentReadsSavedSnapshotWithoutFetching 钉住「原文按需取」这条
// 契约的读半边：GET /api/links/:id/content 只读已保存快照，服务层不抓取。
func TestGetLinkContentReadsSavedSnapshotWithoutFetching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	content := &fakeContentService{getResponse: dto.LinkContentResponse{
		LinkID: "link-1", Content: "saved body", ContentFormat: "plain", FetcherType: "stored",
	}}
	deps := withStubDeps(Dependencies{LinksContent: content})
	router := gin.New()
	RegisterRoutes(router, deps)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links/link-1/content", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if content.getCalls != 1 || content.calls != 0 {
		t.Fatalf("get=%d save/replace=%d，读原文不该触发任何抓取写入", content.getCalls, content.calls)
	}
	if !strings.Contains(rec.Body.String(), "saved body") {
		t.Fatalf("body = %s，want 含已保存正文", rec.Body.String())
	}
}

func TestPatchLinkContentPassesJSONAndReturnsCanonicalResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	content := &fakeContentService{editResponse: dto.LinkContentResponse{
		LinkID: "link-1", Content: "edited body", ContentFormat: "plain",
		ContentSource: "user", ContentRevision: 8, FetcherType: "stored",
	}}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{LinksContent: content}))

	body := `{"content":"edited body","expected_content_revision":7}`
	for _, path := range []string{"/api/links/link-1/content", "/api/v1/links/link-1/content"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if content.editRequest == nil || content.editRequest.Content != "edited body" || content.editRequest.ExpectedContentRevision != 7 {
				t.Fatalf("edit request = %#v, want decoded body and revision 7", content.editRequest)
			}
			if !strings.Contains(rec.Body.String(), `"content_source":"user"`) || !strings.Contains(rec.Body.String(), `"content_revision":8`) {
				t.Fatalf("response = %s, want source=user and revision=8", rec.Body.String())
			}
		})
	}
}

func TestPatchLinkContentMapsBindingAndServiceErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		body       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid revision", body: `{"content":"body","expected_content_revision":0}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "service error", body: `{"content":"body","expected_content_revision":7}`, serviceErr: httperr.NewWithCode(http.StatusConflict, httperr.CodeContentRevisionConflict, "stale"), wantStatus: http.StatusConflict, wantCode: httperr.CodeContentRevisionConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := &fakeContentService{editErr: tc.serviceErr}
			router := gin.New()
			RegisterRoutes(router, withStubDeps(Dependencies{LinksContent: content}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/api/links/link-1/content", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				var envelope dto.ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if envelope.Error.ErrorCode != tc.wantCode {
					t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, tc.wantCode)
				}
			}
		})
	}
}

func TestPatchLinkContentRejectsOversizedJSONEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	content := &fakeContentService{}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{LinksContent: content}))

	body := `{"content":"` + strings.Repeat("x", (8<<20)+1) + `","expected_content_revision":7}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/links/link-1/content", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var envelope dto.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.ErrorCode != httperr.CodeContentTooLarge {
		t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, httperr.CodeContentTooLarge)
	}
	if content.calls != 0 {
		t.Fatalf("oversized request called service %d times, want 0", content.calls)
	}
}

// TestGetLinkPassesIncludeContentToService 钉住详情端点对 include_content 的
// 解析：默认 true（老客户端行为不变），显式 false 才让详情不带正文。
func TestGetLinkPassesIncludeContentToService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		query string
		want  bool
	}{
		{"", true},
		{"?include_content=true", true},
		{"?include_content=1", true},
		{"?include_content=false", false},
	} {
		t.Run("query="+tt.query, func(t *testing.T) {
			link := &fakeLinkService{}
			router := gin.New()
			RegisterRoutes(router, withStubDeps(Dependencies{LinksRead: link}))

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links/link-1"+tt.query, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if link.lastIncludeContent != tt.want {
				t.Fatalf("includeContent = %v, want %v", link.lastIncludeContent, tt.want)
			}
		})
	}
}

func TestPostLinksRejectsOversizedRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	oversizedDescription := strings.Repeat("x", 1<<20)
	body := []byte(`{"url":"https://example.com/a","description":"` + oversizedDescription + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestPostLinksKeepsMalformedJSONAsUnprocessableEntity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

// TestAPIV1AliasMatchesLegacyPrefix 验证 Wave 9 MED M6 加入的 /api/v1/*
// 别名跟 /api/* 走同一份 handler、返回相同响应。两个前缀分别 POST 一次
// 同一个 LinkCreateRequest，断言 status、body 字段都一致。任意一侧未挂
// 上路由（404）或返回不同结果都会被这条测试捕获，避免后续新增 endpoint
// 时只在一个前缀注册而留下 silent drift。
func TestAPIV1AliasMatchesLegacyPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jobID := "job-1"
	deps := linkFakeDeps(&fakeLinkService{
		submitResponse: dto.SubmitResponse{JobID: &jobID, LinkID: "link-1", Status: "pending"},
	})
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"url":"https://example.com/a"}`)
	doPost := func(path string) (int, dto.SubmitResponse) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		var got dto.SubmitResponse
		if rec.Code == http.StatusAccepted {
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
		}
		return rec.Code, got
	}

	legacyCode, legacyBody := doPost("/api/links")
	v1Code, v1Body := doPost("/api/v1/links")

	if legacyCode != http.StatusAccepted {
		t.Fatalf("/api/links status = %d, want %d", legacyCode, http.StatusAccepted)
	}
	if v1Code != http.StatusAccepted {
		t.Fatalf("/api/v1/links status = %d, want %d", v1Code, http.StatusAccepted)
	}
	if legacyBody.LinkID != v1Body.LinkID || legacyBody.Status != v1Body.Status {
		t.Fatalf("alias response drift: legacy=%+v v1=%+v", legacyBody, v1Body)
	}
}

func TestGetJobReturnsLinkOnlyWhenDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Dependencies{
		Jobs: &fakeJobService{
			response: dto.JobResponse{
				ID:       "job-1",
				LinkID:   "link-1",
				Status:   "done",
				ErrorMsg: nil,
				Link: &dto.LinkResponse{
					ID:        "link-1",
					URL:       "https://example.com",
					Status:    "done",
					Tags:      []string{"tag"},
					CreatedAt: time.Unix(0, 0).UTC(),
					UpdatedAt: time.Unix(0, 0).UTC(),
				},
			},
		},
	}

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/job-1", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestListJobsParsesIDsQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Dependencies{
		Jobs: &fakeJobService{
			listResponse: []dto.JobResponse{{ID: "job-1", LinkID: "link-1", Status: "pending"}},
		},
	}

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs?ids=job-1,job-2,,job-3", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	svc := deps.Jobs.(*fakeJobService)
	if len(svc.listRequestIDs) != 3 {
		t.Fatalf("List() ids len = %d, want 3", len(svc.listRequestIDs))
	}
	if svc.listRequestIDs[0] != "job-1" || svc.listRequestIDs[1] != "job-2" || svc.listRequestIDs[2] != "job-3" {
		t.Fatalf("List() ids = %#v, want [job-1 job-2 job-3]", svc.listRequestIDs)
	}
}

// TestListLinksParsesQueryParams pins the listLinks handler's query
// parsing surface that previously sat at 11.1% coverage. The handler is
// the sole call site for queryInt and the only place tag/domain/
// content_type filters reach LinkReadService.List, so a regression in
// its query → DTO mapping would silently filter the wrong rows.
func TestListLinksParsesQueryParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{
		listResponse: dto.PaginatedLinksResponse{Page: 2, Limit: 50},
	}
	deps := linkFakeDeps(svc)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links?tags=ai,go&domain=example.com&content_type=article&status=pending,failed&created_from=2026-08-10T16%3A00%3A00Z&created_before=2026-08-11T16%3A00%3A00Z&page=2&limit=50", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if svc.listRequest == nil {
		t.Fatal("List was not called")
	}
	if svc.listRequest.Tags != "ai,go" {
		t.Fatalf("Tags = %q, want %q", svc.listRequest.Tags, "ai,go")
	}
	if svc.listRequest.Status != "pending,failed" {
		t.Fatalf("Status = %q, want %q", svc.listRequest.Status, "pending,failed")
	}
	if svc.listRequest.Domain != "example.com" {
		t.Fatalf("Domain = %q, want %q", svc.listRequest.Domain, "example.com")
	}
	if svc.listRequest.ContentType != "article" {
		t.Fatalf("ContentType = %q, want %q", svc.listRequest.ContentType, "article")
	}
	if svc.listRequest.CreatedFrom != "2026-08-10T16:00:00Z" {
		t.Fatalf("CreatedFrom = %q, want RFC3339 lower bound", svc.listRequest.CreatedFrom)
	}
	if svc.listRequest.CreatedBefore != "2026-08-11T16:00:00Z" {
		t.Fatalf("CreatedBefore = %q, want RFC3339 upper bound", svc.listRequest.CreatedBefore)
	}
	if svc.listRequest.Page != 2 {
		t.Fatalf("Page = %d, want 2", svc.listRequest.Page)
	}
	if svc.listRequest.Limit != 50 {
		t.Fatalf("Limit = %d, want 50", svc.listRequest.Limit)
	}
}

// TestListLinksAppliesCursorDefaultsWhenAfterEmpty exercises the
// cursor-mode first-page path the flat-list UI now uses: an explicit
// empty ?after= should opt into cursor mode while still keeping the
// service-layer defaults for page/limit intact.
func TestListLinksAppliesCursorDefaultsWhenAfterEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{}
	deps := linkFakeDeps(svc)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links?after=", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if svc.listRequest == nil {
		t.Fatal("List was not called")
	}
	if !svc.listRequest.Cursor {
		t.Fatal("expected empty query to opt into cursor mode for the flat list")
	}
	if svc.listRequest.Page != 1 || svc.listRequest.Limit != 20 {
		t.Fatalf("defaults: Page=%d Limit=%d, want 1/20", svc.listRequest.Page, svc.listRequest.Limit)
	}
	if svc.listRequest.After != "" {
		t.Fatalf("After = %q, want empty cursor start", svc.listRequest.After)
	}
	if svc.listRequest.Tags != "" || svc.listRequest.Domain != "" || svc.listRequest.ContentType != "" {
		t.Fatalf("expected empty filters, got %+v", svc.listRequest)
	}
}

// TestListLinksFallsBackToDefaultsOnMalformedNumeric makes sure that a
// non-numeric page/limit parameter does not propagate a 400 — queryInt
// silently swaps in the fallback so callers can be sloppy without
// breaking the listing endpoint.
func TestListLinksFallsBackToDefaultsOnMalformedNumeric(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{}
	deps := linkFakeDeps(svc)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links?page=foo&limit=bar", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if svc.listRequest.Page != 1 || svc.listRequest.Limit != 20 {
		t.Fatalf("malformed numerics did not fall back: Page=%d Limit=%d", svc.listRequest.Page, svc.listRequest.Limit)
	}
}

// TestListLinksWriteServiceErrorAsHTTPError verifies the writeError path
// surfaces an httperr.Error as the matching HTTP status instead of
// crashing or returning 200.
func TestListLinksWriteServiceErrorAsHTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{
		listErr: httperr.New(http.StatusBadRequest, "bad filter combination"),
	}
	deps := linkFakeDeps(svc)
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/links?domain=", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bad filter combination") {
		t.Fatalf("body missing service error message: %s", rec.Body.String())
	}
}

// TestRegisterRoutesPanicsOnMissingService pins the M4 boot-time
// fail-fast assertion: a nil required dependency must crash at
// RegisterRoutes (visible to operators in startup logs) rather than at
// request time (visible only to whoever happens to hit the route).
func TestRegisterRoutesPanicsOnMissingService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		deps Dependencies
		want string
	}{
		{"missing all", Dependencies{}, "LinksWrite, LinksRead, Ingest, Jobs, Tags, Tree"},
		{"missing LinksWrite", Dependencies{LinksRead: &fakeLinkService{}, Ingest: &fakeLinkService{}, Jobs: &fakeJobService{}, Tags: &fakeTagService{}, Tree: &fakeTreeService{}}, "LinksWrite"},
		{"missing Tree", func() Dependencies {
			d := linkFakeDeps(&fakeLinkService{})
			d.Jobs = &fakeJobService{}
			d.Tags = &fakeTagService{}
			return d
		}(), "Tree"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				rec := recover()
				if rec == nil {
					t.Fatal("expected panic on missing required service")
				}
				msg, ok := rec.(string)
				if !ok {
					t.Fatalf("panic value is %T, want string", rec)
				}
				if !strings.Contains(msg, tt.want) {
					t.Fatalf("panic message %q missing expected service list %q", msg, tt.want)
				}
			}()
			RegisterRoutes(gin.New(), tt.deps)
		})
	}
}

func TestDeleteLinkReturnsOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/links/link-1", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if rec.Body.Len() != 0 {
		t.Fatalf("expected empty body, got %q", rec.Body.String())
	}
}

// TestDeleteLinkPropagatesServiceError 锁定 deleteLink 的 err →
// writeError 分支。404（删除不存在的 link）和 500（DB 故障）两种
// status 各跑一次，确认 service 的 httperr 透传不被吞。原 happy-path
// 测试只覆盖 60%，加这两个 case 把 deleteLink 拉到 100%。
func TestDeleteLinkPropagatesServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("not_found_propagates_404", func(t *testing.T) {
		svc := &fakeLinkService{deleteErr: httperr.New(http.StatusNotFound, "link not found")}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(linkFakeDeps(svc)))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/links/missing", nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("plain_error_maps_to_500", func(t *testing.T) {
		svc := &fakeLinkService{deleteErr: errors.New("db offline")}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(linkFakeDeps(svc)))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/links/x", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("non-StatusCarrier error should map to 500, got %d", rec.Code)
		}
	})
}

func TestTagsEndpointReturnsBareArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Dependencies{
		Tags: &fakeTagService{
			response: []dto.TagCountResponse{{Tag: "go", Count: 2}},
		},
	}

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got []dto.TagCountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if len(got) != 1 || got[0].Tag != "go" || got[0].Count != 2 {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestTreeEndpointReturnsNodesEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Dependencies{
		Tree: &fakeTreeService{
			response: dto.TreeResponse{
				Nodes: []dto.TreeNodeResponse{{ID: "n1", URL: "https://example.com", Status: "done"}},
				Total: 1,
			},
		},
	}

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tree", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestGetLinkLogsUnexpectedInternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	router := gin.New()
	router.Use(middleware.RequestID(logger))
	RegisterRoutes(router, withStubDeps(linkFakeDeps(&fakeLinkService{
		getErr: errors.New("database offline"),
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/links/link-1", nil)
	req.Header.Set(middleware.RequestIDHeader, "req-handler-500")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuffer.Bytes()), &entry); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}

	if entry["msg"] != "http handler returned internal error" {
		t.Fatalf("msg = %v, want %q", entry["msg"], "http handler returned internal error")
	}
	if entry["request_id"] != "req-handler-500" {
		t.Fatalf("request_id = %v, want %q", entry["request_id"], "req-handler-500")
	}
	if entry["path"] != "/api/links/link-1" {
		t.Fatalf("path = %v, want %q", entry["path"], "/api/links/link-1")
	}
	if entry["method"] != http.MethodGet {
		t.Fatalf("method = %v, want %q", entry["method"], http.MethodGet)
	}
	// Wave 2 H5：writeError 现在用 observability.SafeError 包装 err，
	// JSON 输出里 "error" 是 group，含 msg / category / chain。原始
	// 错误文本出现在 chain 里。category 也应当被分类为 "unknown"
	// （ "database offline" 不命中任何 errsafe 子串规则）。
	errGroup, ok := entry["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %v, want JSON group", entry["error"])
	}
	if chain, ok := errGroup["chain"].(string); !ok || chain != "database offline" {
		t.Fatalf("error.chain = %v, want %q", errGroup["chain"], "database offline")
	}
	if category := errGroup["category"]; category != "unknown" {
		t.Fatalf("error.category = %v, want %q", category, "unknown")
	}
}

func TestGetLinkDoesNotLogKnownHTTPErrorAsInternalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	router := gin.New()
	router.Use(middleware.RequestID(logger))
	RegisterRoutes(router, withStubDeps(linkFakeDeps(&fakeLinkService{
		getErr: httperr.New(http.StatusNotFound, "not found"),
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/links/missing", nil)
	req.Header.Set(middleware.RequestIDHeader, "req-handler-404")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if logBuffer.Len() != 0 {
		t.Fatalf("expected no internal error log for known HTTP error, got %q", logBuffer.String())
	}
}

// TestWriteErrorPropagatesServiceSlug 验证 service 层用 httperr.NewWithCode
// 注入的 slug 透传到 envelope.error.error_code 字段，而不是被 default_<status>
// 兜底覆盖。这条路径是 service → handler → JSON envelope 唯一的 slug 透出口，
// 回归保护的是 ErrorCoder 接口在 writeError 中被正确识别。
func TestWriteErrorPropagatesServiceSlug(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(linkFakeDeps(&fakeLinkService{
		getErr: httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found"),
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/links/missing", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var env dto.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.ErrorCode != httperr.CodeLinkNotFound {
		t.Fatalf("error_code = %q, want %q", env.Error.ErrorCode, httperr.CodeLinkNotFound)
	}
	if env.Error.Message != "link not found" {
		t.Fatalf("message = %q, want %q", env.Error.Message, "link not found")
	}
}

// TestWriteErrorFallsBackToDefaultSlugWhenServiceOmitsCode 验证未带 slug 的
// httperr.New 走到 envelope 时回退到 default_<status>，与 Wave 6 之前的行为
// 等价；防止 ErrorCoder 接口判定误把空 slug 透出。
func TestWriteErrorFallsBackToDefaultSlugWhenServiceOmitsCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(linkFakeDeps(&fakeLinkService{
		getErr: httperr.New(http.StatusNotFound, "legacy not found"),
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/links/missing", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var env dto.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal error envelope: %v", err)
	}
	if env.Error.ErrorCode != "default_404" {
		t.Fatalf("error_code = %q, want %q", env.Error.ErrorCode, "default_404")
	}
}

type fakeLinkService struct {
	submitResponse  dto.SubmitResponse
	refreshResponse dto.SubmitResponse
	batchResponse   dto.BatchSubmitResponse
	ingestResponse  dto.SubmitResponse
	submitErr       error
	refreshErr      error
	batchErr        error
	ingestErr       error
	listErr         error
	listResponse    dto.PaginatedLinksResponse
	listRequest     *dto.ListLinksRequest
	getErr          error
	// lastIncludeContent 记下 GET /api/links/:id 解析出的 include_content，
	// 用来钉住「Reader 传 false 时详情不带正文」这条契约。
	lastIncludeContent bool
	deleteErr          error
	ingestRequest      *dto.IngestRequest
	exportPayload      string
	exportErr          error
	exportCalls        int
	// Phase 14 (v4.0 M3)：GET /api/export/concepts 桩字段。
	exportConceptsPayload string
	exportConceptsErr     error
	exportConceptsCalls   int
}

type fakeContentService struct {
	calls        int
	getCalls     int
	getResponse  dto.LinkContentResponse
	getErr       error
	editResponse dto.LinkContentResponse
	editErr      error
	editRequest  *dto.ContentEditRequest
}

func (f *fakeContentService) Save(context.Context, string) (dto.LinkContentResponse, error) {
	f.calls++
	return dto.LinkContentResponse{}, nil
}

func (f *fakeContentService) Replace(context.Context, string) (dto.LinkContentResponse, error) {
	f.calls++
	return dto.LinkContentResponse{}, nil
}

func (f *fakeContentService) Edit(_ context.Context, _ string, request dto.ContentEditRequest) (dto.LinkContentResponse, error) {
	f.calls++
	copy := request
	f.editRequest = &copy
	return f.editResponse, f.editErr
}

func (f *fakeContentService) Get(context.Context, string) (dto.LinkContentResponse, error) {
	f.getCalls++
	return f.getResponse, f.getErr
}

func (f *fakeLinkService) Submit(context.Context, dto.LinkCreateRequest) (dto.SubmitResponse, error) {
	return f.submitResponse, f.submitErr
}

func (f *fakeLinkService) Refresh(context.Context, string) (dto.SubmitResponse, error) {
	return f.refreshResponse, f.refreshErr
}

func (f *fakeLinkService) Batch(context.Context, dto.BatchCreateRequest) (dto.BatchSubmitResponse, error) {
	return f.batchResponse, f.batchErr
}

func (f *fakeLinkService) Ingest(_ context.Context, req dto.IngestRequest) (dto.SubmitResponse, error) {
	reqCopy := req
	f.ingestRequest = &reqCopy
	return f.ingestResponse, f.ingestErr
}

func (f *fakeLinkService) List(_ context.Context, req dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	reqCopy := req
	f.listRequest = &reqCopy
	return f.listResponse, f.listErr
}

func (f *fakeLinkService) Get(context.Context, string) (dto.LinkResponse, error) {
	return dto.LinkResponse{}, f.getErr
}

func (f *fakeLinkService) GetWithContent(_ context.Context, _ string, includeContent bool) (dto.LinkResponse, error) {
	f.lastIncludeContent = includeContent
	return dto.LinkResponse{}, f.getErr
}

func (f *fakeLinkService) Delete(context.Context, string) error {
	return f.deleteErr
}

func (f *fakeLinkService) Export(_ context.Context, w io.Writer) error {
	f.exportCalls++
	if f.exportPayload != "" {
		if _, err := io.WriteString(w, f.exportPayload); err != nil {
			return err
		}
	}
	return f.exportErr
}

func (f *fakeLinkService) ExportConcepts(_ context.Context, w io.Writer) error {
	f.exportConceptsCalls++
	if f.exportConceptsPayload != "" {
		if _, err := io.WriteString(w, f.exportConceptsPayload); err != nil {
			return err
		}
	}
	return f.exportConceptsErr
}

type fakeJobService struct {
	response       dto.JobResponse
	err            error
	listResponse   []dto.JobResponse
	listErr        error
	listRequestIDs []string
}

func (f *fakeJobService) Get(context.Context, string) (dto.JobResponse, error) {
	return f.response, f.err
}

func (f *fakeJobService) List(_ context.Context, ids []string) ([]dto.JobResponse, error) {
	f.listRequestIDs = append([]string(nil), ids...)
	return f.listResponse, f.listErr
}

type fakeTagService struct {
	response []dto.TagCountResponse
	err      error
}

func (f *fakeTagService) List(context.Context) ([]dto.TagCountResponse, error) {
	return f.response, f.err
}

type fakeTreeService struct {
	response         dto.TreeResponse
	err              error
	gotDomain        string
	gotLibraryKind   string
	domainsResponse  dto.DomainTreeSummaryEnvelope
	domainsErr       error
	listDomainsCalls int
}

func (f *fakeTreeService) Get(_ context.Context, domain string) (dto.TreeResponse, error) {
	f.gotDomain = domain
	return f.response, f.err
}

func (f *fakeTreeService) ListDomains(context.Context) (dto.DomainTreeSummaryEnvelope, error) {
	f.listDomainsCalls++
	return f.domainsResponse, f.domainsErr
}

func (f *fakeTreeService) ListDomainsScoped(_ context.Context, scope string) (dto.DomainTreeSummaryEnvelope, error) {
	f.listDomainsCalls++
	f.gotLibraryKind = scope
	return f.domainsResponse, f.domainsErr
}

// withStubDeps fills in any nil required service field on deps with a
// zero-value stub. RegisterRoutes panics on nil services since M4
// (Wave 12) — tests that only care about, say, the Tags route still
// have to populate every link sub-interface to pass the boot-time
// check, and this helper keeps that boilerplate out of every test.
func withStubDeps(deps Dependencies) Dependencies {
	if deps.LinksWrite == nil {
		deps.LinksWrite = &fakeLinkService{}
	}
	if deps.LinksRead == nil {
		deps.LinksRead = &fakeLinkService{}
	}
	if deps.Ingest == nil {
		deps.Ingest = &fakeLinkService{}
	}
	if deps.Jobs == nil {
		deps.Jobs = &fakeJobService{}
	}
	if deps.Tags == nil {
		deps.Tags = &fakeTagService{}
	}
	if deps.Tree == nil {
		deps.Tree = &fakeTreeService{}
	}
	return deps
}

// linkFakeDeps wires svc as LinksWrite, LinksRead, and Ingest in one
// shot. fakeLinkService satisfies all three narrow handler interfaces
// (its method set still covers the original 7-method surface), so a
// single instance covers every link-side route the test might hit.
// Use this when a test exercises the link surface but doesn't care
// which sub-interface receives the call.
func linkFakeDeps(svc *fakeLinkService) Dependencies {
	return Dependencies{
		LinksWrite: svc,
		LinksRead:  svc,
		Ingest:     svc,
	}
}

// TestGetJobPropagatesServiceError 锁定 getJob 的 err → writeError 分支。
// 之前只有 happy-path 测试（TestGetJobReturnsLinkOnlyWhenDone），覆盖率
// 66.7% 留在错误路径上。回归会让 service 抛出的 4xx/5xx 直接静默成 200。
func TestGetJobPropagatesServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Dependencies{
		Jobs: &fakeJobService{err: httperr.New(http.StatusNotFound, "job not found")},
	}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs/job-x", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (body=%s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestListTagsHappyAndErrorPaths 一并锁定 listTags 的 200 与错误路径，
// 把 tags.go 覆盖率从 66.7% 拉到 100%。listTags 是 /api/tags endpoint
// 的唯一实现，错误路径回归会让前端按异常处理的逻辑断。
func TestListTagsHappyAndErrorPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("happy_path", func(t *testing.T) {
		deps := Dependencies{
			Tags: &fakeTagService{
				response: []dto.TagCountResponse{{Tag: "ai", Count: 3}, {Tag: "go", Count: 7}},
			},
		}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		var body []dto.TagCountResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(body) != 2 || body[0].Tag != "ai" || body[1].Count != 7 {
			t.Errorf("unexpected response body: %+v", body)
		}
	})

	t.Run("empty_array", func(t *testing.T) {
		deps := Dependencies{Tags: &fakeTagService{response: nil}}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
			t.Fatalf("body = %s, want []", got)
		}
	})

	t.Run("service_error", func(t *testing.T) {
		deps := Dependencies{
			Tags: &fakeTagService{err: errors.New("tag store offline")},
		}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("non-StatusCarrier error should map to 500, got %d", rec.Code)
		}
	})
}

// TestGetTreeHappyAndErrorPaths 锁定 tree handler 的 200 / 错误两路径，
// 并验证 ?domain= query 被原样传给 service.Get。getTree 是 /api/tree
// endpoint 的唯一实现，是前端目录导航的数据来源。
func TestGetTreeHappyAndErrorPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("happy_path_passes_domain_to_service", func(t *testing.T) {
		svc := &fakeTreeService{response: dto.TreeResponse{}}
		deps := Dependencies{Tree: svc}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tree?domain=example.com", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if svc.gotDomain != "example.com" {
			t.Errorf("Tree.Get called with domain=%q, want %q", svc.gotDomain, "example.com")
		}
	})

	t.Run("domains_view_uses_lightweight_summary", func(t *testing.T) {
		svc := &fakeTreeService{
			domainsResponse: dto.DomainTreeSummaryEnvelope{
				Domains: []dto.DomainTreeSummaryResponse{{Domain: "example.com", Count: 3}},
				Total:   4,
			},
		}
		deps := Dependencies{Tree: svc}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tree?view=domains", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if svc.listDomainsCalls != 1 {
			t.Fatalf("ListDomains() calls = %d, want 1", svc.listDomainsCalls)
		}
		if svc.gotDomain != "" {
			t.Fatalf("Get() should not be used in domains view, got domain %q", svc.gotDomain)
		}
		if !strings.Contains(rec.Body.String(), `"total":4`) {
			t.Fatalf("body = %s, want independent total", rec.Body.String())
		}
	})

	t.Run("domains_view_passes_library_scope", func(t *testing.T) {
		svc := &fakeTreeService{domainsResponse: dto.DomainTreeSummaryEnvelope{Total: 1}}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(Dependencies{Tree: svc}))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tree?view=domains&library_kind=reading", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if svc.gotLibraryKind != "reading" {
			t.Fatalf("ListDomainsScoped scope = %q, want reading", svc.gotLibraryKind)
		}
	})

	t.Run("domains_view_rejects_invalid_library_scope_without_legacy_fallback", func(t *testing.T) {
		svc := &fakeTreeService{domainsResponse: dto.DomainTreeSummaryEnvelope{Total: 9}}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(Dependencies{Tree: svc}))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tree?view=domains&library_kind=all", nil))

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
		}
		if svc.listDomainsCalls != 0 {
			t.Fatalf("invalid scope called aggregate service %d times", svc.listDomainsCalls)
		}
	})

	t.Run("service_error", func(t *testing.T) {
		deps := Dependencies{
			Tree: &fakeTreeService{err: httperr.New(http.StatusBadGateway, "tree upstream timeout")},
		}
		router := gin.New()
		RegisterRoutes(router, withStubDeps(deps))

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tree", nil))

		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
		}
	})
}
