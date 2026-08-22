package worker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
)

type discardRecordingProcessor struct {
	discards []model.ParseAttempt
}

func (*discardRecordingProcessor) Run(context.Context, model.ParseAttempt) error { return nil }

func (p *discardRecordingProcessor) RecordDiscard(_ context.Context, attempt model.ParseAttempt, _ error) error {
	p.discards = append(p.discards, attempt)
	return nil
}

type translationRecordingProcessor struct {
	runAttempt model.TranslationAttempt
	runCalls   int
}

func (p *translationRecordingProcessor) Run(_ context.Context, attempt model.TranslationAttempt) error {
	p.runCalls++
	p.runAttempt = attempt
	return nil
}

func (*translationRecordingProcessor) RecordFailure(context.Context, model.TranslationAttempt, error) error {
	return nil
}

func TestParseLinkArgsCarriesImmutableAttemptFence(t *testing.T) {
	t.Parallel()

	attempt := model.ParseAttempt{
		LinkID:                   uuid.New(),
		Generation:               7,
		ExpectedMetadataRevision: 11,
	}
	args := parseLinkArgs(attempt)
	if args.LinkID != attempt.LinkID || args.ParseGeneration != attempt.Generation ||
		args.ExpectedMetadataRevision != attempt.ExpectedMetadataRevision {
		t.Fatalf("parseLinkArgs() = %+v, want %+v", args, attempt)
	}
}

func TestTranslationArgsCarryRepositoryAttemptSeed(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revision := int64(23)
	seed := model.TranslationAttemptSeed{
		TranslationID: translationID, AttemptGeneration: 11,
		SourceHash: sourceHash, SourceContentRevision: &revision,
	}

	args, err := translationArgsFromSeed(seed)
	if err != nil {
		t.Fatalf("translationArgsFromSeed() error = %v", err)
	}
	if args.TranslationID != translationID || args.AttemptGeneration != 11 ||
		args.SourceHash != sourceHash || args.SourceContentRevision == nil ||
		*args.SourceContentRevision != revision {
		t.Fatalf("translation args = %#v", args)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "tenant_id") {
		t.Fatalf("translation args unexpectedly contain tenant_id: %s", encoded)
	}
}

func TestTranslationArgsRejectIncompleteSourceIdentityBeforeInsert(t *testing.T) {
	t.Parallel()

	valid := model.TranslationAttemptSeed{
		TranslationID: uuid.New(), AttemptGeneration: 1,
		SourceHash: "abababababababababababababababababababababababababababababababab",
	}
	invalidRevision := int64(0)
	missingHash := valid
	missingHash.SourceHash = ""
	invalidSourceRevision := valid
	invalidSourceRevision.SourceContentRevision = &invalidRevision
	for _, seed := range []model.TranslationAttemptSeed{missingHash, invalidSourceRevision} {
		if _, err := translationArgsFromSeed(seed); err == nil {
			t.Fatalf("translationArgsFromSeed(%+v) error = nil", seed)
		}
	}
}

func TestNewRiverQueueTerminalRetentionPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		{name: "default", want: defaultTerminalJobRetention},
		{name: "rollback escape hatch", retention: -1, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			queue, err := NewRiverQueue(RiverQueueOptions{
				Pool: &pgxpool.Pool{}, Processor: &discardRecordingProcessor{},
				TerminalJobRetention: tc.retention,
			})
			if err != nil {
				t.Fatalf("NewRiverQueue() error = %v", err)
			}
			config := reflect.ValueOf(queue.client).Elem().FieldByName("config").Elem()
			for _, fieldName := range []string{
				"CancelledJobRetentionPeriod", "CompletedJobRetentionPeriod", "DiscardedJobRetentionPeriod",
			} {
				field := config.FieldByName(fieldName)
				if !field.IsValid() {
					t.Fatalf("River client config field %s unavailable", fieldName)
				}
				if got := time.Duration(field.Int()); got != tc.want {
					t.Errorf("River client %s = %s, want %s", fieldName, got, tc.want)
				}
			}
		})
	}
	if _, err := NewRiverQueue(RiverQueueOptions{
		Pool: &pgxpool.Pool{}, Processor: &discardRecordingProcessor{}, TerminalJobRetention: -2,
	}); err == nil || !strings.Contains(err.Error(), "terminal job retention cannot be less than -1") {
		t.Fatalf("invalid retention error = %v", err)
	}
}

func TestParseErrorHandlerProjectsOnlyFinalAttempt(t *testing.T) {
	t.Parallel()

	attempt := model.ParseAttempt{
		LinkID:                   uuid.New(),
		Generation:               5,
		ExpectedMetadataRevision: 9,
	}
	encoded, err := json.Marshal(parseLinkArgs(attempt))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	processor := &discardRecordingProcessor{}
	handler := &riverErrorHandler{processor: processor}
	job := &rivertype.JobRow{
		ID: 42, Kind: (service.ParseLinkArgs{}).Kind(), EncodedArgs: encoded,
		Attempt: 2, MaxAttempts: 3,
	}
	handler.HandleError(context.Background(), job, errors.New("temporary outage"))
	if len(processor.discards) != 0 {
		t.Fatalf("intermediate attempt projected %d discards", len(processor.discards))
	}
	job.Attempt = job.MaxAttempts
	handler.HandleError(context.Background(), job, errors.New("final outage"))
	if len(processor.discards) != 1 || processor.discards[0] != attempt {
		t.Fatalf("parse discard calls = %+v", processor.discards)
	}
}

func TestTranslationWorkerFactoryRunsAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	processor := &translationRecordingProcessor{}
	worker := newTranslationWorker(RiverQueueOptions{
		TranslationProcessor: processor, TranslationJobTimeout: time.Minute,
	})
	err := worker.Work(context.Background(), &river.Job[linktranslation.JobArgs]{
		JobRow: &rivertype.JobRow{ID: 48, Kind: model.TranslationJobKind},
		Args: linktranslation.JobArgs{
			TranslationID: translationID, AttemptGeneration: 4, SourceHash: sourceHash,
		},
	})
	if err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 4, RiverJobID: 48, SourceHash: sourceHash,
	}
	if processor.runCalls != 1 || processor.runAttempt != want {
		t.Fatalf("run calls=%d attempt=%+v", processor.runCalls, processor.runAttempt)
	}
}
