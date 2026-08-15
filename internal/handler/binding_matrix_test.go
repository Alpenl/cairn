package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/middleware"
	"webtag/internal/observability"
)

func TestJSONBindingErrorsMatchAcrossAPIPrefixes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &classificationRuleHandlerFake{}
	router := gin.New()
	RegisterRoutes(router, withStubDeps(Dependencies{ClassificationRules: service}))

	oversized := `{"host":"` + strings.Repeat("x", int(defaultMaxJSONBodyBytes)) + `","target_kind":"site"}`
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "malformed", body: `{"host":`, wantStatus: http.StatusUnprocessableEntity, wantCode: middleware.ErrCodeInvalidRequestBody},
		{name: "empty", body: "", wantStatus: http.StatusUnprocessableEntity, wantCode: middleware.ErrCodeInvalidRequestBody},
		{name: "field type", body: `{"host":123,"target_kind":"site"}`, wantStatus: http.StatusUnprocessableEntity, wantCode: middleware.ErrCodeInvalidRequestBody},
		{name: "oversized", body: oversized, wantStatus: http.StatusRequestEntityTooLarge, wantCode: middleware.ErrCodeBodyTooLarge},
	}

	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, test := range tests {
			t.Run(prefix+"/"+test.name, func(t *testing.T) {
				service.createCalls = 0
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				requestContext := observability.ContextWithLogger(t.Context(), logger)
				req := httptest.NewRequest(http.MethodPost, prefix+"/library-classification-rules", strings.NewReader(test.body)).WithContext(requestContext)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				if rec.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.wantStatus, rec.Body.String())
				}
				var payload struct {
					Error struct {
						ErrorCode string `json:"error_code"`
						Message   string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if payload.Error.ErrorCode != test.wantCode {
					t.Fatalf("error_code = %q, want %q", payload.Error.ErrorCode, test.wantCode)
				}
				if service.createCalls != 0 {
					t.Fatalf("service calls = %d, want 0", service.createCalls)
				}
				if strings.Contains(logs.String(), "http handler returned internal error") {
					t.Fatalf("binding failure emitted internal-error log: %s", logs.String())
				}
				for _, leaked := range []string{"unexpected EOF", "cannot unmarshal", "ClassificationRuleCreateRequest"} {
					if strings.Contains(payload.Error.Message, leaked) {
						t.Fatalf("client message %q leaked decoder detail %q", payload.Error.Message, leaked)
					}
				}
			})
		}
	}
}
