package dbintegration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

func TestFreshMigrationTargetCreatesCurrentSchemaContract(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	runMigrateCommand(t, dsn, migrate.FreshInstallTarget, false)

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open contract verification pool: %v", err)
	}
	assertCurrentSchemaLedger(t, pool)
	assertIndexExists(t, pool, "idx_link_translations_saved_revision_unique", true)
	assertIndexExists(t, pool, "idx_link_translations_summary_source_unique", true)
	assertIndexExists(t, pool, "idx_link_translations_legacy_source_unique", false)
	pool.Close()
}
