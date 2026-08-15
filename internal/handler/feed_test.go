package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/service"
)

type feedHandlerStub struct {
	FeedService
	overview   model.FeedSubscriptionsResponse
	itemFilter service.FeedItemFilter
	items      model.PaginatedFeedItems
}

func (s *feedHandlerStub) ListSubscriptions(context.Context, string) (model.FeedSubscriptionsResponse, error) {
	return s.overview, nil
}

func (s *feedHandlerStub) ListItems(_ context.Context, filter service.FeedItemFilter) (model.PaginatedFeedItems, error) {
	s.itemFilter = filter
	return s.items, nil
}

func TestRegisterFeedRoutesMountsCanonicalAndV1SubscriptionLists(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	subscriptionID := uuid.New()
	stub := &feedHandlerStub{overview: model.FeedSubscriptionsResponse{
		Folders:       []model.FeedFolder{},
		Subscriptions: []model.FeedSubscription{{ID: subscriptionID, URL: "https://example.com/feed", FeedURL: "https://example.com/feed", Active: true}},
		Counts:        model.FeedCounts{All: 2, Unread: 1},
	}}
	router := gin.New()
	RegisterFeedRoutes(router, stub)
	for _, path := range []string{"/api/subscriptions", "/api/v1/subscriptions"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s", path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), subscriptionID.String()) || !strings.Contains(response.Body.String(), `"counts"`) {
			t.Fatalf("GET %s body = %s", path, response.Body.String())
		}
	}
}

func TestFeedItemsUngroupedQueryMapsToApplicationFilter(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	stub := &feedHandlerStub{items: model.PaginatedFeedItems{Items: []model.FeedItem{}, Page: 1, Limit: 20}}
	router := gin.New()
	RegisterFeedRoutes(router, stub)
	request := httptest.NewRequest(http.MethodGet, "/api/feed-items?folder_id=ungrouped&view=unread", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if !stub.itemFilter.Ungrouped || stub.itemFilter.FolderID != nil || stub.itemFilter.View != "unread" {
		t.Fatalf("filter = %#v", stub.itemFilter)
	}
}

func TestImportFeedOPMLRejectsInvalidJSONOnce(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterFeedRoutes(router, &feedHandlerStub{})
	request := httptest.NewRequest(http.MethodPost, "/api/subscriptions/opml", strings.NewReader(`{"opml":`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if strings.Count(response.Body.String(), `"error"`) != 1 {
		t.Fatalf("expected one error envelope, body=%s", response.Body.String())
	}
}
