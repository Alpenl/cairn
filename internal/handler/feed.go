package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
	"webtag/internal/model"
	"webtag/internal/service"
)

// FeedSubscriptionRoutes is owned by the subscription route factory. It
// contains only behavior mounted below /subscriptions.
type FeedSubscriptionRoutes interface {
	ListSubscriptions(context.Context, string) (model.FeedSubscriptionsResponse, error)
	Discover(context.Context, string) (model.FeedDiscoveryResponse, error)
	Subscribe(context.Context, string, *uuid.UUID, bool) (model.FeedSubscription, error)
	UpdateSubscription(context.Context, string, service.FeedSubscriptionUpdateCommand) (model.FeedSubscription, error)
	Unsubscribe(context.Context, string) error
	ScheduleRefresh(context.Context, string) error
	ScheduleAllRefreshes(context.Context) (int64, error)
	ExportOPML(context.Context) ([]byte, error)
	ImportOPML(context.Context, []byte) (model.OPMLImportResponse, error)
}

// FeedItemRoutes is owned by the feed-item route factory.
type FeedItemRoutes interface {
	ListItems(context.Context, service.FeedItemFilter) (model.PaginatedFeedItems, error)
	GetItem(context.Context, string) (model.FeedItem, error)
	UpdateItemState(context.Context, string, service.FeedItemStateCommand) (model.FeedItem, error)
	MarkItemsRead(context.Context, service.FeedItemFilter) (int64, error)
	AnalyzeItem(context.Context, string) (model.FeedItem, dto.SubmitResponse, error)
}

// FeedFolderRoutes is owned by the folder route factory.
type FeedFolderRoutes interface {
	CreateFolder(context.Context, string) (model.FeedFolder, error)
	UpdateFolder(context.Context, string, string) (model.FeedFolder, error)
	DeleteFolder(context.Context, string) error
}

// FeedService is the composition shape accepted by RegisterFeedRoutes. The
// actual handlers consume the three narrower interfaces above.
type FeedService interface {
	FeedSubscriptionRoutes
	FeedItemRoutes
	FeedFolderRoutes
}

type feedURLRequest struct {
	URL string `json:"url" binding:"required,max=2048"`
}

type feedSubscriptionRequest struct {
	URL      string          `json:"url" binding:"required,max=2048"`
	FolderID json.RawMessage `json:"folder_id"`
}

type feedSubscriptionUpdateRequest struct {
	FolderID json.RawMessage `json:"folder_id"`
	Title    *string         `json:"title" binding:"omitempty,max=1024"`
}

type feedFolderRequest struct {
	Name string `json:"name" binding:"required,max=128"`
}

type feedItemStateRequest struct {
	Read      *bool `json:"read"`
	Starred   *bool `json:"starred"`
	ReadLater *bool `json:"read_later"`
}

type markReadRequest struct {
	View           string `json:"view"`
	SubscriptionID string `json:"subscription_id"`
	FolderID       string `json:"folder_id"`
	Query          string `json:"q"`
}

// RegisterFeedRoutes mounts canonical /api routes and the repository's
// established /api/v1 aliases.
func RegisterFeedRoutes(router gin.IRouter, feeds FeedService) {
	if feeds == nil {
		return
	}
	for _, prefix := range apiRoutePrefixes {
		registerFeedSubscriptionRoutes(router, prefix, feeds)
		registerFeedItemRoutes(router, prefix, feeds)
		registerFeedFolderRoutes(router, prefix, feeds)
	}
}

func registerFeedSubscriptionRoutes(router gin.IRouter, prefix string, feeds FeedSubscriptionRoutes) {
	router.GET(prefix+"/subscriptions/opml", exportFeedOPML(feeds))
	router.POST(prefix+"/subscriptions/opml", importFeedOPML(feeds))
	router.POST(prefix+"/subscriptions/discover", discoverFeeds(feeds))
	router.POST(prefix+"/subscriptions/refresh", scheduleAllFeedRefreshes(feeds))
	router.GET(prefix+"/subscriptions", listFeedSubscriptions(feeds))
	router.POST(prefix+"/subscriptions", createFeedSubscription(feeds))
	router.PUT(prefix+"/subscriptions/:id", updateFeedSubscription(feeds))
	router.DELETE(prefix+"/subscriptions/:id", deleteFeedSubscription(feeds))
	router.POST(prefix+"/subscriptions/:id/refresh", scheduleFeedRefresh(feeds))
}

func registerFeedItemRoutes(router gin.IRouter, prefix string, feeds FeedItemRoutes) {
	router.GET(prefix+"/feed-items", listFeedItems(feeds))
	router.POST(prefix+"/feed-items/mark-read", markFeedItemsRead(feeds))
	router.GET(prefix+"/feed-items/:id", getFeedItem(feeds))
	router.PUT(prefix+"/feed-items/:id/state", updateFeedItemState(feeds))
	router.POST(prefix+"/feed-items/:id/analyze", analyzeFeedItem(feeds))
}

func registerFeedFolderRoutes(router gin.IRouter, prefix string, feeds FeedFolderRoutes) {
	router.POST(prefix+"/feed-folders", createFeedFolder(feeds))
	router.PUT(prefix+"/feed-folders/:id", updateFeedFolder(feeds))
	router.DELETE(prefix+"/feed-folders/:id", deleteFeedFolder(feeds))
}

func listFeedSubscriptions(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		response, err := feeds.ListSubscriptions(c.Request.Context(), c.Query("url"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func discoverFeeds(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedURLRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := feeds.Discover(c.Request.Context(), request.URL)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func createFeedSubscription(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedSubscriptionRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		folderID, setFolder, err := decodeOptionalUUID(request.FolderID)
		if err != nil {
			writeError(c, err)
			return
		}
		subscription, err := feeds.Subscribe(c.Request.Context(), request.URL, folderID, setFolder)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"subscription": subscription})
	}
}

func updateFeedSubscription(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedSubscriptionUpdateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		folderID, setFolder, err := decodeOptionalUUID(request.FolderID)
		if err != nil {
			writeError(c, err)
			return
		}
		subscription, err := feeds.UpdateSubscription(c.Request.Context(), c.Param("id"), service.FeedSubscriptionUpdateCommand{
			FolderID: folderID, SetFolder: setFolder, Title: request.Title,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"subscription": subscription})
	}
}

func deleteFeedSubscription(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := feeds.Unsubscribe(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func scheduleFeedRefresh(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := feeds.ScheduleRefresh(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"scheduled": true})
	}
}

func scheduleAllFeedRefreshes(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		count, err := feeds.ScheduleAllRefreshes(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"scheduled": count})
	}
}

func listFeedItems(feeds FeedItemRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, err := feedItemFilterFromQuery(c)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := feeds.ListItems(c.Request.Context(), filter)
		if err != nil {
			writeError(c, err)
			return
		}
		middleware.CacheableJSON(c, http.StatusOK, response)
	}
}

func getFeedItem(feeds FeedItemRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		item, err := feeds.GetItem(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, item)
	}
}

func updateFeedItemState(feeds FeedItemRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedItemStateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		item, err := feeds.UpdateItemState(c.Request.Context(), c.Param("id"), service.FeedItemStateCommand{
			Read: request.Read, Starred: request.Starred, ReadLater: request.ReadLater,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, item)
	}
}

func markFeedItemsRead(feeds FeedItemRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request markReadRequest
		if c.Request.ContentLength != 0 {
			if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
				return
			}
		}
		filter, err := feedItemFilterFromValues(request.View, request.SubscriptionID, request.FolderID, request.Query, 1, 100)
		if err != nil {
			writeError(c, err)
			return
		}
		updated, err := feeds.MarkItemsRead(c.Request.Context(), filter)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"updated": updated})
	}
}

func analyzeFeedItem(feeds FeedItemRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		item, submission, err := feeds.AnalyzeItem(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"item": item, "submission": submission})
	}
}

func createFeedFolder(feeds FeedFolderRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedFolderRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		folder, err := feeds.CreateFolder(c.Request.Context(), request.Name)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"folder": folder})
	}
}

func updateFeedFolder(feeds FeedFolderRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request feedFolderRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		folder, err := feeds.UpdateFolder(c.Request.Context(), c.Param("id"), request.Name)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"folder": folder})
	}
}

func deleteFeedFolder(feeds FeedFolderRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := feeds.DeleteFolder(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func exportFeedOPML(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		payload, err := feeds.ExportOPML(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		c.Header("Content-Disposition", `attachment; filename="webtag-subscriptions.opml"`)
		c.Data(http.StatusOK, "text/x-opml; charset=utf-8", payload)
	}
}

func importFeedOPML(feeds FeedSubscriptionRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		payload, err := readOPMLPayload(c)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := feeds.ImportOPML(c.Request.Context(), payload)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readOPMLPayload(c *gin.Context) ([]byte, error) {
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, defaultMaxJSONBodyBytes+1))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, httperr.NewWithCode(http.StatusRequestEntityTooLarge, "opml_body_too_large", "OPML body exceeds size limit")
		}
		return nil, httperr.NewWithCode(http.StatusBadRequest, "invalid_opml_body", "unable to read OPML body")
	}
	if int64(len(body)) > defaultMaxJSONBodyBytes {
		return nil, httperr.NewWithCode(http.StatusRequestEntityTooLarge, "opml_body_too_large", "OPML body exceeds size limit")
	}
	if strings.Contains(contentType, "json") {
		var request struct {
			OPML string `json:"opml"`
		}
		if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.OPML) == "" {
			return nil, httperr.NewWithCode(http.StatusBadRequest, "invalid_opml_json", "JSON body must contain a non-empty opml string")
		}
		return []byte(request.OPML), nil
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, httperr.NewWithCode(http.StatusBadRequest, "empty_opml_body", "OPML body is required")
	}
	return body, nil
}

func decodeOptionalUUID(raw json.RawMessage) (*uuid.UUID, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, serviceInvalidID("invalid feed folder id")
	}
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, true, serviceInvalidID("invalid feed folder id")
	}
	return &id, true, nil
}

func serviceInvalidID(message string) error {
	return httperr.NewWithCode(http.StatusBadRequest, "invalid_feed_id", message)
}

func feedItemFilterFromQuery(c *gin.Context) (service.FeedItemFilter, error) {
	return feedItemFilterFromValues(c.Query("view"), c.Query("subscription_id"), c.Query("folder_id"), c.Query("q"), queryInt(c, "page", 1), queryInt(c, "limit", 20))
}

func feedItemFilterFromValues(view, subscriptionRaw, folderRaw, query string, page, limit int) (service.FeedItemFilter, error) {
	filter := service.FeedItemFilter{View: view, Query: query, Page: page, Limit: limit}
	if strings.TrimSpace(subscriptionRaw) != "" {
		id, err := uuid.Parse(strings.TrimSpace(subscriptionRaw))
		if err != nil {
			return filter, serviceInvalidID("invalid subscription id")
		}
		filter.SubscriptionID = &id
	}
	if strings.EqualFold(strings.TrimSpace(folderRaw), "ungrouped") {
		filter.Ungrouped = true
	} else if strings.TrimSpace(folderRaw) != "" {
		id, err := uuid.Parse(strings.TrimSpace(folderRaw))
		if err != nil {
			return filter, serviceInvalidID("invalid feed folder id")
		}
		filter.FolderID = &id
	}
	return filter, nil
}
