package migrate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRunExactTargetInstallsCurrentSchema(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	result, err := Run(context.Background(), db, RunRequest{Target: CurrentSchemaMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Mode != "target" || result.Target != CurrentSchemaMigrationID ||
		result.StartVersion != "" || result.EndVersion != CurrentSchemaMigrationID || result.AlreadyAtTarget {
		t.Fatalf("result = %+v, want a fresh exact-target install", result)
	}
	if !slices.Equal(result.Applied, []string{CurrentSchemaMigrationID}) ||
		!slices.Equal(db.inserts, []string{CurrentSchemaMigrationID}) {
		t.Fatalf("applied/recorded = %v/%v, want current head", result.Applied, db.inserts)
	}
}

func TestRunProductionBaselineUsesAggregateUpgrade(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: ProductionBaselineVersions()}}
	result, err := Run(context.Background(), db, RunRequest{Target: CurrentSchemaMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.StartVersion != ProductionBaselineMigrationID || result.EndVersion != CurrentSchemaMigrationID {
		t.Fatalf("start/end = %q/%q, want production baseline/current", result.StartVersion, result.EndVersion)
	}
	joined := strings.ToLower(strings.Join(db.execs, "\n"))
	if !strings.Contains(joined, "drop extension if exists vector") ||
		!strings.Contains(joined, "delete from public.schema_migrations") {
		t.Fatalf("production upgrade SQL did not execute: %s", joined)
	}
	if strings.Contains(joined, "create table public.links") {
		t.Fatal("production baseline incorrectly executed the fresh-install snapshot")
	}
}

func TestRunAlreadyCurrentIsANoOp(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{CurrentSchemaMigrationID}}}
	result, err := Run(context.Background(), db, RunRequest{Target: CurrentSchemaMigrationID})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.AlreadyAtTarget || len(result.Applied) != 0 || len(db.inserts) != 0 {
		t.Fatalf("result/recorded = %+v/%v, want an idempotent no-op", result, db.inserts)
	}
}

func TestFreshTargetRejectsNonEmptyLedger(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{versions: []string{CurrentSchemaMigrationID}}}
	_, err := Run(context.Background(), db, RunRequest{Target: FreshInstallTarget})
	if !errors.Is(err, ErrLedgerAhead) || !strings.Contains(err.Error(), "requires an empty schema ledger") {
		t.Fatalf("Run(fresh) error = %v, want a non-empty-ledger refusal", err)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("refused fresh run recorded %v", db.inserts)
	}
}

func TestRunRejectsInvalidLedgersBeforeMigrationSQL(t *testing.T) {
	t.Parallel()

	for _, versions := range [][]string{
		ProductionBaselineVersions()[:2],
		{CurrentSchemaMigrationID, "future2027010101"},
	} {
		db := &fakeQuerier{rows: &fakeRows{versions: versions}}
		_, err := Run(context.Background(), db, RunRequest{Target: CurrentSchemaMigrationID})
		if !errors.Is(err, ErrLedgerAhead) {
			t.Fatalf("Run() with ledger %v error = %v, want ErrLedgerAhead", versions, err)
		}
		if len(db.inserts) != 0 {
			t.Fatalf("refused ledger %v recorded %v", versions, db.inserts)
		}
	}
}

func TestRunRejectsUnknownTargetBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	_, err := Run(context.Background(), db, RunRequest{Target: "no-such-step"})
	if !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Run() error = %v, want ErrUnknownTarget", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("unknown target executed %d statements", len(db.execs))
	}
}

func TestUpAppliesTheCurrentHead(t *testing.T) {
	t.Parallel()

	db := &fakeQuerier{rows: &fakeRows{}}
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if !slices.Equal(db.inserts, []string{CurrentSchemaMigrationID}) {
		t.Fatalf("recorded = %v, want current head", db.inserts)
	}
}
