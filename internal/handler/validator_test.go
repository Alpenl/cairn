package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/middleware"
)

// validatorRequestEnvelope 是这套测试共享的"读回 422 错误信封"工具。
// 所有 validator 失败路径走的都是 middleware.JSONErrorWithSlug，与
// bindJSONWithLimit 的现有 contract 一致：HTTP status 422 + ErrorCode
// "invalid_request_body" + Message 给出字段级原因。
func decodeErrorEnvelope(t *testing.T, body []byte) dto.ErrorResponse {
	t.Helper()
	var env dto.ErrorResponse
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("Unmarshal error envelope: %v; body=%s", err, string(body))
	}
	return env
}

// TestPostLinksRejectsMissingURLViaValidator pins the Wave 10 API MED M5
// contract: a LinkCreateRequest without "url" must be rejected at the
// validator layer with 422 + invalid_request_body before reaching the
// service, and the message must include the offending field name so the
// client can localize the failure without scraping logs.
func TestPostLinksRejectsMissingURLViaValidator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"description":"missing url"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.ErrorCode != middleware.ErrCodeInvalidRequestBody {
		t.Fatalf("error_code = %q, want %q", env.Error.ErrorCode, middleware.ErrCodeInvalidRequestBody)
	}
	if !strings.Contains(env.Error.Message, "url") {
		t.Fatalf("message %q should mention the offending field", env.Error.Message)
	}
}

// TestPostLinksRejectsMalformedURLViaValidator covers the "url" validator
// tag: an obviously malformed URL (no scheme + space) must fail the binding
// layer with a "must be a valid URL" message; service.validateURL would
// also reject it but the validator is the early gate.
func TestPostLinksRejectsMalformedURLViaValidator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := linkFakeDeps(&fakeLinkService{})

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"url":"not a url"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.ErrorCode != middleware.ErrCodeInvalidRequestBody {
		t.Fatalf("error_code = %q, want %q", env.Error.ErrorCode, middleware.ErrCodeInvalidRequestBody)
	}
	if !strings.Contains(env.Error.Message, "url") {
		t.Fatalf("message %q should reference the url field", env.Error.Message)
	}
}

// TestPostIngestRejectsUnknownKindViaValidator pins the oneof tag on
// IngestSource.Kind: a Kind outside the {url,text,image,browser_capture}
// allow-list is rejected at the binding layer.
func TestPostIngestRejectsUnknownKindViaValidator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{}
	deps := linkFakeDeps(svc)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"sources":[{"kind":"video","url":"https://example.com/a.mp4"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.ErrorCode != middleware.ErrCodeInvalidRequestBody {
		t.Fatalf("error_code = %q, want %q", env.Error.ErrorCode, middleware.ErrCodeInvalidRequestBody)
	}
	if !strings.Contains(env.Error.Message, "kind") {
		t.Fatalf("message %q should reference the kind field", env.Error.Message)
	}
	if svc.ingestRequest != nil {
		t.Fatal("validator should reject before service is called")
	}
}

// TestPostIngestRejectsEmptySourcesViaValidator pins min=1 on
// IngestRequest.Sources: an empty array short-circuits at validator.
func TestPostIngestRejectsEmptySourcesViaValidator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakeLinkService{}
	deps := linkFakeDeps(svc)

	router := gin.New()
	RegisterRoutes(router, withStubDeps(deps))

	body := []byte(`{"sources":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}
