package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/observability"
	"webtag/internal/service"
)

type contentHistoryCleanupOutcome struct {
	deleted int
	err     error
}

type contentHistoryCleanupCall struct {
	limit int
}

type contentHistoryCleanupStoreStub struct {
	outcomes       []contentHistoryCleanupOutcome
	defaultDeleted int
	calls          []contentHistoryCleanupCall
	afterCall      func(int)
}

func (s *contentHistoryCleanupStoreStub) CleanupContentHistoryBatch(ctx context.Context, limit int) (int, error) {
	s.calls = append(s.calls, contentHistoryCleanupCall{limit: limit})
	callNumber := len(s.calls)
	if s.afterCall != nil {
		defer s.afterCall(callNumber)
	}
	if len(s.outcomes) == 0 {
		return s.defaultDeleted, nil
	}
	outcome := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	return outcome.deleted, outcome.err
}

func TestContentHistoryRetentionWorkerStopsAfterPartialBatch(t *testing.T) {
	t.Parallel()

	store := &contentHistoryCleanupStoreStub{outcomes: []contentHistoryCleanupOutcome{{deleted: 100}, {deleted: 25}}}
	worker := newContentHistoryRetentionWorkerForTest(
		store,
		time.Minute,
		nil,
		nil,
	)

	if err := worker.Work(context.Background(), &river.Job[ContentHistoryRetentionJobArgs]{}); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("cleanup calls = %#v, want 2", store.calls)
	}
	for i, call := range store.calls {
		if call.limit != ContentHistoryRetentionBatchSize {
			t.Fatalf("cleanup call %d = %#v, want limit %d", i, call, ContentHistoryRetentionBatchSize)
		}
	}
}

func TestContentHistoryRetentionWorkerCapsInstallationRunAtOneThousand(t *testing.T) {
	t.Parallel()

	store := &contentHistoryCleanupStoreStub{defaultDeleted: ContentHistoryRetentionBatchSize}
	worker := newContentHistoryRetentionWorkerForTest(
		store,
		time.Minute,
		nil,
		nil,
	)

	job := &river.Job[ContentHistoryRetentionJobArgs]{}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	wantCalls := ContentHistoryRetentionRunLimit / ContentHistoryRetentionBatchSize
	if len(store.calls) != wantCalls {
		t.Fatalf("cleanup calls = %d, want %d", len(store.calls), wantCalls)
	}
	for i, call := range store.calls {
		if call.limit != ContentHistoryRetentionBatchSize {
			t.Fatalf("cleanup call %d = %#v, want limit %d", i, call, ContentHistoryRetentionBatchSize)
		}
	}
}

func TestContentHistoryRetentionWorkerStopsOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	store := &contentHistoryCleanupStoreStub{
		defaultDeleted: ContentHistoryRetentionBatchSize,
		afterCall: func(call int) {
			if call == 1 {
				cancel()
			}
		},
	}
	worker := newContentHistoryRetentionWorkerForTest(store, time.Minute, nil, nil)

	err := worker.Work(ctx, &river.Job[ContentHistoryRetentionJobArgs]{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Work() error = %v, want context.Canceled", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("cleanup calls after cancellation = %d, want 1", len(store.calls))
	}
}

func TestContentHistoryRetentionWorkerReturnsRetryableError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("temporary database failure")
	store := &contentHistoryCleanupStoreStub{outcomes: []contentHistoryCleanupOutcome{{err: wantErr}, {deleted: 0}}}
	worker := newContentHistoryRetentionWorkerForTest(
		store,
		time.Minute,
		nil,
		nil,
	)

	err := worker.Work(context.Background(), &river.Job[ContentHistoryRetentionJobArgs]{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("first Work() error = %v, want wrapped transient error", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("first attempt calls = %#v, want one failed batch", store.calls)
	}
	if strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("worker error exposed underlying database text: %q", err)
	}

	if err := worker.Work(context.Background(), &river.Job[ContentHistoryRetentionJobArgs]{}); err != nil {
		t.Fatalf("retry Work() error = %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("retry calls = %#v, want failed batch retried", store.calls)
	}
}

type convergingContentHistoryCleanupStore struct {
	remaining int
	calls     int
}

type contentHistoryPeriodicReaderStub struct{}

func (contentHistoryPeriodicReaderStub) RunReaderInboxSummaryJob(context.Context, service.ReaderInboxSummaryJobArgs, int, int) error {
	return nil
}

func (contentHistoryPeriodicReaderStub) RunReaderInboxExpiryJob(context.Context, int) error {
	return nil
}

type contentHistoryPeriodicBackfillStub struct{}

func (contentHistoryPeriodicBackfillStub) Run(context.Context) (int, int, int, error) {
	return 0, 0, 0, nil
}

func (s *convergingContentHistoryCleanupStore) CleanupContentHistoryBatch(ctx context.Context, limit int) (int, error) {
	s.calls++
	deleted := min(limit, s.remaining)
	s.remaining -= deleted
	return deleted, nil
}

func TestContentHistoryRetentionWorkerRepeatedExecutionIsIdempotent(t *testing.T) {
	t.Parallel()

	store := &convergingContentHistoryCleanupStore{remaining: 225}
	worker := newContentHistoryRetentionWorkerForTest(store, time.Minute, nil, nil)
	job := &river.Job[ContentHistoryRetentionJobArgs]{}

	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("first Work() error = %v", err)
	}
	if store.remaining != 0 || store.calls != 3 {
		t.Fatalf("first Work() left remaining=%d calls=%d, want 0/3", store.remaining, store.calls)
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("second Work() error = %v", err)
	}
	if store.remaining != 0 || store.calls != 4 {
		t.Fatalf("second Work() left remaining=%d calls=%d, want idempotent 0/4", store.remaining, store.calls)
	}
}

func TestContentHistoryRetentionPeriodicRegistrationIsUniqueAndFixed(t *testing.T) {
	t.Parallel()

	store := &contentHistoryCleanupStoreStub{}
	options := RiverQueueOptions{
		ReaderInboxProcessor:    contentHistoryPeriodicReaderStub{},
		LinkEmbeddingBackfill:   contentHistoryPeriodicBackfillStub{},
		ContentHistoryRetention: store,
	}
	registrations := riverPeriodicJobRegistrations(options)
	if len(registrations) != 3 {
		t.Fatalf("periodic registrations = %#v, want expiry, backfill, and retention", registrations)
	}
	seen := make(map[string]struct{}, len(registrations))
	var retentionRegistrations []riverPeriodicJobRegistration
	for _, registration := range registrations {
		if _, duplicate := seen[registration.id]; duplicate {
			t.Fatalf("duplicate periodic job ID %q", registration.id)
		}
		seen[registration.id] = struct{}{}
		if registration.id == ContentHistoryRetentionJobKind {
			retentionRegistrations = append(retentionRegistrations, registration)
		}
	}
	if len(retentionRegistrations) != 1 {
		t.Fatalf("retention registrations = %#v, want exactly one ID %q", retentionRegistrations, ContentHistoryRetentionJobKind)
	}
	registration := retentionRegistrations[0]
	if !registration.runOnStart {
		t.Fatal("retention RunOnStart = false, want true")
	}
	if registration.interval != 15*time.Minute {
		t.Fatalf("retention interval = %s, want 15m", registration.interval)
	}
	if ContentHistoryRetentionInterval != 15*time.Minute {
		t.Fatalf("ContentHistoryRetentionInterval = %s, want 15m", ContentHistoryRetentionInterval)
	}
	args, insertOpts := registration.constructor()
	if _, ok := args.(ContentHistoryRetentionJobArgs); !ok {
		t.Fatalf("periodic constructor args = %T, want ContentHistoryRetentionJobArgs", args)
	}
	if insertOpts != nil {
		t.Fatalf("periodic constructor opts = %#v, want args-owned defaults", insertOpts)
	}

	jobOpts := (ContentHistoryRetentionJobArgs{}).InsertOpts()
	if jobOpts.MaxAttempts != ContentHistoryRetentionJobMaxAttempts || !jobOpts.UniqueOpts.ByArgs {
		t.Fatalf("retention insert opts = %#v, want max attempts %d and ByArgs uniqueness", jobOpts, ContentHistoryRetentionJobMaxAttempts)
	}
	wantStates := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
		rivertype.JobStateRetryable,
	}
	if !reflect.DeepEqual(jobOpts.UniqueOpts.ByState, wantStates) {
		t.Fatalf("retention unique states = %v, want %v", jobOpts.UniqueOpts.ByState, wantStates)
	}

	queue, err := NewRiverQueue(RiverQueueOptions{
		Pool:                    &pgxpool.Pool{},
		Processor:               &discardRecordingProcessor{},
		ReaderInboxProcessor:    contentHistoryPeriodicReaderStub{},
		LinkEmbeddingBackfill:   contentHistoryPeriodicBackfillStub{},
		ContentHistoryRetention: store,
	})
	if err != nil {
		t.Fatalf("NewRiverQueue() error = %v", err)
	}
	configValue := reflect.ValueOf(queue.client).Elem().FieldByName("config").Elem()
	periodicJobs := configValue.FieldByName("PeriodicJobs")
	if !periodicJobs.IsValid() || periodicJobs.Len() != 3 {
		t.Fatalf("River periodic jobs = %v, want expiry, backfill, and exactly one retention schedule", periodicJobs)
	}
}

func TestContentHistoryRetentionObservabilityNeverContainsSnapshotText(t *testing.T) {
	t.Parallel()

	const secretSnapshot = "SECRET-SNAPSHOT-TEXT-MUST-NOT-LEAK"
	wantErr := errors.New("database rejected body " + secretSnapshot)
	store := &contentHistoryCleanupStoreStub{outcomes: []contentHistoryCleanupOutcome{{deleted: 100}, {err: wantErr}}}
	metrics := observability.NewMetrics()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	worker := newContentHistoryRetentionWorkerForTest(store, time.Minute, logger, metrics)

	err := worker.Work(context.Background(), &river.Job[ContentHistoryRetentionJobArgs]{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Work() error = %v, want wrapped store error", err)
	}
	if strings.Contains(err.Error(), secretSnapshot) {
		t.Fatalf("worker error leaked snapshot text: %q", err)
	}
	if strings.Contains(logs.String(), secretSnapshot) {
		t.Fatalf("worker logs leaked snapshot text: %s", logs.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("decode retention log: %v; raw=%s", err, logs.String())
	}
	allowedLogKeys := map[string]bool{
		"time": true, "level": true, "msg": true,
		"deleted": true, "backlog": true, "failure_category": true,
	}
	for key := range entry {
		if !allowedLogKeys[key] {
			t.Fatalf("retention log contains unsupported key %q: %v", key, entry)
		}
	}
	if entry["failure_category"] != "database" {
		t.Fatalf("retention log classification = %v, want database", entry)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricBody := recorder.Body.String()
	if strings.Contains(metricBody, secretSnapshot) {
		t.Fatalf("retention metrics leaked snapshot text:\n%s", metricBody)
	}
	for _, want := range []string{
		`webtag_reader_content_history_deleted_total 100`,
		`webtag_reader_content_history_cleanup_runs_total{backlog="true",failure_category="database"} 1`,
	} {
		if !strings.Contains(metricBody, want) {
			t.Fatalf("retention metrics missing %q:\n%s", want, metricBody)
		}
	}
}

func TestContentHistoryRetentionWorkerKeepsConfiguredTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 37 * time.Second
	worker := newContentHistoryRetentionWorkerForTest(
		&contentHistoryCleanupStoreStub{},
		timeout,
		nil,
		nil,
	)
	if got := worker.Timeout(nil); got != timeout {
		t.Fatalf("Timeout() = %s, want %s", got, timeout)
	}
}
