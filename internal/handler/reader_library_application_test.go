package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/service"
)

type readerHomeApplicationStore struct {
	service.ReaderLibraryStore
	aggregate model.ReaderHomeAggregate
}

type readerMetadataApplicationStore struct {
	service.ReaderLibraryStore
	calls  int
	patch  model.ReaderLinkMetadataPatch
	result model.ReaderLinkMetadataUpdate
}

func (s *readerMetadataApplicationStore) UpdateLinkMetadata(_ context.Context, patch model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error) {
	s.calls++
	s.patch = patch
	return s.result, nil
}

func (s *readerHomeApplicationStore) LoadHomeAggregate(context.Context) (model.ReaderHomeAggregate, error) {
	return s.aggregate, nil
}

func TestReaderLibraryRoutesMapHomeDomainResultToWire(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	linkID, todoID := uuid.New(), uuid.New()
	store := &readerHomeApplicationStore{aggregate: model.ReaderHomeAggregate{
		Freshness: model.ReaderHomeFreshnessFresh,
		Counts:    map[string]int{"pending": 1, "todos": 1},
		ContinueReading: []model.ReaderFeedItem{{
			Key: "link:" + linkID.String(), Source: "reading", LinkID: &linkID,
			Title: "Continue", URL: "https://example.test", CreatedAt: when,
		}},
		Todos: []model.ReaderTodo{{ID: todoID, Text: "Review", OriginKind: "standalone", CreatedAt: when, UpdatedAt: when}},
	}}
	applications := readerTestApplications(store)
	response, err := NewReaderLibraryRoutes(applications.Library).Home(context.Background())
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if response.Freshness != "fresh" || response.Partial || response.Stale || len(response.ContinueReading) != 1 || len(response.Todos) != 1 {
		t.Fatalf("Home() = %#v", response)
	}
	if response.ContinueReading[0].ResourceKey != "link:"+linkID.String() || response.Todos[0].ID != todoID.String() {
		t.Fatalf("Home identities = %#v / %#v", response.ContinueReading[0], response.Todos[0])
	}

	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal Home response: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("unmarshal Home response: %v", err)
	}
	for _, field := range []string{"continue_reading", "recent_thoughts", "todos", "freshness", "partial", "stale"} {
		if _, ok := envelope[field]; !ok {
			t.Fatalf("Home wire response missing %q: %s", field, wire)
		}
	}
}

func TestReaderLibraryRoutesPreserveMetadataFieldPresence(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`{}`, `{"title":null,"summary":null}`, `{"title":null,"tags":[]}`} {
		var request dto.ReaderLinkMetadataRequest
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		store := &readerMetadataApplicationStore{}
		applications := readerTestApplications(store)
		_, err := NewReaderLibraryRoutes(applications.Library).PatchLinkMetadata(context.Background(), uuid.NewString(), request, 7)
		carrier, ok := httperr.As(err)
		if !ok || carrier.HTTPStatus() != http.StatusUnprocessableEntity {
			t.Fatalf("PatchLinkMetadata(%s) error = %v, want 422", raw, err)
		}
		coder, ok := carrier.(httperr.ErrorCoder)
		if !ok || coder.HTTPErrorCode() != httperr.CodeMetadataFieldsRequired {
			t.Fatalf("PatchLinkMetadata(%s) error = %v, want %q", raw, err, httperr.CodeMetadataFieldsRequired)
		}
		if store.calls != 0 {
			t.Fatalf("PatchLinkMetadata(%s) store calls = %d, want 0", raw, store.calls)
		}
	}
}

func TestReaderLibraryRoutesForwardExplicitNullMetadataReplacement(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	var request dto.ReaderLinkMetadataRequest
	if err := json.Unmarshal([]byte(`{"title":null,"summary":null,"tags":[]}`), &request); err != nil {
		t.Fatalf("decode complete replacement: %v", err)
	}
	store := &readerMetadataApplicationStore{result: model.ReaderLinkMetadataUpdate{MetadataRevision: 8}}
	applications := readerTestApplications(store)
	response, err := NewReaderLibraryRoutes(applications.Library).PatchLinkMetadata(context.Background(), linkID.String(), request, 7)
	if err != nil {
		t.Fatalf("PatchLinkMetadata() error = %v", err)
	}
	if response.LinkID != linkID.String() || response.MetadataRevision != 8 {
		t.Fatalf("PatchLinkMetadata() = %#v", response)
	}
	if store.calls != 1 || store.patch.LinkID != linkID || store.patch.Title != nil || store.patch.Summary != nil || store.patch.Tags == nil || len(store.patch.Tags) != 0 {
		t.Fatalf("forwarded metadata patch = %#v, calls = %d", store.patch, store.calls)
	}
}
