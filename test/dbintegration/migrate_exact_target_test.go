package dbintegration

import (
	"slices"
	"strings"
	"testing"

	"webtag/internal/migrate"
)

// exactTargetStep is the step the exact-target tests migrate to. It is the
// River terminal-history index — early enough that several steps remain
// unapplied afterwards, which is what makes "not one step further" observable.
const exactTargetStep = "b671c9d2e411"

// TestMigrateStopsAtTheExactTarget is the property issue #41's helper depends
// on at state-machine step 10: given a manifest's schema target, the migrate
// binary lands on exactly that step, applies the ones before it and none after.
func TestMigrateStopsAtTheExactTarget(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)

	stdout, stderr := runMigrate(t, migrateInvocation{
		dsn:    dsn,
		target: exactTargetStep,
		args:   []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)

	if !report.OK || report.Error != nil {
		t.Fatalf("exact-target run reported not ok: %+v", report.Error)
	}
	if report.Mode != "target" || report.Target != exactTargetStep {
		t.Fatalf("mode/target = %q/%q, want target/%s", report.Mode, report.Target, exactTargetStep)
	}
	if report.StartVersion != "" {
		t.Fatalf("start_version = %q, want empty on a database that had never been migrated", report.StartVersion)
	}
	if report.EndVersion != exactTargetStep {
		t.Fatalf("end_version = %q, want %q", report.EndVersion, exactTargetStep)
	}
	want := []string{migrate.TranslationSourceContractMigrationID, exactTargetStep}
	if !slices.Equal(report.Applied, want) {
		t.Fatalf("applied = %v, want %v", report.Applied, want)
	}

	// The ledger itself, not just the report, must stop there.
	recorded := schemaLedgerVersions(t, pool)
	slices.Sort(want)
	if !slices.Equal(recorded, want) {
		t.Fatalf("schema_migrations = %v, want exactly %v", recorded, want)
	}

	// The human stream still says what happened; the silent-success failure
	// mode is what let a deploy rebuild containers over unapplied migrations.
	for _, fragment := range []string{"start=(none)", "end=" + exactTargetStep, "applied 2 step(s)"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("human output missing %q:\n%s", fragment, stderr)
		}
	}
}

// TestMigrateExactTargetIsIdempotent proves a helper may safely retry the same
// job: re-running the same target is a success that applies nothing.
func TestMigrateExactTargetIsIdempotent(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)

	runMigrate(t, migrateInvocation{dsn: dsn, target: exactTargetStep})
	stdout, _ := runMigrate(t, migrateInvocation{
		dsn:    dsn,
		target: exactTargetStep,
		args:   []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)
	if !report.OK {
		t.Fatalf("repeat run reported not ok: %+v", report.Error)
	}
	if !report.AlreadyAtTarget || len(report.Applied) != 0 {
		t.Fatalf("repeat run applied %v (already_at_target=%t), want a no-op", report.Applied, report.AlreadyAtTarget)
	}
}

// TestMigrateExactTargetCarriesTheOnlineUpdateVerdict checks the field the
// deploy UI renders: the range from an empty ledger through the fresh-install
// step is reviewed incompatible, and the report names the step that refuses.
func TestMigrateExactTargetCarriesTheOnlineUpdateVerdict(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)

	stdout, _ := runMigrate(t, migrateInvocation{
		dsn:    dsn,
		target: exactTargetStep,
		args:   []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)
	if report.OnlineUpdate == nil {
		t.Fatal("report carries no online_update verdict for an exact-target run")
	}
	if report.OnlineUpdate.Allowed {
		t.Fatal("online_update.allowed = true across the fresh-install step")
	}
	found := false
	for _, blocker := range report.OnlineUpdate.Blockers {
		if blocker.StepID == migrate.TranslationSourceContractMigrationID {
			found = true
			if blocker.Reason != "incompatible" || strings.TrimSpace(blocker.Detail) == "" {
				t.Fatalf("blocker = %+v, want an incompatible reason with an operator-facing detail", blocker)
			}
		}
	}
	if !found {
		t.Fatalf("blockers = %+v, want one naming %s", report.OnlineUpdate.Blockers, migrate.TranslationSourceContractMigrationID)
	}
}

// TestMigrateExactTargetFailsClosed covers every refusal the helper must be
// able to distinguish, each against a real database, and each proving the
// ledger was left untouched.
func TestMigrateExactTargetFailsClosed(t *testing.T) {
	t.Run("unknown target", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		stdout, stderr := runMigrate(t, migrateInvocation{
			dsn:       dsn,
			target:    "definitely-not-a-step",
			args:      []string{"--report-json"},
			wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "unknown_target")
		if !strings.Contains(report.Error.Message, migrate.ReaderTodoProjectionLedgerMigrationID) {
			t.Errorf("unknown-target message does not list the real targets: %q", report.Error.Message)
		}
		if strings.Contains(stderr, "applied 1 step") {
			t.Errorf("unknown target applied something:\n%s", stderr)
		}
	})

	t.Run("target behind the applied ledger", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		pool := migrationTargetPool(t, dsn)
		runMigrate(t, migrateInvocation{dsn: dsn})
		before := schemaLedgerVersions(t, pool)

		stdout, _ := runMigrate(t, migrateInvocation{
			dsn:       dsn,
			target:    exactTargetStep,
			args:      []string{"--report-json"},
			wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "target_behind_ledger")
		if !strings.Contains(report.Error.Message, "restoring the bound dump") {
			t.Errorf("rollback refusal is not actionable: %q", report.Error.Message)
		}
		if after := schemaLedgerVersions(t, pool); !slices.Equal(before, after) {
			t.Fatalf("refused rollback changed the ledger: %v -> %v", before, after)
		}
	})

	t.Run("ledger written by a newer binary", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		pool := migrationTargetPool(t, dsn)
		runMigrate(t, migrateInvocation{dsn: dsn, target: exactTargetStep})
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO schema_migrations(version) VALUES ('future2099010101')`); err != nil {
			t.Fatalf("seed a future ledger entry: %v", err)
		}
		before := schemaLedgerVersions(t, pool)

		stdout, _ := runMigrate(t, migrateInvocation{
			dsn:       dsn,
			target:    migrate.ReaderTodoProjectionLedgerMigrationID,
			args:      []string{"--report-json"},
			wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "ledger_ahead")
		if !strings.Contains(report.Error.Message, "future2099010101") {
			t.Errorf("ledger-ahead refusal does not name the version: %q", report.Error.Message)
		}
		if after := schemaLedgerVersions(t, pool); !slices.Equal(before, after) {
			t.Fatalf("refused run changed the ledger: %v -> %v", before, after)
		}
	})

	t.Run("declared river ledger target unreachable", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		stdout, _ := runMigrate(t, migrateInvocation{
			dsn:       dsn,
			target:    migrate.ReaderTodoProjectionLedgerMigrationID,
			args:      []string{"--report-json"},
			extraEnv:  []string{"MIGRATION_RIVER_LEDGER_TARGET=999"},
			wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "ledger_mismatch")
		if report.Ledgers == nil || report.Ledgers.OK {
			t.Fatalf("ledgers = %+v, want a reconciliation that failed", report.Ledgers)
		}
	})
}

func assertReportError(t *testing.T, report migrationReport, wantKind string) {
	t.Helper()
	if report.OK {
		t.Fatal("failed run reported ok=true")
	}
	if report.Error == nil {
		t.Fatal("failed run carries no structured error")
	}
	if report.Error.Kind != wantKind {
		t.Fatalf("error.kind = %q, want %q (message: %s)", report.Error.Kind, wantKind, report.Error.Message)
	}
}
