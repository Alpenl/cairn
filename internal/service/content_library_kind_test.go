package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestContentServiceRejectsSiteAndUnclassifiedLinks(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind *model.LibraryKind
		code string
	}{
		{name: "site", kind: libraryKindPtr(model.LibraryKindSite), code: httperr.CodeSiteOriginalContentForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &contentKindStore{link: &model.Link{ID: uuid.New(), Status: model.LinkStatusDone, LibraryKind: tt.kind}}
			_, err := NewContentService(store, nil, nil).Save(context.Background(), store.link.ID.String())
			var statusErr *httperr.Error
			if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != 409 || statusErr.HTTPErrorCode() != tt.code {
				t.Fatalf("Save() error = %v, want 409 %s", err, tt.code)
			}
			if store.contentWriteCalled {
				t.Fatal("Save() attempted a content write for a disallowed library kind")
			}
		})
	}
}

type contentKindStore struct {
	link               *model.Link
	contentWriteCalled bool
}

func (s *contentKindStore) GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error) {
	if s.link == nil {
		return nil, nil
	}
	projection := contentParseInput(s.link)
	return &projection, nil
}
func (s *contentKindStore) GetContent(context.Context, uuid.UUID) (*model.SavedContent, error) {
	return nil, nil
}
func (s *contentKindStore) UpdateContentIfCurrent(context.Context, uuid.UUID, time.Time, model.SavedContent) (int64, bool, error) {
	s.contentWriteCalled = true
	return 0, false, nil
}
func (s *contentKindStore) ReplaceContentIfCurrentWithRevision(context.Context, uuid.UUID, time.Time, int64, model.SavedContent) (int64, bool, error) {
	s.contentWriteCalled = true
	return 0, false, nil
}
func (s *contentKindStore) EditContentIfRevision(context.Context, uuid.UUID, int64, model.SavedContent) (int64, bool, error) {
	s.contentWriteCalled = true
	return 0, false, nil
}

func libraryKindPtr(value model.LibraryKind) *model.LibraryKind { return &value }
