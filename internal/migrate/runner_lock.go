package migrate

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
	"webtag/internal/lockkey"
)

const migrationRunnerCleanupTimeout = 5 * time.Second

// pgxpool computes a connection's expiry as creation time plus this value;
// zero therefore means "expire immediately", not "disabled". A century is
// effectively unbounded for one migration command while remaining within
// time.Duration and time.Time limits.
const migrationSessionConnectionLifetime = 100 * 365 * 24 * time.Hour

var errMigrationSessionLost = errors.New("migration session lost advisory lock")

// migrationSession owns the only connection in a private pool. River's
// migrator requires a *pgxpool.Pool, while the application migrations only
// require database.Querier. A one-connection pool lets both use the same
// backend without reserving one slot for the lock and deadlocking a
// pool_max_conns=1 runner.
type migrationSession struct {
	pool       *pgxpool.Pool
	backendPID atomic.Uint32
}

// withMigrationRunnerSession serializes a whole migration run and binds all
// work to the session that owns the advisory lock. Test fakes keep their
// lightweight path and do not need pool plumbing.
func withMigrationRunnerSession(
	ctx context.Context,
	db database.Querier,
	run func(database.Querier) error,
) error {
	sourcePool, ok := db.(*pgxpool.Pool)
	if !ok {
		return run(db)
	}

	session, err := newMigrationSession(ctx, sourcePool)
	if err != nil {
		return err
	}
	runErr := run(session.pool)
	releaseErr := session.release(ctx)
	return errors.Join(runErr, releaseErr)
}

func newMigrationSession(ctx context.Context, sourcePool *pgxpool.Pool) (*migrationSession, error) {
	config := sourcePool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	// The session lock is the lifetime policy for this private pool. Client-side
	// idle/lifetime reaping would silently replace the backend between River
	// migrations, so disable it and detect server/proxy replacement below.
	config.MaxConnLifetime = migrationSessionConnectionLifetime
	config.MaxConnLifetimeJitter = 0
	config.MaxConnIdleTime = migrationSessionConnectionLifetime

	session := &migrationSession{}
	originalPrepareConn := config.PrepareConn
	// Preserve callers that still use pgx's legacy hook by composing it into
	// PrepareConn, which otherwise takes precedence and silently ignores it.
	originalBeforeAcquire := config.BeforeAcquire //nolint:staticcheck // compatibility bridge for pgx's deprecated hook
	config.BeforeAcquire = nil                    //nolint:staticcheck // PrepareConn below now owns the composed policy
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		if originalPrepareConn != nil {
			valid, err := originalPrepareConn(ctx, conn)
			if !valid || err != nil {
				return valid, err
			}
		} else if originalBeforeAcquire != nil && !originalBeforeAcquire(ctx, conn) {
			return false, nil
		}

		expectedPID := session.backendPID.Load()
		if expectedPID == 0 {
			return true, nil
		}
		var actualPID uint32
		var lockHeld bool
		if err := conn.QueryRow(ctx, `SELECT pg_backend_pid(), EXISTS (
			SELECT 1
			FROM pg_catalog.pg_locks
			WHERE locktype = 'advisory'
			  AND pid = pg_backend_pid()
			  AND classid = $1::oid
			  AND objid = $2::oid
			  AND objsubid = 2
			  AND granted
		)`, lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration).Scan(&actualPID, &lockHeld); err != nil {
			return false, fmt.Errorf("verify migration backend session: %w", err)
		}
		if actualPID != expectedPID || !lockHeld {
			return false, fmt.Errorf("%w: expected backend %d, got backend %d (lock held: %t)",
				errMigrationSessionLost, expectedPID, actualPID, lockHeld)
		}
		return true, nil
	}

	privatePool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create migration session pool: %w", err)
	}
	session.pool = privatePool

	conn, err := privatePool.Acquire(ctx)
	if err != nil {
		privatePool.Close()
		return nil, fmt.Errorf("acquire migration session: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`,
		lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration); err != nil {
		conn.Release()
		privatePool.Close()
		return nil, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	var backendPID uint32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationRunnerCleanupTimeout)
		_ = conn.Conn().Close(cleanupCtx)
		cancel()
		conn.Release()
		privatePool.Close()
		return nil, fmt.Errorf("identify migration backend session: %w", err)
	}
	session.backendPID.Store(backendPID)
	conn.Release()
	return session, nil
}

func (s *migrationSession) release(runCtx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), migrationRunnerCleanupTimeout)
	defer cancel()

	var released bool
	err := s.pool.QueryRow(cleanupCtx, `SELECT pg_advisory_unlock($1, $2)`,
		lockkey.ClassSchemaMigration, lockkey.ObjectSchemaMigration).Scan(&released)
	if err != nil {
		err = fmt.Errorf("release migration advisory lock: %w", err)
	} else if !released {
		err = fmt.Errorf("release migration advisory lock: PostgreSQL reported lock not held")
	}
	// No executor escapes the synchronous run callback, so Close cannot wait on
	// an acquired connection. Closing also guarantees that any uncertain lock
	// state is cleared by terminating the private backend.
	s.pool.Close()
	return err
}
