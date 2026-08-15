package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type inboxJobStoreStub struct {
	repository.ReaderVNextStore
	inbox           model.ReaderInbox
	job             *model.ReaderInboxJob
	beginCalls      int
	claims          int
	retries         int
	fails           int
	completed       int
	lastSummary     string
	lastTags        []string
	expiryClaims    int
	expiryFinalized int
	expiryLeaseID   uuid.UUID
	expiryNow       time.Time
}

func (s *inboxJobStoreStub) GetInbox(context.Context, uuid.UUID) (*model.ReaderInbox, error) {
	item := s.inbox
	item.Tags = append([]string(nil), s.inbox.Tags...)
	item.SuggestedTags = append([]string(nil), s.inbox.SuggestedTags...)
	return &item, nil
}

func (s *inboxJobStoreStub) BeginInboxResummarizeJob(_ context.Context, inboxID uuid.UUID, expectedRevision int64) (*model.ReaderInboxJob, bool, error) {
	s.beginCalls++
	if s.job != nil && s.job.Status != "failed" {
		job := *s.job
		return &job, false, nil
	}
	now := time.Unix(100, 0).UTC()
	job := &model.ReaderInboxJob{
		ID:                       uuid.New(),
		InboxID:                  inboxID,
		ExpectedMetadataRevision: expectedRevision,
		Status:                   "queued",
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	s.job = job
	id := job.ID
	s.inbox.JobID = &id
	return job, true, nil
}

func (s *inboxJobStoreStub) GetInboxJob(context.Context, uuid.UUID) (*model.ReaderInboxJob, error) {
	if s.job == nil {
		return nil, repository.ErrNotFound
	}
	job := *s.job
	return &job, nil
}

func (s *inboxJobStoreStub) ClaimInboxJob(context.Context, uuid.UUID) (*model.ReaderInboxJob, error) {
	if s.job == nil {
		return nil, repository.ErrNotFound
	}
	if s.job.Status != "queued" && s.job.Status != "running" {
		return nil, repository.ErrReaderInboxJobNotRunnable
	}
	s.claims++
	s.job.Status = "running"
	s.job.Attempts++
	job := *s.job
	return &job, nil
}

func (s *inboxJobStoreStub) RetryInboxJob(context.Context, uuid.UUID, string) error {
	if s.job == nil || s.job.Status != "running" {
		return repository.ErrReaderInboxJobNotRunnable
	}
	s.retries++
	s.job.Status = "queued"
	message := "reader_inbox_ai_failed"
	s.job.ErrorMessage = &message
	return nil
}

func (s *inboxJobStoreStub) FailInboxJob(context.Context, uuid.UUID, string) error {
	if s.job == nil || (s.job.Status != "running" && s.job.Status != "queued") {
		return repository.ErrReaderInboxJobNotRunnable
	}
	s.fails++
	s.job.Status = "failed"
	message := "reader_inbox_ai_failed"
	s.job.ErrorMessage = &message
	return nil
}

func (s *inboxJobStoreStub) CompleteInboxJob(_ context.Context, _ uuid.UUID, summary string, tags []string) error {
	if s.job == nil || s.job.Status != "running" {
		return repository.ErrReaderInboxJobNotRunnable
	}
	s.completed++
	s.lastSummary = summary
	s.lastTags = append([]string(nil), tags...)
	s.job.Status = "completed"
	s.inbox.Summary = &summary
	s.inbox.SuggestedTags = append([]string(nil), tags...)
	return nil
}

func (s *inboxJobStoreStub) ClaimExpiredInbox(_ context.Context, leaseID uuid.UUID, now, _ time.Time, _ int) ([]model.ReaderInbox, error) {
	s.expiryClaims++
	s.expiryLeaseID = leaseID
	s.expiryNow = now
	if s.expiryClaims > 1 {
		return nil, nil
	}
	return []model.ReaderInbox{{ID: uuid.New(), Status: "pending", Expired: true}}, nil
}

func (s *inboxJobStoreStub) FinalizeExpiredInbox(_ context.Context, leaseID uuid.UUID, now time.Time) (int64, error) {
	if leaseID != s.expiryLeaseID || !now.Equal(s.expiryNow) {
		return 0, errors.New("expiry lease mismatch")
	}
	s.expiryFinalized++
	return 1, nil
}

type inboxJobAIStub struct {
	body    string
	summary string
	tags    []string
	err     error
	calls   int
}

func (s *inboxJobAIStub) SummarizeInbox(_ context.Context, body string, _ []string) (string, []string, error) {
	s.calls++
	s.body = body
	if s.err != nil {
		return "", nil, s.err
	}
	return s.summary, append([]string(nil), s.tags...), nil
}

type inboxJobSchedulerStub struct {
	args []ReaderInboxSummaryJobArgs
	err  error
}

func (s *inboxJobSchedulerStub) EnqueueReaderInboxSummary(_ context.Context, args ReaderInboxSummaryJobArgs) error {
	s.args = append(s.args, args)
	return s.err
}

func newInboxJobService(store *inboxJobStoreStub, ai *inboxJobAIStub, scheduler *inboxJobSchedulerStub) *ReaderVNextService {
	service := NewReaderVNextService(store, nil)
	service.ConfigureReaderInboxJobs(ai, scheduler)
	return service
}

func TestResummarizeInboxJobReusesPendingJob(t *testing.T) {
	inboxID := uuid.New()
	store := &inboxJobStoreStub{inbox: model.ReaderInbox{ID: inboxID, Body: "draft body", Status: "pending", MetadataRevision: 4}}
	scheduler := &inboxJobSchedulerStub{}
	service := newInboxJobService(store, &inboxJobAIStub{}, scheduler)

	first, err := service.ResummarizeInboxJob(context.Background(), inboxID.String())
	if err != nil {
		t.Fatalf("first ResummarizeInboxJob() error = %v", err)
	}
	second, err := service.ResummarizeInboxJob(context.Background(), inboxID.String())
	if err != nil {
		t.Fatalf("second ResummarizeInboxJob() error = %v", err)
	}
	if first.JobID == "" || second.JobID != first.JobID || first.Status != "queued" || second.Status != "queued" {
		t.Fatalf("job responses = %#v and %#v, want one queued job", first, second)
	}
	if store.beginCalls != 2 || len(scheduler.args) != 1 {
		t.Fatalf("begin calls = %d, enqueue calls = %d, want 2 and 1", store.beginCalls, len(scheduler.args))
	}
	if scheduler.args[0].JobID.String() != first.JobID || scheduler.args[0].ExpectedMetadataRevision != 4 {
		t.Fatalf("enqueued args = %#v, want job %s revision 4", scheduler.args[0], first.JobID)
	}
}

func TestRunReaderInboxSummaryJobOnlyCommitsAIFields(t *testing.T) {
	inboxID := uuid.New()
	store := &inboxJobStoreStub{inbox: model.ReaderInbox{
		ID:               inboxID,
		Title:            stringPtr("user title"),
		Body:             "current draft body",
		Tags:             []string{"user-tag"},
		Status:           "pending",
		MetadataRevision: 2,
	}}
	ai := &inboxJobAIStub{summary: "AI summary", tags: []string{"suggested"}}
	service := newInboxJobService(store, ai, &inboxJobSchedulerStub{})
	response, err := service.ResummarizeInboxJob(context.Background(), inboxID.String())
	if err != nil {
		t.Fatalf("ResummarizeInboxJob() error = %v", err)
	}

	args := ReaderInboxSummaryJobArgs{JobID: uuid.MustParse(response.JobID), InboxID: inboxID, ExpectedMetadataRevision: 2}
	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 1, 3); err != nil {
		t.Fatalf("RunReaderInboxSummaryJob() error = %v", err)
	}
	if ai.body != "current draft body" || ai.calls != 1 {
		t.Fatalf("AI input = %q, calls = %d, want current body and one call", ai.body, ai.calls)
	}
	if store.lastSummary != "AI summary" || len(store.lastTags) != 1 || store.lastTags[0] != "suggested" || store.completed != 1 {
		t.Fatalf("committed fields = summary %q tags %#v completed %d", store.lastSummary, store.lastTags, store.completed)
	}
	if store.inbox.Title == nil || *store.inbox.Title != "user title" || len(store.inbox.Tags) != 1 || store.inbox.Tags[0] != "user-tag" || store.inbox.Body != "current draft body" {
		t.Fatalf("user fields changed: %#v", store.inbox)
	}
	if store.inbox.MetadataRevision != 2 || store.job.Status != "completed" {
		t.Fatalf("revision/status = %d/%s, want 2/completed", store.inbox.MetadataRevision, store.job.Status)
	}
}

func TestRunReaderInboxSummaryJobWritesOnlyProposalFieldsAfterUserEdit(t *testing.T) {
	inboxID := uuid.New()
	store := &inboxJobStoreStub{inbox: model.ReaderInbox{ID: inboxID, Body: "body", Status: "pending", MetadataRevision: 1}}
	ai := &inboxJobAIStub{summary: "must not be used"}
	service := newInboxJobService(store, ai, &inboxJobSchedulerStub{})
	response, err := service.ResummarizeInboxJob(context.Background(), inboxID.String())
	if err != nil {
		t.Fatalf("ResummarizeInboxJob() error = %v", err)
	}
	store.inbox.MetadataRevision = 2
	err = service.RunReaderInboxSummaryJob(context.Background(), ReaderInboxSummaryJobArgs{JobID: uuid.MustParse(response.JobID), InboxID: inboxID, ExpectedMetadataRevision: 1}, 1, 3)
	if err != nil {
		t.Fatalf("late proposal job returned %v", err)
	}
	if ai.calls != 1 || store.completed != 1 || store.fails != 0 || store.job.Status != "completed" || store.inbox.MetadataRevision != 2 {
		t.Fatalf("late proposal result ai_calls=%d completed=%d fails=%d status=%s revision=%d", ai.calls, store.completed, store.fails, store.job.Status, store.inbox.MetadataRevision)
	}
}

func TestRunReaderInboxSummaryJobRetriesAndThenFailsAIError(t *testing.T) {
	inboxID := uuid.New()
	store := &inboxJobStoreStub{inbox: model.ReaderInbox{ID: inboxID, Body: "body", Status: "pending", MetadataRevision: 1}}
	ai := &inboxJobAIStub{err: errors.New("provider detail must not persist")}
	service := newInboxJobService(store, ai, &inboxJobSchedulerStub{})
	response, err := service.ResummarizeInboxJob(context.Background(), inboxID.String())
	if err != nil {
		t.Fatalf("ResummarizeInboxJob() error = %v", err)
	}
	args := ReaderInboxSummaryJobArgs{JobID: uuid.MustParse(response.JobID), InboxID: inboxID, ExpectedMetadataRevision: 1}
	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 1, 3); err == nil {
		t.Fatal("first AI failure returned nil")
	}
	if store.retries != 1 || store.job.Status != "queued" || store.job.ErrorMessage == nil || *store.job.ErrorMessage != "reader_inbox_ai_failed" {
		t.Fatalf("after retry: retries=%d status=%s error=%v", store.retries, store.job.Status, store.job.ErrorMessage)
	}
	if err := service.RunReaderInboxSummaryJob(context.Background(), args, 3, 3); err == nil {
		t.Fatal("terminal AI failure returned nil")
	}
	if store.fails != 1 || store.job.Status != "failed" || store.job.ErrorMessage == nil || *store.job.ErrorMessage != "reader_inbox_ai_failed" {
		t.Fatalf("after terminal failure: fails=%d status=%s error=%v", store.fails, store.job.Status, store.job.ErrorMessage)
	}
}

func TestReaderInboxSummaryJobInsertOptsExcludeCompleted(t *testing.T) {
	opts := (ReaderInboxSummaryJobArgs{}).InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false, want true")
	}
	for _, state := range opts.UniqueOpts.ByState {
		if state == rivertype.JobStateCompleted {
			t.Fatal("completed jobs must not block a new job")
		}
	}
}

func TestRunReaderInboxExpiryJobClaimsAndFinalizesPendingRows(t *testing.T) {
	store := &inboxJobStoreStub{}
	service := NewReaderVNextService(store, nil)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	if err := service.RunReaderInboxExpiryJob(context.Background(), 10); err != nil {
		t.Fatalf("RunReaderInboxExpiryJob() error = %v", err)
	}
	if store.expiryClaims != 1 || store.expiryFinalized != 1 || store.expiryLeaseID == uuid.Nil {
		t.Fatalf("expiry calls = claims %d finalized %d lease %s, want one claimed/finalized batch", store.expiryClaims, store.expiryFinalized, store.expiryLeaseID)
	}
}

func TestReaderInboxExpiryJobInsertOptsExcludeCompleted(t *testing.T) {
	opts := (ReaderInboxExpiryJobArgs{}).InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts().UniqueOpts.ByArgs = false, want true")
	}
	for _, state := range opts.UniqueOpts.ByState {
		if state == rivertype.JobStateCompleted {
			t.Fatal("completed expiry jobs must not block a later sweep")
		}
	}
}
