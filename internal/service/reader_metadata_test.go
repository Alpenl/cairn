package service

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerMetadataStoreStub struct {
	ReaderLibraryStore
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

func TestPatchLinkMetadataRejectsInvalidCommandBeforeStore(t *testing.T) {
	tooManyTags := make([]string, maxLinkMetadataTags+1)
	for index := range tooManyTags {
		tooManyTags[index] = "tag"
	}
	tests := []struct {
		name    string
		command ReaderLinkMetadataCommand
		code    string
	}{
		{name: "null tags", command: ReaderLinkMetadataCommand{Tags: nil}, code: "invalid_link_metadata"},
		{name: "blank tag", command: ReaderLinkMetadataCommand{Tags: []string{"   "}}, code: "invalid_link_metadata"},
		{name: "too many tags", command: ReaderLinkMetadataCommand{Tags: tooManyTags}, code: "invalid_link_metadata"},
		{name: "long title", command: ReaderLinkMetadataCommand{Title: readerStringPointer(strings.Repeat("t", maxLinkMetadataTitleRunes+1)), Tags: []string{}}, code: "invalid_link_metadata"},
		{name: "long summary", command: ReaderLinkMetadataCommand{Summary: readerStringPointer(strings.Repeat("s", maxLinkMetadataSummaryRunes+1)), Tags: []string{}}, code: "invalid_link_metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataStoreStub{}
			reader := newReaderTestFeatureSet(readerTestStores(store), nil)

			tc.command.LinkID = uuid.New()
			tc.command.ExpectedRevision = 7
			_, err := reader.PatchLinkMetadata(context.Background(), tc.command)
			assertReaderHTTPError(t, err, http.StatusUnprocessableEntity, tc.code)
			if store.calls != 0 {
				t.Fatalf("UpdateLinkMetadata calls = %d, want 0", store.calls)
			}
		})
	}
}

func TestPatchLinkMetadataNormalizesCompleteReplacement(t *testing.T) {
	linkID := uuid.New()
	store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: 8, TagsChanged: true}}
	reader := newReaderTestFeatureSet(readerTestStores(store), nil)

	response, err := reader.PatchLinkMetadata(
		context.Background(),
		ReaderLinkMetadataCommand{LinkID: linkID, Tags: []string{" Go ", "go", "Rust", " RUST "}, ExpectedRevision: 7},
	)
	if err != nil {
		t.Fatalf("PatchLinkMetadata() error = %v", err)
	}
	if response.MetadataRevision != 8 {
		t.Fatalf("response = %#v, want revision 8", response)
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
}

func TestPatchLinkMetadataUsesUnicodeCaseFoldForNoopReplacement(t *testing.T) {
	linkID := uuid.New()
	store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: 7, TagsChanged: false}}
	reader := newReaderTestFeatureSet(readerTestStores(store), nil)

	response, err := reader.PatchLinkMetadata(
		context.Background(),
		ReaderLinkMetadataCommand{LinkID: linkID, Title: readerStringPointer("same"), Tags: []string{" \u03a3 ", "\u03c2", "Stra\u00dfe", "STRASSE"}, ExpectedRevision: 7},
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
}

func TestPatchLinkMetadataHandlesNoopAndConflict(t *testing.T) {
	linkID := uuid.New()
	command := ReaderLinkMetadataCommand{LinkID: linkID, Title: readerStringPointer("same"), Tags: []string{"same"}, ExpectedRevision: 7}

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
			reader := newReaderTestFeatureSet(readerTestStores(store), nil)

			response, err := reader.PatchLinkMetadata(context.Background(), command)
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
		})
	}
}

func TestPatchLinkMetadataRejectsOutOfRangeStoreRevision(t *testing.T) {
	command := ReaderLinkMetadataCommand{LinkID: uuid.New(), Title: readerStringPointer("same"), Tags: []string{"same"}, ExpectedRevision: model.LinkMetadataMaxRevision}
	for _, tc := range []struct {
		name     string
		revision int64
	}{
		{name: "zero", revision: 0},
		{name: "above JavaScript safe maximum", revision: model.LinkMetadataMaxRevision + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataStoreStub{result: model.ReaderLinkMetadataUpdate{MetadataRevision: tc.revision, TagsChanged: true}}
			reader := newReaderTestFeatureSet(readerTestStores(store), nil)

			_, err := reader.PatchLinkMetadata(context.Background(), command)
			assertReaderHTTPError(t, err, http.StatusConflict, httperr.CodeMetadataRevisionConflict)
			if store.calls != 1 {
				t.Fatalf("UpdateLinkMetadata calls = %d, want 1", store.calls)
			}
		})
	}
}

func readerStringPointer(value string) *string {
	return &value
}
