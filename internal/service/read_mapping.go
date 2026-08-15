package service

import (
	"context"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/representation"
)

// fetchDisplayNames batch-queries concept display names for the
// supplied links. Returns an empty map (never nil) on any failure —
// the caller treats missing keys as "fall back to link.tags" so a
// hiccup on the concept layer never 500s the read API. The lookup is
// only called when conceptDisplay is non-nil and there is at least
// one link to inspect.
func (s *LinkReadService) fetchDisplayNames(ctx context.Context, links []model.Link) map[uuid.UUID][]string {
	ids := make([]uuid.UUID, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.ID)
	}
	return s.fetchDisplayNamesByIDs(ctx, ids)
}

func (s *LinkReadService) fetchDisplayNamesByIDs(ctx context.Context, ids []uuid.UUID) map[uuid.UUID][]string {
	if s == nil || len(ids) == 0 {
		return map[uuid.UUID][]string{}
	}
	if s.conceptDisplay == nil {
		representation.MarkResponseNonCacheable(ctx)
		return map[uuid.UUID][]string{}
	}
	out, err := s.conceptDisplay.ListDisplayNamesByLinkIDs(ctx, ids)
	if err != nil {
		// Best-effort: surface as an empty map so callers fall back to
		// the raw link.tags. A future observability pass should attach
		// the logger here so this failure is at least visible.
		representation.MarkResponseNonCacheable(ctx)
		return map[uuid.UUID][]string{}
	}
	return out
}

func linkDetailToResponse(link repository.LinkDetailProjection) dto.LinkResponse {
	tags := link.Tags
	if tags == nil {
		tags = []string{}
	}
	return dto.LinkResponse{
		ID:                        link.ID.String(),
		HasContent:                link.HasContent,
		ContentCJKChars:           link.ContentCJKChars,
		ContentWords:              link.ContentWords,
		URL:                       link.URL,
		Title:                     link.Title,
		Summary:                   link.Summary,
		Description:               link.Description,
		Tags:                      tags,
		ContentType:               link.ContentType,
		LibraryKind:               libraryKindString(link.LibraryKind),
		LibraryKindSource:         libraryKindSourceString(link.LibraryKindSource),
		LibraryKindLocked:         link.LibraryKindLocked,
		PredictedLibraryKind:      libraryKindString(link.PredictedLibraryKind),
		ClassificationConfidence:  link.ClassificationConfidence,
		ClassificationReason:      link.ClassificationReason,
		ClassificationExplanation: link.ClassificationExplanation,
		ClassifierVersion:         link.ClassifierVersion,
		ContentRevision:           link.ContentRevision,
		MetadataRevision:          link.MetadataRevision,
		Status:                    string(link.Status),
		Domain:                    link.Domain,
		PathDepth:                 link.PathDepth,
		ParentID:                  uuidStringPtr(link.ParentID),
		CreatedAt:                 link.CreatedAt,
		UpdatedAt:                 link.UpdatedAt,
		FetcherType:               link.FetcherType,
		IsLowConfidence:           link.IsLowConfidence,
		LowConfidenceReason:       deriveLowConfidenceReasonFields(link.IsLowConfidence, link.LowConfidenceReason, link.ErrorMsg, link.FetcherType),
		ErrorCategory:             stringPtr(deriveLinkErrorCategoryFields(link.Status, link.ErrorMsg)),
		ErrorMsg:                  link.ErrorMsg,
		ParentPath:                link.ParentPath,
	}
}

// linkToResponse 把仓储模型映射成对外的 LinkResponse；这是读侧统一出口，
// 任何派生字段（lowConfidenceReason / errorCategory）都在这里收口。
func linkToResponse(link model.Link) dto.LinkResponse {
	tags := link.Tags
	if tags == nil {
		tags = []string{}
	}

	return dto.LinkResponse{
		ID: link.ID.String(),
		// PF6：列表如实汇报「有没有已保存原文」与两项阅读计数。此前列表恒报
		// has_content=false，Reader 因此要专门打补丁把详情端的真值保住，
		// 否则折叠头会每 30 秒翻转成「保存原文」。
		HasContent:      link.HasContent,
		ContentCJKChars: link.ContentCJKChars,
		ContentWords:    link.ContentWords,
		URL:             link.URL,
		Title:           link.Title,
		Summary:         link.Summary,
		Description:     link.Description,
		// Keep the wire contract stable for pending and failed rows, whose
		// analysis has not populated tags yet. A nil Go slice encodes as null,
		// while both OpenAPI and first-party clients require an array.
		Tags:                      tags,
		ContentType:               link.ContentType,
		LibraryKind:               libraryKindString(link.LibraryKind),
		LibraryKindSource:         libraryKindSourceString(link.LibraryKindSource),
		LibraryKindLocked:         link.LibraryKindLocked,
		PredictedLibraryKind:      libraryKindString(link.PredictedLibraryKind),
		ClassificationConfidence:  link.ClassificationConfidence,
		ClassificationReason:      link.ClassificationReason,
		ClassificationExplanation: link.ClassificationExplanation,
		ClassifierVersion:         link.ClassifierVersion,
		ContentRevision:           link.ContentRevision,
		MetadataRevision:          link.MetadataRevision,
		Status:                    string(link.Status),
		Domain:                    link.Domain,
		PathDepth:                 link.PathDepth,
		ParentID:                  parentIDPtr(link),
		CreatedAt:                 link.CreatedAt,
		UpdatedAt:                 link.UpdatedAt,
		FetcherType:               link.FetcherType,
		IsLowConfidence:           link.IsLowConfidence,
		LowConfidenceReason:       deriveLowConfidenceReason(link),
		ErrorCategory:             stringPtr(deriveLinkErrorCategory(link)),
		ErrorMsg:                  link.ErrorMsg,
		ParentPath:                link.ParentPath,
	}
}

func libraryKindString(value *model.LibraryKind) *string {
	if value == nil {
		return nil
	}
	result := string(*value)
	return &result
}

func libraryKindSourceString(value *model.LibraryKindSource) *string {
	if value == nil {
		return nil
	}
	result := string(*value)
	return &result
}

// mapRepositoryTagCounts 把仓储层的 TagCount 直通转换成 DTO 层的标签计数响应。
func mapRepositoryTagCounts(rows []repository.TagCount) []dto.TagCountResponse {
	return mapTagResponses(mapServiceTagCounts(rows))
}

// mapServiceTagCounts 把仓储类型转换为 service 内部的 TagCount，让缓存契约
// 不外泄仓储依赖。
func mapServiceTagCounts(rows []repository.TagCount) []TagCount {
	out := make([]TagCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, TagCount{
			Tag:   row.Tag,
			Count: row.Count,
		})
	}
	return out
}

// mapScopedTagCounts 把带 scope 的仓储行转换为 service 内部的 TagCount，
// 让缓存契约不外泄仓储依赖（与 mapServiceTagCounts 同口径）。
func mapScopedTagCounts(rows []repository.ScopedTagCount) []TagCount {
	out := make([]TagCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, TagCount{
			Tag:          row.Tag,
			Count:        row.Count,
			ReadingCount: row.ReadingCount,
			SiteCount:    row.SiteCount,
		})
	}
	return out
}

// mapTagResponses 把 service.TagCount 投影为对外的 DTO 响应。
func mapTagResponses(rows []TagCount) []dto.TagCountResponse {
	out := make([]dto.TagCountResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, dto.TagCountResponse{
			Tag:   row.Tag,
			Count: row.Count,
		})
	}
	return out
}
