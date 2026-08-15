package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/lockkey"
	"webtag/internal/migrate"
)

// TestMigrateSerializesConcurrentRunners proves the runner lock is really taken
// against a live pool.
//
// It matters because invalid-index recovery turned concurrent migration runs
// from merely wasteful into destructive: PostgreSQL publishes an
// indisvalid=false catalog row for the whole duration of a
// CREATE INDEX CONCURRENTLY, so a second runner's recovery probe cannot tell an
// abandoned remnant from an index another runner is still building, and would
// drop the freshly valid index. Unit tests cannot cover this — their fake
// querier is not a *pgxpool.Pool, so lockMigrationRunner is a no-op there.
func TestMigrateSerializesConcurrentRunners(t *testing.T) {
	pool := StartPostgres(t)

	// Hold the runner lock from an independent session, the way a second
	// concurrently running migrate container would.
	blocker, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire blocking connection: %v", err)
	}
	defer blocker.Release()
	var acquired bool
	if err := blocker.QueryRow(t.Context(), `SELECT pg_try_advisory_lock($1, $2)`,
		lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration).Scan(&acquired); err != nil {
		t.Fatalf("take runner lock from blocking session: %v", err)
	}
	if !acquired {
		t.Fatal("runner lock was already held before the test started")
	}

	blocked := make(chan error, 1)
	go func() {
		blocked <- migrate.Up(t.Context(), pool)
	}()

	// Up must not finish while the lock is held elsewhere. A fixed wait is the
	// only way to assert "did not happen"; keep it short since the failure mode
	// (no lock at all) returns almost immediately on an already-migrated
	// database.
	select {
	case err := <-blocked:
		t.Fatalf("migrate.Up() returned %v while the runner lock was held; it did not serialize", err)
	case <-time.After(2 * time.Second):
	}

	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_unlock($1, $2)`,
		lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration); err != nil {
		t.Fatalf("release runner lock: %v", err)
	}

	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("migrate.Up() after lock release error = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("migrate.Up() did not proceed after the runner lock was released")
	}

	assertRunnerLockReleased(t, pool)
}

// TestMigrateReleasesRunnerLockOnFailure covers the path where the run aborts:
// a lock that outlives a failed migration blocks every later runner until the
// connection is reaped.
func TestMigrateReleasesRunnerLockOnFailure(t *testing.T) {
	pool := StartPostgres(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := migrate.Up(ctx, pool)
	if err == nil {
		t.Fatal("migrate.Up() with a canceled context returned nil, want failure")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("migrate.Up() error = %v (not context.Canceled, still expected to release)", err)
	}

	assertRunnerLockReleased(t, pool)
}

func assertRunnerLockReleased(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var held int
	if err := pool.QueryRow(t.Context(), `SELECT count(*)
		FROM pg_locks
		WHERE locktype = 'advisory' AND classid = $1 AND objid = $2`,
		lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration).Scan(&held); err != nil {
		t.Fatalf("probe leftover runner locks: %v", err)
	}
	if held != 0 {
		t.Fatalf("advisory locks in the migration namespace = %d, want 0", held)
	}
}
