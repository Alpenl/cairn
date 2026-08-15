package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type libraryReviewStoreFake struct {
	list    repository.ListLibraryReviewsParams
	resolve repository.ResolveLibraryReviewParams
	err     error
}

type migrationReviewActionFake struct {
	id       uuid.UUID
	revision int64
	err      error
}

func (f *migrationReviewActionFake) KeepHistoricalMigrationReading(_ context.Context, id uuid.UUID, revision int64) (*model.LibraryReviewItem, error) {
	f.id, f.revision = id, revision
	if f.err != nil {
		return nil, f.err
	}
	return &model.LibraryReviewItem{ID: id, Kind: model.LibraryReviewKindMigrationSuggestion, Status: model.LibraryReviewStatusApplied, Revision: revision + 1}, nil
}
func (f *migrationReviewActionFake) MoveHistoricalMigrationToSite(_ context.Context, id uuid.UUID, revision int64) (*model.LibraryReviewItem, error) {
	f.id, f.revision = id, revision
	if f.err != nil {
		return nil, f.err
	}
	return &model.LibraryReviewItem{ID: id, Kind: model.LibraryReviewKindMigrationSuggestion, Status: model.LibraryReviewStatusApplied, Revision: revision + 1}, nil
}

func (f *libraryReviewStoreFake) ListLibraryReviews(_ context.Context, p repository.ListLibraryReviewsParams) ([]model.LibraryReviewItem, error) {
	f.list = p
	return nil, f.err
}
func (f *libraryReviewStoreFake) ResolveLibraryReview(_ context.Context, p repository.ResolveLibraryReviewParams) (*model.LibraryReviewItem, error) {
	f.resolve = p
	if f.err != nil {
		return nil, f.err
	}
	return &model.LibraryReviewItem{ID: p.ID, Status: p.Status, Revision: p.Revision + 1}, nil
}

func TestLibraryReviewServiceRejectsInvalidFilters(t *testing.T) {
	_, err := NewLibraryReviewService(&libraryReviewStoreFake{}).List(context.Background(), "nope", "", 30, 0)
	var statusErr httperr.StatusCarrier
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnprocessableEntity {
		t.Fatalf("List error=%v", err)
	}
}
func TestLibraryReviewServiceResolveMapsCASConflict(t *testing.T) {
	fake := &libraryReviewStoreFake{err: repository.ErrRevisionConflict}
	_, err := NewLibraryReviewService(fake).Resolve(context.Background(), uuid.New().String(), dto.LibraryReviewResolveRequest{ExpectedRevision: 2, Resolution: "dismissed"})
	var statusErr httperr.StatusCarrier
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusConflict {
		t.Fatalf("Resolve error=%v", err)
	}
}

func TestLibraryReviewServiceKeepReadingUsesActionTransaction(t *testing.T) {
	id := uuid.New()
	action := &migrationReviewActionFake{}
	service := NewLibraryReviewService(&libraryReviewStoreFake{}, action)
	got, err := service.Resolve(context.Background(), id.String(), dto.LibraryReviewResolveRequest{ExpectedRevision: 3, Resolution: "applied", Action: "keep_reading"})
	if err != nil || action.id != id || action.revision != 3 || got.Status != "applied" {
		t.Fatalf("Resolve() = %#v, %v; action=%#v", got, err, action)
	}
}
