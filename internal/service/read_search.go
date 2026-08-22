package service

import (
	"context"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// searchLinks validates q= and combines it with the regular list filters.
func (s *LinkReadService) searchLinks(ctx context.Context, req dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	query := strings.TrimSpace(req.Query)
	if len([]rune(query)) > maxListQueryLen {
		return dto.PaginatedLinksResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeQueryTooLong, "search query too long")
	}

	filter, err := s.searchFilter(req, query)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	links, total, err := s.links.ListDone(ctx, filter)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}

	items := mapLinksToItems(links)
	return dto.PaginatedLinksResponse{
		Items: items,
		Total: total,
		Limit: len(items),
	}, nil
}

// searchFilter parses and validates the filters that combine with q=.
func (s *LinkReadService) searchFilter(req dto.ListLinksRequest, query string) (repository.ListLinksFilter, error) {
	tags, err := splitAndValidateTags(req.Tags)
	if err != nil {
		return repository.ListLinksFilter{}, err
	}
	if err := validateContentTypeFilter(req.ContentType); err != nil {
		return repository.ListLinksFilter{}, err
	}
	if err := validateDomainFilter(req.Domain); err != nil {
		return repository.ListLinksFilter{}, err
	}
	statuses, err := splitAndValidateStatuses(req.Status)
	if err != nil {
		return repository.ListLinksFilter{}, err
	}

	q := query
	return repository.ListLinksFilter{
		Tags:        tags,
		Domain:      stringPtr(req.Domain),
		ContentType: stringPtr(req.ContentType),
		Statuses:    statuses,
		Query:       &q,
		Limit:       searchResponseLimit,
	}, nil
}

const searchResponseLimit = 50

// findByURL handles the Phase 9 ?url= existence check: it normalizes the URL
// through the same validateURL the submit path uses (so the comparison matches
// links.url byte-for-byte), then returns the 0-or-1 matching link as a single-
// element (or empty) items array with no pagination. An invalid URL surfaces
// the validateURL 422 rather than silently returning empty.
func (s *LinkReadService) findByURL(ctx context.Context, req dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	normalized, err := validateURL(req.URL)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}

	link, err := s.links.GetDetailByURL(ctx, normalized)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	if link == nil {
		return dto.PaginatedLinksResponse{Items: []dto.LinkResponse{}, Total: 0, Limit: 0}, nil
	}

	items := mapLinkDetailsToItems([]repository.LinkDetailProjection{*link})
	return dto.PaginatedLinksResponse{Items: items, Total: len(items), Limit: len(items)}, nil
}

func mapLinksToItems(links []model.Link) []dto.LinkResponse {
	items := make([]dto.LinkResponse, 0, len(links))
	for _, link := range links {
		items = append(items, linkToResponse(link))
	}
	return items
}

func mapLinkDetailsToItems(links []repository.LinkDetailProjection) []dto.LinkResponse {
	items := make([]dto.LinkResponse, 0, len(links))
	for _, link := range links {
		items = append(items, linkDetailToResponse(link))
	}
	return items
}
