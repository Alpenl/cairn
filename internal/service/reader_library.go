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

	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

type ReaderActivityResult struct {
	Kind       string
	Items      []model.ReaderActivity
	NextCursor string
}

type ReaderLinkMetadataCommand struct {
	LinkID           uuid.UUID
	Title            *string
	Summary          *string
	Tags             []string
	ExpectedRevision int64
}

func (s *ReaderLibraryApplication) GetEngagement(ctx context.Context, id uuid.UUID) (model.ReaderEngagement, error) {
	item, err := s.library.GetEngagement(ctx, id)
	if err != nil {
		return model.ReaderEngagement{}, mapReaderError(err)
	}
	return *item, nil
}

func (s *ReaderLibraryApplication) PatchEngagement(ctx context.Context, patch model.ReaderEngagementPatch) (model.ReaderEngagement, error) {
	if patch.Read == nil && patch.Progress == nil && patch.ReadLater == nil {
		return model.ReaderEngagement{}, problem.NewWithCode(problem.Invalid, "engagement_patch_empty", "at least one engagement field is required")
	}
	if patch.Progress != nil && (math.IsNaN(float64(*patch.Progress)) || math.IsInf(float64(*patch.Progress), 0) || *patch.Progress < 0 || *patch.Progress > 1) {
		return model.ReaderEngagement{}, problem.NewWithCode(problem.Invalid, "invalid_progress", "progress must be between 0 and 1")
	}
	item, err := s.library.PatchEngagement(ctx, patch)
	if err != nil {
		return model.ReaderEngagement{}, mapReaderError(err)
	}
	return *item, nil
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

// FeedWithSources returns one live mixed-feed page. The cursor carries its mode
// and source filter, so changing either parameter while paging is rejected.
func (s *ReaderLibraryApplication) FeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (model.ReaderFeedPage, error) {
	mode = strings.TrimSpace(mode)
	if err := validateReaderFeedRequestMode(mode); err != nil {
		return model.ReaderFeedPage{}, err
	}
	normalizedSources, err := normalizeReaderFeedSources(sources)
	if err != nil {
		return model.ReaderFeedPage{}, err
	}
	page, err := s.library.ListFeedWithSources(ctx, mode, after, normalizedSources, limit)
	if err != nil {
		return model.ReaderFeedPage{}, mapReaderError(err)
	}
	if page == nil {
		return model.ReaderFeedPage{}, problem.NewWithCode(problem.Internal, "reader_feed_unavailable", "reader feed returned no page")
	}
	responseMode := strings.TrimSpace(page.Mode)
	if responseMode == "" {
		responseMode = mode
		if responseMode == "" {
			responseMode = "recommended"
		}
	}
	if responseMode != "recommended" && responseMode != "chronological" {
		return model.ReaderFeedPage{}, problem.NewWithCode(problem.Invalid, "invalid_feed_mode", "unsupported feed mode")
	}
	page.Mode = responseMode
	return *page, nil
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

func (s *ReaderLibraryApplication) FeedbackFeed(ctx context.Context, itemKey, action string) (model.ReaderFeedFeedback, error) {
	identity, err := readerFeedActionIdentityForKey(itemKey)
	if err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	if action != "hide" && action != "save" && action != "unsave" {
		return model.ReaderFeedFeedback{}, problem.NewWithCode(problem.Invalid, "invalid_feed_action", "unsupported feed action")
	}
	if identity.source != "subscription" && (action == "save" || action == "unsave") {
		return model.ReaderFeedFeedback{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "only subscription items can be saved")
	}
	if s.feedFeedback == nil {
		return model.ReaderFeedFeedback{}, errors.New("reader Feed feedback commands are not configured")
	}
	feedback, err := s.feedFeedback.FeedbackFeed(ctx, identity.key, action)
	if err = mapReaderError(err); err != nil {
		return model.ReaderFeedFeedback{}, err
	}
	return feedback, nil
}

func (s *ReaderLibraryApplication) Home(ctx context.Context) (ReaderHomeResult, error) {
	return s.HomeAggregate(ctx)
}

func (s *ReaderLibraryApplication) RelatedTags(ctx context.Context, linkID *uuid.UUID, limit int) ([]string, error) {
	items, err := s.library.RelatedTags(ctx, linkID, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

func (s *ReaderLibraryApplication) Activity(ctx context.Context, rawKind, rawAfter string, limit int) (ReaderActivityResult, error) {
	kind, err := normalizeReaderActivityKind(rawKind)
	if err != nil {
		return ReaderActivityResult{}, mapReaderError(err)
	}
	after, err := s.decodeReaderActivityCursor(ctx, kind, rawAfter)
	if err != nil {
		return ReaderActivityResult{}, mapReaderError(err)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page, err := s.library.ListActivity(ctx, model.ReaderActivityQuery{Kind: kind, After: after, Limit: limit})
	if err != nil {
		return ReaderActivityResult{}, mapReaderError(err)
	}
	for _, item := range page.Items {
		switch item.Kind {
		case model.ReaderActivityKindTag, model.ReaderActivityKindDomain:
		default:
			return ReaderActivityResult{}, mapReaderError(fmt.Errorf("%w: invalid activity row kind", repository.ErrInvalidReaderCursor))
		}
	}
	result := ReaderActivityResult{Kind: kind, Items: page.Items}
	if page.HasMore {
		if len(page.Items) == 0 {
			return ReaderActivityResult{}, mapReaderError(fmt.Errorf("%w: empty activity continuation page", repository.ErrInvalidReaderCursor))
		}
		result.NextCursor = s.encodeReaderActivityCursor(ctx, kind, page.Items[len(page.Items)-1])
	}
	return result, nil
}

func (s *ReaderLibraryApplication) PatchLinkMetadata(ctx context.Context, command ReaderLinkMetadataCommand) (model.ReaderLinkMetadataUpdate, error) {
	if err := validateLinkMetadataCommand(&command); err != nil {
		return model.ReaderLinkMetadataUpdate{}, err
	}
	update, err := s.library.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{LinkID: command.LinkID, Title: command.Title, Summary: command.Summary, Tags: command.Tags, ExpectedRevision: command.ExpectedRevision})
	if err != nil {
		if errors.Is(err, repository.ErrRevisionConflict) {
			return model.ReaderLinkMetadataUpdate{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is stale")
		}
		return model.ReaderLinkMetadataUpdate{}, mapReaderError(err)
	}
	if update.MetadataRevision < 1 || update.MetadataRevision > model.LinkMetadataMaxRevision {
		return model.ReaderLinkMetadataUpdate{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is outside the JavaScript-safe range")
	}
	return update, nil
}

const (
	maxLinkMetadataTitleRunes   = 512
	maxLinkMetadataSummaryRunes = 4096
	maxLinkMetadataTags         = 50
	maxLinkMetadataTagRunes     = 64
)

func validateLinkMetadataCommand(command *ReaderLinkMetadataCommand) error {
	if command.Title != nil && utf8.RuneCountInString(*command.Title) > maxLinkMetadataTitleRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "title exceeds 512 characters")
	}
	if command.Summary != nil && utf8.RuneCountInString(*command.Summary) > maxLinkMetadataSummaryRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "summary exceeds 4096 characters")
	}
	if command.Tags == nil {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags must be an array")
	}
	if len(command.Tags) > maxLinkMetadataTags {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags may contain at most 50 items")
	}

	folder := cases.Fold()
	seen := make(map[string]struct{}, len(command.Tags))
	tags := make([]string, 0, len(command.Tags))
	for _, raw := range command.Tags {
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
	command.Tags = tags
	return nil
}
