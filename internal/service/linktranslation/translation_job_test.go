package linktranslation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
	"webtag/internal/model"
)

func TestTranslationJobArgsCarryWholeAttemptIdentity(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revision := int64(17)
	args := JobArgs{
		TranslationID:         translationID,
		AttemptGeneration:     9,
		SourceHash:            sourceHash,
		SourceContentRevision: &revision,
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, exists := wire["tenant_id"]; exists {
		t.Fatalf("encoded args unexpectedly contain tenant_id: %s", encoded)
	}
	if wire["translation_id"] != translationID.String() ||
		wire["attempt_generation"] != float64(9) || wire["source_hash"] != sourceHash ||
		wire["source_content_revision"] != float64(revision) {
		t.Fatalf("encoded args = %s", encoded)
	}

	got, rejection := resolveAttempt(400, args.Kind(), args, false)
	if rejection != "" || got.TranslationID != translationID ||
		got.AttemptGeneration != 9 || got.RiverJobID != 400 || got.SourceHash != sourceHash ||
		got.SourceContentRevision == nil || *got.SourceContentRevision != revision {
		t.Fatalf("resolveAttempt() = %+v, %q", got, rejection)
	}
}

func TestTranslationJobArgsUseActiveStateUniqueness(t *testing.T) {
	t.Parallel()

	opts := (JobArgs{}).InsertOpts()
	if opts.MaxAttempts != 3 || !opts.UniqueOpts.ByArgs {
		t.Fatalf("InsertOpts() = %+v", opts)
	}
	for _, state := range opts.UniqueOpts.ByState {
		if state == rivertype.JobStateCompleted {
			t.Fatal("completed translations must not block force retranslation")
		}
	}
}

type recordingTranslationProcessor struct {
	attempt        model.TranslationAttempt
	runCalls       int
	runErr         error
	runPanic       any
	failureCalls   int
	failureAttempt model.TranslationAttempt
	failureCtxErr  error
	failureErr     error
}

func (p *recordingTranslationProcessor) Run(_ context.Context, attempt model.TranslationAttempt) error {
	p.runCalls++
	p.attempt = attempt
	if p.runPanic != nil {
		panic(p.runPanic)
	}
	return p.runErr
}

func (p *recordingTranslationProcessor) RecordFailure(ctx context.Context, attempt model.TranslationAttempt, _ error) error {
	p.failureCalls++
	p.failureAttempt = attempt
	p.failureCtxErr = ctx.Err()
	return p.failureErr
}

func TestTranslationWorkerRunsCompleteAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	revision := int64(31)
	const sourceHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	processor := &recordingTranslationProcessor{}
	worker := NewWorkerWithOptions(processor, WorkerOptions{JobTimeout: 41 * time.Minute, Logger: nil})
	err := worker.Work(context.Background(), &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: 401, Kind: (JobArgs{}).Kind()},
		Args: JobArgs{
			TranslationID: translationID, AttemptGeneration: 6,
			SourceHash: sourceHash, SourceContentRevision: &revision,
		},
	})
	if err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 6, RiverJobID: 401,
		SourceHash: sourceHash, SourceContentRevision: &revision,
	}
	if processor.runCalls != 1 || processor.attempt != want {
		t.Fatalf("processor calls=%d attempt=%+v, want %+v", processor.runCalls, processor.attempt, want)
	}
	if timeout := worker.Timeout(nil); timeout != 41*time.Minute {
		t.Fatalf("Timeout() = %s, want 41m", timeout)
	}
}

func TestTranslationWorkerProjectsOnlyTheFinalFailure(t *testing.T) {
	t.Parallel()

	attemptErr := errors.New("translator unavailable")
	processor := &recordingTranslationProcessor{runErr: attemptErr}
	worker := NewWorkerWithOptions(processor, WorkerOptions{})
	job := &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: 411, Kind: (JobArgs{}).Kind(), Attempt: 2, MaxAttempts: 3},
		Args: JobArgs{
			TranslationID: uuid.New(), AttemptGeneration: 2,
			SourceHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	if err := worker.Work(context.Background(), job); !errors.Is(err, attemptErr) {
		t.Fatalf("intermediate Work() error = %v", err)
	}
	if processor.failureCalls != 0 {
		t.Fatalf("intermediate failure projections = %d, want 0", processor.failureCalls)
	}

	job.Attempt = job.MaxAttempts
	if err := worker.Work(context.Background(), job); !errors.Is(err, attemptErr) {
		t.Fatalf("final Work() error = %v", err)
	}
	if processor.failureCalls != 1 || processor.failureAttempt != processor.attempt || processor.failureCtxErr != nil {
		t.Fatalf("final projection = calls:%d attempt:%+v ctx:%v", processor.failureCalls, processor.failureAttempt, processor.failureCtxErr)
	}
}

func TestTranslationWorkerSnoozesUntilTerminalProjectionSucceeds(t *testing.T) {
	t.Parallel()

	processor := &recordingTranslationProcessor{
		runErr:     errors.New("translator unavailable"),
		failureErr: errors.New("database unavailable"),
	}
	job := &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: 412, Kind: (JobArgs{}).Kind(), Attempt: 3, MaxAttempts: 3},
		Args: JobArgs{
			TranslationID: uuid.New(), AttemptGeneration: 3,
			SourceHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
	err := NewWorkerWithOptions(processor, WorkerOptions{}).Work(context.Background(), job)
	var snooze *rivertype.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != terminalProjectionRetryAfter {
		t.Fatalf("Work() error = %v, want %s snooze", err, terminalProjectionRetryAfter)
	}
	if processor.failureCalls != 1 {
		t.Fatalf("failure projections = %d, want 1", processor.failureCalls)
	}
}

func TestTranslationWorkerCancelsPermanentFailureAfterProjection(t *testing.T) {
	t.Parallel()

	processor := &recordingTranslationProcessor{runErr: fmt.Errorf("provider policy: %w", errsafe.ErrUnsafeTarget)}
	job := &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: 413, Kind: (JobArgs{}).Kind(), Attempt: 1, MaxAttempts: 3},
		Args: JobArgs{
			TranslationID: uuid.New(), AttemptGeneration: 4,
			SourceHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
	}
	err := NewWorkerWithOptions(processor, WorkerOptions{}).Work(context.Background(), job)
	var cancelErr *rivertype.JobCancelError
	if !errors.As(err, &cancelErr) {
		t.Fatalf("Work() error = %v, want JobCancelError", err)
	}
	if processor.failureCalls != 1 {
		t.Fatalf("failure projections = %d, want 1", processor.failureCalls)
	}
}

func TestTranslationWorkerProjectsFinalPanic(t *testing.T) {
	t.Parallel()

	processor := &recordingTranslationProcessor{runPanic: "translator panic"}
	job := &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: 414, Kind: (JobArgs{}).Kind(), Attempt: 3, MaxAttempts: 3},
		Args: JobArgs{
			TranslationID: uuid.New(), AttemptGeneration: 5,
			SourceHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		},
	}
	err := NewWorkerWithOptions(processor, WorkerOptions{}).Work(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "translation worker panic") {
		t.Fatalf("Work() error = %v, want recovered panic", err)
	}
	if processor.failureCalls != 1 {
		t.Fatalf("failure projections = %d, want 1", processor.failureCalls)
	}
}

func TestTranslationWorkerRejectsIncompleteAttemptIdentity(t *testing.T) {
	t.Parallel()

	invalidRevision := int64(0)
	valid := JobArgs{
		TranslationID: uuid.New(), AttemptGeneration: 1,
		SourceHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
	for _, tc := range []struct {
		name string
		job  *river.Job[JobArgs]
	}{
		{name: "missing job"},
		{name: "missing row", job: &river.Job[JobArgs]{Args: valid}},
		{name: "river id", job: &river.Job[JobArgs]{JobRow: &rivertype.JobRow{Kind: valid.Kind()}, Args: valid}},
		{name: "kind", job: &river.Job[JobArgs]{JobRow: &rivertype.JobRow{ID: 1, Kind: "translate_link_content"}, Args: valid}},
		{name: "translation", job: &river.Job[JobArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Kind: valid.Kind()},
			Args:   JobArgs{AttemptGeneration: 1, SourceHash: valid.SourceHash},
		}},
		{name: "generation", job: &river.Job[JobArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Kind: valid.Kind()},
			Args:   JobArgs{TranslationID: uuid.New(), SourceHash: valid.SourceHash},
		}},
		{name: "source hash", job: &river.Job[JobArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Kind: valid.Kind()},
			Args:   JobArgs{TranslationID: uuid.New(), AttemptGeneration: 1},
		}},
		{name: "source revision", job: &river.Job[JobArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Kind: valid.Kind()},
			Args: JobArgs{
				TranslationID: uuid.New(), AttemptGeneration: 1,
				SourceHash: valid.SourceHash, SourceContentRevision: &invalidRevision,
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			processor := &recordingTranslationProcessor{}
			if err := NewWorkerWithOptions(processor, WorkerOptions{JobTimeout: time.Minute, Logger: nil}).Work(context.Background(), tc.job); err == nil {
				t.Fatal("Work() error = nil")
			}
			if processor.runCalls != 0 {
				t.Fatalf("processor Run() calls = %d, want 0", processor.runCalls)
			}
		})
	}
}

func TestTranslationWorkerLeavesTransactionalRemoteCancellationAlone(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 404
	const sourceHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 1, RiverJobID: riverJobID,
		SourceHash: sourceHash,
	}
	processor := &recordingTranslationProcessor{runErr: context.Canceled}
	worker := NewWorkerWithOptions(processor, WorkerOptions{})
	workCtx, cancel := context.WithCancelCause(context.Background())
	cancel(rivertype.ErrJobCancelledRemotely)
	err := worker.Work(workCtx, &river.Job[JobArgs]{
		JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (JobArgs{}).Kind(), Attempt: 3, MaxAttempts: 3},
		Args: JobArgs{
			TranslationID: translationID, AttemptGeneration: 1, SourceHash: sourceHash,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if processor.attempt != want || processor.failureCalls != 0 {
		t.Fatalf("attempt=%+v failure projections=%d, want remote cancellation left to its business transaction", processor.attempt, processor.failureCalls)
	}
}

func TestTranslationWorkerRetriesOrdinaryCancellation(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 405
	const sourceHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	processor := &recordingTranslationProcessor{runErr: context.Canceled}
	err := NewWorkerWithOptions(processor, WorkerOptions{}).Work(
		context.Background(),
		&river.Job[JobArgs]{
			JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (JobArgs{}).Kind(), Attempt: 1, MaxAttempts: 3},
			Args: JobArgs{
				TranslationID: translationID, AttemptGeneration: 1, SourceHash: sourceHash,
			},
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if processor.failureCalls != 0 {
		t.Fatalf("worker failure projections = %d, want 0 before final attempt", processor.failureCalls)
	}
}
