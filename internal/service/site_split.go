package service

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
	"webtag/internal/siteidentity"
)

// SiteSplitService keeps selection validation in the service and leaves the
// multi-table move to one repository transaction.
type SiteSplitService struct {
	sites siteSplitStore
}

type siteSplitStore interface {
	siteDetailReader
	ExecuteSiteSplit(context.Context, repository.ExecuteSiteSplitParams) (repository.SiteSplitResult, error)
}

func NewSiteSplitService(sites siteSplitStore) *SiteSplitService {
	return &SiteSplitService{sites: sites}
}

func (s *SiteSplitService) Preview(ctx context.Context, rawSiteID string, request dto.SiteSplitRequest) (dto.SiteSplitPreviewResponse, error) {
	site, entries, payload, eligible, err := s.prepare(ctx, rawSiteID, request)
	if err != nil {
		return dto.SiteSplitPreviewResponse{}, err
	}
	return splitPreview(site, entries, payload, eligible), nil
}

func (s *SiteSplitService) Execute(ctx context.Context, rawSiteID string, request dto.SiteSplitRequest) (dto.SiteSplitExecuteResponse, error) {
	site, entries, payload, _, err := s.prepare(ctx, rawSiteID, request)
	if err != nil {
		return dto.SiteSplitExecuteResponse{}, err
	}
	primary := uuid.MustParse(payload.PrimaryEntryID)
	var identity *string
	if len(payload.IdentityKeysForNewSite) == 1 {
		identity = &payload.IdentityKeysForNewSite[0]
	}
	ids := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	result, err := s.sites.ExecuteSiteSplit(ctx, repository.ExecuteSiteSplitParams{SourceID: site.ID, SourceRevision: site.Revision, EntryIDs: ids, Name: payload.Name, Intro: payload.Intro, HomepageURL: payload.HomepageURL, IconURL: payload.IconURL, UserNote: payload.UserNote, PrimaryEntryID: primary, IdentityKeyForNewSite: identity})
	if errors.Is(err, repository.ErrRevisionConflict) {
		return dto.SiteSplitExecuteResponse{}, siteMergeConflict("site was changed")
	}
	if err != nil {
		return dto.SiteSplitExecuteResponse{}, err
	}
	return dto.SiteSplitExecuteResponse{SourceSiteID: result.SourceID.String(), SourceRevision: result.SourceRevision, NewSiteID: result.NewSiteID.String(), NewSiteRevision: result.NewRevision, MovedEntries: result.MovedEntries}, nil
}

func (s *SiteSplitService) prepare(ctx context.Context, rawSiteID string, request dto.SiteSplitRequest) (*repository.SiteDetail, []model.SiteEntry, dto.SiteSplitRequest, map[string]struct{}, error) {
	if len(request.IdentityKeysForNewSite) > 1 {
		return nil, nil, dto.SiteSplitRequest{}, nil, invalidSiteSplit("at most one identity may move to the new site")
	}
	site, entries, err := s.selectedSplitEntries(ctx, rawSiteID, request.ExpectedRevision, request.EntryIDs)
	if err != nil {
		return nil, nil, dto.SiteSplitRequest{}, nil, err
	}
	payload, err := normalizeSiteSplitRequest(request, entries)
	if err != nil {
		return nil, nil, dto.SiteSplitRequest{}, nil, err
	}
	eligible := eligibleSplitIdentities(site, entries)
	if len(payload.IdentityKeysForNewSite) == 1 {
		if _, ok := eligible[payload.IdentityKeysForNewSite[0]]; !ok {
			return nil, nil, dto.SiteSplitRequest{}, nil, invalidSiteSplit("identity is not eligible for the moved entries")
		}
	}
	return site, entries, payload, eligible, nil
}

func normalizeSiteSplitRequest(request dto.SiteSplitRequest, entries []model.SiteEntry) (dto.SiteSplitRequest, error) {
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 256 {
		return dto.SiteSplitRequest{}, invalidSiteSplit("new site name is required")
	}
	trim := func(value *string, maximum int, field string) (*string, error) {
		if value == nil {
			return nil, nil
		}
		clean := strings.TrimSpace(*value)
		if len(clean) > maximum {
			return nil, invalidSiteSplit("invalid new site " + field)
		}
		return &clean, nil
	}
	var err error
	if request.Intro, err = trim(request.Intro, 1000, "intro"); err != nil {
		return dto.SiteSplitRequest{}, err
	}
	if request.UserNote, err = trim(request.UserNote, 10000, "user_note"); err != nil {
		return dto.SiteSplitRequest{}, err
	}
	if request.HomepageURL, err = normalizeSplitURL(request.HomepageURL, "homepage_url"); err != nil {
		return dto.SiteSplitRequest{}, err
	}
	if request.IconURL, err = normalizeSplitURL(request.IconURL, "icon_url"); err != nil {
		return dto.SiteSplitRequest{}, err
	}
	primary, err := uuid.Parse(request.PrimaryEntryID)
	if err != nil || !containsSplitEntry(entries, primary) {
		return dto.SiteSplitRequest{}, invalidSiteSplit("primary entry must be selected")
	}
	request.PrimaryEntryID = primary.String()
	request.EntryIDs = make([]string, 0, len(entries))
	for _, entry := range entries {
		request.EntryIDs = append(request.EntryIDs, entry.ID.String())
	}
	if len(request.IdentityKeysForNewSite) == 1 {
		key := strings.TrimSpace(request.IdentityKeysForNewSite[0])
		if key == "" || len(key) > 512 {
			return dto.SiteSplitRequest{}, invalidSiteSplit("invalid site identity")
		}
		request.IdentityKeysForNewSite = []string{key}
	} else {
		request.IdentityKeysForNewSite = nil
	}
	return request, nil
}

func normalizeSplitURL(value *string, field string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	clean := strings.TrimSpace(*value)
	if clean == "" {
		return nil, nil
	}
	if len(clean) > 2048 {
		return nil, invalidSiteSplit("invalid new site " + field)
	}
	parsed, err := url.Parse(clean)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, invalidSiteSplit("invalid new site " + field)
	}
	return &clean, nil
}

func eligibleSplitIdentities(site *repository.SiteDetail, entries []model.SiteEntry) map[string]struct{} {
	bound := make(map[string]struct{}, len(site.Identities))
	for _, identity := range site.Identities {
		bound[identity.IdentityKey] = struct{}{}
	}
	eligible := make(map[string]struct{})
	for _, entry := range entries {
		identity, err := siteidentity.FromURL(entry.NormalizedURL)
		if err != nil {
			continue
		}
		if _, ok := bound[identity.Key]; ok {
			eligible[identity.Key] = struct{}{}
		}
	}
	return eligible
}

func (s *SiteSplitService) selectedSplitEntries(ctx context.Context, rawSiteID string, revision int64, rawIDs []string) (*repository.SiteDetail, []model.SiteEntry, error) {
	siteID, err := uuid.Parse(rawSiteID)
	if err != nil || revision < 1 {
		return nil, nil, invalidSiteSplit("invalid source site")
	}
	site, err := s.sites.GetSite(ctx, siteID)
	if err != nil {
		return nil, nil, err
	}
	if site == nil {
		return nil, nil, problem.NewWithCode(problem.NotFound, problem.CodeSiteNotFound, "source site not found")
	}
	if site.Revision != revision {
		return nil, nil, siteMergeConflict("site was changed")
	}
	if len(rawIDs) == 0 {
		return nil, nil, invalidSiteSplit("at least one entry must move")
	}
	if len(rawIDs) >= len(site.Entries) {
		return nil, nil, invalidSiteSplit("at least one source entry must remain")
	}
	byID := map[uuid.UUID]model.SiteEntry{}
	for _, entry := range site.Entries {
		byID[entry.ID] = entry
	}
	seen := map[uuid.UUID]struct{}{}
	entries := make([]model.SiteEntry, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id, parseErr := uuid.Parse(raw)
		entry, found := byID[id]
		if parseErr != nil || !found {
			return nil, nil, invalidSiteSplit("entry does not belong to source site")
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, nil, invalidSiteSplit("duplicate split entry")
		}
		seen[id] = struct{}{}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID.String() < entries[j].ID.String() })
	return site, entries, nil
}

func splitPreview(site *repository.SiteDetail, entries []model.SiteEntry, payload dto.SiteSplitRequest, eligible map[string]struct{}) dto.SiteSplitPreviewResponse {
	out := dto.SiteSplitPreviewResponse{SourceSiteID: site.ID.String(), SourceRevision: site.Revision, Payload: payload, Entries: []dto.SiteMergePreviewEntryResponse{}, Identities: []dto.SiteSplitIdentityResponse{}, Tags: []string{}}
	for _, entry := range entries {
		out.Entries = append(out.Entries, dto.SiteMergePreviewEntryResponse{ID: entry.ID.String(), SiteID: site.ID.String(), LinkID: entry.LinkID.String(), Name: entry.EntryName, URL: entry.NormalizedURL})
	}
	selectedIdentity := ""
	if len(payload.IdentityKeysForNewSite) == 1 {
		selectedIdentity = payload.IdentityKeysForNewSite[0]
	}
	for _, identity := range site.Identities {
		_, canMove := eligible[identity.IdentityKey]
		owner := "source"
		if identity.IdentityKey == selectedIdentity {
			owner = "new_site"
		}
		out.Identities = append(out.Identities, dto.SiteSplitIdentityResponse{IdentityKey: identity.IdentityKey, EligibleForNewSite: canMove, Owner: owner})
	}
	for _, tag := range site.Tags {
		out.Tags = append(out.Tags, tag.Tag)
	}
	sort.Slice(out.Identities, func(i, j int) bool { return out.Identities[i].IdentityKey < out.Identities[j].IdentityKey })
	sort.Strings(out.Tags)
	return out
}

func containsSplitEntry(entries []model.SiteEntry, id uuid.UUID) bool {
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}
func invalidSiteSplit(message string) error {
	return problem.NewWithCode(problem.Invalid, "site_entry_selection_required", message)
}
