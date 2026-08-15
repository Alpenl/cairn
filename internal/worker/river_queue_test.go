package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
)

type discardRecordingProcessor struct {
	discards []service.ParseLinkArgs
}

func (*discardRecordingProcessor) Run(context.Context, uuid.UUID, uuid.UUID) error { return nil }

func (p *discardRecordingProcessor) RecordDiscard(_ context.Context, linkID, jobID uuid.UUID, _ error) error {
	p.discards = append(p.discards, service.ParseLinkArgs{LinkID: linkID, ParseJobID: jobID})
	return nil
}

type translationDiscardProcessor struct {
	runAttempt          model.TranslationAttempt
	discardAttempt      model.TranslationAttempt
	cancellationAttempt model.TranslationAttempt
	runCalls            int
	discardCalls        int
	cancellationCalls   int
	discardErr          error
	cancellationErr     error
}

func (p *translationDiscardProcessor) Run(_ context.Context, attempt model.TranslationAttempt) error {
	p.runCalls++
	p.runAttempt = attempt
	return nil
}

func (p *translationDiscardProcessor) RecordDiscard(_ context.Context, attempt model.TranslationAttempt, _ error) error {
	p.discardCalls++
	p.discardAttempt = attempt
	return p.discardErr
}

func (p *translationDiscardProcessor) RecordCancellation(_ context.Context, attempt model.TranslationAttempt, _ error) error {
	p.cancellationCalls++
	p.cancellationAttempt = attempt
	return p.cancellationErr
}

type terminalLegacyAttemptResolver struct {
	result model.TranslationAttemptResolution
}

func (r terminalLegacyAttemptResolver) ProveCurrentLegacyAttempt(
	context.Context,
	uuid.UUID,
	int64,
	int64,
) (model.TranslationAttemptResolution, error) {
	return r.result, nil
}

func TestParseLinkArgsCarriesImmutableJobIdentity(t *testing.T) {
	t.Parallel()

	linkID, jobID := uuid.New(), uuid.New()
	args := parseLinkArgs(linkID, jobID)
	if args.LinkID != linkID || args.ParseJobID != jobID {
		t.Fatalf("parseLinkArgs() = %+v, want %s/%s", args, linkID, jobID)
	}
}

func TestTranslationV2ArgsCarryRepositoryAttemptSeed(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revision := int64(23)
	seed := model.TranslationAttemptSeed{
		TranslationID: translationID, AttemptGeneration: 11,
		SourceHash: sourceHash, SourceContentRevision: &revision,
	}

	args, err := translationArgsFromSeed(seed, model.TranslationJobScheduleV2)
	if err != nil {
		t.Fatalf("translationArgsFromSeed() error = %v", err)
	}
	v2Args, ok := args.(linktranslation.JobArgs)
	if !ok || v2Args.TranslationID != translationID || v2Args.AttemptGeneration != 11 ||
		v2Args.SourceHash != sourceHash || v2Args.SourceContentRevision == nil ||
		*v2Args.SourceContentRevision != revision {
		t.Fatalf("translation args = %#v", args)
	}
	encoded, err := json.Marshal(v2Args)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "tenant_id") {
		t.Fatalf("translation args unexpectedly contain tenant_id: %s", encoded)
	}
}

func TestTranslationArgsFollowRolloutScheduleProtocol(t *testing.T) {
	t.Parallel()

	revision := int64(43)
	seed := model.TranslationAttemptSeed{
		TranslationID: uuid.New(), AttemptGeneration: 7,
		SourceHash:            "abababababababababababababababababababababababababababababababab",
		SourceContentRevision: &revision,
	}
	legacy, err := translationArgsFromSeed(seed, model.TranslationJobScheduleLegacy)
	if err != nil {
		t.Fatalf("legacy translationArgsFromSeed() error = %v", err)
	}
	legacyArgs, ok := legacy.(linktranslation.LegacyJobArgs)
	if !ok || legacyArgs.TranslationID != seed.TranslationID ||
		legacyArgs.AttemptGeneration != seed.AttemptGeneration || legacyArgs.SourceHash != seed.SourceHash ||
		legacyArgs.SourceContentRevision == nil || *legacyArgs.SourceContentRevision != revision {
		t.Fatalf("legacy args = %#v", legacy)
	}
	if legacy.Kind() != model.TranslationJobKindLegacy {
		t.Fatalf("legacy Kind() = %q", legacy.Kind())
	}
	if _, err := translationArgsFromSeed(seed, model.TranslationJobSchedulePaused); !errors.Is(err, ErrTranslationSchedulingPaused) {
		t.Fatalf("paused translationArgsFromSeed() error = %v", err)
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
		if _, err := translationArgsFromSeed(seed, model.TranslationJobScheduleV2); err == nil {
			t.Fatalf("translationArgsFromSeed(%+v) error = nil", seed)
		}
	}
}

func TestNewRiverQueueRejectsInvalidTranslationJobsRollout(t *testing.T) {
	t.Parallel()

	_, err := NewRiverQueue(RiverQueueOptions{
		Pool: &pgxpool.Pool{}, Processor: &discardRecordingProcessor{},
		TranslationJobsRollout: model.TranslationJobsRolloutStage("invalid"),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid translation jobs rollout stage") {
		t.Fatalf("NewRiverQueue() error = %v", err)
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

	linkID, jobID := uuid.New(), uuid.New()
	encoded, err := json.Marshal(service.ParseLinkArgs{LinkID: linkID, ParseJobID: jobID})
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
	if len(processor.discards) != 1 || processor.discards[0].LinkID != linkID || processor.discards[0].ParseJobID != jobID {
		t.Fatalf("parse discard calls = %+v", processor.discards)
	}
}

func TestTranslationErrorHandlerProjectsFinalFailure(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	revision := int64(37)
	encoded, err := json.Marshal(linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 5,
		SourceHash: sourceHash, SourceContentRevision: &revision,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	processor := &translationDiscardProcessor{}
	handler := &riverErrorHandler{translationProcessor: processor}
	job := &rivertype.JobRow{
		ID: 43, Kind: model.TranslationJobKindV2, EncodedArgs: encoded,
		Attempt: 2, MaxAttempts: 3,
	}
	handler.HandleError(context.Background(), job, errors.New("temporary"))
	if processor.discardCalls != 0 {
		t.Fatalf("intermediate discard calls = %d", processor.discardCalls)
	}
	job.Attempt = job.MaxAttempts
	handler.HandleError(context.Background(), job, errors.New("terminal"))
	got := processor.discardAttempt
	if processor.discardCalls != 1 || got.TranslationID != translationID || got.AttemptGeneration != 5 ||
		got.RiverJobID != 43 || got.SourceHash != sourceHash || got.SourceContentRevision == nil ||
		*got.SourceContentRevision != revision {
		t.Fatalf("discard calls=%d attempt=%+v", processor.discardCalls, got)
	}
}

func TestTranslationErrorHandlerCancelsUnsafeTargetOnFirstAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "abababababababababababababababababababababababababababababababab"
	encoded, err := json.Marshal(linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 7, SourceHash: sourceHash,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	processor := &translationDiscardProcessor{}
	handler := &riverErrorHandler{translationProcessor: processor}
	job := &rivertype.JobRow{
		ID: 45, Kind: model.TranslationJobKindV2, EncodedArgs: encoded,
		Attempt: 1, MaxAttempts: 3,
	}

	result := handler.HandleError(context.Background(), job, fmt.Errorf("provider policy: %w", errsafe.ErrUnsafeTarget))
	if processor.discardCalls != 1 || processor.cancellationCalls != 0 {
		t.Fatalf("discard/cancellation calls=%d/%d, want 1/0", processor.discardCalls, processor.cancellationCalls)
	}
	if processor.discardAttempt.TranslationID != translationID || processor.discardAttempt.RiverJobID != job.ID {
		t.Fatalf("discard attempt=%+v", processor.discardAttempt)
	}
	if result == nil || !result.SetCancelled {
		t.Fatalf("HandleError() result=%+v, want River cancellation", result)
	}
}

func TestTranslationErrorHandlerCancellationOwnership(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	encoded, err := json.Marshal(linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 6, SourceHash: sourceHash,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	job := &rivertype.JobRow{
		ID: 44, Kind: model.TranslationJobKindV2, EncodedArgs: encoded,
		Attempt: 1, MaxAttempts: 3,
	}

	processor := &translationDiscardProcessor{}
	handler := &riverErrorHandler{translationProcessor: processor}
	result := handler.HandleError(context.Background(), job, context.Canceled)
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 6, RiverJobID: 44, SourceHash: sourceHash,
	}
	if processor.cancellationCalls != 1 || processor.discardCalls != 0 || processor.cancellationAttempt != want {
		t.Fatalf("cancellations=%d discards=%d attempt=%+v", processor.cancellationCalls, processor.discardCalls, processor.cancellationAttempt)
	}
	if result == nil || !result.SetCancelled {
		t.Fatalf("HandleError() result = %+v", result)
	}

	processor = &translationDiscardProcessor{}
	handler = &riverErrorHandler{translationProcessor: processor}
	result = handler.HandleError(context.Background(), job, rivertype.ErrJobCancelledRemotely)
	if processor.cancellationCalls != 0 || processor.discardCalls != 0 || result != nil {
		t.Fatalf("remote cancellation: result=%+v cancellation=%d discard=%d", result, processor.cancellationCalls, processor.discardCalls)
	}
}

func TestTranslationV2WorkerFactoryRunsInstallationAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const sourceHash = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	processor := &translationDiscardProcessor{}
	worker := newTranslationV2Worker(RiverQueueOptions{
		TranslationProcessor: processor, TranslationJobTimeout: time.Minute,
	})
	err := worker.Work(context.Background(), &river.Job[linktranslation.JobArgs]{
		JobRow: &rivertype.JobRow{ID: 48, Kind: model.TranslationJobKindV2},
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

func TestTranslationErrorHandlerRejectsMalformedAttemptObservably(t *testing.T) {
	t.Parallel()

	invalidRevision := int64(0)
	valid := linktranslation.JobArgs{
		TranslationID: uuid.New(), AttemptGeneration: 3,
		SourceHash: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	encode := func(args linktranslation.JobArgs) []byte {
		t.Helper()
		payload, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		return payload
	}
	validArgs := encode(valid)
	missingTranslation := valid
	missingTranslation.TranslationID = uuid.Nil
	missingGeneration := valid
	missingGeneration.AttemptGeneration = 0
	missingHash := valid
	missingHash.SourceHash = ""
	badRevision := valid
	badRevision.SourceContentRevision = &invalidRevision
	for _, tc := range []struct {
		name   string
		job    *rivertype.JobRow
		reason model.TranslationAttemptRejectionReason
	}{
		{name: "river id", reason: model.TranslationAttemptRejectionInvalidRiverJobID, job: &rivertype.JobRow{Kind: model.TranslationJobKindV2, EncodedArgs: validArgs}},
		{name: "kind", reason: model.TranslationAttemptRejectionKindMismatch, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindLegacy, EncodedArgs: validArgs}},
		{name: "json", reason: model.TranslationAttemptRejectionMalformedArgs, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindV2, EncodedArgs: []byte("{")}},
		{name: "translation id", reason: model.TranslationAttemptRejectionMissingTranslationID, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindV2, EncodedArgs: encode(missingTranslation)}},
		{name: "generation", reason: model.TranslationAttemptRejectionInvalidAttemptGeneration, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindV2, EncodedArgs: encode(missingGeneration)}},
		{name: "source hash", reason: model.TranslationAttemptRejectionMalformedArgs, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindV2, EncodedArgs: encode(missingHash)}},
		{name: "source revision", reason: model.TranslationAttemptRejectionMalformedArgs, job: &rivertype.JobRow{ID: 51, Kind: model.TranslationJobKindV2, EncodedArgs: encode(badRevision)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			processor := &translationDiscardProcessor{}
			var logs bytes.Buffer
			handler := &riverErrorHandler{
				translationProcessor: processor,
				logger:               slog.New(slog.NewTextHandler(&logs, nil)),
			}
			handler.projectV2TranslationTerminal(context.Background(), tc.job, errors.New("terminal"), false)
			if processor.discardCalls != 0 || processor.cancellationCalls != 0 {
				t.Fatalf("projection calls discard=%d cancellation=%d", processor.discardCalls, processor.cancellationCalls)
			}
			if got := logs.String(); !strings.Contains(got, "translation terminal projection rejected") ||
				!strings.Contains(got, "reason="+tc.reason.String()) {
				t.Fatalf("log = %q, want reason=%s", got, tc.reason)
			}
		})
	}
}

func TestLegacyTranslationErrorHandlerProjectsOnlyProvenTerminalAttempt(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	const riverJobID int64 = 52
	encoded, err := json.Marshal(linktranslation.LegacyJobArgs{TranslationID: translationID})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	want := model.TranslationAttempt{
		TranslationID: translationID, AttemptGeneration: 0, RiverJobID: riverJobID,
	}
	resolver := terminalLegacyAttemptResolver{result: model.TranslationAttemptResolution{Attempt: want}}
	for _, tc := range []struct {
		name             string
		stage            model.TranslationJobsRolloutStage
		cause            error
		attempt          int
		wantDiscard      int
		wantCancellation int
	}{
		{name: "compat final failure", stage: model.TranslationJobsRolloutCompatV1, cause: errors.New("upstream failed"), attempt: 3, wantDiscard: 1},
		{name: "drain cancellation", stage: model.TranslationJobsRolloutDrainV1, cause: context.Canceled, attempt: 1, wantCancellation: 1},
		{name: "strict ignores legacy", stage: model.TranslationJobsRolloutStrictV2, cause: errors.New("upstream failed"), attempt: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			processor := &translationDiscardProcessor{}
			handler := &riverErrorHandler{
				translationProcessor: processor, legacyTranslationAttempts: resolver,
				translationPolicy: tc.stage.JobPolicy(),
			}
			handler.HandleError(context.Background(), &rivertype.JobRow{
				ID: riverJobID, Kind: model.TranslationJobKindLegacy, EncodedArgs: encoded,
				Attempt: tc.attempt, MaxAttempts: 3,
			}, tc.cause)
			if processor.discardCalls != tc.wantDiscard || processor.cancellationCalls != tc.wantCancellation {
				t.Fatalf("discard=%d cancellation=%d", processor.discardCalls, processor.cancellationCalls)
			}
			if tc.wantDiscard > 0 && processor.discardAttempt != want {
				t.Fatalf("discard attempt = %+v", processor.discardAttempt)
			}
			if tc.wantCancellation > 0 && processor.cancellationAttempt != want {
				t.Fatalf("cancellation attempt = %+v", processor.cancellationAttempt)
			}
		})
	}
}

func TestTranslationErrorHandlerLogsProjectionFailure(t *testing.T) {
	t.Parallel()

	translationID := uuid.New()
	encoded, err := json.Marshal(linktranslation.JobArgs{
		TranslationID: translationID, AttemptGeneration: 8,
		SourceHash: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	processor := &translationDiscardProcessor{discardErr: errors.New("database unavailable")}
	var logs bytes.Buffer
	handler := &riverErrorHandler{
		translationProcessor: processor, logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	handler.HandleError(context.Background(), &rivertype.JobRow{
		ID: 53, Kind: model.TranslationJobKindV2, EncodedArgs: encoded,
		Attempt: 3, MaxAttempts: 3,
	}, errors.New("upstream failed"))
	if processor.discardCalls != 1 {
		t.Fatalf("discard calls = %d", processor.discardCalls)
	}
	if got := logs.String(); !strings.Contains(got, "failed to project terminal translation job") ||
		!strings.Contains(got, "projection=failure") || !strings.Contains(got, "river_job_id=53") ||
		!strings.Contains(got, "translation_id="+translationID.String()) {
		t.Fatalf("log = %q", got)
	}
}

func TestRiverClientConfigWiresTranslationErrorHandler(t *testing.T) {
	t.Parallel()

	processor := &translationDiscardProcessor{}
	config := newRiverClientConfig(
		RiverQueueOptions{TranslationProcessor: processor},
		river.NewWorkers(),
		1,
		model.TranslationJobsRolloutStrictV2.JobPolicy(),
	)
	handler, ok := config.ErrorHandler.(*riverErrorHandler)
	if !ok || handler.translationProcessor != processor {
		t.Fatalf("ErrorHandler = %#v", config.ErrorHandler)
	}
}
