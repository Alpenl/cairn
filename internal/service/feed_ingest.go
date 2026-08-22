package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
)

// RSSIngestRequest is the internal bridge between a feed item and the normal
// link parse pipeline. It is intentionally not exposed as a general ingest
// source on /api/ingest; only the feed service can create source_kind=rss.
type RSSIngestRequest struct {
	URL            string
	FeedURL        string
	ExternalID     string
	Title          string
	Text           string
	HTML           string
	SubscriptionID uuid.UUID
	ItemID         uuid.UUID
	UseFeedContent bool
	Destination    string
}

func (s *IngestService) AnalyzeRSS(ctx context.Context, request RSSIngestRequest) (dto.SubmitResponse, error) {
	rawURL, err := validateURL(request.URL)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	destination, err := normalizeCaptureDestination(request.Destination, captureDestinationLibrary)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	params := LinkCapture{
		URL:         strings.TrimSpace(request.URL),
		Destination: destination,
		Status:      model.LinkStatusPending,
		SourceKind:  "rss",
		SourceKey:   rawURL,
		SourceMetadata: withOriginalURL(map[string]any{
			"feed_subscription_id": request.SubscriptionID.String(),
			"feed_item_id":         request.ItemID.String(),
			"feed_external_id":     request.ExternalID,
			"feed_url":             request.FeedURL,
		}, request.URL, rawURL),
		RequestedLibraryKind: model.RequestedLibraryKindReading,
	}
	if destination == captureDestinationInbox {
		return s.submitToInbox(ctx, rawURL, params)
	}
	if title := strings.TrimSpace(request.Title); title != "" {
		params.InputTitle = &title
	}
	if request.UseFeedContent {
		if text := strings.TrimSpace(request.Text); text != "" {
			params.InputText = &text
		}
		if html := strings.TrimSpace(request.HTML); html != "" {
			params.InputHTML = &html
		}
	}

	var response dto.SubmitResponse
	err = s.core.locker.WithURL(ctx, rawURL, func(lockCtx context.Context) error {
		projection, lookupErr := s.core.reader.GetSubmitLookupByURL(lockCtx, rawURL)
		if lookupErr != nil {
			return lookupErr
		}
		if projection == nil {
			response, lookupErr = s.core.createNewLink(lockCtx, params)
			return lookupErr
		}
		link := submitCandidateFromLookup(projection)
		if link.Status == model.LinkStatusFailed {
			response, lookupErr = s.core.requeueExisting(lockCtx, link.ID, &params)
			return lookupErr
		}
		response, lookupErr = s.core.submitExisting(lockCtx, link, &params)
		return lookupErr
	})
	return response, err
}

var _ FeedLinkAnalyzer = (*IngestService)(nil)
