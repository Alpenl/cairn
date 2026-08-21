package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
	"webtag/internal/siteidentity"
)

// conversionContentReader is kept deliberately small so the preview can be
// tested without bringing the content-write surface into its dependency set.
type conversionContentReader interface {
	GetContent(context.Context, uuid.UUID) (*model.SavedContent, error)
}

// ConversionPreviewService describes a conversion without performing an AI
// call or changing the link. Execute will use the returned revision as CAS.
type ConversionPreviewService struct {
	links        repository.LinkLifecycleReader
	content      conversionContentReader
	translations repository.TranslationStore
	sites        repository.SiteIdentityLookup
}

func NewConversionPreviewService(links repository.LinkLifecycleReader, content conversionContentReader, translations repository.TranslationStore, sites repository.SiteIdentityLookup) *ConversionPreviewService {
	if links == nil || content == nil || translations == nil || sites == nil {
		panic("service.NewConversionPreviewService: links, content, translations, and sites are required")
	}
	return &ConversionPreviewService{links: links, content: content, translations: translations, sites: sites}
}

func (s *ConversionPreviewService) Preview(ctx context.Context, rawLinkID string, request dto.ConversionPreviewRequest) (dto.ConversionPreviewResponse, error) { //nolint:gocyclo // 预览需枚举转换的每种前置条件与警告，分支即用户可见的提示项
	id, err := uuid.Parse(strings.TrimSpace(rawLinkID))
	if err != nil {
		return dto.ConversionPreviewResponse{}, problem.NewWithCode(problem.Malformed, problem.CodeInvalidLinkID, "invalid link id")
	}

	link, err := s.links.GetLifecycleByID(ctx, id)
	if err != nil {
		return dto.ConversionPreviewResponse{}, err
	}
	if link == nil {
		return dto.ConversionPreviewResponse{}, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "link not found")
	}
	if link.Status != model.LinkStatusDone || link.LibraryKind == nil {
		return dto.ConversionPreviewResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeLibraryKindNotFinal, "library kind is not final")
	}

	target := model.LibraryKind(strings.TrimSpace(request.TargetKind))
	if target != model.LibraryKindReading && target != model.LibraryKindSite {
		return dto.ConversionPreviewResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeInvalidRequestedLibraryKind, "target_kind must be reading or site")
	}
	if target == *link.LibraryKind {
		return dto.ConversionPreviewResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeConversionTargetUnchanged, "conversion target is unchanged")
	}

	response := dto.ConversionPreviewResponse{
		LinkID:                  link.ID.String(),
		CurrentKind:             string(*link.LibraryKind),
		TargetKind:              string(target),
		ExpectedContentRevision: link.ContentRevision,
		ReparseRequired:         target == model.LibraryKindReading,
	}
	if target != model.LibraryKindSite {
		return response, nil
	}

	response.Destructive = true
	response.AnnotationPolicy = "extract_local_note_then_hide_stale"
	identity, identityErr := siteidentity.FromURL(link.URL)
	if identityErr != nil {
		return dto.ConversionPreviewResponse{}, identityErr
	}
	candidate, candidateErr := s.sites.FindByIdentityKey(ctx, identity.Key)
	if candidateErr != nil {
		return dto.ConversionPreviewResponse{}, candidateErr
	}
	if candidate != nil {
		response.MatchingSite = &dto.ConversionSiteCandidateResponse{ID: candidate.ID.String(), Name: candidate.Name, Revision: candidate.Revision}
	}
	content, err := s.content.GetContent(ctx, id)
	if err != nil {
		return dto.ConversionPreviewResponse{}, err
	}
	response.SavedOriginal = content != nil
	translations, err := s.translations.ListByLink(ctx, id)
	if err != nil {
		return dto.ConversionPreviewResponse{}, err
	}
	response.TranslationCount = len(translations)
	return response, nil
}
