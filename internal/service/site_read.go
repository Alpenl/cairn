package service

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
)

const recentSiteWindow = 720 * time.Hour

type SiteReadService struct {
	sites repository.SiteReader
	now   func() time.Time
}

func NewSiteReadService(sites repository.SiteReader) *SiteReadService {
	return &SiteReadService{sites: sites, now: time.Now}
}
func (s *SiteReadService) List(ctx context.Context, view, tags, rawRecentCutoff string, page, limit int) (dto.PaginatedSitesResponse, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	view = strings.TrimSpace(view)
	if view == "" {
		view = "all"
	}
	if view != "all" && view != "pinned" && view != "recent" && view != "review" {
		return dto.PaginatedSitesResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidSiteView, "unsupported site view")
	}
	cutoff, err := s.recentCutoff(view, rawRecentCutoff, page)
	if err != nil {
		return dto.PaginatedSitesResponse{}, err
	}
	parsed := splitTags(tags)
	items, total, err := s.sites.ListSites(ctx, repository.SiteListFilter{View: view, Tags: parsed, RecentCutoff: cutoff, Limit: limit, Offset: (page - 1) * limit})
	if err != nil {
		return dto.PaginatedSitesResponse{}, err
	}
	out := make([]dto.SiteListItemResponse, 0, len(items))
	for _, item := range items {
		out = append(out, siteListDTO(item))
	}
	return dto.PaginatedSitesResponse{Items: out, Total: total, Page: page, Limit: limit, RecentCutoff: cutoff}, nil
}

func (s *SiteReadService) recentCutoff(view, raw string, page int) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if view != "recent" {
		if raw != "" {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRecentCutoff, "recent_cutoff requires the recent view")
		}
		return nil, nil
	}
	if page == 1 {
		if raw != "" {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRecentCutoff, "recent_cutoff must be omitted for page 1")
		}
		now := time.Now
		if s != nil && s.now != nil {
			now = s.now
		}
		cutoff := now().UTC().Add(-recentSiteWindow)
		return &cutoff, nil
	}
	if raw == "" {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRecentCutoff, "recent_cutoff is required after page 1")
	}
	cutoff, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidRecentCutoff, "recent_cutoff must be an RFC3339 instant")
	}
	cutoff = cutoff.UTC()
	return &cutoff, nil
}
func (s *SiteReadService) Get(ctx context.Context, rawID string) (dto.SiteDetailResponse, error) {
	id, err := uuid.Parse(rawID)
	if err != nil {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidSiteID, "invalid site id")
	}
	detail, err := s.sites.GetSite(ctx, id)
	if err != nil {
		return dto.SiteDetailResponse{}, err
	}
	if detail == nil {
		return dto.SiteDetailResponse{}, httperr.NewWithCode(http.StatusNotFound, httperr.CodeSiteNotFound, "site not found")
	}
	out := dto.SiteDetailResponse{
		SiteListItemResponse: siteListDTO(detail.SiteListItem),
		UserNote:             detail.UserNote,
		GroupingLocked:       detail.GroupingLocked,
		TagsWithSource:       make([]dto.SiteTagResponse, 0, len(detail.Tags)),
		Entries:              make([]dto.SiteEntryResponse, 0, len(detail.Entries)),
		RelatedReadings:      make([]dto.RelatedReadingResponse, 0),
	}
	for _, tag := range detail.Tags {
		out.TagsWithSource = append(out.TagsWithSource, dto.SiteTagResponse{Tag: tag.Tag, Source: string(tag.Source)})
	}
	for _, entry := range detail.Entries {
		out.Entries = append(out.Entries, dto.SiteEntryResponse{ID: entry.ID.String(), LinkID: entry.LinkID.String(), Name: entry.EntryName, NameSource: string(entry.EntryNameSource), Purpose: entry.Purpose, PurposeSource: string(entry.PurposeSource), URL: entry.NormalizedURL, FirstCollectedAt: entry.FirstCollectedAt, LastRecollectedAt: entry.LastRecollectedAt})
	}
	if related, ok := s.sites.(repository.SiteRelatedReader); ok {
		hosts := relatedSiteHosts(detail)
		tags := make([]string, 0, len(detail.Tags))
		for _, tag := range detail.Tags {
			tags = append(tags, tag.NormalizedTag)
		}
		items, relatedErr := related.ListRelatedReadings(ctx, hosts, tags, 6)
		if relatedErr != nil {
			return dto.SiteDetailResponse{}, relatedErr
		}
		for _, item := range items {
			out.RelatedReadings = append(out.RelatedReadings, dto.RelatedReadingResponse{ID: item.ID.String(), Title: item.Title, URL: item.URL, CreatedAt: item.CreatedAt})
		}
	}
	return out, nil
}

func relatedSiteHosts(detail *repository.SiteDetail) []string {
	set := map[string]struct{}{}
	add := func(raw string) {
		parsed, err := url.Parse(raw)
		if err == nil && parsed.Hostname() != "" {
			set[strings.ToLower(parsed.Hostname())] = struct{}{}
		}
	}
	if detail.HomepageURL != nil {
		add(*detail.HomepageURL)
	}
	for _, entry := range detail.Entries {
		add(entry.NormalizedURL)
	}
	out := make([]string, 0, len(set))
	for host := range set {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}
func siteListDTO(item repository.SiteListItem) dto.SiteListItemResponse {
	out := dto.SiteListItemResponse{ID: item.ID.String(), Name: item.Name, Intro: item.Intro, DisplayHost: item.DisplayHost, HomepageURL: item.HomepageURL, IconURL: item.IconURL, Tags: append([]string{}, item.Tags...), EntryCount: item.EntryCount, Pinned: item.Pinned, NeedsReview: item.NeedsReview, Revision: item.Revision, FirstCollectedAt: item.FirstCollectedAt, LastCollectedAt: item.LastCollectedAt}
	if item.PrimaryEntryID != nil && item.PrimaryEntryURL != nil {
		out.PrimaryEntry = &dto.SitePrimaryEntryResponse{ID: item.PrimaryEntryID.String(), URL: *item.PrimaryEntryURL}
	}
	return out
}
