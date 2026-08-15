package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

// searchLinks handles the Phase 9 ?q= hybrid search path. It validates the
// query length, parses the same tags/domain/content_type/status filters the
// list path uses (so q= combines with them), vectorizes the query when an
// embedding model is configured, and runs the repository's semantic∪ILIKE
// merge (or pure ILIKE when no vector is available). The result is top-N
// truncated with Total = len(items); page/after are ignored.
func (s *LinkReadService) searchLinks(ctx context.Context, req dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	query := strings.TrimSpace(req.Query)
	if len([]rune(query)) > maxListQueryLen {
		return dto.PaginatedLinksResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeQueryTooLong, "search query too long")
	}

	filter, err := s.searchFilter(req, query)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	s.attachQueryVector(ctx, &filter, query)

	links, total, err := s.links.ListDone(ctx, filter)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}

	items := s.mapLinksToItems(ctx, links)
	return dto.PaginatedLinksResponse{
		Items: items,
		Total: total,
		Limit: len(items),
	}, nil
}

// searchFilter parses + validates the filters that combine with q= and returns
// the repository filter with Query set (which switches ListDone into hybrid
// search mode). Limit is irrelevant in search mode (the repo applies a fixed
// top-N) but set to the contract cap for clarity.
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
		Limit:       hybridSearchResponseLimit,
	}, nil
}

// hybridSearchResponseLimit mirrors the repository's fixed top-N so the
// response Limit field is meaningful even though the repo ignores it.
const hybridSearchResponseLimit = 50

// attachQueryVector vectorizes the query and stamps the vector + model onto the
// filter so the repository runs the semantic leg. Fail-soft: a nil/disabled
// embedder or any embedding error leaves QueryEmbedding nil, degrading the q=
// search to pure ILIKE (the repo branches on the empty vector), logged so the
// degradation is observable.
func (s *LinkReadService) attachQueryVector(ctx context.Context, filter *repository.ListLinksFilter, query string) {
	if s.queryEmbedder == nil || !s.queryEmbedder.Enabled() {
		return
	}
	model := s.queryEmbedder.Model()
	// singleflight：并发相同 (model, query) 的搜索合并成一次上游 embedding 调用。
	// key 用 \x00 分隔避免 model/query 边界歧义。返回的向量是只读消费（拷进 filter），
	// 多个等待者共享同一份切片是安全的——下游 repository 只读不改。
	v, err, _ := s.querySF.Do(model+"\x00"+query, func() (any, error) {
		vectors, embedErr := s.queryEmbedder.Embed(ctx, []string{query})
		if embedErr != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
			return nil, embedErr
		}
		return vectors[0], nil
	})
	vec, ok := v.([]float32)
	if err != nil || !ok || len(vec) == 0 {
		s.logSearchWarn("q= query vectorization failed; degrading to ILIKE keyword search", err)
		return
	}
	filter.QueryEmbedding = vec
	filter.EmbeddingModel = model
}

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

	items := s.mapLinkDetailsToItems(ctx, []repository.LinkDetailProjection{*link})
	return dto.PaginatedLinksResponse{Items: items, Total: len(items), Limit: len(items)}, nil
}

// mapLinksToItems projects repository links to response items, overriding tags
// with concept display names where available (same best-effort behaviour as
// the list path). Shared by the search and url paths.
func (s *LinkReadService) mapLinksToItems(ctx context.Context, links []model.Link) []dto.LinkResponse {
	displayByLink := s.fetchDisplayNames(ctx, links)
	items := make([]dto.LinkResponse, 0, len(links))
	for _, link := range links {
		resp := linkToResponse(link)
		if names, ok := displayByLink[link.ID]; ok && len(names) > 0 {
			resp.Tags = names
		}
		items = append(items, resp)
	}
	return items
}

func (s *LinkReadService) mapLinkDetailsToItems(ctx context.Context, links []repository.LinkDetailProjection) []dto.LinkResponse {
	ids := make([]uuid.UUID, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.ID)
	}
	displayByLink := s.fetchDisplayNamesByIDs(ctx, ids)
	items := make([]dto.LinkResponse, 0, len(links))
	for _, link := range links {
		resp := linkDetailToResponse(link)
		if names, ok := displayByLink[link.ID]; ok && len(names) > 0 {
			resp.Tags = names
		}
		items = append(items, resp)
	}
	return items
}

// logSearchWarn emits the q= degradation WARN when a logger is wired (nil-safe
// no-op otherwise). SafeError prevents upstream payloads or credentials from
// entering logs while remaining nil-safe.
func (s *LinkReadService) logSearchWarn(msg string, err error) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Warn(msg, "err", observability.SafeError(err))
}
