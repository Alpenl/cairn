package dbintegration

import (
	"testing"

	"webtag/internal/migrate"
)

func TestTruncateAllTablesClearsBusinessRowsAndPreservesSingletons(t *testing.T) {
	pool := StartPostgres(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_notes (title) VALUES ('cleanup-regression')`); err != nil {
		t.Fatalf("seed business row: %v", err)
	}
	var namespace string
	if err := pool.QueryRow(t.Context(), `SELECT representation_namespace::text FROM public.installation_state WHERE singleton`).Scan(&namespace); err != nil {
		t.Fatalf("read installation namespace before cleanup: %v", err)
	}

	if err := truncateAllTables(t.Context(), pool); err != nil {
		t.Fatalf("truncateAllTables() error = %v", err)
	}

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.reader_notes`).Scan(&count); err != nil {
		t.Fatalf("count business rows after cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("reader note rows after cleanup = %d, want 0", count)
	}

	var (
		installationRows int
		migrationRows    int
		storedNamespace  string
		currentApplied   bool
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM public.installation_state),
			(SELECT count(*) FROM public.schema_migrations),
			(SELECT representation_namespace::text FROM public.installation_state WHERE singleton),
		EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = $1)
	`, migrate.CurrentSchemaMigrationID).Scan(
		&installationRows, &migrationRows, &storedNamespace, &currentApplied,
	); err != nil {
		t.Fatalf("read singleton state after cleanup: %v", err)
	}
	if installationRows != 1 {
		t.Fatalf("installation singleton row count = %d, want one", installationRows)
	}
	if migrationRows != 1 || !currentApplied {
		t.Fatalf("schema migration ledger after cleanup = rows:%d current:%v, want 1/true",
			migrationRows, currentApplied)
	}
	if storedNamespace != namespace {
		t.Fatalf("installation namespace after cleanup = %q, want preserved %q", storedNamespace, namespace)
	}

	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM public.feed_subscriptions`).Scan(&count); err != nil {
		t.Fatalf("count feed subscriptions after cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("feed subscriptions after cleanup = %d, want 0", count)
	}
}
