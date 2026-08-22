package dbintegration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

const (
	productionBaselineSchemaPath   = "testdata/v0.1.17/schema.sql"
	productionBaselineSchemaSHA256 = "a6c6af7aaf1e19ed83b9bca8ad9abff3a0d3a09c27000c990efefd788b8dda11"
)

var productionBaselineRiverVersions = []int{1, 2, 3, 4, 5, 6, 7}

type migrationRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func assertMigrationRecorded(t *testing.T, db migrationRowQuerier, id string, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM public.schema_migrations WHERE version = $1
	)`, id).Scan(&got); err != nil {
		t.Fatalf("read migration %s state: %v", id, err)
	}
	if got != want {
		t.Fatalf("migration %s recorded = %v, want %v", id, got, want)
	}
}

func assertIndexExists(t *testing.T, db migrationRowQuerier, name string, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(t.Context(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&got); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("index %s exists = %v, want %v", name, got, want)
	}
}

func isolatedMigrationDatabase(t *testing.T) string {
	t.Helper()
	_ = StartPostgres(t)

	baseURL, err := url.Parse(DSN(t))
	if err != nil {
		t.Fatalf("parse shared Postgres DSN: %v", err)
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	adminPool, err := pgxpool.New(t.Context(), adminURL.String())
	if err != nil {
		t.Fatalf("open Postgres admin pool: %v", err)
	}

	databaseName := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(t.Context(), `CREATE DATABASE `+quotedName); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated migration database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, databaseName)
		if _, err := adminPool.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+quotedName); err != nil {
			t.Errorf("drop isolated migration database: %v", err)
		}
		adminPool.Close()
	})

	targetURL := *baseURL
	targetURL.Path = "/" + databaseName
	return targetURL.String()
}

// prepareProductionUpgradeFixture installs the immutable v0.1.17 schema
// fixture and records the exact production baseline ledger. The fixture is a
// pg_dump snapshot, not a reverse-built current schema. The fixture is
// schema-only, so this helper also restores the seed rows and River ledger that
// v0.1.17 migrations had written by the time production reached that tag.
func prepareProductionUpgradeFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin production upgrade fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), productionBaselineSchemaSQL(t)); err != nil {
		t.Fatalf("install v0.1.17 schema fixture: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `
		SELECT pg_catalog.set_config('search_path','public, pg_catalog',false);
		INSERT INTO public.installation_state (singleton) VALUES (true);
		INSERT INTO public.library_read_revision (singleton) VALUES (true);
		INSERT INTO public.global_read_revision (singleton) VALUES (true);
		INSERT INTO public.feed_read_revision (singleton) VALUES (true);
		INSERT INTO public.feed_subscriptions (url,canonical_url,title)
		VALUES (
			'https://www.ruanyifeng.com/blog/atom.xml',
			'https://www.ruanyifeng.com/blog/atom.xml',
			'阮一峰的网络日志'
		);
		UPDATE public.feed_read_revision SET revision=0,updated_at=now() WHERE singleton;
		DELETE FROM public.schema_migrations;
		DELETE FROM public.river_migration;
	`); err != nil {
		t.Fatalf("seed v0.1.17 baseline rows: %v", err)
	}
	for _, version := range migrate.ProductionBaselineVersions() {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO public.schema_migrations(version) VALUES ($1)`, version); err != nil {
			t.Fatalf("record production baseline migration %s: %v", version, err)
		}
	}
	for _, version := range productionBaselineRiverVersions {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO public.river_migration(line,version) VALUES ($1,$2)`,
			migrate.RiverLedgerLine, version); err != nil {
			t.Fatalf("record v0.1.17 River migration %d: %v", version, err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit production upgrade fixture: %v", err)
	}
	assertProductionBaselineLedger(t, pool)
}

func productionBaselineSchemaSQL(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(productionBaselineSchemaPath)
	if err != nil {
		t.Fatalf("read v0.1.17 schema fixture: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := fmt.Sprintf("%x", sum); got != productionBaselineSchemaSHA256 {
		t.Fatalf("v0.1.17 schema fixture checksum = %s, want %s", got, productionBaselineSchemaSHA256)
	}
	return string(raw)
}

func assertProductionBaselineLedger(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	assertMigrationRecorded(t, pool, migrate.CurrentSchemaMigrationID, false)
	for _, version := range migrate.ProductionBaselineVersions() {
		assertMigrationRecorded(t, pool, version, true)
	}
}

func assertCurrentSchemaLedger(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	assertMigrationRecorded(t, pool, migrate.CurrentSchemaMigrationID, true)
	for _, version := range migrate.ProductionBaselineVersions() {
		assertMigrationRecorded(t, pool, version, false)
	}
}

func runMigrateCommand(t *testing.T, dsn, target string, wantError bool) string {
	t.Helper()
	stdout, stderr := runMigrate(t, migrateInvocation{dsn: dsn, target: target, wantError: wantError})
	return stdout + stderr
}

// migrateInvocation is one cmd/migrate run. It exists so the exact-target and
// fail-closed tests can drive the real command with the real environment the
// deploy helper would use, rather than reaching into the package internals.
type migrateInvocation struct {
	dsn       string
	target    string
	extraEnv  []string
	args      []string
	wantError bool
}

// runMigrate returns stdout and stderr separately. With --report-json the
// contract is that stdout holds exactly one JSON object and every human line
// moves to stderr, so a combined stream would not be parsable.
func runMigrate(t *testing.T, invocation migrateInvocation) (string, string) {
	t.Helper()
	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	args := append([]string{"run", "./cmd/migrate"}, invocation.args...)
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = repositoryRoot
	cmd.Env = append(migrationCommandEnvironment(invocation.dsn, invocation.target), invocation.extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if invocation.wantError && err == nil {
		t.Fatalf("cmd/migrate target %q unexpectedly succeeded\nstdout=%s\nstderr=%s",
			invocation.target, stdout.String(), stderr.String())
	}
	if !invocation.wantError && err != nil {
		t.Fatalf("cmd/migrate target %q failed: %v\nstdout=%s\nstderr=%s",
			invocation.target, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// migrationReport mirrors the JSON contract cmd/migrate emits for the deploy
// helper. Decoding it here is the mechanical check that the contract survives:
// a renamed field shows up as a zero value the assertions reject.
type migrationReport struct {
	SchemaVersion int      `json:"schema_version"`
	Tool          string   `json:"tool"`
	OK            bool     `json:"ok"`
	Mode          string   `json:"mode"`
	Target        string   `json:"target"`
	AllowManual   bool     `json:"allow_manual"`
	StartVersion  string   `json:"start_version"`
	EndVersion    string   `json:"end_version"`
	Applied       []string `json:"applied"`

	AlreadyAtTarget bool `json:"already_at_target"`
	StoppedAtManual bool `json:"-"`

	OnlineUpdate *struct {
		Target   string   `json:"target"`
		Pending  []string `json:"pending"`
		Allowed  bool     `json:"allowed"`
		Blockers []struct {
			StepID string `json:"step_id"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"blockers"`
	} `json:"online_update"`

	Ledgers *struct {
		OK     bool `json:"ok"`
		Schema struct {
			Present  bool     `json:"present"`
			Target   string   `json:"target"`
			Head     string   `json:"head"`
			Applied  []string `json:"applied"`
			Missing  []string `json:"missing"`
			Extra    []string `json:"extra"`
			AtTarget bool     `json:"at_target"`
		} `json:"schema"`
		River struct {
			Present  bool   `json:"present"`
			Line     string `json:"line"`
			Target   int    `json:"target"`
			Head     int    `json:"head"`
			Applied  []int  `json:"applied"`
			Missing  []int  `json:"missing"`
			Extra    []int  `json:"extra"`
			AtTarget bool   `json:"at_target"`
		} `json:"river"`
		Problems []struct {
			Ledger string `json:"ledger"`
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
		} `json:"problems"`
	} `json:"ledgers"`

	Error *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeMigrationReport(t *testing.T, stdout string) migrationReport {
	t.Helper()
	var report migrationReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("cmd/migrate --report-json stdout is not a single JSON object: %v\n%s", err, stdout)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("report schema_version = %d, want 1", report.SchemaVersion)
	}
	if report.Tool != "cairn-migrate" {
		t.Fatalf("report tool = %q, want cairn-migrate", report.Tool)
	}
	return report
}

func migrationTargetPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open pool on isolated migration database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func schemaLedgerVersions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT version FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	versions := make([]string, 0, 16)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	return versions
}

func migrationCommandEnvironment(dsn, target string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DATABASE_URL=") || strings.HasPrefix(entry, "MIGRATION_TARGET=") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "DATABASE_URL="+dsn)
	if target != "" {
		environment = append(environment, "MIGRATION_TARGET="+target)
	}
	return environment
}
