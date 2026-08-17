package dbintegration

import (
	"testing"
)

func TestTruncateAllTablesClearsBusinessRowsAndPreservesSingletons(t *testing.T) {
	pool := StartPostgres(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO public.reader_notes (title) VALUES ('cleanup-regression')`); err != nil {
		t.Fatalf("seed business row: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE public.library_read_revision SET revision=7 WHERE singleton;
		UPDATE public.global_read_revision SET revision=8 WHERE singleton;
		UPDATE public.feed_read_revision SET revision=9 WHERE singleton;
	`); err != nil {
		t.Fatalf("seed mutable singleton revisions: %v", err)
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
		installationRows            int
		libraryRows                 int
		globalRows                  int
		feedRows                    int
		migrationRows               int
		libraryRevision             int64
		globalRevision              int64
		feedRevision                int64
		storedNamespace             string
		integrityApplied            bool
		historicalApplied           bool
		conceptAuditApplied         bool
		lifecycleApplied            bool
		searchIndexApplied          bool
		todoProjectionLedgerApplied bool
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*) FROM public.installation_state),
			(SELECT count(*) FROM public.library_read_revision),
			(SELECT count(*) FROM public.global_read_revision),
			(SELECT count(*) FROM public.feed_read_revision),
			(SELECT count(*) FROM public.schema_migrations),
			(SELECT revision FROM public.library_read_revision WHERE singleton),
			(SELECT revision FROM public.global_read_revision WHERE singleton),
			(SELECT revision FROM public.feed_read_revision WHERE singleton),
			(SELECT representation_namespace::text FROM public.installation_state WHERE singleton),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'integrity2026081401'),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'historical2026081401'),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'conceptaudit2026081401'),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'lifecycle2026081401'),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'readersearch2026081701'),
			EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 'readertodoprojection2026081701')
	`).Scan(
		&installationRows, &libraryRows, &globalRows, &feedRows, &migrationRows,
		&libraryRevision, &globalRevision, &feedRevision, &storedNamespace,
		&integrityApplied, &historicalApplied, &conceptAuditApplied, &lifecycleApplied,
		&searchIndexApplied, &todoProjectionLedgerApplied,
	); err != nil {
		t.Fatalf("read singleton state after cleanup: %v", err)
	}
	if installationRows != 1 || libraryRows != 1 || globalRows != 1 || feedRows != 1 {
		t.Fatalf("singleton row counts = installation:%d library:%d global:%d feed:%d, want one each",
			installationRows, libraryRows, globalRows, feedRows)
	}
	if migrationRows != 9 {
		t.Fatalf("schema migration rows after cleanup = %d, want 9", migrationRows)
	}
	if !todoProjectionLedgerApplied {
		t.Fatal("reader TODO projection ledger migration was not recorded after cleanup")
	}
	if !integrityApplied {
		t.Fatal("integrity migration was not recorded after cleanup")
	}
	if !historicalApplied {
		t.Fatal("historical repair migration was not recorded after cleanup")
	}
	if !searchIndexApplied {
		t.Fatal("thought search trigram index migration was not recorded after cleanup")
	}
	if !conceptAuditApplied {
		t.Fatal("concept audit repair migration was not recorded after cleanup")
	}
	if !lifecycleApplied {
		t.Fatal("lifecycle repair migration was not recorded after cleanup")
	}
	if libraryRevision != 0 || globalRevision != 0 || feedRevision != 0 {
		t.Fatalf("singleton revisions after cleanup = library:%d global:%d feed:%d, want all zero",
			libraryRevision, globalRevision, feedRevision)
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
