package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type inboxProposalStoreStub struct {
	ReaderInboxStore
	inbox            model.ReaderInbox
	claimErr         error
	completeErr      error
	retryErr         error
	failErr          error
	claimCalls       int
	retryCalls       int
	failCalls        int
	completeCalls    int
	lastInboxID      uuid.UUID
	lastRevision     int64
	completedSummary string
	completedTags    []string
}

func (s *inboxProposalStoreStub) GetInbox(context.Context, uuid.UUID) (*model.ReaderInbox, error) {
	item := s.inbox
	item.Tags = append([]string(nil), item.Tags...)
	return &item, nil
}

func (s *inboxProposalStoreStub) ClaimInboxProposal(_ context.Context, id uuid.UUID, revision int64) (*model.ReaderInbox, error) {
	s.claimCalls++
	s.lastInboxID = id
	s.lastRevision = revision
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	item := s.inbox
	item.ProposalStatus = "running"
	return &item, nil
}

func (s *inboxProposalStoreStub) RetryInboxProposal(_ context.Context, id uuid.UUID, revision int64) error {
	s.retryCalls++
	s.lastInboxID = id
	s.lastRevision = revision
	return s.retryErr
}

func (s *inboxProposalStoreStub) FailInboxProposal(_ context.Context, id uuid.UUID, revision int64) error {
	s.failCalls++
	s.lastInboxID = id
	s.lastRevision = revision
	return s.failErr
}

func (s *inboxProposalStoreStub) CompleteInboxProposal(_ context.Context, id uuid.UUID, revision int64, summary string, tags []string) error {
	s.completeCalls++
	s.lastInboxID = id
	s.lastRevision = revision
	if s.completeErr != nil {
		return s.completeErr
	}
	s.completedSummary = summary
	s.completedTags = append([]string(nil), tags...)
	return nil
}

type inboxProposalAIStub struct {
	body    string
	summary string
	tags    []string
	err     error
	calls   int
}

func (s *inboxProposalAIStub) SummarizeInbox(_ context.Context, body string, _ []string) (string, []string, error) {
	s.calls++
	s.body = body
	return s.summary, append([]string(nil), s.tags...), s.err
}

type inboxProposalCommandsStub struct {
	result  InboxProposalResult
	err     error
	command EnsureInboxProposalCommand
}

func (s *inboxProposalCommandsStub) CreateInboxProposal(context.Context, CreateInboxProposalCommand) (InboxProposalResult, error) {
	return s.result, s.err
}

func (s *inboxProposalCommandsStub) EnsureInboxProposal(_ context.Context, command EnsureInboxProposalCommand) (InboxProposalResult, error) {
	s.command = command
	return s.result, s.err
}

func newInboxProposalProcessor(store *inboxProposalStoreStub, ai *inboxProposalAIStub) *ReaderInboxSummaryProcessor {
	return NewReaderInboxSummaryProcessor(store, ai)
}

func TestResummarizeInboxReturnsInboxPollingResource(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	store := &inboxProposalStoreStub{inbox: model.ReaderInbox{ID: inboxID, MetadataRevision: 4, Status: "pending"}}
	commands := &inboxProposalCommandsStub{result: InboxProposalResult{Inbox: &model.ReaderInbox{
		ID: inboxID, URL: "https://example.com", MetadataRevision: 4, Status: "pending", ProposalStatus: "pending",
	}}}
	service := newReaderTestFeatureSet(readerTestStores(store), nil, ReaderApplicationOptions{InboxProposalCommands: commands})

	response, err := service.ResummarizeInbox(context.Background(), inboxID)
	if err != nil {
		t.Fatalf("ResummarizeInbox() error = %v", err)
	}
	if response.ID != inboxID || response.ProposalStatus != "pending" {
		t.Fatalf("ResummarizeInbox() = %+v", response)
	}
	if commands.command.InboxID != inboxID || commands.command.ExpectedMetadataRevision != 4 {
		t.Fatalf("command = %+v", commands.command)
	}
}

func TestRunReaderInboxSummaryJobCommitsOnlyProposalFields(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	title := "user title"
	store := &inboxProposalStoreStub{inbox: model.ReaderInbox{
		ID: inboxID, Title: &title, Body: "current draft body", Tags: []string{"user-tag"},
		Status: "pending", MetadataRevision: 2, ProposalStatus: "pending",
	}}
	ai := &inboxProposalAIStub{summary: "AI summary", tags: []string{"suggested"}}
	service := newInboxProposalProcessor(store, ai)
	args := ReaderInboxSummaryJobArgs{InboxID: inboxID, ExpectedMetadataRevision: 2}

	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 1, 3); err != nil {
		t.Fatalf("RunReaderInboxSummaryJob() error = %v", err)
	}
	if ai.calls != 1 || ai.body != "current draft body" {
		t.Fatalf("AI input = %q, calls = %d", ai.body, ai.calls)
	}
	if store.completeCalls != 1 || store.completedSummary != "AI summary" || len(store.completedTags) != 1 || store.completedTags[0] != "suggested" {
		t.Fatalf("completion = calls:%d summary:%q tags:%v", store.completeCalls, store.completedSummary, store.completedTags)
	}
	if store.lastInboxID != inboxID || store.lastRevision != 2 || store.inbox.MetadataRevision != 2 || *store.inbox.Title != title || store.inbox.Tags[0] != "user-tag" {
		t.Fatalf("user fields or CAS changed: %+v", store)
	}
}

func TestRunReaderInboxSummaryJobSkipsStaleRevisionBeforeAI(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	store := &inboxProposalStoreStub{claimErr: repository.ErrRevisionConflict}
	ai := &inboxProposalAIStub{summary: "stale"}
	service := newInboxProposalProcessor(store, ai)

	if err := service.RunReaderInboxSummaryJob(context.Background(), ReaderInboxSummaryJobArgs{
		InboxID: inboxID, ExpectedMetadataRevision: 1,
	}, 1, 3); err != nil {
		t.Fatalf("stale job error = %v", err)
	}
	if ai.calls != 0 || store.completeCalls != 0 {
		t.Fatalf("stale job reached AI/completion: ai=%d complete=%d", ai.calls, store.completeCalls)
	}
}

func TestRunReaderInboxSummaryJobDropsResultAfterConcurrentEdit(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	store := &inboxProposalStoreStub{
		inbox:       model.ReaderInbox{ID: inboxID, Body: "old body", Status: "pending", MetadataRevision: 1},
		completeErr: repository.ErrRevisionConflict,
	}
	ai := &inboxProposalAIStub{summary: "stale summary"}
	service := newInboxProposalProcessor(store, ai)

	if err := service.RunReaderInboxSummaryJob(context.Background(), ReaderInboxSummaryJobArgs{
		InboxID: inboxID, ExpectedMetadataRevision: 1,
	}, 1, 3); err != nil {
		t.Fatalf("late completion error = %v", err)
	}
	if ai.calls != 1 || store.completeCalls != 1 || store.completedSummary != "" {
		t.Fatalf("late result was persisted: ai=%d complete=%d summary=%q", ai.calls, store.completeCalls, store.completedSummary)
	}
}

func TestRunReaderInboxSummaryJobRetriesThenFails(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	store := &inboxProposalStoreStub{inbox: model.ReaderInbox{ID: inboxID, Body: "body", Status: "pending", MetadataRevision: 1}}
	ai := &inboxProposalAIStub{err: errors.New("provider detail must not persist")}
	service := newInboxProposalProcessor(store, ai)
	args := ReaderInboxSummaryJobArgs{InboxID: inboxID, ExpectedMetadataRevision: 1}

	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 1, 3); err == nil || err.Error() != "reader_inbox_ai_failed" {
		t.Fatalf("retry error = %v", err)
	}
	if store.retryCalls != 1 || store.failCalls != 0 {
		t.Fatalf("first failure transitions = retry:%d fail:%d", store.retryCalls, store.failCalls)
	}
	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 3, 3); err == nil || err.Error() != "reader_inbox_ai_failed" {
		t.Fatalf("terminal error = %v", err)
	}
	if store.retryCalls != 1 || store.failCalls != 1 {
		t.Fatalf("terminal transitions = retry:%d fail:%d", store.retryCalls, store.failCalls)
	}
}

func TestRunReaderInboxSummaryJobStopsWhenFailureTransitionIsStale(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		attempt     int
		maxAttempts int
		configure   func(*inboxProposalStoreStub)
	}{
		{
			name:    "retry transition",
			attempt: 1, maxAttempts: 3,
			configure: func(store *inboxProposalStoreStub) { store.retryErr = repository.ErrReaderInboxProposalNotRunnable },
		},
		{
			name:    "terminal transition",
			attempt: 3, maxAttempts: 3,
			configure: func(store *inboxProposalStoreStub) { store.failErr = repository.ErrRevisionConflict },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			inboxID := uuid.New()
			store := &inboxProposalStoreStub{inbox: model.ReaderInbox{
				ID: inboxID, Body: "body", Status: "pending", MetadataRevision: 1,
			}}
			test.configure(store)
			service := newInboxProposalProcessor(store, &inboxProposalAIStub{err: errors.New("provider failed")})

			err := service.RunReaderInboxSummaryJob(context.Background(), ReaderInboxSummaryJobArgs{
				InboxID: inboxID, ExpectedMetadataRevision: 1,
			}, test.attempt, test.maxAttempts)
			if err != nil {
				t.Fatalf("stale failure transition error = %v, want nil", err)
			}
		})
	}
}

func TestReaderInboxSummaryJobInsertOptsExcludeCompleted(t *testing.T) {
	t.Parallel()

	opts := (ReaderInboxSummaryJobArgs{}).InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false")
	}
	for _, state := range opts.UniqueOpts.ByState {
		if state == rivertype.JobStateCompleted {
			t.Fatal("completed jobs must not block a new proposal")
		}
	}
}
