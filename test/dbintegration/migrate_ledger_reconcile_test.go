package dbintegration

import (
	"slices"
	"testing"

	"webtag/internal/migrate"
)

// TestReconcileLedgersAgainstManifestTargets exercises the check the root-owned
// updater calls after it migrates: Cairn keeps two independent ledgers —
// schema_migrations for this repository's steps and river_migration for River's
// own schema — and a Core update is only complete when BOTH sit exactly on the
// position the signed release manifest declares.
func TestReconcileLedgersAgainstManifestTargets(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)

	riverTarget := migrate.RiverBundleTarget()
	if riverTarget == 0 {
		t.Fatal("River bundle head is 0; a manifest could not declare a river_ledger_target")
	}
	// Derive the head instead of naming it: this test asserts "a completed
	// migration reaches the declared target", and the target it means is
	// whatever the shipped plan ends with — pinning one ID makes the test fail
	// on the next migration for a reason that has nothing to do with ledgers.
	schemaTarget := shippedSchemaHead(t)

	t.Run("before any migration both ledgers are absent", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: schemaTarget, RiverLedgerTarget: riverTarget,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK {
			t.Fatal("an unmigrated database reconciled as ok")
		}
		if result.Schema.Present || result.River.Present {
			t.Fatalf("ledgers reported present on an empty database: %+v", result)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemMissingTable))
		assertHasProblem(t, result, "river_migration", string(migrate.LedgerProblemMissingTable))
	})

	runMigrate(t, migrateInvocation{dsn: dsn})

	t.Run("a completed migration reaches both declared targets", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: schemaTarget, RiverLedgerTarget: riverTarget,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if !result.OK {
			t.Fatalf("reconciliation failed after a complete migration: %+v", result.Problems)
		}
		if !result.Schema.AtTarget || result.Schema.Head != schemaTarget {
			t.Fatalf("schema ledger = %+v, want head %s at target", result.Schema, schemaTarget)
		}
		if !result.River.AtTarget || result.River.Head != riverTarget {
			t.Fatalf("river ledger = %+v, want head %d at target", result.River, riverTarget)
		}
		if result.River.Line != migrate.RiverLedgerLine {
			t.Fatalf("river line = %q, want %q", result.River.Line, migrate.RiverLedgerLine)
		}
		if !slices.Contains(result.Schema.Applied, migrate.TranslationSourceContractMigrationID) {
			t.Fatalf("schema applied list = %v, want it to include the fresh-install step", result.Schema.Applied)
		}
	})

	t.Run("a manifest declaring an earlier schema target detects the overshoot", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: exactTargetStep, RiverLedgerTarget: riverTarget,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK {
			t.Fatal("a database past the declared schema target reconciled as ok")
		}
		if result.Schema.AtTarget || len(result.Schema.Extra) == 0 {
			t.Fatalf("schema ledger = %+v, want extra steps past %s", result.Schema, exactTargetStep)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemAhead))
	})

	t.Run("a manifest declaring an earlier river target detects the overshoot", func(t *testing.T) {
		result, err := migrate.ReconcileLedgers(t.Context(), pool, migrate.LedgerTargets{
			SchemaTarget: schemaTarget, RiverLedgerTarget: riverTarget - 1,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK {
			t.Fatal("a database past the declared River target reconciled as ok")
		}
		if !slices.Contains(result.River.Extra, riverTarget) {
			t.Fatalf("river ledger = %+v, want %d reported as extra", result.River, riverTarget)
		}
		assertHasProblem(t, result, "river_migration", string(migrate.LedgerProblemAhead))
	})

	t.Run("targets this binary cannot produce are refused rather than guessed", func(t *testing.T) {
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

	t.Run("a schema ledger short of its target reports what is missing", func(t *testing.T) {
		shortDSN := isolatedMigrationDatabase(t)
		shortPool := migrationTargetPool(t, shortDSN)
		runMigrate(t, migrateInvocation{dsn: shortDSN, target: exactTargetStep})

		result, err := migrate.ReconcileLedgers(t.Context(), shortPool, migrate.LedgerTargets{
			SchemaTarget: schemaTarget, RiverLedgerTarget: riverTarget,
		})
		if err != nil {
			t.Fatalf("ReconcileLedgers() error = %v", err)
		}
		if result.OK {
			t.Fatal("a half-migrated database reconciled as ok")
		}
		if !slices.Contains(result.Schema.Missing, schemaTarget) {
			t.Fatalf("schema ledger = %+v, want %s reported missing", result.Schema, schemaTarget)
		}
		// River always runs its whole bundle, so it is the schema ledger alone
		// that is behind — which is exactly the asymmetry the two-ledger check
		// exists to surface.
		if !result.River.AtTarget {
			t.Fatalf("river ledger = %+v, want it at target even though the schema ledger is behind", result.River)
		}
		assertHasProblem(t, result, "schema_migrations", string(migrate.LedgerProblemBehind))
	})
}

// TestMigrateReportEmbedsTheLedgerReconciliation proves the helper can read the
// same verdict out of the command's JSON rather than importing Go code.
func TestMigrateReportEmbedsTheLedgerReconciliation(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)

	stdout, _ := runMigrate(t, migrateInvocation{dsn: dsn, args: []string{"--report-json"}})
	report := decodeMigrationReport(t, stdout)
	if report.Ledgers == nil {
		t.Fatal("report carries no ledgers object")
	}
	if !report.Ledgers.OK || !report.Ledgers.Schema.AtTarget || !report.Ledgers.River.AtTarget {
		t.Fatalf("ledgers = %+v, want both at target", report.Ledgers)
	}
	if report.Ledgers.River.Target != migrate.RiverBundleTarget() {
		t.Fatalf("river target = %d, want the bundle head %d", report.Ledgers.River.Target, migrate.RiverBundleTarget())
	}
	if head := shippedSchemaHead(t); report.Ledgers.Schema.Head != head {
		t.Fatalf("schema head = %q, want %q", report.Ledgers.Schema.Head, head)
	}
}

// shippedSchemaHead is the last step of the shipped migration plan — the
// version a fully migrated database records in schema_migrations.
func shippedSchemaHead(t *testing.T) string {
	t.Helper()
	plan := migrate.Steps()
	if len(plan) == 0 {
		t.Fatal("shipped migration plan is empty")
	}
	return plan[len(plan)-1].ID
}

func assertHasProblem(t *testing.T, result migrate.LedgerReconciliation, ledger, kind string) {
	t.Helper()
	for _, problem := range result.Problems {
		if problem.Ledger == ledger && string(problem.Kind) == kind {
			if problem.Detail == "" {
				t.Fatalf("problem %s/%s has no operator-facing detail", ledger, kind)
			}
			return
		}
	}
	t.Fatalf("problems = %+v, want one with ledger=%s kind=%s", result.Problems, ledger, kind)
}
