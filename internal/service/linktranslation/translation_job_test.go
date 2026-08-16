package linktranslation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
)

func TestTranslationV2JobArgsCarryWholeAttemptIdentity(t *testing.T) {
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

	resolution := ResolveV2Attempt(&rivertype.JobRow{
		ID: 400, Kind: args.Kind(), EncodedArgs: encoded,
	})
	got := resolution.Attempt
	if resolution.Rejected() || got.TranslationID != translationID ||
		got.AttemptGeneration != 9 || got.RiverJobID != 400 || got.SourceHash != sourceHash ||
		got.SourceContentRevision == nil || *got.SourceContentRevision != revision {
		t.Fatalf("ResolveV2Attempt() = %+v", resolution)
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
	attempt             model.TranslationAttempt
	runCalls            int
	runErr              error
	cancellationCalls   int
	cancellationAttempt model.TranslationAttempt
	cancellationCtxErr  error
}

func (p *recordingTranslationProcessor) Run(_ context.Context, attempt model.TranslationAttempt) error {
	p.runCalls++
	p.attempt = attempt
	return p.runErr
}

func (*recordingTranslationProcessor) RecordDiscard(context.Context, model.TranslationAttempt, error) error {
	return nil
}

func (p *recordingTranslationProcessor) RecordCancellation(ctx context.Context, attempt model.TranslationAttempt, _ error) error {
	p.cancellationCalls++
	p.cancellationAttempt = attempt
	p.cancellationCtxErr = ctx.Err()
	return nil
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

func TestTranslationV2WorkerRejectsIncompleteAttemptIdentity(t *testing.T) {
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
		{name: "kind", job: &river.Job[JobArgs]{JobRow: &rivertype.JobRow{ID: 1, Kind: (LegacyJobArgs{}).Kind()}, Args: valid}},
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

type staticLegacyAttemptResolver struct {
	result model.TranslationAttemptResolution
	calls  int
}

func (r *staticLegacyAttemptResolver) ProveCurrentLegacyAttempt(
	context.Context,
	uuid.UUID,
	int64,
	int64,
) (model.TranslationAttemptResolution, error) {
	r.calls++
	return r.result, nil
}

func TestLegacyTranslationWorkerRunsProvenHistoricalAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 402
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 0, RiverJobID: riverJobID,
	}
	resolver := &staticLegacyAttemptResolver{result: model.TranslationAttemptResolution{Attempt: want}}
	processor := &recordingTranslationProcessor{}
	worker := NewLegacyWorker(resolver, processor, 17*time.Minute, nil)
	err := worker.Work(context.Background(), &river.Job[LegacyJobArgs]{
		JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (LegacyJobArgs{}).Kind()},
		Args:   LegacyJobArgs{TranslationID: translationID},
	})
	if err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if resolver.calls != 1 || processor.runCalls != 1 || processor.attempt != want {
		t.Fatalf("resolver calls=%d processor calls=%d attempt=%+v", resolver.calls, processor.runCalls, processor.attempt)
	}
	if timeout := worker.Timeout(nil); timeout != 17*time.Minute {
		t.Fatalf("Timeout() = %s, want 17m", timeout)
	}
}

func TestLegacyTranslationWorkerRejectsStaleRiverJobObservably(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	resolver := &staticLegacyAttemptResolver{result: model.TranslationAttemptResolution{
		Rejection: model.TranslationAttemptRejectionNotCurrent,
	}}
	processor := &recordingTranslationProcessor{}
	var logs bytes.Buffer
	worker := NewLegacyWorker(resolver, processor, time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	err := worker.Work(context.Background(), &river.Job[LegacyJobArgs]{
		JobRow: &rivertype.JobRow{ID: 402, Kind: (LegacyJobArgs{}).Kind()},
		Args:   LegacyJobArgs{TranslationID: translationID},
	})
	if err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if processor.runCalls != 0 {
		t.Fatalf("processor Run() calls = %d, want 0", processor.runCalls)
	}
	if got := logs.String(); !strings.Contains(got, "legacy translation attempt rejected") ||
		!strings.Contains(got, "reason="+model.TranslationAttemptRejectionNotCurrent.String()) {
		t.Fatalf("log = %q, want observable not_current rejection", got)
	}
}

func TestLegacyTranslationWorkerRejectsWireSourceMismatchAgainstLockedProduct(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	productRevision, wrongRevision := int64(7), int64(8)
	const riverJobID int64 = 407
	const productHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name string
		args LegacyJobArgs
	}{
		{name: "source hash", args: LegacyJobArgs{
			TranslationID: translationID, AttemptGeneration: 1,
			SourceHash:            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SourceContentRevision: &productRevision,
		}},
		{name: "source revision", args: LegacyJobArgs{
			TranslationID: translationID, AttemptGeneration: 1,
			SourceHash: productHash, SourceContentRevision: &wrongRevision,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolver := &staticLegacyAttemptResolver{result: model.TranslationAttemptResolution{
				Attempt: model.TranslationAttempt{
					TranslationID: translationID, AttemptGeneration: 1, RiverJobID: riverJobID,
					SourceHash: productHash, SourceContentRevision: &productRevision,
				},
			}}
			processor := &recordingTranslationProcessor{}
			var logs bytes.Buffer
			worker := NewLegacyWorker(resolver, processor, time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
			if err := worker.Work(context.Background(), &river.Job[LegacyJobArgs]{
				JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (LegacyJobArgs{}).Kind()}, Args: tc.args,
			}); err != nil {
				t.Fatalf("Work() error = %v", err)
			}
			if processor.runCalls != 0 || !strings.Contains(logs.String(), "reason="+model.TranslationAttemptRejectionIdentityMismatch.String()) {
				t.Fatalf("processor calls=%d log=%q", processor.runCalls, logs.String())
			}
		})
	}
}

func TestLegacyTranslationWorkerProjectsRemoteCancellationWithLiveContext(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 404
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 0, RiverJobID: riverJobID,
	}
	resolver := &staticLegacyAttemptResolver{result: model.TranslationAttemptResolution{Attempt: want}}
	processor := &recordingTranslationProcessor{runErr: context.Canceled}
	worker := NewLegacyWorker(resolver, processor, time.Minute, nil)
	workCtx, cancel := context.WithCancelCause(context.Background())
	cancel(rivertype.ErrJobCancelledRemotely)
	err := worker.Work(workCtx, &river.Job[LegacyJobArgs]{
		JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (LegacyJobArgs{}).Kind()},
		Args:   LegacyJobArgs{TranslationID: translationID},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if processor.cancellationCalls != 1 || processor.cancellationAttempt != want || processor.cancellationCtxErr != nil {
		t.Fatalf("cancellations=%d attempt=%+v ctx_err=%v", processor.cancellationCalls, processor.cancellationAttempt, processor.cancellationCtxErr)
	}
}

func TestLegacyTranslationWorkerLeavesOrdinaryCancellationToRiverErrorHandler(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 405
	resolver := &staticLegacyAttemptResolver{result: model.TranslationAttemptResolution{Attempt: model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 0, RiverJobID: riverJobID,
	}}}
	processor := &recordingTranslationProcessor{runErr: context.Canceled}
	err := NewLegacyWorker(resolver, processor, time.Minute, nil).Work(
		context.Background(),
		&river.Job[LegacyJobArgs]{
			JobRow: &rivertype.JobRow{ID: riverJobID, Kind: (LegacyJobArgs{}).Kind()},
			Args:   LegacyJobArgs{TranslationID: translationID},
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if processor.cancellationCalls != 0 {
		t.Fatalf("worker cancellation projections = %d, want 0", processor.cancellationCalls)
	}
}
