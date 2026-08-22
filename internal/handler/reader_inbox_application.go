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

type readerInboxApplicationRoutes struct {
	application *service.ReaderInboxApplication
}

func NewReaderInboxRoutes(application *service.ReaderInboxApplication) ReaderInboxRoutes {
	if application == nil {
		return nil
	}
	return &readerInboxApplicationRoutes{application: application}
}

func (r *readerInboxApplicationRoutes) CreateInbox(ctx context.Context, request dto.ReaderInboxCreateRequest) (dto.ReaderInboxResponse, error) {
	item, err := r.application.CreateInbox(ctx, service.ReaderInboxCreateCommand{
		URL: request.URL, SourceKind: request.SourceKind, Title: request.Title,
		Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: request.Tags,
	})
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	return readerInboxResponse(item), nil
}

func (r *readerInboxApplicationRoutes) ListInbox(ctx context.Context, rawPartition, after string, limit int) (dto.ReaderInboxResponsePage, error) {
	partition, err := parseReaderInboxPartition(rawPartition, true)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, err
	}
	page, err := r.application.ListInbox(ctx, partition, after, limit)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, err
	}
	response := dto.ReaderInboxResponsePage{
		Items:      make([]dto.ReaderInboxListItemResponse, 0, len(page.Items)),
		NextCursor: page.NextCursor, ActiveCount: page.ActiveCount, ExpiredCount: page.ExpiredCount,
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, readerInboxListItemResponse(item))
	}
	return response, nil
}

func (r *readerInboxApplicationRoutes) GetInbox(ctx context.Context, rawID string) (dto.ReaderInboxResponse, error) {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := r.application.GetInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	return readerInboxResponse(item), nil
}

func (r *readerInboxApplicationRoutes) PatchInbox(ctx context.Context, rawID string, request dto.ReaderInboxPatchRequest, expected int64) (dto.ReaderInboxResponse, error) {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := r.application.PatchInbox(ctx, model.ReaderInboxPatch{
		ID: id, Title: request.Title, Body: request.Body, Note: request.Note,
		Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected,
	})
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	return readerInboxResponse(item), nil
}

func (r *readerInboxApplicationRoutes) ConfirmInbox(ctx context.Context, rawID string, expectedRevision int64) (map[string]string, error) {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return nil, err
	}
	var expected *int64
	if expectedRevision >= 0 {
		expected = &expectedRevision
	}
	linkID, err := r.application.ConfirmInbox(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	return map[string]string{"target_kind": "link", "link_id": linkID.String(), "status": "confirmed"}, nil
}

func (r *readerInboxApplicationRoutes) ConfirmAIProposals(ctx context.Context, rawPartition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
	partition, err := parseReaderInboxPartition(rawPartition, false)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, err
	}
	confirmation, err := r.application.ConfirmAIProposals(ctx, partition)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, err
	}
	response := dto.ReaderInboxConfirmAIProposalsResponse{
		Atomic: true, Items: readerInboxBulkItems(confirmation.Items), RemainingCount: confirmation.RemainingCount,
	}
	return response, nil
}

func (r *readerInboxApplicationRoutes) DiscardInbox(ctx context.Context, rawID string) error {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return r.application.DiscardInbox(ctx, id)
}

func (r *readerInboxApplicationRoutes) RestoreInbox(ctx context.Context, rawID string) error {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return r.application.RestoreInbox(ctx, id)
}

func (r *readerInboxApplicationRoutes) ConfirmInboxBulk(ctx context.Context, rawIDs []string, rawExpectedRevisions map[string]int64) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	expectedRevisions := make(map[uuid.UUID]int64, len(rawExpectedRevisions))
	for rawID, revision := range rawExpectedRevisions {
		id, parseErr := parseReaderUUID(rawID, "expected_revision inbox_id")
		if parseErr != nil || revision < 0 {
			return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must use requested inbox ids and non-negative revisions")
		}
		expectedRevisions[id] = revision
	}
	if len(expectedRevisions) > 0 && len(expectedRevisions) != len(ids) {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
	}
	confirmations := make([]model.ReaderInboxBulkConfirmation, 0, len(ids))
	for _, id := range ids {
		var expectedRevision *int64
		if len(expectedRevisions) > 0 {
			revision, ok := expectedRevisions[id]
			if !ok {
				return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
			}
			revisionCopy := revision
			expectedRevision = &revisionCopy
		}
		confirmations = append(confirmations, model.ReaderInboxBulkConfirmation{ID: id, ExpectedRevision: expectedRevision})
	}
	return r.application.ConfirmInboxBulk(ctx, confirmations)
}

func (r *readerInboxApplicationRoutes) DiscardInboxBulk(ctx context.Context, rawIDs []string) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	return r.application.DiscardInboxBulk(ctx, ids)
}

func (r *readerInboxApplicationRoutes) ResummarizeInbox(ctx context.Context, rawID string) (dto.ReaderInboxResponse, error) {
	id, err := parseReaderUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := r.application.ResummarizeInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	return readerInboxResponse(item), nil
}

func parseReaderInboxPartition(raw string, defaultActive bool) (model.ReaderInboxPartition, error) {
	partition := model.ReaderInboxPartition(strings.TrimSpace(raw))
	if partition == "" && defaultActive {
		return model.ReaderInboxPartitionActive, nil
	}
	if !partition.Valid() {
		return "", problem.NewWithCode(problem.Invalid, "invalid_inbox_partition", "partition must be active or expired")
	}
	return partition, nil
}

func parseReaderInboxBulkIDs(rawIDs []string) ([]uuid.UUID, error) {
	if len(rawIDs) == 0 || len(rawIDs) > 100 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch", "inbox batch must contain between 1 and 100 ids")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	seen := make(map[uuid.UUID]struct{}, len(rawIDs))
	for _, rawID := range rawIDs {
		id, err := parseReaderUUID(rawID, "inbox_id")
		if err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func readerInboxResponse(item model.ReaderInbox) dto.ReaderInboxResponse {
	return dto.ReaderInboxResponse{
		ID: item.ID.String(), URL: item.URL, SourceKind: item.SourceKind, Title: item.Title,
		Body: item.Body, Note: item.Note, Summary: item.Summary, SuggestedTags: item.SuggestedTags,
		ProposalStatus: item.ProposalStatus, Tags: item.Tags, Status: item.Status,
		MetadataRevision: item.MetadataRevision, ExpiresAt: item.ExpiresAt, Expired: item.Expired,
		DeletedAt: item.DeletedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func readerInboxListItemResponse(item model.ReaderInboxListItem) dto.ReaderInboxListItemResponse {
	tags := item.Tags
	if tags == nil {
		tags = []string{}
	}
	return dto.ReaderInboxListItemResponse{
		ID: item.ID.String(), URL: item.URL, SourceKind: item.SourceKind, Title: item.Title,
		Preview: item.Preview, Tags: tags, Status: item.Status, MetadataRevision: item.MetadataRevision,
		Expired: item.Expired, UpdatedAt: item.UpdatedAt,
	}
}

func readerInboxBulkItems(items []model.ReaderInboxBulkResult) []dto.ReaderInboxBulkItemResponse {
	response := make([]dto.ReaderInboxBulkItemResponse, 0, len(items))
	for _, item := range items {
		out := dto.ReaderInboxBulkItemResponse{InboxID: item.ID.String(), Status: item.Status}
		if item.LinkID != nil {
			linkID := item.LinkID.String()
			out.LinkID = &linkID
		}
		response = append(response, out)
	}
	return response
}

var _ ReaderInboxRoutes = (*readerInboxApplicationRoutes)(nil)
