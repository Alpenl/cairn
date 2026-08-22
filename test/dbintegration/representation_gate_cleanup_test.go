package dbintegration

import (
	"testing"

	"webtag/internal/migrate"
)

type representationGateSchemaState struct {
	revisionTables    int
	bumpTriggers      int
	obsoleteLockFns   int
	writeGateFns      int
	writeGateTriggers int
}

func TestRepresentationCleanupRemovesRevisionAndWriteGate(t *testing.T) {
	pool := StartPostgres(t)
	state := readRepresentationGateSchemaState(t, pool)
	assertRepresentationGateSchemaClean(t, state)
}

func TestRepresentationCleanupMigratesLegacyObjects(t *testing.T) {
	dsn := isolatedMigrationDatabase(t)
	pool := migrationTargetPool(t, dsn)
	prepareProductionUpgradeFixture(t, pool)

	before := readRepresentationGateSchemaState(t, pool)
	if before.revisionTables != 3 || before.bumpTriggers != 27 || before.obsoleteLockFns != 3 ||
		before.writeGateFns != 3 || before.writeGateTriggers != 18 {
		t.Fatalf("legacy fixture = tables:%d bump_triggers:%d lock_functions:%d gate_functions:%d gate_triggers:%d, want 3/27/3/3/18",
			before.revisionTables, before.bumpTriggers, before.obsoleteLockFns,
			before.writeGateFns, before.writeGateTriggers)
	}

	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("apply production schema upgrade: %v", err)
	}
	assertCurrentSchemaLedger(t, pool)
	assertRepresentationGateSchemaClean(t, readRepresentationGateSchemaState(t, pool))
}

func readRepresentationGateSchemaState(t *testing.T, pool migrationRowQuerier) representationGateSchemaState {
	t.Helper()

	var state representationGateSchemaState
	if err := pool.QueryRow(t.Context(), `
		SELECT
			(SELECT count(*)
			 FROM information_schema.tables
			 WHERE table_schema='public'
			   AND table_name IN ('library_read_revision','global_read_revision','feed_read_revision')),
			(SELECT count(*)
			 FROM pg_trigger AS trg
			 JOIN pg_class AS rel ON rel.oid=trg.tgrelid
			 JOIN pg_namespace AS ns ON ns.oid=rel.relnamespace
			 JOIN pg_proc AS proc ON proc.oid=trg.tgfoid
			 WHERE NOT trg.tgisinternal
			   AND ns.nspname='public'
			   AND proc.proname LIKE 'bump_%'),
			(SELECT count(*)
			 FROM pg_proc AS proc
			 JOIN pg_namespace AS ns ON ns.oid=proc.pronamespace
			 WHERE ns.nspname='public'
			   AND proc.proname IN ('lock_library_feed_revisions',
					'lock_library_global_revisions','lock_representation_revisions')),
			(SELECT count(*)
			 FROM pg_proc AS proc
			 JOIN pg_namespace AS ns ON ns.oid=proc.pronamespace
			 WHERE ns.nspname='public'
			   AND proc.proname IN ('guard_representation_write_gate',
					'lock_representation_write_gate_shared',
					'lock_representation_write_gate_exclusive')),
			(SELECT count(*)
			 FROM pg_trigger AS trg
			 JOIN pg_class AS rel ON rel.oid=trg.tgrelid
			 JOIN pg_namespace AS ns ON ns.oid=rel.relnamespace
			 JOIN pg_proc AS proc ON proc.oid=trg.tgfoid
			 WHERE NOT trg.tgisinternal
			   AND ns.nspname='public'
			   AND proc.proname='guard_representation_write_gate')
	`).Scan(&state.revisionTables, &state.bumpTriggers, &state.obsoleteLockFns,
		&state.writeGateFns, &state.writeGateTriggers); err != nil {
		t.Fatalf("inspect representation gate schema: %v", err)
	}
	return state
}

func assertRepresentationGateSchemaClean(t *testing.T, state representationGateSchemaState) {
	t.Helper()

	if state.revisionTables != 0 || state.bumpTriggers != 0 || state.obsoleteLockFns != 0 {
		t.Fatalf("obsolete representation state = tables:%d bump_triggers:%d lock_functions:%d, want all zero",
			state.revisionTables, state.bumpTriggers, state.obsoleteLockFns)
	}
	if state.writeGateFns != 0 || state.writeGateTriggers != 0 {
		t.Fatalf("representation write gate = functions:%d triggers:%d, want all zero",
			state.writeGateFns, state.writeGateTriggers)
	}
}
