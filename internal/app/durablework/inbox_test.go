package durablework

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type inboxRepositoryStub struct {
	item       *model.ReaderInbox
	job        *model.ReaderInboxJob
	createErr  error
	beginErr   error
	createCall int
	beginCall  int
	created    bool
	orphans    []repository.ReaderInboxDispatchOrphan
	claimErr   error
	resetErr   error
	claimLimit int
	claimKind  string
	resetIDs   []uuid.UUID
}

func (s *inboxRepositoryStub) CreateInboxTx(_ context.Context, _ pgx.Tx, _ model.ReaderInbox) (*model.ReaderInbox, error) {
	s.createCall++
	return s.item, s.createErr
}

func (s *inboxRepositoryStub) BeginInboxResummarizeJobTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ int64) (*model.ReaderInboxJob, bool, error) {
	s.beginCall++
	return s.job, s.created, s.beginErr
}

func (s *inboxRepositoryStub) ClaimInboxDispatchOrphansTx(_ context.Context, _ pgx.Tx, kind string, limit int) ([]repository.ReaderInboxDispatchOrphan, error) {
	s.claimKind = kind
	s.claimLimit = limit
	return s.orphans, s.claimErr
}

func (s *inboxRepositoryStub) ResetInboxDispatchOrphanTx(_ context.Context, _ pgx.Tx, jobID uuid.UUID) error {
	s.resetIDs = append(s.resetIDs, jobID)
	return s.resetErr
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

func TestInboxCommandsCreateCommitsProductAttemptAndRiverTogether(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	inboxID, jobID := uuid.New(), uuid.New()
	repo := &inboxRepositoryStub{
		item:    &model.ReaderInbox{ID: inboxID, MetadataRevision: 3},
		job:     &model.ReaderInboxJob{ID: jobID, InboxID: inboxID, ExpectedMetadataRevision: 3, Status: "queued"},
		created: true,
	}
	queue := &inboxQueueStub{}
	mock.ExpectBegin()
	mock.ExpectCommit()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: queue})
	result, err := commands.CreateInboxProposal(context.Background(), service.CreateInboxProposalCommand{Inbox: model.ReaderInbox{URL: "https://example.com"}})
	if err != nil {
		t.Fatalf("CreateInboxProposal() error = %v", err)
	}
	if result.Inbox == nil || result.Inbox.ID != inboxID || result.Inbox.JobID == nil || *result.Inbox.JobID != jobID || result.Job == nil || result.Job.ID != jobID {
		t.Fatalf("CreateInboxProposal() = %+v", result)
	}
	if repo.createCall != 1 || repo.beginCall != 1 || !queue.txSet || len(queue.args) != 1 {
		t.Fatalf("create=%d begin=%d tx=%v enqueues=%d", repo.createCall, repo.beginCall, queue.txSet, len(queue.args))
	}
	if queue.args[0].JobID != jobID || queue.args[0].InboxID != inboxID || queue.args[0].ExpectedMetadataRevision != 3 {
		t.Fatalf("enqueued args = %+v", queue.args[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsEnqueueFailureRollsBackProposalAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	inboxID := uuid.New()
	repo := &inboxRepositoryStub{
		item:    &model.ReaderInbox{ID: inboxID, MetadataRevision: 1},
		job:     &model.ReaderInboxJob{ID: uuid.New(), InboxID: inboxID, ExpectedMetadataRevision: 1, Status: "queued"},
		created: true,
	}
	queue := &inboxQueueStub{err: errors.New("River insert failed")}
	mock.ExpectBegin()
	mock.ExpectRollback()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: queue})
	result, err := commands.CreateInboxProposal(context.Background(), service.CreateInboxProposalCommand{Inbox: model.ReaderInbox{URL: "https://example.com"}})
	if err == nil || result.Inbox != nil || result.Job != nil {
		t.Fatalf("CreateInboxProposal() = (%+v, %v), want empty result and error", result, err)
	}
	if repo.beginCall != 1 || len(queue.args) != 1 {
		t.Fatalf("begin=%d enqueues=%d", repo.beginCall, len(queue.args))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsReplayReenqueuesReusedQueuedAttempt(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	inboxID, orphanJobID := uuid.New(), uuid.New()
	repo := &inboxRepositoryStub{
		job: &model.ReaderInboxJob{
			ID: orphanJobID, InboxID: inboxID, ExpectedMetadataRevision: 7, Status: "queued",
		},
		created: false,
	}
	queue := &inboxQueueStub{}
	mock.ExpectBegin()
	mock.ExpectCommit()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: queue})
	result, err := commands.EnsureInboxProposal(context.Background(), service.EnsureInboxProposalCommand{
		InboxID: inboxID, ExpectedMetadataRevision: 7,
	})
	if err != nil {
		t.Fatalf("EnsureInboxProposal() error = %v", err)
	}
	if result.Job == nil || result.Job.ID != orphanJobID || repo.beginCall != 1 || len(queue.args) != 1 {
		t.Fatalf("result=%+v begin=%d enqueues=%d", result, repo.beginCall, len(queue.args))
	}
	if queue.args[0].JobID != orphanJobID {
		t.Fatalf("replayed job = %s, want orphan %s", queue.args[0].JobID, orphanJobID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsRepairsQueuedAndRunningOrphansInOneTransaction(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	queuedJobID, runningJobID := uuid.New(), uuid.New()
	repo := &inboxRepositoryStub{orphans: []repository.ReaderInboxDispatchOrphan{
		{JobID: queuedJobID, InboxID: uuid.New(), ExpectedMetadataRevision: 3, Status: "queued"},
		{JobID: runningJobID, InboxID: uuid.New(), ExpectedMetadataRevision: 7, Status: "running"},
	}}
	queue := &inboxQueueStub{}
	mock.ExpectBegin()
	mock.ExpectCommit()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: queue})
	result, err := commands.RepairInboxProposalOrphans(context.Background(), 20)
	if err != nil {
		t.Fatalf("RepairInboxProposalOrphans() error = %v", err)
	}
	if result.Claimed != 2 || result.Repaired != 2 {
		t.Fatalf("RepairInboxProposalOrphans() = %+v, want claimed=2 repaired=2", result)
	}
	if repo.claimKind != service.ReaderInboxSummaryJobKind || repo.claimLimit != 20 {
		t.Fatalf("claim = (%q, %d), want (%q, 20)", repo.claimKind, repo.claimLimit, service.ReaderInboxSummaryJobKind)
	}
	if len(repo.resetIDs) != 1 || repo.resetIDs[0] != runningJobID {
		t.Fatalf("reset jobs = %v, want [%s]", repo.resetIDs, runningJobID)
	}
	if len(queue.args) != 2 || queue.args[0].JobID != queuedJobID || queue.args[1].JobID != runningJobID {
		t.Fatalf("enqueued args = %+v", queue.args)
	}
	if !queue.txSet {
		t.Fatal("repair enqueue did not receive the caller transaction")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsRepairFailureRollsBackWholeBatch(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := &inboxRepositoryStub{orphans: []repository.ReaderInboxDispatchOrphan{
		{JobID: uuid.New(), InboxID: uuid.New(), ExpectedMetadataRevision: 1, Status: "running"},
	}}
	queue := &inboxQueueStub{err: errors.New("River insert failed")}
	mock.ExpectBegin()
	mock.ExpectRollback()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: queue})
	result, err := commands.RepairInboxProposalOrphans(context.Background(), 10)
	if err == nil {
		t.Fatal("RepairInboxProposalOrphans() error = nil, want enqueue failure")
	}
	if result.Claimed != 1 || result.Repaired != 0 {
		t.Fatalf("RepairInboxProposalOrphans() = %+v, want claimed=1 repaired=0", result)
	}
	if len(repo.resetIDs) != 1 || len(queue.args) != 1 {
		t.Fatalf("reset=%d enqueue=%d, want one attempted repair", len(repo.resetIDs), len(queue.args))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestInboxCommandsCapsOrphanRepairBatch(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := &inboxRepositoryStub{}
	mock.ExpectBegin()
	mock.ExpectCommit()

	commands := NewInboxCommands(InboxCommandsOptions{Transactions: mock, Inbox: repo, Queue: &inboxQueueStub{}})
	if _, err := commands.RepairInboxProposalOrphans(context.Background(), 10_000); err != nil {
		t.Fatalf("RepairInboxProposalOrphans() error = %v", err)
	}
	if repo.claimLimit != MaxInboxProposalOrphanRepairBatchSize {
		t.Fatalf("claim limit = %d, want %d", repo.claimLimit, MaxInboxProposalOrphanRepairBatchSize)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
