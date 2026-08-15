//go:build dbintegration

package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
	"webtag/internal/lockkey"
)

func TestMigrationRunnerSessionUsesOneBackendWithSingleConnectionSource(t *testing.T) {
	source := migrationTestPool(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var backendPID uint32
	var sessionPool *pgxpool.Pool
	err := withMigrationRunnerSession(ctx, source, func(db database.Querier) error {
		sessionPool = db.(*pgxpool.Pool)
		if got := sessionPool.Stat().MaxConns(); got != 1 {
			t.Fatalf("migration MaxConns = %d, want 1", got)
		}
		if err := db.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `CREATE TEMP TABLE migration_session_probe(value integer)`); err != nil {
			return err
		}
		tx, err := sessionPool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO migration_session_probe(value) VALUES (1)`); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_session_probe`).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("temporary table did not persist across migration transactions")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withMigrationRunnerSession() error = %v", err)
	}
	if got := sessionPool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("closed migration pool acquired connections = %d, want 0", got)
	}
	assertMigrationLockReleased(t, source, backendPID)
}

func TestMigrationRunnerSessionRejectsLockLoss(t *testing.T) {
	source := migrationTestPool(t, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var backendPID uint32
	err := withMigrationRunnerSession(ctx, source, func(db database.Querier) error {
		if err := db.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		var terminated bool
		if err := source.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
			return err
		}
		if !terminated {
			return errors.New("PostgreSQL did not terminate migration backend")
		}
		_, err := db.Exec(ctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("lock loss unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "release migration advisory lock") || !errors.Is(err, errMigrationSessionLost) {
		t.Fatalf("lock loss error = %v, want operation failure joined with session-loss cleanup failure", err)
	}
	assertMigrationLockReleased(t, source, backendPID)
}

func TestMigrationRunnerSessionSerializesWholeRun(t *testing.T) {
	source := migrationTestPool(t, 2)
	firstEntered := make(chan struct{})
	allowFirstExit := make(chan struct{})
	firstDone := make(chan error, 1)

	go func() {
		firstDone <- withMigrationRunnerSession(t.Context(), source, func(database.Querier) error {
			close(firstEntered)
			<-allowFirstExit
			return nil
		})
	}()
	<-firstEntered

	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelSecond()
	secondErr := withMigrationRunnerSession(secondCtx, source, func(database.Querier) error {
		return errors.New("second runner entered while first still held lock")
	})
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("second runner error = %v, want deadline while waiting for lock", secondErr)
	}

	close(allowFirstExit)
	if err := <-firstDone; err != nil {
		t.Fatalf("first runner error = %v", err)
	}
}

func TestMigrationRunnerSessionCancellationStillUnlocks(t *testing.T) {
	source := migrationTestPool(t, 1)
	runCtx, cancelRun := context.WithCancel(t.Context())
	var backendPID uint32
	err := withMigrationRunnerSession(runCtx, source, func(db database.Querier) error {
		if err := db.QueryRow(runCtx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		cancelRun()
		return runCtx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "release migration advisory lock") {
		t.Fatalf("cleanup inherited run cancellation: %v", err)
	}
	assertMigrationLockReleased(t, source, backendPID)
}

func migrationTestPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WEBTAG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WEBTAG_TEST_DATABASE_URL is not configured")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse WEBTAG_TEST_DATABASE_URL: %v", err)
	}
	config.MaxConns = maxConns
	config.MinConns = 0
	config.MinIdleConns = 0
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open migration test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertMigrationLockReleased(t *testing.T, pool *pgxpool.Pool, backendPID uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_locks
		WHERE locktype='advisory' AND pid=$1 AND classid=$2::oid AND objid=$3::oid AND objsubid=2 AND granted`,
		backendPID, lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration).Scan(&count); err != nil {
		t.Fatalf("inspect released migration lock: %v", err)
	}
	if count != 0 {
		t.Fatalf("backend %d still owns %d migration advisory locks", backendPID, count)
	}
}
