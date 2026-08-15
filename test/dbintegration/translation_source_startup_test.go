package dbintegration

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app"
	"webtag/internal/config"
	"webtag/internal/migrate"
)

func TestFreshMigrationTargetAndRuntimeContractGuard(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	runMigrateCommand(t, dsn, migrate.FreshInstallTarget, false)

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open contract verification pool: %v", err)
	}
	assertMigrationRecorded(t, pool, migrate.TranslationSourceContractMigrationID, true)
	assertIndexExists(t, pool, "idx_link_translations_saved_revision_unique", true)
	assertIndexExists(t, pool, "idx_link_translations_legacy_source_unique", true)
	pool.Close()

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("AI_BASE_URL", "https://example.com/v1")
	t.Setenv("AI_API_KEY", "rf5a-startup-test")
	t.Setenv("AI_MODEL", "rf5a-startup-test")
	t.Setenv("APP_ENV", "test")
	t.Setenv("READER_CURSOR_SIGNING_KEY", "rf5a-startup-test-reader-cursor-key")
	t.Setenv("TRANSLATION_SOURCE_ROLLOUT", string(config.TranslationSourceRolloutCompat))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	runtime, err := app.BuildRuntime(t.Context(), cfg)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("BuildRuntime() returned a runtime after the contract/compat preflight: %+v", runtime)
	}
	if err == nil {
		t.Fatal("BuildRuntime() error = nil, want contract/compat startup rejection")
	}
	for _, want := range []string{
		"TRANSLATION_SOURCE_ROLLOUT=compat",
		migrate.TranslationSourceContractMigrationID,
		"TRANSLATION_SOURCE_ROLLOUT=strict",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("BuildRuntime() error = %q, want %q", err, want)
		}
	}
}
