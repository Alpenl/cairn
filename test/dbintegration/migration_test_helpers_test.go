package dbintegration

import (
	"context"
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
)

type migrationRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func assertMigrationRecorded(t *testing.T, db migrationRowQuerier, id string, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE version = $1
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

func runMigrateCommand(t *testing.T, dsn, target string, wantError bool) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "run", "./cmd/migrate")
	cmd.Dir = repositoryRoot
	cmd.Env = migrationCommandEnvironment(dsn, target)
	output, err := cmd.CombinedOutput()
	if wantError && err == nil {
		t.Fatalf("cmd/migrate target %q unexpectedly succeeded; output=%s", target, output)
	}
	if !wantError && err != nil {
		t.Fatalf("cmd/migrate target %q failed: %v\n%s", target, err, output)
	}
	return string(output)
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
