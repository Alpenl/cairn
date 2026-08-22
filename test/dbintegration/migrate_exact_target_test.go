package dbintegration

import (
	"slices"
	"strings"
	"testing"

	"webtag/internal/migrate"
)

func TestMigrateCurrentTargetReportsAndRecordsOneHead(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)

	stdout, stderr := runMigrate(t, migrateInvocation{
		dsn: dsn, target: migrate.CurrentSchemaMigrationID, args: []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)
	if !report.OK || report.Error != nil {
		t.Fatalf("exact-target run reported not ok: %+v", report.Error)
	}
	if report.Mode != "target" || report.Target != migrate.CurrentSchemaMigrationID ||
		report.StartVersion != "" || report.EndVersion != migrate.CurrentSchemaMigrationID {
		t.Fatalf("migration report = %+v, want empty -> current exact target", report)
	}
	if !slices.Equal(report.Applied, []string{migrate.CurrentSchemaMigrationID}) {
		t.Fatalf("applied = %v, want one current head", report.Applied)
	}
	if got := schemaLedgerVersions(t, pool); !slices.Equal(got, []string{migrate.CurrentSchemaMigrationID}) {
		t.Fatalf("schema_migrations = %v, want only current head", got)
	}
	for _, fragment := range []string{
		"start=(none)",
		"end=" + migrate.CurrentSchemaMigrationID,
		"applied 1 step(s): " + migrate.CurrentSchemaMigrationID,
	} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("human output missing %q:\n%s", fragment, stderr)
		}
	}
}

func TestMigrateCurrentTargetIsIdempotent(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	runMigrate(t, migrateInvocation{dsn: dsn, target: migrate.CurrentSchemaMigrationID})
	stdout, _ := runMigrate(t, migrateInvocation{
		dsn: dsn, target: migrate.CurrentSchemaMigrationID, args: []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)
	if !report.OK || !report.AlreadyAtTarget || len(report.Applied) != 0 {
		t.Fatalf("repeat run = %+v, want a successful no-op", report)
	}
}

func TestMigrateCurrentTargetCarriesOnlineUpdateRefusal(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	stdout, _ := runMigrate(t, migrateInvocation{
		dsn: dsn, target: migrate.CurrentSchemaMigrationID, args: []string{"--report-json"},
	})
	report := decodeMigrationReport(t, stdout)
	if report.OnlineUpdate == nil || report.OnlineUpdate.Allowed {
		t.Fatalf("online update verdict = %+v, want refused", report.OnlineUpdate)
	}
	if !slices.Equal(report.OnlineUpdate.Pending, []string{migrate.CurrentSchemaMigrationID}) ||
		len(report.OnlineUpdate.Blockers) != 1 {
		t.Fatalf("online update verdict = %+v, want one current-head blocker", report.OnlineUpdate)
	}
	blocker := report.OnlineUpdate.Blockers[0]
	if blocker.StepID != migrate.CurrentSchemaMigrationID || blocker.Reason != "incompatible" ||
		strings.TrimSpace(blocker.Detail) == "" {
		t.Fatalf("online update blocker = %+v, want explained current-head incompatibility", blocker)
	}
}

func TestMigrateCurrentTargetFailsClosed(t *testing.T) {
	t.Run("unknown target", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		stdout, stderr := runMigrate(t, migrateInvocation{
			dsn: dsn, target: "definitely-not-a-step", args: []string{"--report-json"}, wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "unknown_target")
		if !strings.Contains(report.Error.Message, migrate.CurrentSchemaMigrationID) {
			t.Errorf("unknown-target message does not list the current target: %q", report.Error.Message)
		}
		if strings.Contains(stderr, "applied 1 step") {
			t.Errorf("unknown target applied something:\n%s", stderr)
		}
	})

	t.Run("partial production ledger", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		pool := migrationTargetPool(t, dsn)
		if _, err := pool.Exec(t.Context(), `CREATE TABLE public.schema_migrations (
			version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			t.Fatalf("create partial production ledger: %v", err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO public.schema_migrations(version) VALUES ($1)`,
			migrate.ProductionBaselineVersions()[0]); err != nil {
			t.Fatalf("seed partial production ledger: %v", err)
		}
		before := schemaLedgerVersions(t, pool)
		stdout, _ := runMigrate(t, migrateInvocation{
			dsn: dsn, target: migrate.CurrentSchemaMigrationID, args: []string{"--report-json"}, wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "ledger_ahead")
		if after := schemaLedgerVersions(t, pool); !slices.Equal(before, after) {
			t.Fatalf("refused partial ledger changed: %v -> %v", before, after)
		}
	})

	t.Run("ledger written by a newer binary", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		pool := migrationTargetPool(t, dsn)
		runMigrate(t, migrateInvocation{dsn: dsn, target: migrate.CurrentSchemaMigrationID})
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO public.schema_migrations(version) VALUES ('future2099010101')`); err != nil {
			t.Fatalf("seed future ledger entry: %v", err)
		}
		before := schemaLedgerVersions(t, pool)
		stdout, _ := runMigrate(t, migrateInvocation{
			dsn: dsn, target: migrate.CurrentSchemaMigrationID, args: []string{"--report-json"}, wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "ledger_ahead")
		if !strings.Contains(report.Error.Message, "future2099010101") {
			t.Errorf("ledger-ahead refusal does not name the version: %q", report.Error.Message)
		}
		if after := schemaLedgerVersions(t, pool); !slices.Equal(before, after) {
			t.Fatalf("refused future ledger changed: %v -> %v", before, after)
		}
	})

	t.Run("fresh target on a migrated database", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		runMigrate(t, migrateInvocation{dsn: dsn, target: migrate.CurrentSchemaMigrationID})
		stdout, _ := runMigrate(t, migrateInvocation{
			dsn: dsn, target: migrate.FreshInstallTarget, args: []string{"--report-json"}, wantError: true,
		})
		assertReportError(t, decodeMigrationReport(t, stdout), "ledger_ahead")
	})

	t.Run("declared river target unreachable", func(t *testing.T) {
		dsn := isolatedMigrationDatabase(t)
		stdout, _ := runMigrate(t, migrateInvocation{
			dsn:       dsn,
			target:    migrate.CurrentSchemaMigrationID,
			args:      []string{"--report-json"},
			extraEnv:  []string{"MIGRATION_RIVER_LEDGER_TARGET=999"},
			wantError: true,
		})
		report := decodeMigrationReport(t, stdout)
		assertReportError(t, report, "ledger_mismatch")
		if report.Ledgers == nil || report.Ledgers.OK {
			t.Fatalf("ledgers = %+v, want failed reconciliation", report.Ledgers)
		}
	})
}

func assertReportError(t *testing.T, report migrationReport, wantKind string) {
	t.Helper()
	if report.OK || report.Error == nil {
		t.Fatalf("failed run report = %+v, want structured error", report)
	}
	if report.Error.Kind != wantKind {
		t.Fatalf("error.kind = %q, want %q (message: %s)", report.Error.Kind, wantKind, report.Error.Message)
	}
}
