package dbintegration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

func TestLifecycleRepairMigrationCancelsRunningParseWorker(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `INSERT INTO schema_migrations(version)
			VALUES ($1) ON CONFLICT DO NOTHING`, lifecycleRepairMigrationID); err != nil {
			t.Errorf("restore lifecycle repair ledger: %v", err)
		}
	})

	linkID, attemptID := insertPendingLinkAndJob(t, pool,
		"https://lifecycle-repair.example/running-parse")
	processor := &cancellationAwareProcessor{
		targetJobID: attemptID,
		started:     make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	queue := newRiverQueue(t, pool, processor)
	if err := queue.Enqueue(ctx, linkID, attemptID); err != nil {
		t.Fatalf("enqueue running migration fixture: %v", err)
	}
	if err := queue.Start(ctx); err != nil {
		t.Fatalf("start River queue: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := queue.Stop(stopCtx); err != nil {
			t.Errorf("stop River queue: %v", err)
		}
	})

	select {
	case <-processor.started:
	case <-time.After(10 * time.Second):
		t.Fatal("parse worker did not enter running state")
	}

	var riverJobID int64
	var runningState string
	if err := pool.QueryRow(ctx, `SELECT id,state::text
		FROM river_job
		WHERE kind='parse_link' AND args->>'parse_job_id'=$1`, attemptID.String()).
		Scan(&riverJobID, &runningState); err != nil {
		t.Fatalf("read running River job: %v", err)
	}
	if runningState != "running" {
		t.Fatalf("River state before migration = %q, want running", runningState)
	}
	if _, err := pool.Exec(ctx, `UPDATE links SET deleted_at=NOW(),updated_at=NOW() WHERE id=$1`, linkID); err != nil {
		t.Fatalf("trash running migration fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, lifecycleRepairMigrationID); err != nil {
		t.Fatalf("remove lifecycle repair ledger: %v", err)
	}

	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("apply lifecycle repair migration: %v", err)
	}
	select {
	case <-processor.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle migration did not cancel the running worker context")
	}

	first := waitForLifecycleMigrationRunningTerminal(t, pool, riverJobID, attemptID)
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, lifecycleRepairMigrationID); err != nil {
		t.Fatalf("remove lifecycle repair ledger for replay: %v", err)
	}
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("replay lifecycle repair migration: %v", err)
	}
	second := readLifecycleMigrationRunningState(t, pool, riverJobID, attemptID)
	if second != first {
		t.Fatalf("lifecycle repair replay changed terminal state\nfirst:  %#v\nsecond: %#v", first, second)
	}
}

type lifecycleMigrationRunningState struct {
	RiverState       string
	CancelAttempted  string
	RiverFinalizedAt string
	AttemptStatus    string
	AttemptError     string
	AttemptUpdatedAt time.Time
}

func waitForLifecycleMigrationRunningTerminal(
	t *testing.T,
	pool *pgxpool.Pool,
	riverJobID int64,
	attemptID uuid.UUID,
) lifecycleMigrationRunningState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state := readLifecycleMigrationRunningState(t, pool, riverJobID, attemptID)
		if state.RiverState == "cancelled" && state.RiverFinalizedAt != "" {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("River terminal state after migration = %q/%q, want cancelled/non-empty finalized_at",
				state.RiverState, state.RiverFinalizedAt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readLifecycleMigrationRunningState(
	t *testing.T,
	pool *pgxpool.Pool,
	riverJobID int64,
	attemptID uuid.UUID,
) lifecycleMigrationRunningState {
	t.Helper()
	var state lifecycleMigrationRunningState
	if err := pool.QueryRow(t.Context(), `SELECT
		job.state::text,
		COALESCE(job.metadata->>'cancel_attempted_at',''),
		COALESCE(job.finalized_at::text,''),
		attempt.status,
		COALESCE(attempt.error_msg,''),
		attempt.updated_at
		FROM river_job AS job
		JOIN parse_jobs AS attempt ON attempt.id=$2
		WHERE job.id=$1`, riverJobID, attemptID).Scan(
		&state.RiverState,
		&state.CancelAttempted,
		&state.RiverFinalizedAt,
		&state.AttemptStatus,
		&state.AttemptError,
		&state.AttemptUpdatedAt,
	); err != nil {
		t.Fatalf("read lifecycle migration terminal state: %v", err)
	}
	if state.AttemptStatus != "failed" || state.AttemptError != "link_deleted" {
		t.Fatalf("parse attempt after migration = %q/%q, want failed/link_deleted",
			state.AttemptStatus, state.AttemptError)
	}
	if state.CancelAttempted == "" {
		t.Fatal("running River job is missing cancel_attempted_at")
	}
	return state
}
