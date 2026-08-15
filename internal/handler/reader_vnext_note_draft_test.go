package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

type noteDraftHandlerStub struct {
	ReaderService
	errors map[string]error
	note   dto.ReaderNoteResponse
}

func (s *noteDraftHandlerStub) SaveNoteDraft(_ context.Context, id string, _ dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error) {
	if err := s.errors[id]; err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	return s.note, nil
}

func TestReaderSaveNoteDraftHTTPContractAcrossAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	missingID, staleID, successID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	stub := &noteDraftHandlerStub{
		errors: map[string]error{
			missingID: httperr.NewWithCode(http.StatusNotFound, "reader_not_found", "reader resource not found"),
			staleID:   httperr.NewWithCode(http.StatusConflict, "revision_conflict", "reader resource revision conflict"),
		},
		note: dto.ReaderNoteResponse{ID: successID, Title: "Draft", PublishedRevision: 1, DraftRevision: 2, CreatedAt: now, UpdatedAt: now},
	}
	router := gin.New()
	RegisterReaderRoutes(router, stub)

	for _, prefix := range []string{"/api", "/api/v1"} {
		for _, test := range []struct {
			name       string
			id         string
			wantStatus int
			wantCode   string
		}{
			{name: "missing", id: missingID, wantStatus: http.StatusNotFound, wantCode: "reader_not_found"},
			{name: "stale", id: staleID, wantStatus: http.StatusConflict, wantCode: "revision_conflict"},
			{name: "matching", id: successID, wantStatus: http.StatusOK},
		} {
			t.Run(prefix+"/"+test.name, func(t *testing.T) {
				recording := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPatch, prefix+"/notes/"+test.id+"/draft", bytes.NewBufferString(`{"content":"draft","expected_draft_revision":1}`))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recording, request)
				if recording.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", recording.Code, test.wantStatus, recording.Body.String())
				}
				if test.wantCode != "" {
					var body struct {
						Error struct {
							ErrorCode string `json:"error_code"`
						} `json:"error"`
					}
					if err := json.Unmarshal(recording.Body.Bytes(), &body); err != nil || body.Error.ErrorCode != test.wantCode {
						t.Fatalf("error body = %s, decode=%v; want error_code=%q", recording.Body.String(), err, test.wantCode)
					}
				} else if got := recording.Header().Get("ETag"); got != `"2"` {
					t.Fatalf("ETag = %q, want %q", got, `"2"`)
				}
			})
		}
	}
}
