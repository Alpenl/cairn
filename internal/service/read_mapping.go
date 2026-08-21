package service

import (
	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
)

func linkDetailToResponse(link repository.LinkDetailProjection) dto.LinkResponse {
	tags := link.Tags
	if tags == nil {
		tags = []string{}
	}
	return dto.LinkResponse{
		ID:                  link.ID.String(),
		HasContent:          link.HasContent,
		ContentCJKChars:     link.ContentCJKChars,
		ContentWords:        link.ContentWords,
		URL:                 link.URL,
		Title:               link.Title,
		Summary:             link.Summary,
		Description:         link.Description,
		Tags:                tags,
		ContentType:         link.ContentType,
		LibraryKind:         libraryKindString(link.LibraryKind),
		ContentRevision:     link.ContentRevision,
		MetadataRevision:    link.MetadataRevision,
		Status:              string(link.Status),
		Domain:              link.Domain,
		PathDepth:           link.PathDepth,
		ParentID:            uuidStringPtr(link.ParentID),
		CreatedAt:           link.CreatedAt,
		UpdatedAt:           link.UpdatedAt,
		FetcherType:         link.FetcherType,
		IsLowConfidence:     link.IsLowConfidence,
		LowConfidenceReason: deriveLowConfidenceReasonFields(link.IsLowConfidence, link.LowConfidenceReason, link.ErrorMsg, link.FetcherType),
		ErrorCategory:       stringPtr(deriveLinkErrorCategoryFields(link.Status, link.ErrorMsg)),
		ErrorMsg:            link.ErrorMsg,
		ParentPath:          link.ParentPath,
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
		Tags:                tags,
		ContentType:         link.ContentType,
		LibraryKind:         libraryKindString(link.LibraryKind),
		ContentRevision:     link.ContentRevision,
		MetadataRevision:    link.MetadataRevision,
		Status:              string(link.Status),
		Domain:              link.Domain,
		PathDepth:           link.PathDepth,
		ParentID:            parentIDPtr(link),
		CreatedAt:           link.CreatedAt,
		UpdatedAt:           link.UpdatedAt,
		FetcherType:         link.FetcherType,
		IsLowConfidence:     link.IsLowConfidence,
		LowConfidenceReason: deriveLowConfidenceReason(link),
		ErrorCategory:       stringPtr(deriveLinkErrorCategory(link)),
		ErrorMsg:            link.ErrorMsg,
		ParentPath:          link.ParentPath,
	}
}

func libraryKindString(value *model.LibraryKind) *string {
	if value == nil {
		return nil
	}
	result := string(*value)
	return &result
}

func mapRepositoryTagCounts(rows []repository.TagCount) []dto.TagCountResponse {
	out := make([]dto.TagCountResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, dto.TagCountResponse{
			Tag:   row.Tag,
			Count: row.Count,
		})
	}
	return out
}

func mapScopedTagCounts(rows []repository.ScopedTagCount) []dto.TagCountResponse {
	out := make([]dto.TagCountResponse, 0, len(rows))
	for _, row := range rows {
		reading, site := row.ReadingCount, row.SiteCount
		out = append(out, dto.TagCountResponse{
			Tag:          row.Tag,
			Count:        row.Count,
			ReadingCount: &reading,
			SiteCount:    &site,
		})
	}
	return out
}
