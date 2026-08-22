package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/cases"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

func (s *ReaderVNextService) GetEngagement(ctx context.Context, rawID string) (dto.ReaderEngagementResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	item, err := s.library.GetEngagement(ctx, id)
	if err != nil {
		return dto.ReaderEngagementResponse{}, mapReaderError(err)
	}
	return engagementResponse(*item), nil
}

func (s *ReaderVNextService) PatchEngagement(ctx context.Context, rawID string, request dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	if request.Read == nil && request.Progress == nil && request.ReadLater == nil {
		return dto.ReaderEngagementResponse{}, problem.NewWithCode(problem.Invalid, "engagement_patch_empty", "at least one engagement field is required")
	}
	if request.Progress != nil && (math.IsNaN(float64(*request.Progress)) || math.IsInf(float64(*request.Progress), 0) || *request.Progress < 0 || *request.Progress > 1) {
		return dto.ReaderEngagementResponse{}, problem.NewWithCode(problem.Invalid, "invalid_progress", "progress must be between 0 and 1")
	}
	item, err := s.library.PatchEngagement(ctx, model.ReaderEngagementPatch{LinkID: id, Read: request.Read, Progress: request.Progress, ReadLater: request.ReadLater})
	if err != nil {
		return dto.ReaderEngagementResponse{}, mapReaderError(err)
	}
	return engagementResponse(*item), nil
}

func engagementResponse(item model.ReaderEngagement) dto.ReaderEngagementResponse {
	return dto.ReaderEngagementResponse{LinkID: item.LinkID.String(), Read: item.Read, Progress: item.Progress, ReadLater: item.ReadLater, LastOpened: item.LastOpened, UpdatedAt: item.UpdatedAt}
}

type readerFeedActionIdentity struct {
	key    string
	kind   string
	source string
	id     uuid.UUID
}

func readerFeedActionIdentityForKey(itemKey string) (readerFeedActionIdentity, error) {
	itemKey = strings.TrimSpace(itemKey)
	kind, rawID, ok := strings.Cut(itemKey, ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	id, err := uuid.Parse(rawID)
	if err != nil || itemKey != kind+":"+id.String() {
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	var source string
	switch kind {
	case "link":
		source = "reading"
	case "inbox":
		source = "inbox"
	case "subscription":
		source = "subscription"
	default:
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	return readerFeedActionIdentity{key: itemKey, kind: kind, source: source, id: id}, nil
}

func feedItemResponse(item model.ReaderFeedItem) dto.ReaderFeedItemResponse {
	out := dto.ReaderFeedItemResponse{
		Key:         item.Key,
		Source:      item.Source,
		ResourceKey: item.ResourceIdentity(),
		Title:       item.Title,
		Summary:     item.Summary,
		URL:         item.URL,
		Read:        item.Read,
		ReadLater:   item.ReadLater,
		Saved:       item.Saved,
		EventAt:     item.VisibleEventAt(),
	}
	if item.LinkID != nil {
		value := item.LinkID.String()
		out.LinkID = &value
	}
	if item.InboxID != nil {
		value := item.InboxID.String()
		out.InboxID = &value
	}
	if item.FeedItemID != nil {
		value := item.FeedItemID.String()
		out.FeedItemID = &value
	}
	return out
}

// FeedWithSources returns one live mixed-feed page. The cursor carries its mode
// and source filter, so changing either parameter while paging is rejected.
func (s *ReaderVNextService) FeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (dto.ReaderFeedResponse, error) {
	mode = strings.TrimSpace(mode)
	if err := validateReaderFeedRequestMode(mode); err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	normalizedSources, err := normalizeReaderFeedSources(sources)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	page, err := s.library.ListFeedWithSources(ctx, mode, after, normalizedSources, limit)
	if err != nil {
		return dto.ReaderFeedResponse{}, mapReaderError(err)
	}
	if page == nil {
		return dto.ReaderFeedResponse{}, problem.NewWithCode(problem.Internal, "reader_feed_unavailable", "reader feed returned no page")
	}
	responseMode := strings.TrimSpace(page.Mode)
	if responseMode == "" {
		responseMode = mode
		if responseMode == "" {
			responseMode = "recommended"
		}
	}
	if responseMode != "recommended" && responseMode != "chronological" {
		return dto.ReaderFeedResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_mode", "unsupported feed mode")
	}
	out := dto.ReaderFeedResponse{
		Items:      make([]dto.ReaderFeedItemResponse, 0, len(page.Items)),
		NextCursor: page.NextCursor,
		Mode:       responseMode,
	}
	for _, item := range page.Items {
		out.Items = append(out.Items, feedItemResponse(item))
	}
	return out, nil
}

func validateReaderFeedRequestMode(mode string) error {
	if mode != "" && mode != "recommended" && mode != "chronological" {
		return problem.NewWithCode(problem.Invalid, "invalid_feed_mode", "unsupported feed mode")
	}
	return nil
}

func normalizeReaderFeedSources(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			source := strings.ToLower(strings.TrimSpace(part))
			if source == "" {
				continue
			}
			switch source {
			case "saved":
				source = "reading"
			case "pending":
				source = "inbox"
			case "reading", "inbox", "subscription":
			default:
				return nil, problem.NewWithCode(problem.Invalid, "invalid_feed_source", "unsupported feed source")
			}
			seen[source] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
}

func (s *ReaderVNextService) FeedbackFeed(ctx context.Context, itemKey, action string) (dto.ReaderFeedFeedbackResponse, error) {
	identity, err := readerFeedActionIdentityForKey(itemKey)
	if err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	if action != "hide" && action != "save" && action != "unsave" {
		return dto.ReaderFeedFeedbackResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_action", "unsupported feed action")
	}
	if identity.source != "subscription" && (action == "save" || action == "unsave") {
		return dto.ReaderFeedFeedbackResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "only subscription items can be saved")
	}
	feedback, err := s.library.FeedbackFeed(ctx, identity.key, action)
	if err = mapReaderError(err); err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	response := dto.ReaderFeedFeedbackResponse{ItemKey: feedback.ItemKey, Action: feedback.Action}
	if feedback.LinkID != nil {
		linkID := feedback.LinkID.String()
		response.LinkID = &linkID
	}
	return response, nil
}

func (s *ReaderVNextService) Home(ctx context.Context) (dto.ReaderHomeResponse, error) {
	return s.HomeAggregate(ctx)
}

func (s *ReaderVNextService) RelatedTags(ctx context.Context, rawLinkID string, limit int) (dto.ReaderRelatedTagsResponse, error) {
	var linkID *uuid.UUID
	if strings.TrimSpace(rawLinkID) != "" {
		id, err := readerUUID(rawLinkID, "link_id")
		if err != nil {
			return dto.ReaderRelatedTagsResponse{}, err
		}
		linkID = &id
	}
	items, err := s.library.RelatedTags(ctx, linkID, limit)
	if err != nil {
		return dto.ReaderRelatedTagsResponse{}, mapReaderError(err)
	}
	return dto.ReaderRelatedTagsResponse{Items: items}, nil
}

func (s *ReaderVNextService) Activity(ctx context.Context, rawKind, rawAfter string, limit int) (dto.ReaderActivityResponse, error) {
	kind, err := normalizeReaderActivityKind(rawKind)
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	after, err := s.decodeReaderActivityCursor(ctx, kind, rawAfter)
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page, err := s.library.ListActivity(ctx, model.ReaderActivityQuery{Kind: kind, After: after, Limit: limit})
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	out := dto.ReaderActivityResponse{
		Kind:    kind,
		Tags:    make([]dto.ReaderTagActivityResponse, 0, len(page.Items)),
		Domains: make([]dto.ReaderDomainActivityResponse, 0, len(page.Items)),
	}
	for _, item := range page.Items {
		switch item.Kind {
		case model.ReaderActivityKindTag:
			out.Tags = append(out.Tags, dto.ReaderTagActivityResponse{Tag: item.Key, LastAt: item.LastAt})
		case model.ReaderActivityKindDomain:
			out.Domains = append(out.Domains, dto.ReaderDomainActivityResponse{Domain: item.Key, LastAt: item.LastAt})
		default:
			return dto.ReaderActivityResponse{}, mapReaderError(fmt.Errorf("%w: invalid activity row kind", repository.ErrInvalidReaderCursor))
		}
	}
	if page.HasMore {
		if len(page.Items) == 0 {
			return dto.ReaderActivityResponse{}, mapReaderError(fmt.Errorf("%w: empty activity continuation page", repository.ErrInvalidReaderCursor))
		}
		out.NextCursor = s.encodeReaderActivityCursor(ctx, kind, page.Items[len(page.Items)-1])
	}
	return out, nil
}

func (s *ReaderVNextService) PatchLinkMetadata(ctx context.Context, rawID string, request dto.ReaderLinkMetadataRequest, expected int64) (dto.ReaderLinkMetadataResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	if !request.Complete() {
		return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeMetadataFieldsRequired, "title, summary, and tags are required")
	}
	if err := validateLinkMetadataRequest(&request); err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	update, err := s.library.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{LinkID: id, Title: request.Title, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		if errors.Is(err, repository.ErrRevisionConflict) {
			return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is stale")
		}
		return dto.ReaderLinkMetadataResponse{}, mapReaderError(err)
	}
	if update.MetadataRevision < 1 || update.MetadataRevision > model.LinkMetadataMaxRevision {
		return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is outside the JavaScript-safe range")
	}
	return dto.ReaderLinkMetadataResponse{LinkID: id.String(), MetadataRevision: update.MetadataRevision}, nil
}

const (
	maxLinkMetadataTitleRunes   = 512
	maxLinkMetadataSummaryRunes = 4096
	maxLinkMetadataTags         = 50
	maxLinkMetadataTagRunes     = 64
)

func validateLinkMetadataRequest(request *dto.ReaderLinkMetadataRequest) error {
	if request.Title != nil && utf8.RuneCountInString(*request.Title) > maxLinkMetadataTitleRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "title exceeds 512 characters")
	}
	if request.Summary != nil && utf8.RuneCountInString(*request.Summary) > maxLinkMetadataSummaryRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "summary exceeds 4096 characters")
	}
	if request.Tags == nil {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags must be an array")
	}
	if len(request.Tags) > maxLinkMetadataTags {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags may contain at most 50 items")
	}

	folder := cases.Fold()
	seen := make(map[string]struct{}, len(request.Tags))
	tags := make([]string, 0, len(request.Tags))
	for _, raw := range request.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags must not contain empty values")
		}
		if utf8.RuneCountInString(tag) > maxLinkMetadataTagRunes {
			return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags may not exceed 64 characters")
		}
		key := folder.String(tag)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		tags = append(tags, tag)
	}
	request.Tags = tags
	return nil
}
