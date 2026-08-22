package durablework

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
	"webtag/internal/service"
)

type inboxRepositoryStub struct {
	created         *model.ReaderInbox
	started         *model.ReaderInbox
	createErr       error
	startErr        error
	createCalls     int
	startCalls      int
	startedID       uuid.UUID
	startedRevision int64
}

func (s *inboxRepositoryStub) CreateInboxTx(_ context.Context, _ pgx.Tx, _ model.ReaderInbox) (*model.ReaderInbox, error) {
	s.createCalls++
	return s.created, s.createErr
}

func (s *inboxRepositoryStub) StartInboxProposalTx(_ context.Context, _ pgx.Tx, id uuid.UUID, revision int64) (*model.ReaderInbox, error) {
	s.startCalls++
	s.startedID = id
	s.startedRevision = revision
	return s.started, s.startErr
}

type inboxQueueStub struct {
	args  []service.ReaderInboxSummaryJobArgs
	err   error
	txSet bool
}

func (s *inboxQueueStub) EnqueueReaderInboxSummaryTx(_ context.Context, tx pgx.Tx, args service.ReaderInboxSummaryJobArgs) error {
	s.txSet = tx != nil
	s.args = append(s.args, args)
	return s.err
}

func newInboxCommandTestPool(t *testing.T, commit bool) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectBegin()
	if commit {
		mock.ExpectCommit()
	} else {
		mock.ExpectRollback()
	}
	return mock
}

func TestInboxCommandsCreateCommitsProposalAndRiverTogether(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	repo := &inboxRepositoryStub{
		created: &model.ReaderInbox{ID: inboxID, MetadataRevision: 3, ProposalStatus: "idle"},
		started: &model.ReaderInbox{ID: inboxID, MetadataRevision: 3, ProposalStatus: "pending"},
	}
	queue := &inboxQueueStub{}
	mock := newInboxCommandTestPool(t, true)
	commands := NewInboxCommands(mock, repo, queue)

	result, err := commands.CreateInboxProposal(context.Background(), service.CreateInboxProposalCommand{
		Inbox: model.ReaderInbox{URL: "https://example.com"},
	})
	if err != nil {
		t.Fatalf("CreateInboxProposal() error = %v", err)
	}
	if result.Inbox == nil || result.Inbox.ID != inboxID || result.Inbox.ProposalStatus != "pending" {
		t.Fatalf("CreateInboxProposal() = %+v", result)
	}
	if repo.createCalls != 1 || repo.startCalls != 1 || repo.startedID != inboxID || repo.startedRevision != 3 {
		t.Fatalf("repository calls = create:%d start:%d id:%s revision:%d", repo.createCalls, repo.startCalls, repo.startedID, repo.startedRevision)
	}
	if !queue.txSet || len(queue.args) != 1 || queue.args[0].InboxID != inboxID || queue.args[0].ExpectedMetadataRevision != 3 {
		t.Fatalf("queue call = tx:%v args:%+v", queue.txSet, queue.args)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsEnsureUsesExactRevision(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	repo := &inboxRepositoryStub{
		started: &model.ReaderInbox{ID: inboxID, MetadataRevision: 7, ProposalStatus: "pending"},
	}
	queue := &inboxQueueStub{}
	mock := newInboxCommandTestPool(t, true)
	commands := NewInboxCommands(mock, repo, queue)

	result, err := commands.EnsureInboxProposal(context.Background(), service.EnsureInboxProposalCommand{
		InboxID: inboxID, ExpectedMetadataRevision: 7,
	})
	if err != nil {
		t.Fatalf("EnsureInboxProposal() error = %v", err)
	}
	if result.Inbox == nil || repo.startedID != inboxID || repo.startedRevision != 7 || len(queue.args) != 1 || queue.args[0].ExpectedMetadataRevision != 7 {
		t.Fatalf("result=%+v repository=%s/%d queue=%+v", result, repo.startedID, repo.startedRevision, queue.args)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsRiverFailureRollsBackProductState(t *testing.T) {
	t.Parallel()

	inboxID := uuid.New()
	repo := &inboxRepositoryStub{
		created: &model.ReaderInbox{ID: inboxID, MetadataRevision: 1},
		started: &model.ReaderInbox{ID: inboxID, MetadataRevision: 1, ProposalStatus: "pending"},
	}
	queue := &inboxQueueStub{err: errors.New("River insert failed")}
	mock := newInboxCommandTestPool(t, false)
	commands := NewInboxCommands(mock, repo, queue)

	result, err := commands.CreateInboxProposal(context.Background(), service.CreateInboxProposalCommand{
		Inbox: model.ReaderInbox{URL: "https://example.com"},
	})
	if err == nil || result.Inbox != nil {
		t.Fatalf("CreateInboxProposal() = (%+v, %v), want rollback error", result, err)
	}
	if repo.startCalls != 1 || len(queue.args) != 1 {
		t.Fatalf("start calls = %d, queue calls = %d", repo.startCalls, len(queue.args))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
