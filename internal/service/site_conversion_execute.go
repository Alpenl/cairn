package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// ConversionExecuteService validates the preview token and delegates every
// state change to one repository transaction. It does not issue an AI call.
type ConversionExecuteService struct {
	links    repository.LinkLifecycleReader
	commands LinkConversionCommands
}

func NewConversionExecuteService(links repository.LinkLifecycleReader, commands LinkConversionCommands) *ConversionExecuteService {
	if links == nil || commands == nil {
		panic("service.NewConversionExecuteService: links and commands are required")
	}
	return &ConversionExecuteService{
		links: links, commands: commands,
	}
}

func (s *ConversionExecuteService) Execute(ctx context.Context, rawLinkID string, request dto.ConversionExecuteRequest) (dto.ConversionExecuteResponse, error) { //nolint:gocyclo // 转换编排：校验、落库、入队、指标各有失败分支
	id, err := uuid.Parse(strings.TrimSpace(rawLinkID))
	if err != nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Malformed, problem.CodeInvalidLinkID, "invalid link id")
	}
	link, err := s.links.GetLifecycleByID(ctx, id)
	if err != nil {
		return dto.ConversionExecuteResponse{}, err
	}
	if link == nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "link not found")
	}
	if link.Status != model.LinkStatusDone || link.LibraryKind == nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeLibraryKindNotFinal, "library kind is not final")
	}
	target := model.LibraryKind(strings.TrimSpace(request.TargetKind))
	if target != model.LibraryKindReading && target != model.LibraryKindSite {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeInvalidRequestedLibraryKind, "target_kind must be reading or site")
	}
	if target == *link.LibraryKind {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeConversionTargetUnchanged, "conversion target is unchanged")
	}
	if request.ExpectedContentRevision != link.ContentRevision {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeRevisionConflict, "content revision has changed")
	}
	if target == model.LibraryKindSite && !request.ConfirmDestructive {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeDestructiveConfirmationRequired, "destructive conversion requires confirmation")
	}
	var targetSiteID *uuid.UUID
	if request.TargetSiteID != nil && strings.TrimSpace(*request.TargetSiteID) != "" {
		parsed, parseErr := uuid.Parse(strings.TrimSpace(*request.TargetSiteID))
		if parseErr != nil {
			return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Malformed, problem.CodeInvalidSiteID, "invalid target site id")
		}
		targetSiteID = &parsed
	}
	if target == model.LibraryKindReading && targetSiteID != nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeInvalidRequestedLibraryKind, "target_site_id is only valid for site conversion")
	}
	if target == model.LibraryKindReading && request.ExpectedSiteRevision == nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeRevisionConflict, "source site revision is required")
	}
	if targetSiteID != nil && request.ExpectedSiteRevision == nil {
		return dto.ConversionExecuteResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeRevisionConflict, "target site revision is required")
	}

	result, err := s.commands.ConvertLink(ctx, ConvertLinkCommand{
		LinkID: id, TargetKind: target, ExpectedContentRevision: request.ExpectedContentRevision,
		TargetSiteID: targetSiteID, ExpectedSiteRevision: request.ExpectedSiteRevision,
		PreservedUserNote: trimOptional(request.PreservedUserNote),
	})
	if err != nil {
		return dto.ConversionExecuteResponse{}, mapConversionError(err)
	}
	return conversionResultDTO(result), nil
}

func trimOptional(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func mapConversionError(err error) error {
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrSiteEntryNotFound) {
		return problem.NewWithCode(problem.NotFound, problem.CodeLinkNotFound, "link not found")
	}
	if errors.Is(err, repository.ErrRevisionConflict) {
		return problem.NewWithCode(problem.Conflict, problem.CodeRevisionConflict, "revision has changed")
	}
	return err
}

func conversionResultDTO(result ConvertLinkResult) dto.ConversionExecuteResponse {
	out := dto.ConversionExecuteResponse{LinkID: result.LinkID.String(), LibraryKind: string(result.Kind), ContentRevision: result.ContentRevision, Status: string(result.Status), ReparseRequired: result.Kind == model.LibraryKindReading}
	if result.Kind == model.LibraryKindSite {
		out.ReaderTarget = dto.ReaderTarget{View: "sites", LinkID: result.LinkID.String()}
		if result.SiteID != nil {
			out.SiteID, out.ReaderTarget.SiteID = result.SiteID.String(), result.SiteID.String()
		}
		if result.EntryID != nil {
			out.EntryID, out.ReaderTarget.EntryID = result.EntryID.String(), result.EntryID.String()
		}
		if result.SiteRevision != nil {
			out.SiteRevision = *result.SiteRevision
		}
	} else {
		out.ReaderTarget = dto.ReaderTarget{View: "processing", LinkID: result.LinkID.String()}
	}
	return out
}
