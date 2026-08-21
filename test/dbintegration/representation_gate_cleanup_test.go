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

	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE public.library_read_revision (
			singleton boolean PRIMARY KEY DEFAULT true,
			revision bigint NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE public.global_read_revision (
			singleton boolean PRIMARY KEY DEFAULT true,
			revision bigint NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE public.feed_read_revision (
			singleton boolean PRIMARY KEY DEFAULT true,
			revision bigint NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO public.library_read_revision DEFAULT VALUES;
		INSERT INTO public.global_read_revision DEFAULT VALUES;
		INSERT INTO public.feed_read_revision DEFAULT VALUES;

		CREATE FUNCTION public.bump_library_read_revision() RETURNS void
		LANGUAGE sql AS $$
			UPDATE public.library_read_revision
			SET revision=revision+1,updated_at=now() WHERE singleton
		$$;
		CREATE FUNCTION public.bump_library_revision_trigger() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM public.bump_library_read_revision();
			RETURN NULL;
		END
		$$;
		CREATE TRIGGER trg_reader_notes_bump_library_revision
		AFTER INSERT ON public.reader_notes FOR EACH STATEMENT
		EXECUTE FUNCTION public.bump_library_revision_trigger();

		CREATE FUNCTION public.lock_representation_revisions(
			lock_global boolean,lock_library boolean,lock_feed boolean) RETURNS void
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM public.lock_representation_write_gate_exclusive();
			IF lock_global THEN
				PERFORM revision FROM public.global_read_revision WHERE singleton FOR UPDATE;
			END IF;
			IF lock_library THEN
				PERFORM revision FROM public.library_read_revision WHERE singleton FOR UPDATE;
			END IF;
			IF lock_feed THEN
				PERFORM revision FROM public.feed_read_revision WHERE singleton FOR UPDATE;
			END IF;
		END
		$$;
		CREATE FUNCTION public.lock_library_feed_revisions() RETURNS void
		LANGUAGE sql AS $$
			SELECT public.lock_representation_revisions(false,true,true)
		$$;
		CREATE FUNCTION public.lock_library_global_revisions() RETURNS void
		LANGUAGE sql AS $$
			SELECT public.lock_representation_revisions(true,true,false)
		$$;
	`); err != nil {
		t.Fatalf("create legacy representation revision objects: %v", err)
	}

	before := readRepresentationGateSchemaState(t, pool)
	if before.revisionTables != 3 || before.bumpTriggers != 1 || before.obsoleteLockFns != 3 ||
		before.writeGateFns != 3 || before.writeGateTriggers != 14 {
		t.Fatalf("legacy fixture = tables:%d bump_triggers:%d lock_functions:%d gate_functions:%d gate_triggers:%d, want 3/1/3/3/14",
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
