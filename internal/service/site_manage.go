package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
)

type SiteManagementService struct {
	reader      repository.SiteReader
	writer      repository.SiteProfileWriter
	profileTags repository.SiteProfileTagWriter
	management  repository.SiteManagementWriter
}

func NewSiteManagementService(reader repository.SiteReader, writer repository.SiteProfileWriter) *SiteManagementService {
	service := &SiteManagementService{reader: reader, writer: writer}
	service.profileTags, _ = writer.(repository.SiteProfileTagWriter)
	service.management, _ = writer.(repository.SiteManagementWriter)
	return service
}

func (s *SiteManagementService) Update(ctx context.Context, rawID, ifMatch string, request dto.SiteUpdateRequest) (dto.SiteDetailResponse, error) {
	id, err := uuid.Parse(rawID)
	if err != nil {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteID, "invalid site id")
	}
	revision, err := parseSiteRevision(ifMatch)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	params, err := normalizeSiteUpdate(request)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	params.ID, params.Revision = id, revision
	var updated bool
	if len(params.TagAdds) > 0 || len(params.TagRemovals) > 0 {
		if s.profileTags == nil {
			return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site tag management is not configured")
		}
		updated, err = s.profileTags.UpdateSiteProfileAndTags(ctx, params)
	} else {
		updated, err = s.writer.UpdateSiteProfile(ctx, params)
	}
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	if !updated {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeSiteRevisionConflict, "site was changed by another request")
	}
	return NewSiteReadService(s.reader).Get(ctx, rawID)
}

func (s *SiteManagementService) UpdateEntry(ctx context.Context, rawSiteID, rawEntryID, ifMatch string, request dto.SiteEntryUpdateRequest) (dto.SiteDetailResponse, error) {
	siteID, entryID, revision, err := parseSiteEntryMutation(rawSiteID, rawEntryID, ifMatch)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	if s.management == nil {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site management is not configured")
	}
	params, err := normalizeSiteEntryUpdate(request)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	params.SiteID, params.EntryID, params.Revision = siteID, entryID, revision
	if _, err := s.management.UpdateSiteEntry(ctx, params); err != nil {
		return dto.SiteDetailResponse{}, mapSiteManagementError(err)
	}
	return NewSiteReadService(s.reader).Get(ctx, rawSiteID)
}

func (s *SiteManagementService) SetPrimaryEntry(ctx context.Context, rawSiteID, rawEntryID, ifMatch string) (dto.SiteDetailResponse, error) {
	siteID, entryID, revision, err := parseSiteEntryMutation(rawSiteID, rawEntryID, ifMatch)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	if s.management == nil {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site management is not configured")
	}
	if _, err := s.management.SetSitePrimaryEntry(ctx, repository.SetSitePrimaryEntryParams{SiteID: siteID, EntryID: entryID, Revision: revision}); err != nil {
		return dto.SiteDetailResponse{}, mapSiteManagementError(err)
	}
	return NewSiteReadService(s.reader).Get(ctx, rawSiteID)
}

func (s *SiteManagementService) DeleteEntry(ctx context.Context, rawSiteID, rawEntryID, ifMatch string) (dto.SiteEntryDeleteResponse, error) {
	siteID, entryID, revision, err := parseSiteEntryMutation(rawSiteID, rawEntryID, ifMatch)
	if err != nil {
		return dto.SiteEntryDeleteResponse{}, err
	}
	if s.management == nil {
		return dto.SiteEntryDeleteResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site management is not configured")
	}
	result, err := s.management.DeleteSiteEntry(ctx, repository.DeleteSiteEntryParams{SiteID: siteID, EntryID: entryID, Revision: revision})
	if err != nil {
		return dto.SiteEntryDeleteResponse{}, mapSiteManagementError(err)
	}
	return dto.SiteEntryDeleteResponse{DeletedSite: result.DeletedSite}, nil
}

func (s *SiteManagementService) Delete(ctx context.Context, rawSiteID, ifMatch, rawCount string) error {
	siteID, err := uuid.Parse(rawSiteID)
	if err != nil {
		return httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteID, "invalid site id")
	}
	revision, err := parseSiteRevision(ifMatch)
	if err != nil {
		return err
	}
	count, err := strconv.Atoi(strings.TrimSpace(rawCount))
	if err != nil || count < 1 {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeSiteDeleteConfirm, "confirm_entry_count must be a positive integer")
	}
	if s.management == nil {
		return httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site management is not configured")
	}
	if _, err := s.management.DeleteSite(ctx, repository.DeleteSiteParams{ID: siteID, Revision: revision, ConfirmEntryCount: count}); err != nil {
		return mapSiteManagementError(err)
	}
	return nil
}

func parseSiteEntryMutation(rawSiteID, rawEntryID, ifMatch string) (uuid.UUID, uuid.UUID, int64, error) {
	siteID, err := uuid.Parse(rawSiteID)
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteID, "invalid site id")
	}
	entryID, err := uuid.Parse(rawEntryID)
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteEntryID, "invalid site entry id")
	}
	revision, err := parseSiteRevision(ifMatch)
	if err != nil {
		return uuid.Nil, uuid.Nil, 0, err
	}
	return siteID, entryID, revision, nil
}

func normalizeSiteEntryUpdate(request dto.SiteEntryUpdateRequest) (repository.UpdateSiteEntryParams, error) {
	if request.Name == nil && request.Purpose == nil {
		return repository.UpdateSiteEntryParams{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeSiteEntryUpdateEmpty, "site entry update must include at least one field")
	}
	trim := func(value *string, maximum int, required bool) (*string, error) {
		if value == nil {
			return nil, nil
		}
		clean := strings.TrimSpace(*value)
		if !validUnicodeLength(clean, maximum) || (required && clean == "") {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "invalid site entry update")
		}
		return &clean, nil
	}
	name, err := trim(request.Name, 256, true)
	if err != nil {
		return repository.UpdateSiteEntryParams{}, err
	}
	purpose, err := trim(request.Purpose, 1000, false)
	if err != nil {
		return repository.UpdateSiteEntryParams{}, err
	}
	return repository.UpdateSiteEntryParams{Name: name, Purpose: purpose}, nil
}

func mapSiteManagementError(err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return httperr.NewWithCode(http.StatusNotFound, httperr.CodeSiteNotFound, "site not found")
	case errors.Is(err, repository.ErrSiteEntryNotFound):
		return httperr.NewWithCode(http.StatusNotFound, httperr.CodeSiteEntryNotFound, "site entry not found")
	case errors.Is(err, repository.ErrRevisionConflict):
		return httperr.NewWithCode(http.StatusConflict, httperr.CodeSiteRevisionConflict, "site was changed by another request")
	default:
		return err
	}
}

func parseSiteRevision(raw string) (int64, error) {
	clean := strings.Trim(strings.TrimSpace(raw), "\"")
	revision, err := strconv.ParseInt(clean, 10, 64)
	if err != nil || revision < 1 {
		return 0, httperr.NewWithCode(http.StatusPreconditionRequired, httperr.CodeSiteRevisionRequired, "If-Match must contain the current site revision")
	}
	return revision, nil
}

func normalizeSiteUpdate(request dto.SiteUpdateRequest) (repository.UpdateSiteProfileParams, error) { //nolint:gocyclo // 逐字段归一化站点更新，分支数等于可更新字段数
	if request.Name == nil && request.Intro == nil && request.HomepageURL == nil && request.IconURL == nil && request.UserNote == nil && request.Pinned == nil && request.Tags == nil {
		return repository.UpdateSiteProfileParams{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeSiteUpdateEmpty, "site update must include at least one field")
	}
	trim := func(value *string, maximum int, required bool, field string) (*string, error) {
		if value == nil {
			return nil, nil
		}
		clean := strings.TrimSpace(*value)
		if !validUnicodeLength(clean, maximum) || (required && clean == "") {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "invalid site "+field)
		}
		return &clean, nil
	}
	name, err := trim(request.Name, 256, true, "name")
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	intro, err := trim(request.Intro, 1000, false, "intro")
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	note, err := trim(request.UserNote, 10000, false, "user_note")
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	icon, err := trim(request.IconURL, 2048, false, "icon_url")
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	homepage, err := trim(request.HomepageURL, 2048, true, "homepage_url")
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	if err := validateSiteHTTPURL(icon, "icon_url"); err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	if err := validateSiteHTTPURL(homepage, "homepage_url"); err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	adds, removals, err := normalizeSiteTagPatch(request.Tags)
	if err != nil {
		return repository.UpdateSiteProfileParams{}, err
	}
	return repository.UpdateSiteProfileParams{Name: name, Intro: intro, HomepageURL: homepage, IconURL: icon, UserNote: note, Pinned: request.Pinned, TagAdds: adds, TagRemovals: removals}, nil
}

func validateSiteHTTPURL(value *string, field string) error {
	if value == nil {
		return nil
	}
	parsed, err := url.Parse(*value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "invalid site "+field)
	}
	return nil
}

func normalizeSiteTagPatch(patch *dto.SiteTagPatchRequest) ([]repository.SiteTagMutation, []string, error) { //nolint:gocyclo // 标签增删的组合校验，分支覆盖 add/remove 的各种非法组合
	if patch == nil {
		return nil, nil, nil
	}
	if len(patch.Add) == 0 && len(patch.Remove) == 0 {
		return nil, nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "site tag patch must add or remove a tag")
	}
	if len(patch.Add) > 50 || len(patch.Remove) > 50 {
		return nil, nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "too many site tags")
	}
	normalize := func(values []string) ([]repository.SiteTagMutation, error) {
		seen := make(map[string]struct{}, len(values))
		out := make([]repository.SiteTagMutation, 0, len(values))
		for _, raw := range values {
			tag := strings.TrimSpace(raw)
			key := strings.ToLower(tag)
			if tag == "" || !validUnicodeLength(tag, 128) || !validUnicodeLength(key, 128) {
				return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "invalid site tag")
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, repository.SiteTagMutation{Tag: tag, NormalizedTag: key})
		}
		return out, nil
	}
	adds, err := normalize(patch.Add)
	if err != nil {
		return nil, nil, err
	}
	removalMutations, err := normalize(patch.Remove)
	if err != nil {
		return nil, nil, err
	}
	addSet := make(map[string]struct{}, len(adds))
	for _, tag := range adds {
		addSet[tag.NormalizedTag] = struct{}{}
	}
	removals := make([]string, 0, len(removalMutations))
	for _, tag := range removalMutations {
		if _, conflict := addSet[tag.NormalizedTag]; conflict {
			return nil, nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteUpdate, "a site tag cannot be added and removed together")
		}
		removals = append(removals, tag.NormalizedTag)
	}
	return adds, removals, nil
}

// validUnicodeLength keeps the service contract aligned with OpenAPI
// maxLength and PostgreSQL char_length: both count Unicode code points, not
// UTF-8 bytes. Invalid UTF-8 is rejected instead of being counted as RuneError.
func validUnicodeLength(value string, maximum int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum
}
