package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

type readerMetadataHandlerStub struct {
	readerHandlerStub

	calls    int
	linkID   string
	request  dto.ReaderLinkMetadataRequest
	expected int64
	response dto.ReaderLinkMetadataResponse
	err      error
}

func (s *readerMetadataHandlerStub) PatchLinkMetadata(_ context.Context, linkID string, request dto.ReaderLinkMetadataRequest, expected int64) (dto.ReaderLinkMetadataResponse, error) {
	s.calls++
	s.linkID = linkID
	s.request = request
	s.expected = expected
	return s.response, s.err
}

func TestReaderPatchMetadataRequiresCanonicalQuotedRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	linkID := uuid.NewString()

	tests := []struct {
		name       string
		ifMatches  []string
		wantStatus int
		wantCode   string
	}{
		{name: "missing", wantStatus: http.StatusPreconditionRequired, wantCode: "if_match_required"},
		{name: "empty field", ifMatches: []string{""}, wantStatus: http.StatusPreconditionRequired, wantCode: "if_match_required"},
		{name: "unquoted", ifMatches: []string{"7"}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "weak", ifMatches: []string{`W/"7"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "zero", ifMatches: []string{`"0"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "leading zero", ifMatches: []string{`"07"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "non numeric", ifMatches: []string{`"seven"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "above JavaScript safe maximum", ifMatches: []string{`"9007199254740992"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "combined validators", ifMatches: []string{`"7", "8"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "duplicate fields", ifMatches: []string{`"7"`, `"8"`}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
		{name: "surrounding whitespace", ifMatches: []string{` "7" `}, wantStatus: http.StatusUnprocessableEntity, wantCode: "invalid_if_match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &readerMetadataHandlerStub{}
			router := gin.New()
			RegisterReaderRoutes(router, readerTestRoutes(stub))

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/links/"+linkID+"/metadata", nil)
			for _, value := range tc.ifMatches {
				request.Header.Add("If-Match", value)
			}
			router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
			if envelope.Error.ErrorCode != tc.wantCode {
				t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, tc.wantCode)
			}
			if stub.calls != 0 {
				t.Fatalf("PatchLinkMetadata calls = %d, want 0", stub.calls)
			}
		})
	}
}

func TestReaderPatchMetadataForwardsCompleteNullReplacementAndReturnsRevisionETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	linkID := uuid.NewString()
	stub := &readerMetadataHandlerStub{response: dto.ReaderLinkMetadataResponse{LinkID: linkID, MetadataRevision: 8}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/links/"+linkID+"/metadata", strings.NewReader(`{"title":null,"summary":null,"tags":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"7"`)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("ETag"); got != `"8"` {
		t.Fatalf("ETag = %q, want quoted next metadata revision", got)
	}
	var response dto.ReaderLinkMetadataResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response != stub.response {
		t.Fatalf("response = %#v, want %#v", response, stub.response)
	}
	if stub.calls != 1 || stub.linkID != linkID || stub.expected != 7 {
		t.Fatalf("service invocation = calls=%d link=%q expected=%d", stub.calls, stub.linkID, stub.expected)
	}
	if !stub.request.Complete() || stub.request.Title != nil || stub.request.Summary != nil || stub.request.Tags == nil || len(stub.request.Tags) != 0 {
		t.Fatalf("forwarded request = %#v, want complete null/null/empty replacement", stub.request)
	}
}

func TestReaderPatchMetadataAcceptsJavaScriptSafeMaximumRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	linkID := uuid.NewString()
	stub := &readerMetadataHandlerStub{response: dto.ReaderLinkMetadataResponse{LinkID: linkID, MetadataRevision: model.LinkMetadataMaxRevision}}
	router := gin.New()
	RegisterReaderRoutes(router, readerTestRoutes(stub))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/links/"+linkID+"/metadata", strings.NewReader(`{"title":null,"summary":null,"tags":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"9007199254740991"`)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("ETag"); got != `"9007199254740991"` {
		t.Fatalf("ETag = %q, want the JavaScript-safe maximum revision", got)
	}
	if stub.calls != 1 || stub.expected != model.LinkMetadataMaxRevision {
		t.Fatalf("service invocation = calls=%d expected=%d", stub.calls, stub.expected)
	}
}

func TestReaderPatchMetadataMapsCompleteTupleAndConflictErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	linkID := uuid.NewString()
	tests := []struct {
		name       string
		body       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing tuple member",
			body:       `{}`,
			err:        httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeMetadataFieldsRequired, "title, summary, and tags are required"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   httperr.CodeMetadataFieldsRequired,
		},
		{
			name:       "invalid tags",
			body:       `{"title":null,"summary":null,"tags":null}`,
			err:        httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "tags must be an array"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   httperr.CodeInvalidLinkMetadata,
		},
		{
			name:       "stale revision",
			body:       `{"title":"fresh","summary":null,"tags":["fresh"]}`,
			err:        httperr.NewWithCode(http.StatusConflict, httperr.CodeMetadataRevisionConflict, "link metadata revision is stale"),
			wantStatus: http.StatusConflict,
			wantCode:   httperr.CodeMetadataRevisionConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &readerMetadataHandlerStub{err: tc.err}
			router := gin.New()
			RegisterReaderRoutes(router, readerTestRoutes(stub))

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/links/"+linkID+"/metadata", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("If-Match", `"7"`)
			router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
			if envelope.Error.ErrorCode != tc.wantCode {
				t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, tc.wantCode)
			}
		})
	}
}
