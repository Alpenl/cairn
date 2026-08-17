// Package dbintegration is the testcontainers-go Postgres helper used by
// every test in this sub-module. It is migrated from the original
// internal/dbtest/postgres_dbintegration.go; the file no longer needs
// the `//go:build dbintegration` build tag because the entire sub-module
// is opt-in (it lives in its own go.mod and is not built by the main
// module's `go build ./...`).
//
// Lifecycle:
//
//   - First StartPostgres call in a `go test` process boots ONE
//     container, runs internal/migrate.Up against it, and stashes the
//     handle on a process-wide singleton.
//   - Each subsequent call truncates every installation-owned business table
//     (preserving schema_migrations and singleton state) and
//     returns a fresh *pgxpool.Pool the test can use without coordinating
//     with other tests.
//   - The container survives until the test binary exits; pool cleanup
//     is hooked via t.Cleanup. Tests never explicitly Close the pool.
//
// Why a singleton container?
//
//	Booting Postgres takes ~1.5–3s on a warm host. Four tests would
//	pay >6s of pure container churn before any SQL runs. The migration
//	set is small enough (~10 steps) that re-running it per test would
//	add another second of overhead each. One container + TRUNCATE
//	between tests is the cheapest model that still gives row-level
//	isolation.
//
// The helper deliberately returns the full *pgxpool.Pool (not a
// database.Pool interface). Integration tests want raw pg_catalog
// probes, dialect-specific assertions, and SQL the production
// Querier surface does not expose; forcing them through the interface
// would erase exactly the coverage this file exists to provide.
package dbintegration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"webtag/internal/migrate"
)

const (
	dbName     = "webtag_test"
	dbUser     = "webtag"
	dbPassword = "webtag-test"

	// dbIntegrationRequiredEnv makes infrastructure bootstrap failures fatal.
	// Local and sandbox runs leave it unset so an unavailable Docker daemon
	// keeps the opt-in suite skippable; the required Make target sets it.
	dbIntegrationRequiredEnv = "WEBTAG_DBINTEGRATION_REQUIRED"
	// postgresImage is pinned to pgvector/pgvector:pg16 — tag-pinned, not
	// digest-pinned, so the image can still move underneath us. That is
	// precisely why dbintegration.yml keeps a nightly cron: no diff in this
	// repo can announce such a shift. The image is the stock
	// postgres:16 image plus the pgvector extension pre-installed. The
	// Phase 5 (v3.0) migration runs `CREATE EXTENSION vector` + builds an
	// HNSW index, which the plain postgres:16-alpine image cannot satisfy
	// (extension not bundled). pgvector/pgvector:pg16 is the upstream
	// pgvector project's official image (Debian-based, PG 16). Bump this in
	// lockstep with the migration and container smoke scripts when the
	// production target moves.
	postgresImage = "pgvector/pgvector:pg16"
)

// containerState holds the singleton container + base pool shared
// across every test in a single `go test` process. mu guards the
// truncate step so two parallel tests cannot interleave their setup.
type containerState struct {
	mu        sync.Mutex
	once      sync.Once
	initErr   error
	container *tcpg.PostgresContainer
	dsn       string
	// adminPool is the post-migration pool used to TRUNCATE between
	// tests. Tests do not see it; they receive their own per-test pool
	// pointed at the same database so connection lifecycle is isolated.
	adminPool *pgxpool.Pool
}

var sharedContainer = &containerState{}

// StartPostgres boots (or reuses) a Postgres container, provisions a fresh
// database through the complete migration tail, truncates every business table so
// the test starts from a clean slate, and returns a fresh
// *pgxpool.Pool. The pool is auto-closed via t.Cleanup; the container
// itself stays alive for the lifetime of the test binary.
//
// Tests MUST NOT close the returned pool themselves — t.Cleanup does
// it. Doing so manually would leave a half-closed pool registered for
// post-test cleanup and produce confusing nil-deref panics on the
// second close.
//
// When the boot step fails (no Docker daemon, image pull blocked, etc.), a
// local or sandbox run skips because the sub-module is opt-in. The required
// Make target sets WEBTAG_DBINTEGRATION_REQUIRED=true, which makes the same
// bootstrap failure fatal instead of allowing a green run with no PostgreSQL
// coverage.
type postgresTest interface {
	postgresUnavailableReporter
	Context() context.Context
	Cleanup(func())
}

func StartPostgres(t postgresTest) *pgxpool.Pool {
	t.Helper()

	sharedContainer.once.Do(func() {
		sharedContainer.initErr = sharedContainer.boot()
	})
	if sharedContainer.initErr != nil {
		reportPostgresUnavailable(t, sharedContainer.initErr)
		return nil // unreachable for *testing.T: Fatalf and Skipf both call Goexit
	}

	sharedContainer.mu.Lock()
	if err := truncateAllTables(t.Context(), sharedContainer.adminPool); err != nil {
		sharedContainer.mu.Unlock()
		t.Fatalf("dbintegration: truncate tables: %v", err)
	}
	sharedContainer.mu.Unlock()

	pool, err := pgxpool.New(t.Context(), sharedContainer.dsn)
	if err != nil {
		t.Fatalf("dbintegration: open per-test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type postgresUnavailableReporter interface {
	Helper()
	Fatalf(string, ...any)
	Skipf(string, ...any)
}

func reportPostgresUnavailable(t postgresUnavailableReporter, err error) {
	t.Helper()
	if dbIntegrationRequired() {
		t.Fatalf("dbintegration: required postgres container unavailable: %v", err)
		return
	}
	t.Skipf("dbintegration: postgres container unavailable: %v", err)
}

func dbIntegrationRequired() bool {
	raw := strings.TrimSpace(os.Getenv(dbIntegrationRequiredEnv))
	if raw == "" {
		return false
	}
	required, err := strconv.ParseBool(raw)
	if err != nil {
		// An explicit but malformed value must not silently disable a required
		// CI gate. Treat it as enabled and surface the real bootstrap failure.
		return true
	}
	return required
}

// DSN returns the connection string of the shared container. Exposed
// so tests that need to drive a `cmd/migrate`-style probe (or open
// their own pgx.Conn for a session-scoped feature like advisory
// locks) can bypass the per-test pool. StartPostgres must have been
// called at least once first.
func DSN(t *testing.T) string {
	t.Helper()
	if sharedContainer.dsn == "" {
		t.Fatal("dbintegration: DSN() called before StartPostgres()")
	}
	return sharedContainer.dsn
}

// boot starts the container, runs migrations, and stashes the admin
// pool on the shared singleton. Only ever called inside once.Do, so
// no concurrency guard required beyond what once already gives us.
func (c *containerState) boot() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tcpg.Run(
		ctx,
		postgresImage,
		tcpg.WithDatabase(dbName),
		tcpg.WithUsername(dbUser),
		tcpg.WithPassword(dbPassword),
		// BasicWaitStrategies waits for the "ready to accept
		// connections" log line twice (PG prints it once during
		// init then again post-init when the cluster will actually
		// accept inbound connections) and for Docker to bind the
		// published port on the host. Without this the per-test
		// pgxpool.New races the cluster startup on cold-pull
		// machines and surfaces as flaky "connection refused"
		// failures.
		tcpg.BasicWaitStrategies(),
	)
	if err != nil {
		return fmt.Errorf("start postgres container: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return fmt.Errorf("derive connection string: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(ctx)
		return fmt.Errorf("open admin pool: %w", err)
	}

	// The shared container is a fresh database. Provision it through the
	// complete fresh-install path so ordinary integration tests exercise the
	// current runtime schema; tests that specifically verify the release-gated
	// default Up path use an isolated database instead.
	if err := migrate.UpFreshInstall(ctx, pool); err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		return fmt.Errorf("apply fresh-install migrations: %w", err)
	}

	c.container = container
	c.adminPool = pool
	c.dsn = dsn
	return nil
}

// truncateAllTables clears every installation-owned business table the suite
// touches. schema_migrations is intentionally preserved so migrate.Up
// stays idempotent across the test process — re-running migrations on
// a half-cleared schema_migrations would attempt to re-create tables
// that already exist and surface as confusing errors.
//
// CASCADE handles the parse_jobs -> links FK and the
// concept_alias / link_concept / concept_merge_proposal → concept FKs
// without the caller having to spell out a dependency order. RESTART
// IDENTITY is harmless here (no sequences) and future-proofs against
// added SERIAL columns. The concept tables are truncated too so the v3
// vector-gate integration tests start from an empty concept词表 every
// run (a leftover concept with a vector would poison the cold-start /
// nearest-neighbour assertions).
func truncateAllTables(ctx context.Context, pool *pgxpool.Pool) error {
	// River's runtime tables remain under River's ownership except river_job:
	// queued jobs are test data and would contaminate count and lifecycle
	// assertions. Its migration ledger, queue registration, leader, and client
	// tables stay intact. The four singleton tables are preserved and their
	// mutable revisions are reset after business data is removed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin truncate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `TRUNCATE TABLE
		concept,
		concept_alias,
		concept_merge_proposal,
		feed_folders,
		feed_items,
		feed_subscriptions,
		idempotency_keys,
		library_classification_rules,
		library_review_items,
		link_concept,
		link_translations,
		link_url_identities,
		links,
		parse_jobs,
		reader_categories,
		reader_categorizables,
		reader_content_history,
		reader_domain_activity,
		reader_engagement,
		reader_feed_feedback,
		reader_feed_saves,
		reader_feed_snapshots,
		reader_host_purge_receipts,
		reader_inbox,
		reader_inbox_jobs,
		reader_note_history,
		reader_notes,
		reader_tag_activity,
		reader_thought_ops,
		reader_thought_supersession_events,
		reader_thought_tombstones,
		reader_thoughts,
		reader_todo_projection_backfills,
		reader_todos,
		site_entries,
		site_identities,
		site_tags,
		sites,
		river_job
		RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("truncate tables: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE library_read_revision SET revision=0, updated_at=now() WHERE singleton;
		UPDATE global_read_revision SET revision=0, updated_at=now() WHERE singleton;
		UPDATE feed_read_revision SET revision=0, updated_at=now() WHERE singleton;
	`); err != nil {
		return fmt.Errorf("reset representation revisions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit truncate transaction: %w", err)
	}
	return nil
}
