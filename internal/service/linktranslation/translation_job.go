package linktranslation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
	"webtag/internal/model"
)

const (
	translationJobMaxAttempts    = 3
	terminalProjectionTimeout    = 10 * time.Second
	terminalProjectionRetryAfter = 5 * time.Second
)

type JobArgs struct {
	TranslationID         uuid.UUID `json:"translation_id"`
	AttemptGeneration     int64     `json:"attempt_generation"`
	SourceHash            string    `json:"source_hash"`
	SourceContentRevision *int64    `json:"source_content_revision"`
}

func (JobArgs) Kind() string { return model.TranslationJobKind }

func (JobArgs) InsertOpts() river.InsertOpts {
	return translationInsertOpts()
}

type attemptRejection string

const (
	attemptRejectionNotCurrent               attemptRejection = "not_current"
	attemptRejectionInvalidRiverJobID        attemptRejection = "invalid_river_job_id"
	attemptRejectionKindMismatch             attemptRejection = "kind_mismatch"
	attemptRejectionMalformedArgs            attemptRejection = "malformed_args"
	attemptRejectionMissingTranslationID     attemptRejection = "missing_translation_id"
	attemptRejectionInvalidAttemptGeneration attemptRejection = "invalid_attempt_generation"
)

func resolveAttempt(
	riverJobID int64,
	kind string,
	args JobArgs,
	malformedArgs bool,
) (model.TranslationAttempt, attemptRejection) {
	if riverJobID <= 0 {
		return model.TranslationAttempt{}, attemptRejectionInvalidRiverJobID
	}
	if kind != (JobArgs{}).Kind() {
		return model.TranslationAttempt{}, attemptRejectionKindMismatch
	}
	if malformedArgs {
		return model.TranslationAttempt{}, attemptRejectionMalformedArgs
	}
	if args.TranslationID == uuid.Nil {
		return model.TranslationAttempt{}, attemptRejectionMissingTranslationID
	}
	if args.AttemptGeneration <= 0 {
		return model.TranslationAttempt{}, attemptRejectionInvalidAttemptGeneration
	}
	if args.SourceHash == "" {
		return model.TranslationAttempt{}, attemptRejectionMalformedArgs
	}
	if args.SourceContentRevision != nil && *args.SourceContentRevision <= 0 {
		return model.TranslationAttempt{}, attemptRejectionMalformedArgs
	}
	return model.TranslationAttempt{
		TranslationID:         args.TranslationID,
		AttemptGeneration:     args.AttemptGeneration,
		RiverJobID:            riverJobID,
		SourceHash:            args.SourceHash,
		SourceContentRevision: args.SourceContentRevision,
	}, ""
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
	if w == nil || w.processor == nil {
		return errors.New("translation worker is missing dependencies")
	}
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
	attempt, rejection := resolveAttempt(riverJobID, kind, args, false)
	if rejection != "" {
		return fmt.Errorf("translate_link_v2 job rejected: %s", rejection)
	}
	return runTranslationAttempt(ctx, w.processor, attempt, finalRiverAttempt(job.JobRow), w.logger)
}

func finalRiverAttempt(job *rivertype.JobRow) bool {
	return job != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts
}

func runTranslationAttempt(
	ctx context.Context,
	processor JobProcessor,
	attempt model.TranslationAttempt,
	finalAttempt bool,
	logger *slog.Logger,
) (workErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			workErr = fmt.Errorf("translation worker panic: %v", recovered)
			if logger != nil {
				logger.ErrorContext(ctx, "translation worker panicked",
					"river_job_id", attempt.RiverJobID,
					"translation_id", attempt.TranslationID.String(),
					"attempt_generation", attempt.AttemptGeneration,
					"stack", string(debug.Stack()),
				)
			}
			workErr = finishTranslationAttempt(ctx, processor, attempt, workErr, finalAttempt, logger)
		}
	}()

	workErr = processor.Run(ctx, attempt)
	return finishTranslationAttempt(ctx, processor, attempt, workErr, finalAttempt, logger)
}

func finishTranslationAttempt(
	ctx context.Context,
	processor JobProcessor,
	attempt model.TranslationAttempt,
	workErr error,
	finalAttempt bool,
	logger *slog.Logger,
) error {
	if workErr == nil || errors.Is(context.Cause(ctx), rivertype.ErrJobCancelledRemotely) {
		return workErr
	}
	permanent := errors.Is(workErr, errsafe.ErrUnsafeTarget)
	if !finalAttempt && !permanent {
		return workErr
	}

	projectionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalProjectionTimeout)
	defer cancel()
	if err := processor.RecordFailure(projectionCtx, attempt, workErr); err != nil {
		if logger != nil {
			logger.ErrorContext(projectionCtx, "failed to project terminal translation job",
				"river_job_id", attempt.RiverJobID,
				"translation_id", attempt.TranslationID.String(),
				"attempt_generation", attempt.AttemptGeneration,
				"error", err,
			)
		}
		return river.JobSnooze(terminalProjectionRetryAfter)
	}
	if permanent {
		return river.JobCancel(workErr)
	}
	return workErr
}
