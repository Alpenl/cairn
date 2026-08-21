package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/errsafe"
	feedremote "webtag/internal/feed"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

const (
	feedContentMinRunes  = 280
	maxFeedSearchRunes   = 256
	maxFeedFolderRunes   = 128
	maxFeedTitleRunes    = 1024
	defaultFeedPageLimit = 20
	maxFeedPageLimit     = 100
)

// FeedSubscriptionUpdateCommand is the application command consumed by the
// feed subscription surface. Repository patch types stay behind FeedService.
type FeedSubscriptionUpdateCommand struct {
	FolderID  *uuid.UUID
	SetFolder bool
	Title     *string
}

// FeedItemFilter is the application query shared by list and mark-read use
// cases. It is intentionally independent from HTTP DTOs and repository types.
type FeedItemFilter struct {
	View           string
	SubscriptionID *uuid.UUID
	FolderID       *uuid.UUID
	Ungrouped      bool
	Query          string
	Page           int
	Limit          int
}

// FeedItemStateCommand is the application command for mutable item flags.
type FeedItemStateCommand struct {
	Read      *bool
	Starred   *bool
	ReadLater *bool
}

type FeedStore interface {
	ListOverview(context.Context, string) (model.FeedSubscriptionsResponse, error)
	CreateSubscription(context.Context, string, *uuid.UUID, bool, string) (model.FeedSubscription, error)
	GetSubscription(context.Context, uuid.UUID) (model.FeedSubscription, bool, error)
	UpdateSubscription(context.Context, uuid.UUID, repository.FeedSubscriptionPatch) (model.FeedSubscription, error)
	SoftDeleteSubscription(context.Context, uuid.UUID) error
	ScheduleRefresh(context.Context, uuid.UUID) error
	ScheduleAllRefreshes(context.Context) (int64, error)
	CompleteRefresh(context.Context, repository.FeedRefreshSuccess) (int, error)
	FailRefresh(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time, string) error

	ListFolders(context.Context) ([]model.FeedFolder, error)
	CreateFolder(context.Context, string) (model.FeedFolder, error)
	UpdateFolder(context.Context, uuid.UUID, string) (model.FeedFolder, error)
	DeleteFolder(context.Context, uuid.UUID) error

	ListItems(context.Context, repository.FeedItemFilter) (model.PaginatedFeedItems, error)
	GetItem(context.Context, uuid.UUID, bool) (model.FeedItem, bool, error)
	UpdateItemState(context.Context, uuid.UUID, repository.FeedItemStatePatch) (model.FeedItem, error)
	MarkItemsRead(context.Context, repository.FeedItemFilter) (int64, error)
	AssociateItemLink(context.Context, uuid.UUID, uuid.UUID) error
}

type FeedRemote interface {
	Discover(context.Context, string) ([]model.FeedCandidate, error)
	FetchAndParse(context.Context, string, feedremote.ConditionalHeaders) (feedremote.RemoteResult, model.ParsedFeed, error)
}

type FeedLinkAnalyzer interface {
	AnalyzeRSS(context.Context, RSSIngestRequest) (dto.SubmitResponse, error)
}

type FeedServiceOptions struct {
	Store    FeedStore
	Remote   FeedRemote
	Analyzer FeedLinkAnalyzer
	Locker   URLLocker
	Logger   *slog.Logger
	Now      func() time.Time
}

type FeedService struct {
	store    FeedStore
	remote   FeedRemote
	analyzer FeedLinkAnalyzer
	locker   URLLocker
	logger   *slog.Logger
	now      func() time.Time
}

func NewFeedService(options FeedServiceOptions) *FeedService {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	locker := options.Locker
	if locker == nil {
		locker = noopURLLocker{}
	}
	return &FeedService{store: options.Store, remote: options.Remote, analyzer: options.Analyzer, locker: locker, logger: options.Logger, now: now}
}

func (s *FeedService) ListSubscriptions(ctx context.Context, rawURL string) (model.FeedSubscriptionsResponse, error) {
	if s == nil || s.store == nil {
		return model.FeedSubscriptionsResponse{}, fmt.Errorf("feed service is not configured")
	}
	queryURL := strings.TrimSpace(rawURL)
	if queryURL != "" {
		canonical, err := feedremote.ValidateURL(queryURL)
		if err != nil {
			return model.FeedSubscriptionsResponse{}, err
		}
		queryURL = canonical
	}
	response, err := s.store.ListOverview(ctx, queryURL)
	if err != nil {
		return model.FeedSubscriptionsResponse{}, err
	}
	now := s.now().UTC()
	for index := range response.Subscriptions {
		deadline := response.Subscriptions[index].RefreshClaimUntil
		response.Subscriptions[index].Refreshing = deadline != nil && deadline.After(now)
	}
	return response, nil
}

func (s *FeedService) Discover(ctx context.Context, rawURL string) (model.FeedDiscoveryResponse, error) {
	if s == nil || s.remote == nil {
		return model.FeedDiscoveryResponse{}, fmt.Errorf("feed discovery is not configured")
	}
	feeds, err := s.remote.Discover(ctx, rawURL)
	if err != nil {
		return model.FeedDiscoveryResponse{}, err
	}
	return model.FeedDiscoveryResponse{Feeds: feeds}, nil
}

func (s *FeedService) Subscribe(ctx context.Context, rawURL string, folderID *uuid.UUID, setFolder bool) (model.FeedSubscription, error) {
	if s == nil || s.store == nil {
		return model.FeedSubscription{}, fmt.Errorf("feed service is not configured")
	}
	feedURL, err := feedremote.ValidateURL(rawURL)
	if err != nil {
		return model.FeedSubscription{}, err
	}
	var subscription model.FeedSubscription
	err = s.withMutation(ctx, "feed-subscription:"+feedURL, func(lockCtx context.Context) error {
		var createErr error
		subscription, createErr = s.store.CreateSubscription(lockCtx, feedURL, folderID, setFolder, feedURL)
		return createErr
	})
	return subscription, mapFeedRepositoryError(err, "subscription not found")
}

func (s *FeedService) UpdateSubscription(ctx context.Context, rawID string, command FeedSubscriptionUpdateCommand) (model.FeedSubscription, error) {
	id, err := parseFeedUUID(rawID, "invalid_subscription_id", "invalid subscription id")
	if err != nil {
		return model.FeedSubscription{}, err
	}
	if command.Title != nil {
		title := strings.TrimSpace(*command.Title)
		if title == "" || utf8.RuneCountInString(title) > maxFeedTitleRunes {
			return model.FeedSubscription{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_subscription_title", "subscription title must be between 1 and 1024 characters")
		}
		command.Title = &title
	}
	var subscription model.FeedSubscription
	err = s.withMutation(ctx, "feed-subscription:"+id.String(), func(lockCtx context.Context) error {
		var updateErr error
		subscription, updateErr = s.store.UpdateSubscription(lockCtx, id, repository.FeedSubscriptionPatch{
			FolderID: command.FolderID, SetFolder: command.SetFolder, Title: command.Title,
		})
		return updateErr
	})
	return subscription, mapFeedRepositoryError(err, "subscription not found")
}

func (s *FeedService) Unsubscribe(ctx context.Context, rawID string) error {
	id, err := parseFeedUUID(rawID, "invalid_subscription_id", "invalid subscription id")
	if err != nil {
		return err
	}
	err = s.withMutation(ctx, "feed-subscription:"+id.String(), func(lockCtx context.Context) error {
		return s.store.SoftDeleteSubscription(lockCtx, id)
	})
	return mapFeedRepositoryError(err, "subscription not found")
}

func (s *FeedService) ScheduleRefresh(ctx context.Context, rawID string) error {
	id, err := parseFeedUUID(rawID, "invalid_subscription_id", "invalid subscription id")
	if err != nil {
		return err
	}
	return mapFeedRepositoryError(s.store.ScheduleRefresh(ctx, id), "subscription not found")
}

func (s *FeedService) ScheduleAllRefreshes(ctx context.Context) (int64, error) {
	return s.store.ScheduleAllRefreshes(ctx)
}

func (s *FeedService) ListItems(ctx context.Context, filter FeedItemFilter) (model.PaginatedFeedItems, error) {
	normalized, err := normalizeFeedItemFilter(filter)
	if err != nil {
		return model.PaginatedFeedItems{}, err
	}
	return s.store.ListItems(ctx, repositoryFeedItemFilter(normalized))
}

func (s *FeedService) GetItem(ctx context.Context, rawID string) (model.FeedItem, error) {
	id, err := parseFeedUUID(rawID, "invalid_feed_item_id", "invalid feed item id")
	if err != nil {
		return model.FeedItem{}, err
	}
	item, found, err := s.store.GetItem(ctx, id, true)
	if err != nil {
		return model.FeedItem{}, err
	}
	if !found {
		return model.FeedItem{}, httperr.NewWithCode(http.StatusNotFound, "feed_item_not_found", "feed item not found")
	}
	return item, nil
}

func (s *FeedService) UpdateItemState(ctx context.Context, rawID string, command FeedItemStateCommand) (model.FeedItem, error) {
	id, err := parseFeedUUID(rawID, "invalid_feed_item_id", "invalid feed item id")
	if err != nil {
		return model.FeedItem{}, err
	}
	if command.Read == nil && command.Starred == nil && command.ReadLater == nil {
		return model.FeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "feed_item_state_required", "at least one feed item state field is required")
	}
	item, err := s.store.UpdateItemState(ctx, id, repository.FeedItemStatePatch{
		Read: command.Read, Starred: command.Starred, ReadLater: command.ReadLater,
	})
	return item, mapFeedRepositoryError(err, "feed item not found")
}

func (s *FeedService) MarkItemsRead(ctx context.Context, filter FeedItemFilter) (int64, error) {
	normalized, err := normalizeFeedItemFilter(filter)
	if err != nil {
		return 0, err
	}
	return s.store.MarkItemsRead(ctx, repositoryFeedItemFilter(normalized))
}

func (s *FeedService) AnalyzeItem(ctx context.Context, rawID string) (model.FeedItem, dto.SubmitResponse, error) {
	if s == nil || s.analyzer == nil {
		return model.FeedItem{}, dto.SubmitResponse{}, fmt.Errorf("feed analyzer is not configured")
	}
	id, err := parseFeedUUID(rawID, "invalid_feed_item_id", "invalid feed item id")
	if err != nil {
		return model.FeedItem{}, dto.SubmitResponse{}, err
	}
	item, itemURL, err := s.feedItemForAnalysis(ctx, id)
	if err != nil {
		return model.FeedItem{}, dto.SubmitResponse{}, err
	}
	subscription, err := s.subscriptionForAnalysis(ctx, item.SubscriptionID)
	if err != nil {
		return model.FeedItem{}, dto.SubmitResponse{}, err
	}
	request := newRSSIngestRequest(item, itemURL, subscription.URL)
	submission, err := s.analyzer.AnalyzeRSS(ctx, request)
	if err != nil {
		return model.FeedItem{}, dto.SubmitResponse{}, err
	}
	updated, err := s.associateAnalyzedFeedItem(ctx, item.ID, submission.LinkID)
	return updated, submission, err
}

func (s *FeedService) feedItemForAnalysis(ctx context.Context, id uuid.UUID) (model.FeedItem, string, error) {
	item, found, err := s.store.GetItem(ctx, id, true)
	if err != nil {
		return model.FeedItem{}, "", err
	}
	if !found {
		return model.FeedItem{}, "", httperr.NewWithCode(http.StatusNotFound, "feed_item_not_found", "feed item not found")
	}
	itemURL, err := feedremote.ValidateURL(item.URL)
	if err != nil {
		return model.FeedItem{}, "", httperr.NewWithCode(http.StatusConflict, "feed_item_url_unavailable", "feed item has no analyzable public URL")
	}
	return item, itemURL, nil
}

func (s *FeedService) subscriptionForAnalysis(ctx context.Context, id uuid.UUID) (model.FeedSubscription, error) {
	subscription, found, err := s.store.GetSubscription(ctx, id)
	if err != nil {
		return model.FeedSubscription{}, err
	}
	if !found {
		return model.FeedSubscription{}, httperr.NewWithCode(http.StatusNotFound, "subscription_not_found", "subscription not found")
	}
	return subscription, nil
}

func newRSSIngestRequest(item model.FeedItem, itemURL, feedURL string) RSSIngestRequest {
	useFeedContent := item.Content != nil && utf8.RuneCountInString(strings.TrimSpace(*item.Content)) >= feedContentMinRunes
	request := RSSIngestRequest{
		URL:            itemURL,
		FeedURL:        feedURL,
		ExternalID:     item.ExternalID,
		Title:          item.Title,
		SubscriptionID: item.SubscriptionID,
		ItemID:         item.ID,
		UseFeedContent: useFeedContent,
	}
	if useFeedContent {
		request.Text = pointerValue(item.Content)
		request.HTML = pointerValue(item.ContentHTML)
	}
	return request
}

func (s *FeedService) associateAnalyzedFeedItem(ctx context.Context, itemID uuid.UUID, rawLinkID string) (model.FeedItem, error) {
	linkID, err := uuid.Parse(rawLinkID)
	if err != nil {
		return model.FeedItem{}, fmt.Errorf("parse analyzer link id: %w", err)
	}
	if err := s.store.AssociateItemLink(ctx, itemID, linkID); err != nil {
		return model.FeedItem{}, mapFeedRepositoryError(err, "feed item not found")
	}
	updated, found, err := s.store.GetItem(ctx, itemID, false)
	if err != nil {
		return model.FeedItem{}, err
	}
	if !found {
		return model.FeedItem{}, httperr.NewWithCode(http.StatusNotFound, "feed_item_not_found", "feed item not found")
	}
	return updated, nil
}

// RefreshClaimed executes network work only after the repository has issued a
// durable claim. It completes through the installation-scoped store.
func (s *FeedService) RefreshClaimed(ctx context.Context, subscription model.FeedSubscription) error {
	if subscription.RefreshClaimToken == nil {
		return repository.ErrFeedRefreshClaimLost
	}
	remote, parsed, err := s.remote.FetchAndParse(ctx, subscription.URL, feedremote.ConditionalHeaders{
		ETag: subscription.ETag, LastModified: subscription.LastModified,
	})
	now := s.now().UTC()
	if err != nil {
		next := now.Add(feedFailureBackoff(subscription.FailureCount + 1))
		publicError := safeFeedRefreshError(err)
		if s.logger != nil {
			s.logger.Warn("feed refresh failed", "subscription_id", subscription.ID,
				"error", observability.SafeError(err),
				"next_fetch_at", next)
		}
		return s.store.FailRefresh(ctx, subscription.ID, *subscription.RefreshClaimToken, now, next, publicError)
	}
	_, err = s.store.CompleteRefresh(ctx, repository.FeedRefreshSuccess{
		SubscriptionID: subscription.ID,
		ClaimToken:     *subscription.RefreshClaimToken,
		ETag:           remote.ETag,
		LastModified:   remote.LastModified,
		Parsed:         parsed,
		NotModified:    remote.NotModified,
		Initial:        subscription.LastSuccessAt == nil,
		Now:            now,
	})
	return err
}

func feedFailureBackoff(failureCount int) time.Duration {
	switch failureCount {
	case 1:
		return time.Hour
	case 2:
		return 2 * time.Hour
	case 3:
		return 4 * time.Hour
	case 4:
		return 8 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func safeFeedRefreshError(err error) string {
	message := strings.TrimSpace(err.Error())
	if strings.HasPrefix(message, "feed upstream returned HTTP ") {
		return message
	}
	if errors.Is(err, feedremote.ErrFeedItemLimitExceeded) {
		return feedremote.ErrFeedItemLimitExceeded.Error()
	}
	if errors.Is(err, feedremote.ErrUnsupportedFeedDocument) || errors.Is(err, feedremote.ErrMalformedFeedDocument) {
		return "feed content is not valid RSS, Atom, or RDF"
	}
	if carrier, ok := httperr.As(err); ok {
		return carrier.HTTPMessage()
	}
	category := errsafe.ClassifyError(err)
	if errors.Is(err, context.DeadlineExceeded) || category == "timeout" {
		return "feed request timed out"
	}
	if strings.Contains(message, "parse rss or atom document") || category == "parse" {
		return "feed content is not valid RSS or Atom"
	}
	if category == "unsafe_target" {
		return "feed target is blocked by SSRF protection"
	}
	return "feed is temporarily unreachable"
}

func normalizeFeedItemFilter(filter FeedItemFilter) (FeedItemFilter, error) {
	filter.View = strings.ToLower(strings.TrimSpace(filter.View))
	if filter.View == "" {
		filter.View = "all"
	}
	switch filter.View {
	case "all", "unread", "starred", "later":
	default:
		return filter, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_view", "feed view must be all, unread, starred, or later")
	}
	filter.Query = strings.TrimSpace(filter.Query)
	if utf8.RuneCountInString(filter.Query) > maxFeedSearchRunes {
		return filter, httperr.NewWithCode(http.StatusUnprocessableEntity, "feed_query_too_long", "feed search query exceeds length limit")
	}
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.Limit <= 0 {
		filter.Limit = defaultFeedPageLimit
	}
	if filter.Limit > maxFeedPageLimit {
		filter.Limit = maxFeedPageLimit
	}
	return filter, nil
}

func repositoryFeedItemFilter(filter FeedItemFilter) repository.FeedItemFilter {
	return repository.FeedItemFilter{
		View:           filter.View,
		SubscriptionID: filter.SubscriptionID,
		FolderID:       filter.FolderID,
		Ungrouped:      filter.Ungrouped,
		Query:          filter.Query,
		Page:           filter.Page,
		Limit:          filter.Limit,
	}
}

func (s *FeedService) CreateFolder(ctx context.Context, name string) (model.FeedFolder, error) {
	name, err := validateFeedFolderName(name)
	if err != nil {
		return model.FeedFolder{}, err
	}
	var folder model.FeedFolder
	err = s.withMutation(ctx, "feed-folder:new:"+strings.ToLower(name), func(lockCtx context.Context) error {
		var createErr error
		folder, createErr = s.store.CreateFolder(lockCtx, name)
		return createErr
	})
	return folder, mapFeedFolderError(err)
}

func (s *FeedService) UpdateFolder(ctx context.Context, rawID, name string) (model.FeedFolder, error) {
	id, err := parseFeedUUID(rawID, "invalid_feed_folder_id", "invalid feed folder id")
	if err != nil {
		return model.FeedFolder{}, err
	}
	name, err = validateFeedFolderName(name)
	if err != nil {
		return model.FeedFolder{}, err
	}
	var folder model.FeedFolder
	err = s.withMutation(ctx, "feed-folder:"+id.String(), func(lockCtx context.Context) error {
		var updateErr error
		folder, updateErr = s.store.UpdateFolder(lockCtx, id, name)
		return updateErr
	})
	return folder, mapFeedFolderError(mapFeedRepositoryError(err, "feed folder not found"))
}

func (s *FeedService) DeleteFolder(ctx context.Context, rawID string) error {
	id, err := parseFeedUUID(rawID, "invalid_feed_folder_id", "invalid feed folder id")
	if err != nil {
		return err
	}
	err = s.withMutation(ctx, "feed-folder:"+id.String(), func(lockCtx context.Context) error {
		return s.store.DeleteFolder(lockCtx, id)
	})
	return mapFeedRepositoryError(err, "feed folder not found")
}

func validateFeedFolderName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxFeedFolderRunes {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_folder_name", "folder name must be between 1 and 128 characters")
	}
	return name, nil
}

func parseFeedUUID(raw, code, message string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, httperr.NewWithCode(http.StatusBadRequest, code, message)
	}
	return id, nil
}

func mapFeedRepositoryError(err error, notFoundMessage string) error {
	if errors.Is(err, repository.ErrNotFound) {
		return httperr.NewWithCode(http.StatusNotFound, "feed_not_found", notFoundMessage)
	}
	return err
}

func mapFeedFolderError(err error) error {
	if errors.Is(err, repository.ErrFeedFolderNameConflict) {
		return httperr.NewWithCode(http.StatusConflict, "feed_folder_name_conflict", "a folder with this name already exists")
	}
	return err
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *FeedService) withMutation(ctx context.Context, key string, fn func(context.Context) error) error {
	return s.locker.WithURL(ctx, key, fn)
}

func (s *FeedService) withMutations(ctx context.Context, keys []string, fn func(context.Context) error) error {
	return s.locker.WithURLs(ctx, keys, fn)
}
