package handler

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerLibraryApplicationRoutes struct {
	application *service.ReaderLibraryApplication
}

func NewReaderLibraryRoutes(application *service.ReaderLibraryApplication) ReaderLibraryRoutes {
	if application == nil {
		return nil
	}
	return &readerLibraryApplicationRoutes{application: application}
}

func (r *readerLibraryApplicationRoutes) GetEngagement(ctx context.Context, id string) (dto.ReaderEngagementResponse, error) {
	linkID, err := parseReaderUUID(id, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	engagement, err := r.application.GetEngagement(ctx, linkID)
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	return readerEngagementResponse(engagement), nil
}

func (r *readerLibraryApplicationRoutes) PatchEngagement(ctx context.Context, id string, request dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error) {
	linkID, err := parseReaderUUID(id, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	engagement, err := r.application.PatchEngagement(ctx, model.ReaderEngagementPatch{
		LinkID: linkID, Read: request.Read, Progress: request.Progress, ReadLater: request.ReadLater,
	})
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	return readerEngagementResponse(engagement), nil
}

func (r *readerLibraryApplicationRoutes) Home(ctx context.Context) (dto.ReaderHomeResponse, error) {
	home, err := r.application.Home(ctx)
	if err != nil {
		return dto.ReaderHomeResponse{}, err
	}
	response := dto.ReaderHomeResponse{
		Today: home.Today, Summary: home.Summary, Counts: home.Counts,
		Freshness: string(home.Freshness), Partial: home.Partial, Stale: home.Stale,
	}
	if home.ContinueReading != nil {
		response.ContinueReading = make([]dto.ReaderFeedItemResponse, 0, len(home.ContinueReading))
		for _, item := range home.ContinueReading {
			response.ContinueReading = append(response.ContinueReading, readerFeedItemResponse(item))
		}
	}
	if home.RecentThoughts != nil {
		response.RecentThoughts = make([]dto.ReaderThoughtResponse, 0, len(home.RecentThoughts))
		for _, item := range home.RecentThoughts {
			response.RecentThoughts = append(response.RecentThoughts, readerThoughtResponse(item))
		}
	}
	if home.Todos != nil {
		response.Todos = make([]dto.ReaderTodoResponse, 0, len(home.Todos))
		for _, item := range home.Todos {
			response.Todos = append(response.Todos, readerTodoResponse(item))
		}
	}
	return response, nil
}

func (r *readerLibraryApplicationRoutes) FeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (dto.ReaderFeedResponse, error) {
	page, err := r.application.FeedWithSources(ctx, mode, after, sources, limit)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	response := dto.ReaderFeedResponse{
		Items:      make([]dto.ReaderFeedItemResponse, 0, len(page.Items)),
		NextCursor: page.NextCursor, Mode: page.Mode,
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, readerFeedItemResponse(item))
	}
	return response, nil
}

func (r *readerLibraryApplicationRoutes) FeedbackFeed(ctx context.Context, itemKey, action string) (dto.ReaderFeedFeedbackResponse, error) {
	feedback, err := r.application.FeedbackFeed(ctx, itemKey, action)
	if err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	response := dto.ReaderFeedFeedbackResponse{ItemKey: feedback.ItemKey, Action: feedback.Action}
	if feedback.LinkID != nil {
		linkID := feedback.LinkID.String()
		response.LinkID = &linkID
	}
	return response, nil
}

func (r *readerLibraryApplicationRoutes) RelatedTags(ctx context.Context, id string, limit int) (dto.ReaderRelatedTagsResponse, error) {
	var linkID *uuid.UUID
	if strings.TrimSpace(id) != "" {
		parsed, err := parseReaderUUID(id, "link_id")
		if err != nil {
			return dto.ReaderRelatedTagsResponse{}, err
		}
		linkID = &parsed
	}
	items, err := r.application.RelatedTags(ctx, linkID, limit)
	if err != nil {
		return dto.ReaderRelatedTagsResponse{}, err
	}
	return dto.ReaderRelatedTagsResponse{Items: items}, nil
}

func (r *readerLibraryApplicationRoutes) Activity(ctx context.Context, kind, after string, limit int) (dto.ReaderActivityResponse, error) {
	activity, err := r.application.Activity(ctx, kind, after, limit)
	if err != nil {
		return dto.ReaderActivityResponse{}, err
	}
	response := dto.ReaderActivityResponse{
		Kind:       activity.Kind,
		Tags:       make([]dto.ReaderTagActivityResponse, 0, len(activity.Items)),
		Domains:    make([]dto.ReaderDomainActivityResponse, 0, len(activity.Items)),
		NextCursor: activity.NextCursor,
	}
	for _, item := range activity.Items {
		switch item.Kind {
		case model.ReaderActivityKindTag:
			response.Tags = append(response.Tags, dto.ReaderTagActivityResponse{Tag: item.Key, LastAt: item.LastAt})
		case model.ReaderActivityKindDomain:
			response.Domains = append(response.Domains, dto.ReaderDomainActivityResponse{Domain: item.Key, LastAt: item.LastAt})
		}
	}
	return response, nil
}

func (r *readerLibraryApplicationRoutes) PatchLinkMetadata(ctx context.Context, id string, request dto.ReaderLinkMetadataRequest, expected int64) (dto.ReaderLinkMetadataResponse, error) {
	linkID, err := parseReaderUUID(id, "link_id")
	if err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	if !request.Complete() {
		return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeMetadataFieldsRequired, "title, summary, and tags are required")
	}
	update, err := r.application.PatchLinkMetadata(ctx, service.ReaderLinkMetadataCommand{
		LinkID: linkID, Title: request.Title, Summary: request.Summary,
		Tags: request.Tags, ExpectedRevision: expected,
	})
	if err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	return dto.ReaderLinkMetadataResponse{LinkID: linkID.String(), MetadataRevision: update.MetadataRevision}, nil
}

func (r *readerLibraryApplicationRoutes) CompleteAI(ctx context.Context, request dto.ReaderAIRequest) (dto.ReaderAIResponse, error) {
	result, err := r.application.CompleteAI(ctx, service.ReaderAICommand{
		Prompt: request.Prompt, Scope: request.Scope, LinkID: request.LinkID, SelectedText: request.SelectedText,
	})
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	return dto.ReaderAIResponse{Enabled: result.Enabled, Answer: result.Answer, Model: result.Model}, nil
}

func readerEngagementResponse(item model.ReaderEngagement) dto.ReaderEngagementResponse {
	return dto.ReaderEngagementResponse{
		LinkID: item.LinkID.String(), Read: item.Read, Progress: item.Progress,
		ReadLater: item.ReadLater, LastOpened: item.LastOpened, UpdatedAt: item.UpdatedAt,
	}
}

func readerFeedItemResponse(item model.ReaderFeedItem) dto.ReaderFeedItemResponse {
	response := dto.ReaderFeedItemResponse{
		Key: item.Key, Source: item.Source, ResourceKey: item.ResourceIdentity(),
		Title: item.Title, Summary: item.Summary, URL: item.URL,
		Read: item.Read, ReadLater: item.ReadLater, Saved: item.Saved, EventAt: item.VisibleEventAt(),
	}
	if item.LinkID != nil {
		value := item.LinkID.String()
		response.LinkID = &value
	}
	if item.InboxID != nil {
		value := item.InboxID.String()
		response.InboxID = &value
	}
	if item.FeedItemID != nil {
		value := item.FeedItemID.String()
		response.FeedItemID = &value
	}
	return response
}

var _ ReaderLibraryRoutes = (*readerLibraryApplicationRoutes)(nil)
