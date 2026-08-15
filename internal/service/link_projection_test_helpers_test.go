package service

import (
	"webtag/internal/model"
	"webtag/internal/repository"
)

func parseInputForTest(link *model.Link) *repository.LinkParseInput {
	if link == nil {
		return nil
	}
	projection := contentParseInput(link)
	return &projection
}

func detailForTest(link *model.Link) *repository.LinkDetailProjection {
	if link == nil {
		return nil
	}
	return &repository.LinkDetailProjection{
		ID: link.ID, URL: link.URL, Title: link.Title, Summary: link.Summary, Tags: link.Tags,
		FetcherType: link.FetcherType, IsLowConfidence: link.IsLowConfidence,
		LowConfidenceReason: link.LowConfidenceReason, Status: link.Status, ErrorMsg: link.ErrorMsg,
		Description: link.Description, Domain: link.Domain, ContentType: link.ContentType,
		LibraryKind: link.LibraryKind, LibraryKindSource: link.LibraryKindSource,
		LibraryKindLocked: link.LibraryKindLocked, PredictedLibraryKind: link.PredictedLibraryKind,
		ClassificationConfidence:  link.ClassificationConfidence,
		ClassificationReason:      link.ClassificationReason,
		ClassificationExplanation: link.ClassificationExplanation,
		ClassifierVersion:         link.ClassifierVersion, ContentRevision: link.ContentRevision, MetadataRevision: link.MetadataRevision,
		ContentSource: link.ContentSource, HasContent: link.HasContent,
		ContentCJKChars: link.ContentCJKChars, ContentWords: link.ContentWords,
		PathDepth: link.PathDepth, ParentPath: link.ParentPath, ParentID: link.ParentID,
		CreatedAt: link.CreatedAt, UpdatedAt: link.UpdatedAt,
	}
}

func lifecycleForTest(link *model.Link) *repository.LinkLifecycleProjection {
	if link == nil {
		return nil
	}
	return &repository.LinkLifecycleProjection{
		ID: link.ID, URL: link.URL, Status: link.Status, LibraryKind: link.LibraryKind,
		LibraryKindSource: link.LibraryKindSource, LibraryKindLocked: link.LibraryKindLocked,
		ClassificationReason: link.ClassificationReason, ContentRevision: link.ContentRevision,
		HasContent: link.HasContent,
	}
}
