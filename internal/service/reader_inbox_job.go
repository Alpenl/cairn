package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/repository"
)

const (
	// ReaderInboxSummaryJobKind is a durable River kind. Changing it would
	// strand already queued jobs, so it remains a named wire contract.
	ReaderInboxSummaryJobKind        = "reader_inbox_resummarize"
	ReaderInboxSummaryJobMaxAttempts = 3
	ReaderInboxSummaryJobTimeout     = 2 * time.Minute
)

// ReaderInboxSummaryJobArgs carries the Inbox identity and the exact draft
// revision the worker may update. River owns attempts and terminal job state;
// the product database does not duplicate them.
type ReaderInboxSummaryJobArgs struct {
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
// assistant. It returns structured fields so a proposal cannot overwrite user
// tags with arbitrary assistant prose.
type ReaderInboxAIBackend interface {
	SummarizeInbox(context.Context, string, []string) (summary string, suggestedTags []string, err error)
}

type ReaderInboxSummaryJobProcessor interface {
	RunReaderInboxSummaryJob(context.Context, ReaderInboxSummaryJobArgs, int, int) error
}

type readerInboxSummaryStore interface {
	ClaimInboxProposal(context.Context, uuid.UUID, int64) (*model.ReaderInbox, error)
	CompleteInboxProposal(context.Context, uuid.UUID, int64, string, []string) error
	RetryInboxProposal(context.Context, uuid.UUID, int64) error
	FailInboxProposal(context.Context, uuid.UUID, int64) error
}

// ReaderInboxSummaryProcessor is the River worker dependency. Keeping it
// separate from ReaderInboxApplication breaks the queue/application construction cycle:
// the worker needs only proposal state and AI, not the HTTP-facing Reader API.
type ReaderInboxSummaryProcessor struct {
	store readerInboxSummaryStore
	ai    ReaderInboxAIBackend
}

func NewReaderInboxSummaryProcessor(store readerInboxSummaryStore, ai ReaderInboxAIBackend) *ReaderInboxSummaryProcessor {
	return &ReaderInboxSummaryProcessor{store: store, ai: ai}
}

// ResummarizeInbox changes the proposal state and inserts the matching River
// job in one transaction. The returned Inbox is the only polling resource.
func (s *ReaderInboxApplication) ResummarizeInbox(ctx context.Context, id uuid.UUID) (model.ReaderInbox, error) {
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return model.ReaderInbox{}, mapReaderError(err)
	}
	if s.inboxCommands == nil {
		return model.ReaderInbox{}, errors.New("resummarize Reader inbox: durable commands are not configured")
	}
	result, err := s.inboxCommands.EnsureInboxProposal(ctx, EnsureInboxProposalCommand{
		InboxID: id, ExpectedMetadataRevision: item.MetadataRevision,
	})
	if err != nil {
		return model.ReaderInbox{}, mapReaderError(err)
	}
	if result.Inbox == nil {
		return model.ReaderInbox{}, errors.New("resummarize Reader inbox: durable command returned nil item")
	}
	return *result.Inbox, nil
}

// RunReaderInboxSummaryJob commits AI output only when the Inbox still has the
// exact metadata revision captured by the River job. Editing a draft makes an
// older result stale instead of letting it overwrite newer user content.
func (p *ReaderInboxSummaryProcessor) RunReaderInboxSummaryJob(ctx context.Context, args ReaderInboxSummaryJobArgs, attempt, maxAttempts int) error {
	if args.InboxID == uuid.Nil || args.ExpectedMetadataRevision < 1 {
		return errors.New("reader inbox job has invalid identity")
	}
	item, err := p.store.ClaimInboxProposal(ctx, args.InboxID, args.ExpectedMetadataRevision)
	if errors.Is(err, repository.ErrReaderInboxProposalNotRunnable) || errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrRevisionConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.ai == nil {
		return p.handleFailure(ctx, args, attempt, maxAttempts)
	}
	summary, suggestedTags, err := p.ai.SummarizeInbox(ctx, item.Body, item.Tags)
	if err != nil || strings.TrimSpace(summary) == "" {
		return p.handleFailure(ctx, args, attempt, maxAttempts)
	}
	if err := p.store.CompleteInboxProposal(ctx, args.InboxID, args.ExpectedMetadataRevision, summary, suggestedTags); err != nil {
		if errors.Is(err, repository.ErrReaderInboxProposalNotRunnable) || errors.Is(err, repository.ErrRevisionConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (p *ReaderInboxSummaryProcessor) handleFailure(ctx context.Context, args ReaderInboxSummaryJobArgs, attempt, maxAttempts int) error {
	const message = "reader_inbox_ai_failed"
	var err error
	if attempt < maxAttempts {
		err = p.store.RetryInboxProposal(ctx, args.InboxID, args.ExpectedMetadataRevision)
	} else {
		err = p.store.FailInboxProposal(ctx, args.InboxID, args.ExpectedMetadataRevision)
	}
	if err != nil {
		if errors.Is(err, repository.ErrReaderInboxProposalNotRunnable) || errors.Is(err, repository.ErrRevisionConflict) || errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	return errors.New(message)
}

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
