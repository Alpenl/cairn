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
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	readerservice "webtag/internal/service"
)

type readerHostLifecycleHTTPStore struct {
	readerservice.ReaderInboxStore
	readerservice.ReaderHostStore
	state string
}

func (s *readerHostLifecycleHTTPStore) SoftDeleteHost(_ context.Context, kind model.ReaderHostKind, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result := model.ReaderHostLifecycleResult{HostKind: kind, HostID: id, State: model.ReaderHostTrashed}
	switch s.state {
	case "live":
		result.Changed = true
		return result, nil
	case "trashed":
		return result, nil
	default:
		return result, repository.ErrNotFound
	}
}

func (s *readerHostLifecycleHTTPStore) RestoreHost(_ context.Context, kind model.ReaderHostKind, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result := model.ReaderHostLifecycleResult{HostKind: kind, HostID: id, State: model.ReaderHostLive}
	switch s.state {
	case "trashed":
		result.Changed = true
		return result, nil
	case "live":
		return result, nil
	default:
		return result, repository.ErrNotFound
	}
}

// The production Inbox restore has its own expiry-aware repository method.
// This lifecycle-only HTTP fake delegates to its existing state matrix so the
// generic route coverage remains focused on transport status mapping.
func (s *readerHostLifecycleHTTPStore) RestoreInbox(ctx context.Context, id uuid.UUID) error {
	_, err := s.RestoreHost(ctx, model.ReaderHostInbox, id)
	return err
}

func (s *readerHostLifecycleHTTPStore) DiscardInbox(ctx context.Context, id uuid.UUID) error {
	_, err := s.SoftDeleteHost(ctx, model.ReaderHostInbox, id)
	return err
}

func (s *readerHostLifecycleHTTPStore) PurgeHost(context.Context, model.ReaderHostKind, uuid.UUID, uuid.UUID) error {
	switch s.state {
	case "live":
		return repository.ErrReaderHostNotTrashed
	case "trashed", "purged-replay":
		return nil
	default:
		return repository.ErrNotFound
	}
}

func (s *readerHostLifecycleHTTPStore) ListTrash(context.Context, *model.ReaderHostKind, string, int) ([]model.ReaderTrashItem, int, string, error) {
	return []model.ReaderTrashItem{}, 0, "", nil
}

type readerHostLifecycleLinkHTTPService struct {
	LinkReadService
	err error
}

func (s *readerHostLifecycleLinkHTTPService) Delete(context.Context, string) error {
	return s.err
}

func TestReaderHostLifecycleHTTPStateAndMachineCodeMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hostID := uuid.MustParse("00000000-0000-0000-0000-000000000053")
	operationID := uuid.MustParse("00000000-0000-0000-0000-000000000054")
	kinds := []model.ReaderHostKind{model.ReaderHostLink, model.ReaderHostInbox, model.ReaderHostNote}
	commands := []string{"delete", "restore", "purge"}
	states := []string{"live", "trashed", "missing", "purged"}

	for _, kind := range kinds {
		for _, command := range commands {
			commandStates := append([]string(nil), states...)
			if command == "purge" {
				commandStates = append(commandStates, "purged-replay")
			}
			for _, state := range commandStates {
				name := strings.Join([]string{string(kind), command, state}, "/")
				t.Run(name, func(t *testing.T) {
					router := gin.New()
					method, path := readerHostLifecycleRequest(kind, command, hostID.String())
					if kind == model.ReaderHostLink && command == "delete" {
						var err error
						if state == "missing" || state == "purged" {
							err = httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
						}
						router.DELETE("/api/links/:link_id", deleteLink(&readerHostLifecycleLinkHTTPService{err: err}))
					} else {
						store := &readerHostLifecycleHTTPStore{state: state}
						reader := readerservice.NewReaderApplications(
							readerServiceTestStores(store), nil,
							readerservice.ReaderApplicationOptions{HostRestoreCommands: store},
						)
						RegisterReaderRoutes(router, readerTestRoutes(reader))
					}

					var body *bytes.Reader
					if command == "purge" {
						body = bytes.NewReader([]byte(`{"operation_id":"` + operationID.String() + `"}`))
					} else {
						body = bytes.NewReader(nil)
					}
					request := httptest.NewRequest(method, path, body)
					if command == "purge" {
						request.Header.Set("Content-Type", "application/json")
					}
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)

					wantStatus, wantCode := readerHostLifecycleHTTPExpectation(kind, command, state)
					if response.Code != wantStatus {
						t.Fatalf("status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
					}
					if wantCode == "" {
						return
					}
					var envelope dto.ErrorResponse
					if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
						t.Fatalf("decode error envelope: %v; body=%s", err, response.Body.String())
					}
					if envelope.Error.ErrorCode != wantCode {
						t.Fatalf("error_code = %q, want %q", envelope.Error.ErrorCode, wantCode)
					}
				})
			}
		}
	}
}

func readerHostLifecycleRequest(kind model.ReaderHostKind, command, id string) (string, string) {
	switch command {
	case "delete":
		switch kind {
		case model.ReaderHostLink:
			return http.MethodDelete, "/api/links/" + id
		case model.ReaderHostInbox:
			return http.MethodPost, "/api/inbox/" + id + "/discard"
		case model.ReaderHostNote:
			return http.MethodDelete, "/api/notes/" + id
		}
	case "restore":
		return http.MethodPost, "/api/" + readerHostLifecyclePathKind(kind) + "/" + id + "/restore"
	case "purge":
		return http.MethodDelete, "/api/" + readerHostLifecyclePathKind(kind) + "/" + id + "/purge"
	}
	panic("unsupported lifecycle request")
}

func readerHostLifecyclePathKind(kind model.ReaderHostKind) string {
	switch kind {
	case model.ReaderHostLink:
		return "links"
	case model.ReaderHostNote:
		return "notes"
	case model.ReaderHostInbox:
		return "inbox"
	default:
		panic("unsupported lifecycle host kind")
	}
}

func readerHostLifecycleHTTPExpectation(kind model.ReaderHostKind, command, state string) (int, string) {
	if command == "purge" {
		switch state {
		case "live":
			return http.StatusConflict, "host_not_trashed"
		case "trashed", "purged-replay":
			return http.StatusNoContent, ""
		default:
			return http.StatusNotFound, "reader_not_found"
		}
	}
	if state == "missing" || state == "purged" {
		if kind == model.ReaderHostLink && command == "delete" {
			return http.StatusNotFound, httperr.CodeLinkNotFound
		}
		return http.StatusNotFound, "reader_not_found"
	}
	return http.StatusOK, ""
}
