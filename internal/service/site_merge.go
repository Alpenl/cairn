package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
)

// SiteMergeService starts with a pure preview phase. The mutating phase is
// intentionally separate because it needs all preview conflicts resolved and
// must lock every site in one repository transaction.
type SiteMergeService struct {
	sites siteMergeStore
}

type siteMergeStore interface {
	siteDetailReader
	ExecuteSiteMerge(context.Context, repository.ExecuteSiteMergeParams) (repository.SiteMergeResult, error)
}

func NewSiteMergeService(sites siteMergeStore) *SiteMergeService {
	return &SiteMergeService{sites: sites}
}

func (s *SiteMergeService) Preview(ctx context.Context, request dto.SiteMergePreviewRequest) (dto.SiteMergePreviewResponse, error) {
	target, details, err := s.loadMergeDetails(ctx, request.TargetSiteID, request.TargetRevision, request.Sources)
	if err != nil {
		return dto.SiteMergePreviewResponse{}, err
	}
	return siteMergePreview(target, details), nil
}

func (s *SiteMergeService) Execute(ctx context.Context, request dto.SiteMergeExecuteRequest) (dto.SiteMergeExecuteResponse, error) {
	target, details, err := s.loadMergeDetails(ctx, request.TargetSiteID, request.TargetRevision, request.Sources)
	if err != nil {
		return dto.SiteMergeExecuteResponse{}, err
	}
	overrides, err := resolveSiteMergeConflicts(siteMergePreview(target, details).FieldConflicts, request.Resolutions)
	if err != nil {
		return dto.SiteMergeExecuteResponse{}, err
	}
	sources := make([]repository.SiteMergeSource, 0, len(request.Sources))
	for _, source := range request.Sources {
		id, _ := uuid.Parse(source.SiteID)
		sources = append(sources, repository.SiteMergeSource{ID: id, Revision: source.Revision})
	}
	result, err := s.sites.ExecuteSiteMerge(ctx, repository.ExecuteSiteMergeParams{TargetID: target.ID, TargetRevision: target.Revision, Sources: sources, Name: overrides.name, Intro: overrides.intro, HomepageURL: overrides.homepage, IconURL: overrides.icon, UserNote: overrides.note})
	if errors.Is(err, repository.ErrRevisionConflict) {
		return dto.SiteMergeExecuteResponse{}, siteMergeConflict("site was changed")
	}
	if err != nil {
		return dto.SiteMergeExecuteResponse{}, err
	}
	return dto.SiteMergeExecuteResponse{SiteID: result.SiteID.String(), Revision: result.Revision, MovedEntries: result.MovedEntries, DeletedLinks: result.DeletedLinks}, nil
}

func (s *SiteMergeService) loadMergeDetails(ctx context.Context, rawTarget string, targetRevision int64, sources []dto.SiteRevisionRef) (*repository.SiteDetail, []*repository.SiteDetail, error) {
	targetID, err := uuid.Parse(rawTarget)
	if err != nil || targetRevision < 1 {
		return nil, nil, invalidSiteMerge("invalid target site")
	}
	seen := map[uuid.UUID]struct{}{targetID: {}}
	target, err := s.sites.GetSite(ctx, targetID)
	if err != nil {
		return nil, nil, err
	}
	if target == nil {
		return nil, nil, httperr.NewWithCode(404, httperr.CodeSiteNotFound, "target site not found")
	}
	if target.Revision != targetRevision {
		return nil, nil, siteMergeConflict("target site was changed")
	}
	details := []*repository.SiteDetail{target}
	for _, source := range sources {
		id, parseErr := uuid.Parse(source.SiteID)
		if parseErr != nil || source.Revision < 1 || id == targetID {
			return nil, nil, invalidSiteMerge("invalid merge source")
		}
		if _, exists := seen[id]; exists {
			return nil, nil, invalidSiteMerge("duplicate merge source")
		}
		seen[id] = struct{}{}
		detail, getErr := s.sites.GetSite(ctx, id)
		if getErr != nil {
			return nil, nil, getErr
		}
		if detail == nil {
			return nil, nil, httperr.NewWithCode(404, httperr.CodeSiteNotFound, "source site not found")
		}
		if detail.Revision != source.Revision {
			return nil, nil, siteMergeConflict("source site was changed")
		}
		details = append(details, detail)
	}
	return target, details, nil
}

func siteMergePreview(target *repository.SiteDetail, details []*repository.SiteDetail) dto.SiteMergePreviewResponse {
	out := dto.SiteMergePreviewResponse{TargetSiteID: target.ID.String(), TargetRevision: target.Revision, Entries: []dto.SiteMergePreviewEntryResponse{}, Tags: []string{}, IdentityKeys: []string{}, FieldConflicts: []dto.SiteMergeFieldConflictResponse{}}
	urls := map[string]struct{}{}
	tags := map[string]string{}
	for index, detail := range details {
		for _, entry := range detail.Entries {
			_, duplicate := urls[entry.NormalizedURL]
			urls[entry.NormalizedURL] = struct{}{}
			out.Entries = append(out.Entries, dto.SiteMergePreviewEntryResponse{ID: entry.ID.String(), SiteID: detail.ID.String(), LinkID: entry.LinkID.String(), Name: entry.EntryName, URL: entry.NormalizedURL, Duplicate: duplicate})
		}
		for _, tag := range detail.Tags {
			tags[tag.NormalizedTag] = tag.Tag
		}
		for _, identity := range detail.Identities {
			out.IdentityKeys = append(out.IdentityKeys, identity.IdentityKey)
		}
		if index > 0 {
			appendSiteMergeConflicts(&out, target, detail)
		}
	}
	for _, tag := range tags {
		out.Tags = append(out.Tags, tag)
	}
	sort.Strings(out.Tags)
	sort.Strings(out.IdentityKeys)
	out.RequiresResolution = len(out.FieldConflicts) > 0
	return out
}

func appendSiteMergeConflicts(out *dto.SiteMergePreviewResponse, target, source *repository.SiteDetail) {
	compare := func(field, targetValue, sourceValue string) {
		if strings.TrimSpace(targetValue) != "" && strings.TrimSpace(sourceValue) != "" && targetValue != sourceValue {
			out.FieldConflicts = append(out.FieldConflicts, dto.SiteMergeFieldConflictResponse{Field: field, TargetValue: targetValue, SourceSiteID: source.ID.String(), SourceValue: sourceValue})
		}
	}
	compare("name", target.Name, source.Name)
	compare("intro", target.Intro, source.Intro)
	compare("homepage_url", stringValue(target.HomepageURL), stringValue(source.HomepageURL))
	compare("icon_url", stringValue(target.IconURL), stringValue(source.IconURL))
	compare("user_note", target.UserNote, source.UserNote)
}

type siteMergeOverrides struct{ name, intro, homepage, icon, note *string }

func resolveSiteMergeConflicts(conflicts []dto.SiteMergeFieldConflictResponse, resolutions []dto.SiteMergeFieldResolutionRequest) (siteMergeOverrides, error) { //nolint:gocyclo // 按字段逐个裁决合并冲突，分支数等于可合并字段数
	if len(conflicts) == 0 && len(resolutions) != 0 {
		return siteMergeOverrides{}, invalidSiteMerge("merge has no conflicts to resolve")
	}
	byKey := map[string]dto.SiteMergeFieldResolutionRequest{}
	for _, resolution := range resolutions {
		key := resolution.Field + ":" + resolution.SourceSiteID
		if _, duplicate := byKey[key]; duplicate {
			return siteMergeOverrides{}, invalidSiteMerge("duplicate merge resolution")
		}
		byKey[key] = resolution
	}
	overrides := siteMergeOverrides{}
	selectedSource := map[string]string{}
	for _, conflict := range conflicts {
		key := conflict.Field + ":" + conflict.SourceSiteID
		resolution, ok := byKey[key]
		if !ok {
			return siteMergeOverrides{}, siteMergeConflict("every field conflict requires a resolution")
		}
		if resolution.Choice == "target" {
			if resolution.SourceSiteID != conflict.SourceSiteID {
				return siteMergeOverrides{}, invalidSiteMerge("target resolution does not match conflict")
			}
			continue
		}
		if resolution.Choice != "source" || resolution.SourceSiteID != conflict.SourceSiteID {
			return siteMergeOverrides{}, invalidSiteMerge("source resolution does not match conflict")
		}
		if prior, exists := selectedSource[conflict.Field]; exists && prior != conflict.SourceSiteID {
			return siteMergeOverrides{}, invalidSiteMerge("only one source value may win each field")
		}
		selectedSource[conflict.Field] = conflict.SourceSiteID
		value := conflict.SourceValue
		switch conflict.Field {
		case "name":
			overrides.name = &value
		case "intro":
			overrides.intro = &value
		case "homepage_url":
			overrides.homepage = &value
		case "icon_url":
			overrides.icon = &value
		case "user_note":
			overrides.note = &value
		}
	}
	if len(byKey) != len(conflicts) {
		return siteMergeOverrides{}, invalidSiteMerge("resolution does not match a reported conflict")
	}
	return overrides, nil
}

func invalidSiteMerge(message string) error {
	return httperr.NewWithCode(422, "invalid_site_merge", message)
}
func siteMergeConflict(message string) error {
	return httperr.NewWithCode(409, "site_merge_conflict", message)
}
