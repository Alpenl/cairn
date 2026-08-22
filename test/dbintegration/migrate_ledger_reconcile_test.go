package dbintegration

import (
	"slices"
	"testing"

	"webtag/internal/migrate"
)

func TestReconcileLedgersAgainstManifestTargets(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)
	riverTarget := migrate.RiverBundleTarget()
	targets := migrate.LedgerTargets{
		SchemaTarget: migrate.CurrentSchemaMigrationID, RiverLedgerTarget: riverTarget,
	}

	t.Run("empty database", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, targets)
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK || result.Schema.Present || result.River.Present {
			t.Fatalf("empty database reconciliation = %+v, want both ledgers absent", result)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemMissingTable))
		assertHasProblem(t, result, "river_migration", string(migrate.LedgerProblemMissingTable))
	})

	runMigrate(t, migrateInvocation{dsn: dsn})

	t.Run("current schema and River heads", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, targets)
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if !result.OK || !result.Schema.AtTarget || !result.River.AtTarget {
			t.Fatalf("completed migration reconciliation = %+v", result)
		}
		if result.Schema.Head != migrate.CurrentSchemaMigrationID ||
			!slices.Equal(result.Schema.Applied, []string{migrate.CurrentSchemaMigrationID}) {
			t.Fatalf("schema ledger = %+v, want one current head", result.Schema)
		}
		if result.River.Head != riverTarget || result.River.Line != migrate.RiverLedgerLine {
			t.Fatalf("River ledger = %+v, want %s:%d", result.River, migrate.RiverLedgerLine, riverTarget)
		}
	})

	t.Run("v0.1.17 production ledger is behind", func(t *testing.T) {
		if _, err := pool.Exec(t.Context(), `DELETE FROM public.schema_migrations`); err != nil {
			t.Fatalf("clear current ledger: %v", err)
		}
		for _, version := range migrate.ProductionBaselineVersions() {
			if _, err := pool.Exec(t.Context(),
				`INSERT INTO public.schema_migrations(version) VALUES ($1)`, version); err != nil {
				t.Fatalf("seed production ledger %s: %v", version, err)
			}
		}
		result, err := migrate.ReconcileLedgers(t.Context(), pool, targets)
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK || result.Schema.Head != migrate.ProductionBaselineMigrationID ||
			!slices.Equal(result.Schema.Missing, []string{migrate.CurrentSchemaMigrationID}) {
			t.Fatalf("production baseline reconciliation = %+v, want one missing current head", result.Schema)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemBehind))

		if _, err := pool.Exec(t.Context(), `DELETE FROM public.schema_migrations`); err != nil {
			t.Fatalf("clear production ledger: %v", err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO public.schema_migrations(version) VALUES ($1)`,
			migrate.CurrentSchemaMigrationID); err != nil {
			t.Fatalf("restore current ledger: %v", err)
		}
	})

	t.Run("River target behind bundle", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: migrate.CurrentSchemaMigrationID, RiverLedgerTarget: riverTarget - 1,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK || !slices.Contains(result.River.Extra, riverTarget) {
			t.Fatalf("River overshoot reconciliation = %+v", result.River)
		}
		assertHasProblem(t, result, "river_migration", string(migrate.LedgerProblemAhead))
	})

	t.Run("unknown manifest targets", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: "manifest-from-a-newer-release", RiverLedgerTarget: 9999,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK {
			t.Fatal("unknown manifest targets reconciled as ok")
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemUnknownTarget))
		assertHasProblem(t, result, "river_migration", string(migrate.LedgerProblemUnknownTarget))
	})

	t.Run("unknown schema ledger version", func(t *testing.T) {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO public.schema_migrations(version) VALUES ('future2099010101')`); err != nil {
			t.Fatalf("seed future schema ledger: %v", err)
		}
		defer func() {
			_, _ = pool.Exec(t.Context(),
				`DELETE FROM public.schema_migrations WHERE version='future2099010101'`)
		}()
		result, err := migrate.ReconcileLedgers(t.Context(), pool, targets)
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK || !slices.Contains(result.Schema.Extra, "future2099010101") {
			t.Fatalf("future schema ledger reconciliation = %+v", result.Schema)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemAhead))
	})
}

func TestMigrateReportEmbedsLedgerReconciliation(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	stdout, _ := runMigrate(t, migrateInvocation{dsn: dsn, args: []string{"--report-json"}})
	report := decodeMigrationReport(t, stdout)
	if report.Ledgers == nil || !report.Ledgers.OK ||
		!report.Ledgers.Schema.AtTarget || !report.Ledgers.River.AtTarget {
		t.Fatalf("report ledgers = %+v, want both at target", report.Ledgers)
	}
	if report.Ledgers.Schema.Head != migrate.CurrentSchemaMigrationID ||
		report.Ledgers.River.Target != migrate.RiverBundleTarget() {
		t.Fatalf("report ledgers = %+v, want current schema and bundled River heads", report.Ledgers)
	}
}

func assertHasProblem(t *testing.T, result migrate.LedgerReconciliation, ledger, kind string) {
	t.Helper()
	for _, problem := range result.Problems {
		if problem.Ledger == ledger && string(problem.Kind) == kind {
			if problem.Detail == "" {
				t.Fatalf("problem %s/%s has no detail", ledger, kind)
			}
			return
		}
	}
	t.Fatalf("problems = %+v, want ledger=%s kind=%s", result.Problems, ledger, kind)
}
