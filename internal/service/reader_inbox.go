package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

type ReaderInboxCreateCommand struct {
	URL        string
	SourceKind string
	Title      *string
	Body       string
	Note       string
	Summary    *string
	Tags       []string
}

type ReaderInboxPage struct {
	Items        []model.ReaderInboxListItem
	NextCursor   string
	ActiveCount  int
	ExpiredCount int
}

// CreateInbox uses the same URL safety and canonical identity contract as
// Link capture before entering the durable proposal transaction.
func (s *ReaderInboxApplication) CreateInbox(ctx context.Context, command ReaderInboxCreateCommand) (model.ReaderInbox, error) {
	normalizedURL, err := validateURL(command.URL)
	if err != nil {
		return model.ReaderInbox{}, err
	}
	input := model.ReaderInbox{
		URL: strings.TrimSpace(command.URL), IdentityKey: normalizedURL, SourceKind: command.SourceKind,
		Title: command.Title, Body: command.Body, Note: command.Note, Summary: command.Summary,
		Tags: append([]string(nil), command.Tags...), ProposalStatus: "idle",
	}
	if s.inboxCommands == nil {
		return model.ReaderInbox{}, errors.New("create Reader inbox: durable commands are not configured")
	}
	result, err := s.inboxCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: input})
	if err != nil {
		return model.ReaderInbox{}, mapReaderError(err)
	}
	if result.Inbox == nil {
		return model.ReaderInbox{}, errors.New("create Reader inbox: durable command returned nil item")
	}
	return *result.Inbox, nil
}

func (s *ReaderInboxApplication) ListInbox(
	ctx context.Context,
	partition model.ReaderInboxPartition,
	after string,
	limit int,
) (ReaderInboxPage, error) {
	if !partition.Valid() {
		return ReaderInboxPage{}, problem.NewWithCode(problem.Invalid, "invalid_inbox_partition", "partition must be active or expired")
	}
	items, activeCount, expiredCount, next, err := s.inbox.ListInbox(ctx, partition, after, limit)
	if err != nil {
		return ReaderInboxPage{}, mapReaderError(err)
	}
	return ReaderInboxPage{Items: items, NextCursor: next, ActiveCount: activeCount, ExpiredCount: expiredCount}, nil
}

func (s *ReaderInboxApplication) GetInbox(ctx context.Context, id uuid.UUID) (model.ReaderInbox, error) {
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return model.ReaderInbox{}, mapReaderError(err)
	}
	return *item, nil
}

func (s *ReaderInboxApplication) PatchInbox(ctx context.Context, command model.ReaderInboxPatch) (model.ReaderInbox, error) {
	item, err := s.inbox.PatchInbox(ctx, command)
	if err != nil {
		return model.ReaderInbox{}, mapReaderError(err)
	}
	return *item, nil
}

func (s *ReaderInboxApplication) ConfirmInbox(ctx context.Context, id uuid.UUID, expectedRevision *int64) (uuid.UUID, error) {
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return uuid.Nil, mapReaderError(err)
	}
	if item.Title == nil || strings.TrimSpace(*item.Title) == "" {
		return uuid.Nil, problem.NewWithCode(problem.Invalid, "inbox_title_required", "inbox title must not be blank when confirming")
	}
	if expectedRevision != nil && item.MetadataRevision != *expectedRevision {
		return uuid.Nil, mapReaderError(repository.ErrRevisionConflict)
	}
	if s.inboxConfirm == nil {
		return uuid.Nil, errors.New("reader Inbox confirmation commands are not configured")
	}
	linkID, err := s.inboxConfirm.ConfirmInbox(ctx, id, expectedRevision)
	if err != nil {
		return uuid.Nil, mapReaderError(err)
	}
	return linkID, nil
}

func (s *ReaderInboxApplication) DiscardInbox(ctx context.Context, id uuid.UUID) error {
	return mapReaderError(s.inbox.DiscardInbox(ctx, id))
}

func (s *ReaderInboxApplication) RestoreInbox(ctx context.Context, id uuid.UUID) error {
	return mapReaderError(s.inbox.RestoreInbox(ctx, id))
}

func (s *ReaderInboxApplication) ConfirmAIProposals(
	ctx context.Context,
	partition model.ReaderInboxPartition,
) (model.ReaderInboxAIProposalConfirmation, error) {
	if !partition.Valid() {
		return model.ReaderInboxAIProposalConfirmation{}, problem.NewWithCode(problem.Invalid, "invalid_inbox_partition", "partition must be active or expired")
	}
	if s.inboxAIConfirm == nil {
		return model.ReaderInboxAIProposalConfirmation{}, errors.New("reader Inbox confirmation commands are not configured")
	}
	confirmation, err := s.inboxAIConfirm.ConfirmAIProposals(ctx, partition)
	if err != nil {
		return model.ReaderInboxAIProposalConfirmation{}, mapReaderError(err)
	}
	return confirmation, nil
}

func (s *ReaderInboxApplication) ConfirmInboxBulk(
	ctx context.Context,
	confirmations []model.ReaderInboxBulkConfirmation,
) ([]model.ReaderInboxBulkResult, error) {
	if s.inboxBulkConfirm == nil {
		return nil, errors.New("reader Inbox confirmation commands are not configured")
	}
	items, err := s.inboxBulkConfirm.BulkConfirmInbox(ctx, confirmations)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

func (s *ReaderInboxApplication) DiscardInboxBulk(ctx context.Context, ids []uuid.UUID) ([]model.ReaderInboxBulkResult, error) {
	items, err := s.inbox.BulkDiscardInbox(ctx, ids)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}
