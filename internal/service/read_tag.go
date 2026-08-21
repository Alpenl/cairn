package service

import (
	"context"
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
)

// TagReadService 提供 /api/tags 标签聚合读取。
type TagReadService struct {
	tags tagReadStore
}

type tagReadStore interface {
	ListCounts(context.Context) ([]repository.TagCount, error)
	ListScopedCounts(context.Context, string) ([]repository.ScopedTagCount, error)
}

// NormalizeTagLibraryKind returns the canonical query variant shared by the
// service and conditional route policy.
func NormalizeTagLibraryKind(raw string) (string, bool) {
	scope := strings.ToLower(strings.TrimSpace(raw))
	switch scope {
	case "", "reading", "site", "all":
		return scope, true
	default:
		return "", false
	}
}

// ListScoped returns collection-aware counts. An omitted scope preserves the
// all-library response used by existing clients.
func (s *TagReadService) ListScoped(ctx context.Context, rawScope string) ([]dto.TagCountResponse, error) {
	scope, valid := NormalizeTagLibraryKind(rawScope)
	if !valid {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "library_kind must be reading, site, or all")
	}
	if scope == "" {
		return s.List(ctx)
	}
	rows, err := s.tags.ListScopedCounts(ctx, scope)
	if err != nil {
		return nil, err
	}
	return mapScopedTagCounts(rows), nil
}

func NewTagReadService(tags tagReadStore) *TagReadService {
	return &TagReadService{tags: tags}
}

// List 返回全量标签计数。
func (s *TagReadService) List(ctx context.Context) ([]dto.TagCountResponse, error) {
	counts, err := s.tags.ListCounts(ctx)
	if err != nil {
		return nil, err
	}
	return mapRepositoryTagCounts(counts), nil
}
