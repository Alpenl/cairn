package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

// ConversionExecuteService validates the preview token and delegates every
// state change to one repository transaction. It does not issue an AI call.
type ConversionExecuteService struct {
	links                   repository.LinkLifecycleReader
	commands                LinkConversionCommands
	metrics                 *observability.Metrics
	disableSiteLibraryWrite bool
}

func NewConversionExecuteServiceWithMetrics(links repository.LinkLifecycleReader, commands LinkConversionCommands, metrics *observability.Metrics) *ConversionExecuteService {
	return NewConversionExecuteServiceWithOptions(ConversionExecuteServiceOptions{Links: links, Commands: commands, Metrics: metrics})
}

func NewConversionExecuteService(links repository.LinkLifecycleReader, commands LinkConversionCommands) *ConversionExecuteService {
	return NewConversionExecuteServiceWithOptions(ConversionExecuteServiceOptions{Links: links, Commands: commands})
}

// ConversionExecuteServiceOptions keeps the rollout gate at the mutation
// boundary. Existing sites may still convert back to reading while site writes
// are disabled, but no conversion may create a new Site or SiteEntry.
type ConversionExecuteServiceOptions struct {
	Links                   repository.LinkLifecycleReader
	Commands                LinkConversionCommands
	Metrics                 *observability.Metrics
	DisableSiteLibraryWrite bool
}

func NewConversionExecuteServiceWithOptions(options ConversionExecuteServiceOptions) *ConversionExecuteService {
	return &ConversionExecuteService{
		links: options.Links, commands: options.Commands, metrics: options.Metrics,
		disableSiteLibraryWrite: options.DisableSiteLibraryWrite,
	}
}

func (s *ConversionExecuteService) Execute(ctx context.Context, rawLinkID string, request dto.ConversionExecuteRequest) (dto.ConversionExecuteResponse, error) { //nolint:gocyclo // 转换编排：校验、落库、入队、指标各有失败分支
	id, err := uuid.Parse(strings.TrimSpace(rawLinkID))
	if err != nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidLinkID, "invalid link id")
	}
	link, err := s.links.GetLifecycleByID(ctx, id)
	if err != nil {
		return dto.ConversionExecuteResponse{}, err
	}
	if link == nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	}
	if link.Status != model.LinkStatusDone || link.LibraryKind == nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeLibraryKindNotFinal, "library kind is not final")
	}
	target := model.LibraryKind(strings.TrimSpace(request.TargetKind))
	if target != model.LibraryKindReading && target != model.LibraryKindSite {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "target_kind must be reading or site")
	}
	if target == model.LibraryKindSite {
		if err := requireSiteLibraryWrite(model.RequestedLibraryKindSite, s.disableSiteLibraryWrite); err != nil {
			return dto.ConversionExecuteResponse{}, err
		}
	}
	if target == *link.LibraryKind {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeConversionTargetUnchanged, "conversion target is unchanged")
	}
	if request.ExpectedContentRevision != link.ContentRevision {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "content revision has changed")
	}
	if target == model.LibraryKindSite && !request.ConfirmDestructive {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeDestructiveConfirmationRequired, "destructive conversion requires confirmation")
	}
	var targetSiteID *uuid.UUID
	if request.TargetSiteID != nil && strings.TrimSpace(*request.TargetSiteID) != "" {
		parsed, parseErr := uuid.Parse(strings.TrimSpace(*request.TargetSiteID))
		if parseErr != nil {
			return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteID, "invalid target site id")
		}
		targetSiteID = &parsed
	}
	if target == model.LibraryKindReading && targetSiteID != nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRequestedLibraryKind, "target_site_id is only valid for site conversion")
	}
	if target == model.LibraryKindReading && request.ExpectedSiteRevision == nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "source site revision is required")
	}
	if targetSiteID != nil && request.ExpectedSiteRevision == nil {
		return dto.ConversionExecuteResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "target site revision is required")
	}

	if s.commands == nil {
		return dto.ConversionExecuteResponse{}, errors.New("convert link: durable commands are not configured")
	}
	result, err := s.commands.ConvertLink(ctx, ConvertLinkCommand{
		LinkID: id, TargetKind: target, ExpectedContentRevision: request.ExpectedContentRevision,
		TargetSiteID: targetSiteID, ExpectedSiteRevision: request.ExpectedSiteRevision,
		PreservedUserNote: trimOptional(request.PreservedUserNote),
	})
	if err != nil {
		s.recordConversion(*link.LibraryKind, target, conversionMetricResult(err))
		return dto.ConversionExecuteResponse{}, mapConversionError(err)
	}
	s.recordConversion(*link.LibraryKind, target, "success")
	s.recordClassificationCorrection(link, target)
	return conversionResultDTO(result), nil
}

func (s *ConversionExecuteService) recordConversion(from, to model.LibraryKind, result string) {
	if s != nil && s.metrics != nil && s.metrics.SiteConversionTotal != nil {
		s.metrics.SiteConversionTotal.WithLabelValues(string(from), string(to), result).Inc()
	}
}

// recordClassificationCorrection intentionally uses the pre-conversion Link:
// ConvertLink changes the source to user, while the quality signal is whether a
// user corrected the original automatic decision. Classification reason stays
// out of labels; only the fixed personal-rule predicate is exposed.
func (s *ConversionExecuteService) recordClassificationCorrection(link *repository.LinkLifecycleProjection, to model.LibraryKind) {
	if s == nil || s.metrics == nil || s.metrics.LibraryClassificationCorrectionTotal == nil || link == nil || link.LibraryKind == nil || link.LibraryKindSource == nil || *link.LibraryKindSource != model.LibraryKindSourceAuto {
		return
	}
	hadRule := "false"
	if link.ClassificationReason != nil && strings.HasPrefix(*link.ClassificationReason, "personal_rule_") {
		hadRule = "true"
	}
	s.metrics.LibraryClassificationCorrectionTotal.WithLabelValues(string(*link.LibraryKind), string(to), hadRule).Inc()
}

func conversionMetricResult(err error) string {
	if errors.Is(err, repository.ErrRevisionConflict) {
		return "conflict"
	}
	return "error"
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
		return httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	}
	if errors.Is(err, repository.ErrRevisionConflict) {
		return httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "revision has changed")
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
		if result.ParseJobID != nil {
			out.ParseJobID = result.ParseJobID.String()
		}
	}
	return out
}
