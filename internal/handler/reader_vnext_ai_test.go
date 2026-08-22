package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

type readerAIHandlerStub struct {
	ReaderLibraryRoutes
	response dto.ReaderAIResponse
	err      error
	calls    int
	context  context.Context
}

func (s *readerAIHandlerStub) CompleteAI(ctx context.Context, _ dto.ReaderAIRequest) (dto.ReaderAIResponse, error) {
	s.calls++
	s.context = ctx
	return s.response, s.err
}

func TestReaderAIHandlerPreservesMachineReadableErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "timeout", status: http.StatusGatewayTimeout, code: "ai_timeout"},
		{name: "canceled", status: 499, code: "ai_request_canceled"},
		{name: "refresh cooldown", status: http.StatusTooManyRequests, code: "cooldown_active"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &readerAIHandlerStub{err: httperr.NewWithCode(tt.status, tt.code, "safe AI error")}
			router := gin.New()
			RegisterReaderRoutes(router, readerTestRoutes(stub))

			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/ai", bytes.NewBufferString(`{"prompt":"question"}`))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)

			if response.Code != tt.status {
				t.Fatalf("POST /api/ai status = %d, want %d; body=%s", response.Code, tt.status, response.Body.String())
			}
			var body dto.ErrorResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if body.Error.Code != tt.status || body.Error.ErrorCode != tt.code || body.Error.Message != "safe AI error" {
				t.Fatalf("error envelope = %#v, want status/code/message %d/%q/%q", body.Error, tt.status, tt.code, "safe AI error")
			}
			if stub.calls != 1 || stub.context == nil {
				t.Fatalf("service calls/context = %d/%v, want one request context", stub.calls, stub.context != nil)
			}
		})
	}
}

func TestReaderAIHandlerRejectsOversizedPromptBeforeService(t *testing.T) {
	stub := &readerAIHandlerStub{}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	response := httptest.NewRecorder()
	prompt := strings.Repeat("x", 16001)
	request := httptest.NewRequest(http.MethodPost, "/api/ai", bytes.NewBufferString(`{"prompt":"`+prompt+`"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized prompt status = %d, want 422; body=%s", response.Code, response.Body.String())
	}
	var body dto.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode validation envelope: %v", err)
	}
	if body.Error.ErrorCode != "invalid_request_body" {
		t.Fatalf("validation error code = %q, want invalid_request_body", body.Error.ErrorCode)
	}
	if stub.calls != 0 {
		t.Fatalf("service calls = %d, want 0 for invalid prompt", stub.calls)
	}
}
