package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestParseStateAndRequeueShareLinkFirstLockOrder guards the transaction lock
// order shared by refresh/re-ingest and the parse worker. The held link lock
// puts requeue first in PostgreSQL's wait queue, then starts the worker state
// transition behind it. With the old parse_jobs -> links worker order, the
// worker held the job row while requeue held the link row and PostgreSQL
// aborted one side with 40P01. The links -> parse_jobs order serializes both.
func TestParseStateAndRequeueShareLinkFirstLockOrder(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	setup := repository.NewPGXLinkRepository(pool)
	link, oldJob, err := setup.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/lock-order",
		SourceKind: "url",
		SourceKey:  "https://example.com/lock-order",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	requeuePool := openNamedPool(t, "webtag_requeue_lock_order")
	workerPool := openNamedPool(t, "webtag_worker_lock_order")
	requeueRepo := repository.NewPGXLinkRepository(requeuePool)
	workerRepo := repository.NewPGXLinkRepository(workerPool)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var lockedID string
	if err := blocker.QueryRow(ctx, `SELECT id::text FROM links WHERE id = $1 FOR UPDATE`, link.ID).Scan(&lockedID); err != nil {
		t.Fatalf("lock link: %v", err)
	}

	requeueDone := make(chan error, 1)
	go func() {
		_, requeueErr := requeueRepo.RequeueExisting(ctx, link.ID, nil)
		requeueDone <- requeueErr
	}()
	waitForPostgresLock(t, ctx, pool, "webtag_requeue_lock_order")

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- workerRepo.MarkParseProcessing(ctx, link.ID, oldJob.ID)
	}()
	waitForPostgresLock(t, ctx, pool, "webtag_worker_lock_order")

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}

	requeueErr := <-requeueDone
	workerErr := <-workerDone
	assertNotDeadlock(t, "requeue", requeueErr)
	assertNotDeadlock(t, "worker", workerErr)
	if requeueErr != nil {
		t.Fatalf("RequeueExistingTx: %v", requeueErr)
	}
	if workerErr != nil && !errors.Is(workerErr, repository.ErrParseJobNotRunnable) {
		t.Fatalf("MarkParseProcessing: %v, want nil or ErrParseJobNotRunnable", workerErr)
	}

	var linkStatus model.LinkStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM links WHERE id = $1`, link.ID).Scan(&linkStatus); err != nil {
		t.Fatalf("read final link status: %v", err)
	}
	if linkStatus != model.LinkStatusPending {
		t.Fatalf("final link status = %q, want pending", linkStatus)
	}

	var oldStatus model.JobStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM parse_jobs WHERE id = $1`, oldJob.ID).Scan(&oldStatus); err != nil {
		t.Fatalf("read old job status: %v", err)
	}
	if oldStatus != model.JobStatusFailed {
		t.Fatalf("old job status = %q, want failed", oldStatus)
	}
}

func openNamedPool(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = postgresApplicationName(name)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open named pool %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// PostgreSQL stores application_name in a 64-byte field including its NUL
// terminator. Keep the observer and the connection startup parameter in sync
// when a test's descriptive prefix and UUID would otherwise exceed 63 bytes.
func postgresApplicationName(name string) string {
	const maxApplicationNameBytes = 63
	if len(name) <= maxApplicationNameBytes {
		return name
	}
	clipped := name[:maxApplicationNameBytes]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

func TestPostgresApplicationNamePreservesUTF8Boundary(t *testing.T) {
	name := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa你"
	got := postgresApplicationName(name)
	if len(got) != 62 || !utf8.ValidString(got) {
		t.Fatalf("postgresApplicationName(%q) = %q (%d bytes), want valid 62-byte prefix", name, got, len(got))
	}
}

func waitForPostgresLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applicationName string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	applicationName = postgresApplicationName(applicationName)
	for {
		var waiting bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE application_name = $1 AND wait_event_type = 'Lock'
			)`, applicationName).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe %s lock wait: %v", applicationName, err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never entered a PostgreSQL lock wait: %v", applicationName, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertNotDeadlock(t *testing.T, operation string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("%s hit a PostgreSQL deadlock: %v", operation, err)
	}
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s did not finish after lock release: %v", operation, err)
	}
}
