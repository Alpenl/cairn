package service

import (
	"context"
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// List 处理 /api/links 列表：支持页码或游标两种分页（互斥）、可选的标签 /
// 内容类型 / 域名过滤。
//
//nolint:gocyclo // 请求参数归一化与多过滤维度组合共享同一组局部状态。
func (s *LinkReadService) List(ctx context.Context, req dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	createdFrom, createdBefore, err := validateCreatedRange(req.CreatedFrom, req.CreatedBefore)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}

	// url= (exact existence check) beats q= (keyword
	// search), which beats the normal list path. Both ignore pagination. A
	// blank q/url falls through to the historical list path (back-compat).
	if strings.TrimSpace(req.URL) != "" {
		return s.findByURL(ctx, req)
	}
	if strings.TrimSpace(req.Query) != "" {
		return s.searchLinks(ctx, req)
	}

	page := req.Page
	if page < 1 {
		page = 1
	}

	limit := req.Limit
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	tags, err := splitAndValidateTags(req.Tags)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	if err := validateContentTypeFilter(req.ContentType); err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	libraryKind, err := validateLibraryKindFilter(req.LibraryKind)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	if err := validateDomainFilter(req.Domain); err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	lowConfidence, err := validateLowConfidenceFilter(req.LowConfidence)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	statuses, err := splitAndValidateStatuses(req.Status)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}
	// Legacy default: a plain GET /api/links remains an article library view.
	// Explicit status queries are operational views and intentionally retain
	// every kind, including unresolved automatic captures.
	if strings.TrimSpace(req.Status) == "" {
		statuses = []string{string(model.LinkStatusDone)}
		if libraryKind == nil {
			kind := model.LibraryKindReading
			libraryKind = &kind
		}
	}

	cursorMode := req.Cursor
	if cursorMode && req.Page > 1 {
		// invalid_cursor slug 同时覆盖"游标与 page 冲突"和"游标 token 无法解码"
		// 两条 422 路径——客户端按 error_code 分支就能命中"换成另一种分页方式"
		// 的统一处理逻辑。
		return dto.PaginatedLinksResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidCursor, "cursor pagination cannot be combined with a page parameter")
	}

	filter := repository.ListLinksFilter{
		Tags:          tags,
		Domain:        stringPtr(req.Domain),
		ContentType:   stringPtr(req.ContentType),
		LibraryKind:   libraryKind,
		CreatedFrom:   createdFrom,
		CreatedBefore: createdBefore,
		LowConfidence: lowConfidence,
		Statuses:      statuses,
		Limit:         limit,
	}
	if cursorMode {
		filter.Cursor = true
		// Empty After plus Cursor=true means "start of stream" — leave
		// filter.After nil so the repository uses the cursor SQL with
		// no tuple comparison.
		if strings.TrimSpace(req.After) != "" {
			cursor, err := s.decodeListCursor(req.After)
			if err != nil {
				return dto.PaginatedLinksResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidCursor, "invalid after cursor")
			}
			filter.After = cursor
		}
	} else {
		filter.Offset = (page - 1) * limit
	}

	links, total, err := s.links.ListDone(ctx, filter)
	if err != nil {
		return dto.PaginatedLinksResponse{}, err
	}

	items := make([]dto.LinkResponse, 0, len(links))
	for _, link := range links {
		items = append(items, linkToResponse(link))
	}

	resp := dto.PaginatedLinksResponse{
		Items: items,
		Limit: limit,
	}
	if cursorMode {
		// Cursor exhaustion: a partial page means there are no more
		// rows. A full page may or may not be the last — emit
		// next_cursor and let the client see an empty response on the
		// follow-up call to confirm the end. This avoids a second
		// COUNT-style query just to set NextCursor=nil one page
		// earlier.
		if len(links) == limit {
			last := links[len(links)-1]
			token := s.encodeListCursor(last.CreatedAt, last.ID)
			resp.NextCursor = &token
		}
	} else {
		resp.Total = total
		resp.Page = page
	}
	return resp, nil
}
