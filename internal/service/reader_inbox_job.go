package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

const (
	// ReaderInboxSummaryJobKind is a durable River kind. Changing it would
	// strand already queued jobs, so it is intentionally a named contract.
	ReaderInboxSummaryJobKind        = "reader_inbox_resummarize"
	ReaderInboxSummaryJobMaxAttempts = 3
	ReaderInboxSummaryJobTimeout     = 2 * time.Minute

	// ReaderInboxExpiryJobKind is the maintenance job that moves due pending
	// Inbox rows into the materialized expired partition without changing their
	// pending lifecycle state.
	ReaderInboxExpiryJobKind        = "reader_inbox_expire"
	ReaderInboxExpiryJobMaxAttempts = 3
	ReaderInboxExpiryJobTimeout     = 2 * time.Minute
	ReaderInboxExpiryInterval       = time.Minute
	ReaderInboxExpiryBatchSize      = 100
	ReaderInboxExpiryLeaseDuration  = 5 * time.Minute
)

// ReaderInboxSummaryJobArgs carries only opaque identities and the revision
// captured when the user requested the job. The worker re-reads the current
// draft body; it never serializes body text into River args.
type ReaderInboxSummaryJobArgs struct {
	JobID                    uuid.UUID `json:"job_id"`
	InboxID                  uuid.UUID `json:"inbox_id"`
	ExpectedMetadataRevision int64     `json:"expected_metadata_revision"`
}

func (ReaderInboxSummaryJobArgs) Kind() string { return ReaderInboxSummaryJobKind }

func (ReaderInboxSummaryJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: ReaderInboxSummaryJobMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// ReaderInboxAIBackend is deliberately narrower than the interactive Reader
// assistant. It returns structured fields so a resummarize job cannot mistake
// arbitrary assistant prose for a summary or silently overwrite user tags.
type ReaderInboxAIBackend interface {
	SummarizeInbox(context.Context, string, []string) (summary string, suggestedTags []string, err error)
}

// ReaderInboxJobScheduler is implemented by the River façade. Keeping it in
// service avoids making the service package depend on the worker package.
type ReaderInboxJobScheduler interface {
	EnqueueReaderInboxSummary(context.Context, ReaderInboxSummaryJobArgs) error
}

// ReaderInboxSummaryJobProcessor is the worker-facing service contract.
type ReaderInboxSummaryJobProcessor interface {
	RunReaderInboxSummaryJob(context.Context, ReaderInboxSummaryJobArgs, int, int) error
}

// ReaderInboxExpiryJobArgs intentionally contains no item body. The durable
// trigger processes the one installed library in bounded leased batches.
type ReaderInboxExpiryJobArgs struct {
	BatchSize int `json:"batch_size,omitempty"`
}

func (ReaderInboxExpiryJobArgs) Kind() string { return ReaderInboxExpiryJobKind }

func (ReaderInboxExpiryJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: ReaderInboxExpiryJobMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// ReaderInboxExpiryJobProcessor is implemented by ReaderVNextService and
// called once for the installed library by the River worker.
type ReaderInboxExpiryJobProcessor interface {
	RunReaderInboxExpiryJob(context.Context, int) error
}

// ConfigureReaderInboxJobs wires optional runtime capabilities after the
// service has been constructed. The queue is created before the rest of the
// runtime services, so the scheduler is installed in a second step.
func (s *ReaderVNextService) ConfigureReaderInboxJobs(ai ReaderInboxAIBackend, scheduler ReaderInboxJobScheduler) {
	s.inboxAI = ai
	s.inboxScheduler = scheduler
}

// ConfigureReaderInboxProposalCommands installs the atomic product + River
// adapter after the queue has been constructed by the composition root.
func (s *ReaderVNextService) ConfigureReaderInboxProposalCommands(commands InboxProposalCommands) {
	s.inboxCommands = commands
}

func inboxJobResponse(job model.ReaderInboxJob) dto.ReaderInboxJobResponse {
	out := dto.ReaderInboxJobResponse{
		InboxID:    job.InboxID.String(),
		Status:     job.Status,
		JobID:      job.ID.String(),
		Attempts:   job.Attempts,
		CreatedAt:  job.CreatedAt,
		UpdatedAt:  job.UpdatedAt,
		FinishedAt: job.FinishedAt,
	}
	if job.ErrorMessage != nil {
		out.Error = *job.ErrorMessage
	}
	return out
}

// ResummarizeInboxJob only creates or reuses a durable job. It does not read
// body text to compute a fake result and it never bumps metadata_revision.
func (s *ReaderVNextService) ResummarizeInboxJob(ctx context.Context, rawID string) (dto.ReaderInboxJobResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxJobResponse{}, err
	}
	item, err := s.store.GetInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxJobResponse{}, mapReaderError(err)
	}
	if s.inboxCommands != nil {
		result, commandErr := s.inboxCommands.EnsureInboxProposal(ctx, EnsureInboxProposalCommand{
			InboxID: id, ExpectedMetadataRevision: item.MetadataRevision,
		})
		if commandErr != nil {
			return dto.ReaderInboxJobResponse{}, mapReaderError(commandErr)
		}
		if result.Job == nil {
			return dto.ReaderInboxJobResponse{}, errors.New("resummarize Reader inbox: durable command returned nil job")
		}
		return inboxJobResponse(*result.Job), nil
	}
	job, created, err := s.store.BeginInboxResummarizeJob(ctx, id, item.MetadataRevision)
	if err != nil {
		return dto.ReaderInboxJobResponse{}, mapReaderError(err)
	}
	if !created {
		return inboxJobResponse(*job), nil
	}
	if s.inboxScheduler == nil {
		message := "Reader job scheduler is not configured"
		_ = s.store.FailInboxJob(ctx, job.ID, "reader_inbox_jobs_unavailable")
		return dto.ReaderInboxJobResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "reader_inbox_jobs_unavailable", message)
	}
	args := ReaderInboxSummaryJobArgs{
		JobID:                    job.ID,
		InboxID:                  job.InboxID,
		ExpectedMetadataRevision: job.ExpectedMetadataRevision,
	}
	if err := s.inboxScheduler.EnqueueReaderInboxSummary(ctx, args); err != nil {
		_ = s.store.FailInboxJob(ctx, job.ID, "reader_inbox_job_enqueue_failed")
		return dto.ReaderInboxJobResponse{}, fmt.Errorf("enqueue Reader inbox job: %w", err)
	}
	return inboxJobResponse(*job), nil
}

// GetInboxJob returns the durable state without touching the Inbox body. It is
// safe for polling because terminal rows are immutable from the worker's point
// of view.
func (s *ReaderVNextService) GetInboxJob(ctx context.Context, rawID string) (dto.ReaderInboxJobResponse, error) {
	id, err := readerUUID(rawID, "job_id")
	if err != nil {
		return dto.ReaderInboxJobResponse{}, err
	}
	job, err := s.store.GetInboxJob(ctx, id)
	if err != nil {
		return dto.ReaderInboxJobResponse{}, mapReaderError(err)
	}
	return inboxJobResponse(*job), nil
}

// RunReaderInboxSummaryJob is called by River. All body access happens here,
// inside the worker, and only the AI-derived fields are committed on success.
func (s *ReaderVNextService) RunReaderInboxSummaryJob(ctx context.Context, args ReaderInboxSummaryJobArgs, attempt, maxAttempts int) error {
	if args.JobID == uuid.Nil || args.InboxID == uuid.Nil {
		return errors.New("reader inbox job has invalid identity")
	}
	job, err := s.store.ClaimInboxJob(ctx, args.JobID)
	if errors.Is(err, repository.ErrReaderInboxJobNotRunnable) {
		return nil
	}
	if err != nil {
		return err
	}
	item, err := s.store.GetInbox(ctx, job.InboxID)
	if err != nil {
		return s.handleInboxJobFailure(ctx, job.ID, attempt, maxAttempts, err)
	}
	if item.Status != "pending" {
		return s.handleInboxJobFailure(ctx, job.ID, maxAttempts, maxAttempts, errors.New("inbox is no longer pending"))
	}
	if s.inboxAI == nil {
		return s.handleInboxJobFailure(ctx, job.ID, attempt, maxAttempts, errors.New("reader AI backend is not configured"))
	}
	summary, suggestedTags, err := s.inboxAI.SummarizeInbox(ctx, item.Body, item.Tags)
	if err != nil {
		return s.handleInboxJobFailure(ctx, job.ID, attempt, maxAttempts, err)
	}
	if strings.TrimSpace(summary) == "" {
		return s.handleInboxJobFailure(ctx, job.ID, attempt, maxAttempts, errors.New("reader AI returned an empty summary"))
	}
	if err := s.store.CompleteInboxJob(ctx, job.ID, summary, suggestedTags); err != nil {
		if errors.Is(err, repository.ErrRevisionConflict) || errors.Is(err, repository.ErrReaderInboxJobNotRunnable) {
			return nil
		}
		return err
	}
	return nil
}

// RunReaderInboxExpiryJob drains due rows for the installed library. Each batch
// has a lease owner and deadline; if the process disappears between
// claim and finalize, a later River run can reclaim the rows after the lease
// expires. The status remains pending so the expired partition stays visible
// and can still be confirmed or discarded.
func (s *ReaderVNextService) RunReaderInboxExpiryJob(ctx context.Context, batchSize int) error {
	if batchSize <= 0 || batchSize > 500 {
		batchSize = ReaderInboxExpiryBatchSize
	}
	for {
		now := s.now().UTC()
		leaseID := uuid.New()
		items, err := s.store.ClaimExpiredInbox(ctx, leaseID, now, now.Add(ReaderInboxExpiryLeaseDuration), batchSize)
		if err != nil {
			return fmt.Errorf("claim Reader inbox expiry batch: %w", err)
		}
		if len(items) == 0 {
			return nil
		}
		if _, err := s.store.FinalizeExpiredInbox(ctx, leaseID, now); err != nil {
			return fmt.Errorf("finalize Reader inbox expiry batch: %w", err)
		}
		if len(items) < batchSize {
			return nil
		}
	}
}

func (s *ReaderVNextService) handleInboxJobFailure(ctx context.Context, jobID uuid.UUID, attempt, maxAttempts int, _ error) error {
	// Persist a stable code rather than provider text. This keeps prompts,
	// response bodies, and arbitrary upstream errors out of durable diagnostics.
	message := "reader_inbox_ai_failed"
	if attempt < maxAttempts {
		if err := s.store.RetryInboxJob(ctx, jobID, message); err != nil && !errors.Is(err, repository.ErrReaderInboxJobNotRunnable) {
			return err
		}
	} else {
		if err := s.store.FailInboxJob(ctx, jobID, message); err != nil && !errors.Is(err, repository.ErrReaderInboxJobNotRunnable) {
			return err
		}
	}
	return errors.New(message)
}

// ReaderInboxSummaryWorker adapts the service processor to River's worker
// contract.
type ReaderInboxSummaryWorker struct {
	river.WorkerDefaults[ReaderInboxSummaryJobArgs]
	processor ReaderInboxSummaryJobProcessor
	timeout   time.Duration
}

func NewReaderInboxSummaryWorker(processor ReaderInboxSummaryJobProcessor, timeout time.Duration) *ReaderInboxSummaryWorker {
	return &ReaderInboxSummaryWorker{processor: processor, timeout: timeout}
}

func (w *ReaderInboxSummaryWorker) Timeout(*river.Job[ReaderInboxSummaryJobArgs]) time.Duration {
	return w.timeout
}

func (w *ReaderInboxSummaryWorker) Work(ctx context.Context, job *river.Job[ReaderInboxSummaryJobArgs]) error {
	if w.processor == nil {
		return errors.New("reader inbox job processor is not configured")
	}
	return w.processor.RunReaderInboxSummaryJob(ctx, job.Args, job.Attempt, job.MaxAttempts)
}
