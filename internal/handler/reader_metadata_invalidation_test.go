package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/middleware"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type readerMetadataRouteStore struct {
	repository.ReaderVNextStore

	calls  int
	patch  model.ReaderLinkMetadataPatch
	result model.ReaderLinkMetadataUpdate
}

func (s *readerMetadataRouteStore) UpdateLinkMetadata(_ context.Context, patch model.ReaderLinkMetadataPatch) (model.ReaderLinkMetadataUpdate, error) {
	s.calls++
	s.patch = patch
	return s.result, nil
}

type readerMetadataRouteInvalidator struct{ calls int }

func (i *readerMetadataRouteInvalidator) Invalidate(context.Context) { i.calls++ }

func TestReaderPatchMetadataInvalidatesAggregatesExactlyWhenTagsChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	linkID := uuid.New()

	tests := []struct {
		name              string
		body              string
		result            model.ReaderLinkMetadataUpdate
		wantRevision      int64
		wantTags          []string
		wantInvalidations int
	}{
		{
			name:              "normalized unicode no-op",
			body:              `{"title":"same","summary":null,"tags":[" \u03a3 ","\u03c2","Stra\u00dfe","STRASSE"]}`,
			result:            model.ReaderLinkMetadataUpdate{MetadataRevision: 7, TagsChanged: false},
			wantRevision:      7,
			wantTags:          []string{"\u03a3", "Stra\u00dfe"},
			wantInvalidations: 0,
		},
		{
			name:              "changed tags",
			body:              `{"title":"same","summary":null,"tags":["fresh"]}`,
			result:            model.ReaderLinkMetadataUpdate{MetadataRevision: 8, TagsChanged: true},
			wantRevision:      8,
			wantTags:          []string{"fresh"},
			wantInvalidations: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &readerMetadataRouteStore{result: tc.result}
			invalidator := &readerMetadataRouteInvalidator{}
			reader := service.NewReaderVNextService(store, nil)
			reader.ConfigureMetadataCacheInvalidator(invalidator)

			router := gin.New()
			router.Use(middleware.InvalidateAggregatesOnWrite(invalidator))
			RegisterReaderRoutes(router, reader)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/links/"+linkID.String()+"/metadata", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("If-Match", `"7"`)
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("ETag"); got != `"`+strconv.FormatInt(tc.wantRevision, 10)+`"` {
				t.Fatalf("ETag = %q, want revision %d", got, tc.wantRevision)
			}
			if store.calls != 1 || !slices.Equal(store.patch.Tags, tc.wantTags) {
				t.Fatalf("store call/tags = %d/%#v, want 1/%#v", store.calls, store.patch.Tags, tc.wantTags)
			}
			if invalidator.calls != tc.wantInvalidations {
				t.Fatalf("aggregate invalidations = %d, want %d", invalidator.calls, tc.wantInvalidations)
			}
		})
	}
}
