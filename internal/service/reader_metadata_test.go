package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerMetadataStoreStub struct {
	repository.ReaderVNextStore

	calls  int
	patch  model.ReaderLinkMetadataPatch
	result model.ReaderLinkMetadataUpdate
	err    error
}

func (s *readerMetadataStoreStub) UpdateLinkMetadata(_ context.Context, patch model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error) {
	s.calls++
	s.patch = patch
	return s.result, s.err
}

type readerMetadataCacheSpy struct{ calls int }

func (s *readerMetadataCacheSpy) Invalidate(context.Context) { s.calls++ }

func TestPatchLinkMetadataRejectsIncompleteOrInvalidTupleBeforeStore(t *testing.T) {
	tooManyTags := make([]string, maxLinkMetadataTags+1)
	for index := range tooManyTags {
		tooManyTags[index] = "tag"
	}
	encodedManyTags, err := json.Marshal(map[string]any{"title": nil, "summary": nil, "tags": tooManyTags})
	if err != nil {
		t.Fatalf("marshal too-many-tags fixture: %v", err)
	}

	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty object", body: `{}`, code: "metadata_fields_required"},
		{name: "missing tags", body: `{"title":null,"summary":null}`, code: "metadata_fields_required"},
		{name: "missing summary", body: `{"title":null,"tags":[]}`, code: "metadata_fields_required"},
		{name: "null tags", body: `{"title":null,"summary":null,"tags":null}`, code: "invalid_link_metadata"},
		{name: "blank tag", body: `{"title":null,"summary":null,"tags":["   "]}`, code: "invalid_link_metadata"},
		{name: "too many tags", body: string(encodedManyTags), code: "invalid_link_metadata"},
		{name: "long title", body: `{"title":"` + strings.Repeat("t", maxLinkMetadataTitleRunes+1) + `","summary":null,"tags":[]}`, code: "invalid_link_metadata"},
		{name: "long summary", body: `{"title":null,"summary":"` + strings.Repeat("s", maxLinkMetadataSummaryRunes+1) + `","tags":[]}`, code: "invalid_link_metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataStoreStub{}
			cache := &readerMetadataCacheSpy{}
			reader := NewReaderVNextService(store, nil)
			reader.ConfigureMetadataCacheInvalidator(cache)

			_, err := reader.PatchLinkMetadata(context.Background(), uuid.NewString(), readerMetadataRequest(t, tc.body), 7)
			assertReaderHTTPError(t, err, http.StatusUnprocessableEntity, tc.code)
			if store.calls != 0 {
				t.Fatalf("UpdateLinkMetadata calls = %d, want 0", store.calls)
			}
			if cache.calls != 0 {
				t.Fatalf("cache invalidations = %d, want 0", cache.calls)
			}
		})
	}
}

func TestPatchLinkMetadataNormalizesCompleteReplacementAndInvalidatesChangedTags(t *testing.T) {
	linkID := uuid.New()
	store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: 8, TagsChanged: true}}
	cache := &readerMetadataCacheSpy{}
	reader := NewReaderVNextService(store, nil)
	reader.ConfigureMetadataCacheInvalidator(cache)

	response, err := reader.PatchLinkMetadata(
		context.Background(),
		linkID.String(),
		readerMetadataRequest(t, `{"title":null,"summary":null,"tags":[" Go ","go","Rust"," RUST "]}`),
		7,
	)
	if err != nil {
		t.Fatalf("PatchLinkMetadata() error = %v", err)
	}
	if response.LinkID != linkID.String() || response.MetadataRevision != 8 {
		t.Fatalf("response = %#v, want link %s at revision 8", response, linkID)
	}
	if store.calls != 1 || store.patch.LinkID != linkID || store.patch.ExpectedRevision != 7 {
		t.Fatalf("store patch identity = %#v", store.patch)
	}
	if store.patch.Title != nil || store.patch.Summary != nil {
		t.Fatalf("store patch nullable fields = title=%v summary=%v, want explicit nulls", store.patch.Title, store.patch.Summary)
	}
	if !slices.Equal(store.patch.Tags, []string{"Go", "Rust"}) {
		t.Fatalf("store tags = %#v, want trim/dedupe replacement", store.patch.Tags)
	}
	if cache.calls != 1 {
		t.Fatalf("cache invalidations = %d, want 1 after a changed tag tuple", cache.calls)
	}
}

func TestPatchLinkMetadataUsesUnicodeCaseFoldForNoopReplacement(t *testing.T) {
	linkID := uuid.New()
	store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: 7, TagsChanged: false}}
	cache := &readerMetadataCacheSpy{}
	reader := NewReaderVNextService(store, nil)
	reader.ConfigureMetadataCacheInvalidator(cache)

	response, err := reader.PatchLinkMetadata(
		context.Background(),
		linkID.String(),
		readerMetadataRequest(t, `{"title":"same","summary":null,"tags":[" \u03a3 ","\u03c2","Stra\u00dfe","STRASSE"]}`),
		7,
	)
	if err != nil {
		t.Fatalf("PatchLinkMetadata() error = %v", err)
	}
	if response.MetadataRevision != 7 {
		t.Fatalf("metadata revision = %d, want normalized no-op to retain 7", response.MetadataRevision)
	}
	if !slices.Equal(store.patch.Tags, []string{"\u03a3", "Stra\u00dfe"}) {
		t.Fatalf("store tags = %#v, want full Unicode case-fold replacement", store.patch.Tags)
	}
	if cache.calls != 0 {
		t.Fatalf("cache invalidations = %d, want 0 for normalized no-op", cache.calls)
	}
}

func TestPatchLinkMetadataDoesNotInvalidateForNoopOrConflict(t *testing.T) {
	linkID := uuid.NewString()
	request := readerMetadataRequest(t, `{"title":"same","summary":null,"tags":["same"]}`)

	tests := []struct {
		name       string
		result     model.ReaderLinkMetadataUpdate
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:   "no-op",
			result: model.ReaderLinkMetadataUpdate{MetadataRevision: 7, TagsChanged: false},
		},
		{
			name:       "stale revision",
			err:        repository.ErrRevisionConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "metadata_revision_conflict",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataStoreStub{result: tc.result, err: tc.err}
			cache := &readerMetadataCacheSpy{}
			reader := NewReaderVNextService(store, nil)
			reader.ConfigureMetadataCacheInvalidator(cache)

			response, err := reader.PatchLinkMetadata(context.Background(), linkID, request, 7)
			if tc.err == nil {
				if err != nil || response.MetadataRevision != 7 {
					t.Fatalf("no-op response/error = %#v, %v", response, err)
				}
			} else {
				assertReaderHTTPError(t, err, tc.wantStatus, tc.wantCode)
				if !errors.Is(tc.err, repository.ErrRevisionConflict) {
					t.Fatalf("test setup err = %v, want revision conflict", tc.err)
				}
			}
			if store.calls != 1 {
				t.Fatalf("UpdateLinkMetadata calls = %d, want 1", store.calls)
			}
			if cache.calls != 0 {
				t.Fatalf("cache invalidations = %d, want 0", cache.calls)
			}
		})
	}
}

func TestPatchLinkMetadataRejectsOutOfRangeStoreRevision(t *testing.T) {
	request := readerMetadataRequest(t, `{"title":"same","summary":null,"tags":["same"]}`)
	for _, tc := range []struct {
		name     string
		revision int64
	}{
		{name: "zero", revision: 0},
		{name: "above JavaScript safe maximum", revision: model.LinkMetadataMaxRevision + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: tc.revision, TagsChanged: true}}
			cache := &readerMetadataCacheSpy{}
			reader := NewReaderVNextService(store, nil)
			reader.ConfigureMetadataCacheInvalidator(cache)

			_, err := reader.PatchLinkMetadata(context.Background(), uuid.NewString(), request, model.LinkMetadataMaxRevision)
			assertReaderHTTPError(t, err, http.StatusConflict, httperr.CodeMetadataRevisionConflict)
			if store.calls != 1 {
				t.Fatalf("UpdateLinkMetadata calls = %d, want 1", store.calls)
			}
			if cache.calls != 0 {
				t.Fatalf("cache invalidations = %d, want 0", cache.calls)
			}
		})
	}
}

func readerMetadataRequest(t *testing.T, raw string) dto.ReaderLinkMetadataRequest {
	t.Helper()
	var request dto.ReaderLinkMetadataRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode metadata request %s: %v", raw, err)
	}
	return request
}
