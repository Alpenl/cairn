package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
)

type readerReattachHandlerStub struct {
	ReaderService

	err     error
	calls   int
	thought string
	request dto.ReaderThoughtReattachRequest
}

func (s *readerReattachHandlerStub) ReattachThought(_ context.Context, thought string, request dto.ReaderThoughtReattachRequest) (dto.ReaderThoughtResponse, error) {
	s.calls++
	s.thought = thought
	s.request = request
	return dto.ReaderThoughtResponse{}, s.err
}

func TestReaderReattachThoughtPreservesStableErrorsOnBothAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	targetID := uuid.NewString()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "missing source or target", err: httperr.NewWithCode(http.StatusNotFound, "reader_not_found", "reader resource not found"), wantStatus: http.StatusNotFound, wantCode: "reader_not_found"},
		{name: "active source", err: httperr.NewWithCode(http.StatusConflict, "thought_reattach_invalid_state", "thought is not reattachable"), wantStatus: http.StatusConflict, wantCode: "thought_reattach_invalid_state"},
		{name: "stale revision", err: httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "resource revision is stale"), wantStatus: http.StatusConflict, wantCode: httperr.CodeRevisionConflict},
		{name: "invalid reanchor", err: httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_reanchor_ops", "invalid reanchor operations"), wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_reanchor_ops"},
	}

	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, tc := range tests {
			t.Run(prefix+"/"+tc.name, func(t *testing.T) {
				stub := &readerReattachHandlerStub{err: tc.err}
				router := gin.New()
				RegisterReaderRoutes(router, stub)
				body := []byte(`{"target_host_kind":"link","target_host_id":"` + targetID + `","expected_last_sequence":4,"expected_host_revision":2}`)
				request := httptest.NewRequest(http.MethodPost, prefix+"/annotations/thought-1/reattach", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)

				if response.Code != tc.wantStatus {
					t.Fatalf("POST reattach status = %d, want %d; body=%s", response.Code, tc.wantStatus, response.Body.String())
				}
				if stub.calls != 1 || stub.thought != "thought-1" || stub.request.TargetHostID != targetID {
					t.Fatalf("handler calls/thought/request = %d/%q/%#v, want one request for thought-1", stub.calls, stub.thought, stub.request)
				}
				var envelope dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("decode error envelope: %v; body=%s", err, response.Body.String())
				}
				if envelope.Error.Code != tc.wantStatus || envelope.Error.ErrorCode != tc.wantCode {
					t.Fatalf("error envelope = %#v, want status/code %d/%q", envelope.Error, tc.wantStatus, tc.wantCode)
				}
			})
		}
	}
}

func TestReaderReattachThoughtRejectsInvalidRevisionOnBothAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	targetID := uuid.NewString()
	for _, prefix := range []string{"/api", "/api/v1"} {
		t.Run(prefix, func(t *testing.T) {
			stub := &readerReattachHandlerStub{}
			router := gin.New()
			RegisterReaderRoutes(router, stub)
			body := []byte(`{"target_host_kind":"link","target_host_id":"` + targetID + `","expected_last_sequence":4,"expected_host_revision":0}`)
			request := httptest.NewRequest(http.MethodPost, prefix+"/annotations/thought-1/reattach", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("POST reattach invalid revision status = %d, want 422; body=%s", response.Code, response.Body.String())
			}
			if stub.calls != 0 {
				t.Fatalf("service calls = %d, want 0 for invalid request", stub.calls)
			}
			var envelope dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, response.Body.String())
			}
			if envelope.Error.ErrorCode != middleware.ErrCodeInvalidRequestBody {
				t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, middleware.ErrCodeInvalidRequestBody)
			}
		})
	}
}
