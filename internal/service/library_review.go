package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type LibraryReviewService struct {
	reviews repository.LibraryReviewStore
	actions repository.MigrationReviewActionExecutor
}

func NewLibraryReviewService(reviews repository.LibraryReviewStore, actions ...repository.MigrationReviewActionExecutor) *LibraryReviewService {
	service := &LibraryReviewService{reviews: reviews}
	if len(actions) > 0 {
		service.actions = actions[0]
	}
	return service
}

func (s *LibraryReviewService) List(ctx context.Context, status, kind string, limit, offset int) ([]dto.LibraryReviewResponse, error) {
	p := repository.ListLibraryReviewsParams{Limit: limit, Offset: offset}
	if status != "" {
		value := model.LibraryReviewStatus(strings.TrimSpace(status))
		if value != model.LibraryReviewStatusPending && value != model.LibraryReviewStatusApplied && value != model.LibraryReviewStatusDismissed {
			return nil, invalidReview("unsupported review status")
		}
		p.Status = &value
	}
	if kind != "" {
		value := model.LibraryReviewKind(strings.TrimSpace(kind))
		if !validReviewKind(value) {
			return nil, invalidReview("unsupported review type")
		}
		p.Kind = &value
	}
	if p.Limit < 1 || p.Limit > 100 || p.Offset < 0 {
		return nil, invalidReview("invalid review pagination")
	}
	items, err := s.reviews.ListLibraryReviews(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]dto.LibraryReviewResponse, 0, len(items))
	for _, item := range items {
		out = append(out, reviewResponse(item))
	}
	return out, nil
}

func (s *LibraryReviewService) Resolve(ctx context.Context, id string, request dto.LibraryReviewResolveRequest) (dto.LibraryReviewResponse, error) { //nolint:gocyclo // 复核裁决需覆盖每种 action × 每种当前状态的组合
	parsed, err := uuid.Parse(id)
	if err != nil || request.ExpectedRevision < 1 {
		return dto.LibraryReviewResponse{}, invalidReview("invalid review id or revision")
	}
	status := model.LibraryReviewStatus(request.Resolution)
	if status != model.LibraryReviewStatusApplied && status != model.LibraryReviewStatusDismissed {
		return dto.LibraryReviewResponse{}, invalidReview("resolution must be applied or dismissed")
	}
	if request.Action == "keep_reading" {
		if status != model.LibraryReviewStatusApplied || s.actions == nil {
			return dto.LibraryReviewResponse{}, invalidReview("keep_reading requires applied resolution")
		}
		item, actionErr := s.actions.KeepHistoricalMigrationReading(ctx, parsed, request.ExpectedRevision)
		if errors.Is(actionErr, repository.ErrRevisionConflict) {
			return dto.LibraryReviewResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "review or link changed")
		}
		if actionErr != nil {
			return dto.LibraryReviewResponse{}, actionErr
		}
		return reviewResponse(*item), nil
	}
	if request.Action == "move_to_site" {
		if status != model.LibraryReviewStatusApplied || s.actions == nil {
			return dto.LibraryReviewResponse{}, invalidReview("move_to_site requires applied resolution")
		}
		if !request.ConfirmDestructive {
			return dto.LibraryReviewResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeDestructiveConfirmationRequired, "moving migration suggestion to site requires confirmation")
		}
		item, actionErr := s.actions.MoveHistoricalMigrationToSite(ctx, parsed, request.ExpectedRevision)
		if errors.Is(actionErr, repository.ErrRevisionConflict) {
			return dto.LibraryReviewResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "review or link changed")
		}
		if actionErr != nil {
			return dto.LibraryReviewResponse{}, actionErr
		}
		return reviewResponse(*item), nil
	}
	item, err := s.reviews.ResolveLibraryReview(ctx, repository.ResolveLibraryReviewParams{ID: parsed, Revision: request.ExpectedRevision, Status: status})
	if errors.Is(err, repository.ErrRevisionConflict) {
		return dto.LibraryReviewResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "review was already resolved or changed")
	}
	if err != nil {
		return dto.LibraryReviewResponse{}, err
	}
	return reviewResponse(*item), nil
}

func validReviewKind(value model.LibraryReviewKind) bool {
	return value == model.LibraryReviewKindClassificationUncertain || value == model.LibraryReviewKindMigrationSuggestion || value == model.LibraryReviewKindNoteConflict || value == model.LibraryReviewKindMergeConflict
}

func invalidReview(message string) error {
	return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_library_review", message)
}

func reviewResponse(item model.LibraryReviewItem) dto.LibraryReviewResponse {
	payload := map[string]any{}
	// The migration constraint requires a JSON object. Keep a defensive empty
	// object here so a legacy malformed row cannot make the read API emit an
	// invalid response shape.
	_ = json.Unmarshal(item.Payload, &payload)
	out := dto.LibraryReviewResponse{ID: item.ID.String(), Kind: string(item.Kind), Payload: payload, Status: string(item.Status), Revision: item.Revision, CreatedAt: item.CreatedAt, ResolvedAt: item.ResolvedAt}
	if item.LinkID != nil {
		value := item.LinkID.String()
		out.LinkID = &value
	}
	if item.SiteID != nil {
		value := item.SiteID.String()
		out.SiteID = &value
	}
	return out
}
