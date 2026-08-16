package linktranslation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
)

const translationJobMaxAttempts = 3

// LegacyJobArgs is the legacy wire shape used during compat/drain rollout.
// RF6A compat schedulers populate every source-identity field; older jobs may
// omit generation. Strict-v2 never schedules this kind.
type LegacyJobArgs struct {
	TranslationID         uuid.UUID `json:"translation_id"`
	AttemptGeneration     int64     `json:"attempt_generation,omitempty"`
	SourceHash            string    `json:"source_hash,omitempty"`
	SourceContentRevision *int64    `json:"source_content_revision,omitempty"`
}

func (LegacyJobArgs) Kind() string { return model.TranslationJobKindLegacy }

func (LegacyJobArgs) InsertOpts() river.InsertOpts { return translationInsertOpts() }

type JobArgs struct {
	TranslationID         uuid.UUID `json:"translation_id"`
	AttemptGeneration     int64     `json:"attempt_generation"`
	SourceHash            string    `json:"source_hash"`
	SourceContentRevision *int64    `json:"source_content_revision"`
}

func (JobArgs) Kind() string { return model.TranslationJobKindV2 }

func (JobArgs) InsertOpts() river.InsertOpts {
	return translationInsertOpts()
}

// ResolveV2Attempt validates and decodes a raw v2 River row into the complete
// attempt identity used by terminal handlers and reconcilers.
func ResolveV2Attempt(job *rivertype.JobRow) model.TranslationAttemptResolution {
	var (
		riverJobID    int64
		kind          string
		args          JobArgs
		malformedArgs bool
	)
	if job != nil {
		riverJobID = job.ID
		kind = job.Kind
		malformedArgs = json.Unmarshal(job.EncodedArgs, &args) != nil
	}
	return resolveV2Attempt(riverJobID, kind, args, malformedArgs)
}

func resolveV2Attempt(
	riverJobID int64,
	kind string,
	args JobArgs,
	malformedArgs bool,
) model.TranslationAttemptResolution {
	reject := func(reason model.TranslationAttemptRejectionReason) model.TranslationAttemptResolution {
		return model.TranslationAttemptResolution{Rejection: reason}
	}
	if riverJobID <= 0 {
		return reject(model.TranslationAttemptRejectionInvalidRiverJobID)
	}
	if kind != (JobArgs{}).Kind() {
		return reject(model.TranslationAttemptRejectionKindMismatch)
	}
	if malformedArgs {
		return reject(model.TranslationAttemptRejectionMalformedArgs)
	}
	if args.TranslationID == uuid.Nil {
		return reject(model.TranslationAttemptRejectionMissingTranslationID)
	}
	if args.AttemptGeneration <= 0 {
		return reject(model.TranslationAttemptRejectionInvalidAttemptGeneration)
	}
	if args.SourceHash == "" {
		return reject(model.TranslationAttemptRejectionMalformedArgs)
	}
	if args.SourceContentRevision != nil && *args.SourceContentRevision <= 0 {
		return reject(model.TranslationAttemptRejectionMalformedArgs)
	}
	return model.TranslationAttemptResolution{Attempt: model.TranslationAttempt{
		TranslationID:         args.TranslationID,
		AttemptGeneration:     args.AttemptGeneration,
		RiverJobID:            riverJobID,
		SourceHash:            args.SourceHash,
		SourceContentRevision: args.SourceContentRevision,
	}}
}

func translationInsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: translationJobMaxAttempts,
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

type Worker struct {
	river.WorkerDefaults[JobArgs]
	processor  JobProcessor
	jobTimeout time.Duration
	logger     *slog.Logger
}

type WorkerOptions struct {
	JobTimeout time.Duration
	Logger     *slog.Logger
}

func NewWorkerWithOptions(processor JobProcessor, opts WorkerOptions) *Worker {
	return &Worker{
		processor:  processor,
		jobTimeout: opts.JobTimeout,
		logger:     opts.Logger,
	}
}

func (w *Worker) Timeout(*river.Job[JobArgs]) time.Duration {
	return w.jobTimeout
}

func (w *Worker) Work(ctx context.Context, job *river.Job[JobArgs]) error {
	var (
		riverJobID int64
		kind       string
		args       JobArgs
	)
	if job != nil {
		args = job.Args
		if job.JobRow != nil {
			riverJobID = job.ID
			kind = job.Kind
		}
	}
	resolution := resolveV2Attempt(riverJobID, kind, args, false)
	if resolution.Rejected() {
		return fmt.Errorf("translate_link_v2 job rejected: %s", resolution.Rejection)
	}
	attempt := resolution.Attempt
	return runTranslationAttempt(ctx, w.processor, attempt, w.logger)
}

// LegacyAttemptResolver proves whether a legacy River job still owns the
// persisted current translation attempt before the worker may project state.
type LegacyAttemptResolver interface {
	ProveCurrentLegacyAttempt(context.Context, uuid.UUID, int64, int64) (model.TranslationAttemptResolution, error)
}

// LegacyWorker consumes the legacy River kind only during compat-v1 and
// drain-v1 rollout. It must prove exact current-attempt ownership through
// LegacyAttemptResolver before invoking the shared translation processor.
type LegacyWorker struct {
	river.WorkerDefaults[LegacyJobArgs]
	attempts   LegacyAttemptResolver
	processor  JobProcessor
	jobTimeout time.Duration
	logger     *slog.Logger
}

// NewLegacyWorker constructs the proof-before-run worker retained for
// compat-v1/drain-v1. Queue rollout policy, not callers of this constructor,
// decides whether the legacy kind is registered.
func NewLegacyWorker(
	attempts LegacyAttemptResolver,
	processor JobProcessor,
	jobTimeout time.Duration,
	logger *slog.Logger,
) *LegacyWorker {
	return &LegacyWorker{
		attempts: attempts, processor: processor, jobTimeout: jobTimeout, logger: logger,
	}
}

func (w *LegacyWorker) Timeout(*river.Job[LegacyJobArgs]) time.Duration {
	return w.jobTimeout
}

func (w *LegacyWorker) Work(ctx context.Context, job *river.Job[LegacyJobArgs]) error {
	if job == nil || job.JobRow == nil || job.ID <= 0 {
		return errors.New("legacy translation job is missing river_job_id")
	}
	if job.Kind != (LegacyJobArgs{}).Kind() {
		return errors.New("legacy translation worker received mismatched job kind")
	}
	if job.Args.TranslationID == uuid.Nil {
		return errors.New("legacy translation job is missing translation_id")
	}
	if w.processor == nil {
		return errors.New("legacy translation worker is missing dependencies")
	}
	resolution, err := ResolveLegacyAttempt(ctx, w.attempts, job.Args, job.ID)
	if err != nil {
		return fmt.Errorf("resolve legacy translation attempt: %w", err)
	}
	if resolution.Rejected() {
		w.logRejected(ctx, job, resolution.Rejection)
		return nil
	}
	attempt := resolution.Attempt
	return runTranslationAttempt(ctx, w.processor, attempt, w.logger)
}

const cancellationProjectionTimeout = 10 * time.Second

func runTranslationAttempt(
	ctx context.Context,
	processor JobProcessor,
	attempt model.TranslationAttempt,
	logger *slog.Logger,
) error {
	err := processor.Run(ctx, attempt)
	if err == nil || !errors.Is(context.Cause(ctx), rivertype.ErrJobCancelledRemotely) {
		return err
	}
	// River skips ErrorHandler for its remote JobCancelError. This is the one
	// cancellation path the worker owns; ordinary context cancellation is left
	// to the error handler so the product transition is projected exactly once.
	projectionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancellationProjectionTimeout)
	defer cancel()
	if projectionErr := processor.RecordCancellation(projectionCtx, attempt, err); projectionErr != nil && logger != nil {
		logger.ErrorContext(projectionCtx, "failed to project cancelled translation job",
			"river_job_id", attempt.RiverJobID,
			"translation_id", attempt.TranslationID.String(),
			"attempt_generation", attempt.AttemptGeneration,
			"error", projectionErr,
		)
	}
	return err
}

// ResolveLegacyAttempt proves that a pre-v2 River row still owns the exact
// persisted translation attempt. Generation zero is valid only on this
// compatibility path because historical jobs did not encode a generation.
func ResolveLegacyAttempt(
	ctx context.Context,
	attempts LegacyAttemptResolver,
	args LegacyJobArgs,
	riverJobID int64,
) (model.TranslationAttemptResolution, error) {
	if attempts == nil {
		return model.TranslationAttemptResolution{}, errors.New("legacy attempt resolver is missing")
	}
	if args.AttemptGeneration < 0 {
		return model.TranslationAttemptResolution{Rejection: model.TranslationAttemptRejectionInvalidGeneration}, nil
	}
	resolution, err := attempts.ProveCurrentLegacyAttempt(
		ctx,
		args.TranslationID,
		riverJobID,
		args.AttemptGeneration,
	)
	if err != nil {
		return model.TranslationAttemptResolution{}, err
	}
	if resolution.Rejected() {
		return resolution, nil
	}
	if reason := validateResolvedLegacyAttempt(args, resolution.Attempt, riverJobID); reason != "" {
		return model.TranslationAttemptResolution{Rejection: reason}, nil
	}
	return resolution, nil
}

func validateResolvedLegacyAttempt(
	args LegacyJobArgs,
	attempt model.TranslationAttempt,
	riverJobID int64,
) model.TranslationAttemptRejectionReason {
	if attempt.TranslationID == uuid.Nil {
		return model.TranslationAttemptRejectionTranslationNotFound
	}
	if attempt.TranslationID != args.TranslationID {
		return model.TranslationAttemptRejectionIdentityMismatch
	}
	if attempt.RiverJobID != riverJobID {
		return model.TranslationAttemptRejectionNotCurrent
	}
	if attempt.AttemptGeneration < 0 {
		return model.TranslationAttemptRejectionInvalidGeneration
	}
	if args.AttemptGeneration > 0 && args.AttemptGeneration != attempt.AttemptGeneration {
		return model.TranslationAttemptRejectionGenerationMismatch
	}
	if args.SourceHash != "" && !legacyJobSourceMatchesAttempt(args, attempt) {
		return model.TranslationAttemptRejectionIdentityMismatch
	}
	return ""
}

func legacyJobSourceMatchesAttempt(args LegacyJobArgs, attempt model.TranslationAttempt) bool {
	if args.SourceHash != attempt.SourceHash {
		return false
	}
	if args.SourceContentRevision == nil || attempt.SourceContentRevision == nil {
		return args.SourceContentRevision == nil && attempt.SourceContentRevision == nil
	}
	return *args.SourceContentRevision == *attempt.SourceContentRevision
}

func (w *LegacyWorker) logRejected(
	ctx context.Context,
	job *river.Job[LegacyJobArgs],
	reason model.TranslationAttemptRejectionReason,
) {
	if w.logger == nil {
		return
	}
	w.logger.WarnContext(ctx, "legacy translation attempt rejected",
		"reason", reason.String(),
		"translation_id", job.Args.TranslationID.String(),
		"river_job_id", job.ID,
	)
}
