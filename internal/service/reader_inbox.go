package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// CreateInbox is a collection entry point, so it goes through the same
// validateURL contract as /api/links and /api/ingest.
//
// It previously trusted the binding tag's `url` rule alone, which accepts any
// scheme that carries a host and performs no SSRF check. Confirming such an
// item copies its URL verbatim into links.url and links.source_key, so a
// `file://` or private-network address reached the collection write path with
// nothing in between — and a variant spelling created a second reading record
// that /api/links would have deduplicated.
func (s *ReaderVNextService) CreateInbox(ctx context.Context, request dto.ReaderInboxCreateRequest) (dto.ReaderInboxResponse, error) {
	normalizedURL, err := validateURL(request.URL)
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	input := model.ReaderInbox{URL: strings.TrimSpace(request.URL), IdentityKey: normalizedURL, SourceKind: request.SourceKind, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: append([]string(nil), request.Tags...), ProposalStatus: "idle"}
	if s.inboxCommands == nil {
		return dto.ReaderInboxResponse{}, errors.New("create Reader inbox: durable commands are not configured")
	}
	result, commandErr := s.inboxCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: input})
	if commandErr != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(commandErr)
	}
	item := result.Inbox
	if item == nil {
		return dto.ReaderInboxResponse{}, errors.New("create Reader inbox: durable command returned nil item")
	}
	return inboxResponse(*item), nil
}

func (s *ReaderVNextService) ListInbox(ctx context.Context, rawPartition, after string, limit int) (dto.ReaderInboxResponsePage, error) {
	partition, err := parseReaderInboxPartition(rawPartition, true)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, err
	}
	items, activeCount, expiredCount, next, err := s.inbox.ListInbox(ctx, partition, after, limit)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, mapReaderError(err)
	}
	out := dto.ReaderInboxResponsePage{
		Items:        make([]dto.ReaderInboxListItemResponse, 0, len(items)),
		NextCursor:   next,
		ActiveCount:  activeCount,
		ExpiredCount: expiredCount,
	}
	for _, item := range items {
		out.Items = append(out.Items, inboxListItemResponse(item))
	}
	return out, nil
}

// inboxListItemResponse maps the queue projection. It exists so the list path
// cannot accidentally reuse inboxResponse and reintroduce the body/note the
// projection was created to leave behind.
func inboxListItemResponse(item model.ReaderInboxListItem) dto.ReaderInboxListItemResponse {
	tags := item.Tags
	if tags == nil {
		tags = []string{}
	}
	return dto.ReaderInboxListItemResponse{
		ID:               item.ID.String(),
		URL:              item.URL,
		SourceKind:       item.SourceKind,
		Title:            item.Title,
		Preview:          item.Preview,
		Tags:             tags,
		Status:           item.Status,
		MetadataRevision: item.MetadataRevision,
		Expired:          item.Expired,
		UpdatedAt:        item.UpdatedAt,
	}
}

func (s *ReaderVNextService) GetInbox(ctx context.Context, rawID string) (dto.ReaderInboxResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

func inboxResponse(item model.ReaderInbox) dto.ReaderInboxResponse {
	return dto.ReaderInboxResponse{ID: item.ID.String(), URL: item.URL, SourceKind: item.SourceKind, Title: item.Title, Body: item.Body, Note: item.Note, Summary: item.Summary, SuggestedTags: item.SuggestedTags, ProposalStatus: item.ProposalStatus, Tags: item.Tags, Status: item.Status, MetadataRevision: item.MetadataRevision, ExpiresAt: item.ExpiresAt, Expired: item.Expired, DeletedAt: item.DeletedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func (s *ReaderVNextService) PatchInbox(ctx context.Context, rawID string, request dto.ReaderInboxPatchRequest, expected int64) (dto.ReaderInboxResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := s.inbox.PatchInbox(ctx, model.ReaderInboxPatch{ID: id, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

func (s *ReaderVNextService) ConfirmInbox(ctx context.Context, rawID string, expectedRevision int64) (map[string]string, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return nil, err
	}
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return nil, mapReaderError(err)
	}
	if item.Title == nil || strings.TrimSpace(*item.Title) == "" {
		return nil, problem.NewWithCode(problem.Invalid, "inbox_title_required", "inbox title must not be blank when confirming")
	}
	if expectedRevision >= 0 && item.MetadataRevision != expectedRevision {
		return nil, mapReaderError(repository.ErrRevisionConflict)
	}
	var expected *int64
	if expectedRevision >= 0 {
		expected = &expectedRevision
	}
	linkID, err := s.inbox.ConfirmInbox(ctx, id, expected)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return map[string]string{"target_kind": "link", "link_id": linkID.String(), "status": "confirmed"}, nil
}

func (s *ReaderVNextService) DiscardInbox(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.inbox.DiscardInbox(ctx, id))
}

// RestoreInbox is an Inbox-specific lifecycle command because an expired live
// row is not trashed. The repository atomically renews only expired pending
// rows and leaves all user/AI-owned content untouched.
func (s *ReaderVNextService) RestoreInbox(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.inbox.RestoreInbox(ctx, id))
}

// ConfirmAIProposals confirms the next stable server-selected set of completed
// AI proposals. The client supplies only the partition; eligibility and the
// atomic transition stay at the repository boundary.
func (s *ReaderVNextService) ConfirmAIProposals(ctx context.Context, rawPartition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
	partition, err := parseReaderInboxPartition(rawPartition, false)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, err
	}
	confirmation, err := s.inbox.ConfirmAIProposals(ctx, partition)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, mapReaderError(err)
	}
	response := dto.ReaderInboxConfirmAIProposalsResponse{
		Atomic:         true,
		Items:          make([]dto.ReaderInboxBulkItemResponse, 0, len(confirmation.Items)),
		RemainingCount: confirmation.RemainingCount,
	}
	for _, item := range confirmation.Items {
		out := dto.ReaderInboxBulkItemResponse{InboxID: item.ID.String(), Status: item.Status}
		if item.LinkID != nil {
			linkID := item.LinkID.String()
			out.LinkID = &linkID
		}
		response.Items = append(response.Items, out)
	}
	return response, nil
}

func parseReaderInboxBulkIDs(rawIDs []string) ([]uuid.UUID, error) {
	if len(rawIDs) == 0 || len(rawIDs) > 100 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch", "inbox batch must contain between 1 and 100 ids")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	seen := make(map[uuid.UUID]struct{}, len(rawIDs))
	for _, rawID := range rawIDs {
		id, err := readerUUID(rawID, "inbox_id")
		if err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch", "inbox batch must contain at least one unique id")
	}
	return ids, nil
}

// ConfirmInboxBulk is an internal service seam for the future batch endpoint.
// The repository commits the whole batch atomically and returns the same link
// id for retries of already-confirmed captures.
func (s *ReaderVNextService) ConfirmInboxBulk(ctx context.Context, rawIDs []string, rawExpectedRevisions map[string]int64) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	expectedRevisions := make(map[uuid.UUID]int64, len(rawExpectedRevisions))
	for rawID, revision := range rawExpectedRevisions {
		id, parseErr := readerUUID(rawID, "expected_revision inbox_id")
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
	items, err := s.inbox.BulkConfirmInbox(ctx, confirmations)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

// DiscardInboxBulk is the matching internal seam for batch discard. A trashed
// item is safe to retry; a confirmed item is rejected so a bulk
// action cannot remove a saved link's source capture accidentally.
func (s *ReaderVNextService) DiscardInboxBulk(ctx context.Context, rawIDs []string) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	items, err := s.inbox.BulkDiscardInbox(ctx, ids)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}
