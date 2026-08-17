package migrate

import (
	"os"
	"strings"
	"testing"
)

func TestFreshSchemaContainsInstallationContracts(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"CREATE TABLE public.installation_state",
		"CREATE TABLE public.links",
		"CREATE TABLE public.link_translations",
		"CREATE TABLE public.link_url_identities",
		"CREATE TABLE public.feed_subscriptions",
		"CREATE TABLE public.sites",
		"CREATE TABLE public.reader_inbox_jobs",
		"CREATE TABLE public.reader_thought_supersession_events",
		"CREATE TABLE public.reader_content_history",
		"CREATE FUNCTION public.reader_cleanup_content_history(p_batch_size integer DEFAULT 100, p_keep_per_link integer DEFAULT 20)",
		"CREATE FUNCTION public.advance_link_metadata_revision() RETURNS trigger",
		"CONSTRAINT chk_links_metadata_revision_safe",
		"CREATE TRIGGER links_metadata_revision_bump",
		"CREATE INDEX idx_reader_inbox_pending_expiry",
		"CREATE UNIQUE INDEX idx_link_translations_saved_revision_unique",
		"CREATE UNIQUE INDEX idx_link_translations_legacy_source_unique",
		"CREATE INDEX idx_reader_thoughts_search_trgm",
		"CREATE INDEX idx_reader_thought_tombstones_search_trgm",
		"INSERT INTO public.installation_state (singleton) VALUES (true)",
		"INSERT INTO public.library_read_revision (singleton) VALUES (true)",
		"INSERT INTO public.global_read_revision (singleton) VALUES (true)",
		"INSERT INTO public.feed_read_revision (singleton) VALUES (true)",
		"https://www.ruanyifeng.com/blog/atom.xml",
	} {
		if !strings.Contains(singleInstallSchemaSQL, want) {
			t.Errorf("fresh schema missing %q", want)
		}
	}
}

func TestFreshSchemaExcludesCommercialAndIsolationStructures(t *testing.T) {
	t.Parallel()

	lowered := strings.ToLower(singleInstallSchemaSQL)
	for _, forbidden := range []string{
		"tenant_id",
		"create table public.tenants",
		"create table public.api_keys",
		"create table public.usage_events",
		"create table public.feed_bootstraps",
		"create table public.tenant_read_revision",
		"create table public.tenant_feed_revision",
		"enable row level security",
		"force row level security",
		"create policy",
		"app.tenant_id",
		"app.billing_scope",
		"stripe",
		"subscription_status",
	} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("fresh schema contains forbidden commercial/isolation fragment %q", forbidden)
		}
	}
}

// TestFreshSchemaKeepsNoLexemeIndexForSubstringThoughtSearch pins the reason the
// tsvector index was dropped: thought search is a `%query%` ILIKE contract, and
// no substring predicate can consume to_tsvector output. Reintroducing it would
// re-add write amplification on every thought for a reader that cannot use it.
func TestFreshSchemaKeepsNoLexemeIndexForSubstringThoughtSearch(t *testing.T) {
	t.Parallel()

	for _, forbidden := range []string{
		"idx_reader_thought_search ",
		"to_tsvector('simple'::regconfig, body)",
	} {
		if strings.Contains(singleInstallSchemaSQL, forbidden) {
			t.Errorf("fresh schema still carries unusable thought-search index fragment %q", forbidden)
		}
	}
	tail := strings.Join(steps[len(steps)-1].SQL, "\n")
	for _, want := range []string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_reader_thoughts_search_trgm",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_reader_thought_tombstones_search_trgm",
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_reader_thought_search",
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("trigram search migration missing %q", want)
		}
	}
	if !steps[len(steps)-1].NonTransactional {
		t.Error("CONCURRENTLY index migration must be NonTransactional")
	}
}

func TestFreshSchemaLeavesRiverObjectsToRiverMigrations(t *testing.T) {
	t.Parallel()

	for _, forbidden := range []string{
		"CREATE TYPE public.river_job_state",
		"CREATE FUNCTION public.river_job_state_in_bitmask",
		"CREATE SEQUENCE public.river_job_id_seq",
		"CREATE TABLE public.river_job",
	} {
		if strings.Contains(singleInstallSchemaSQL, forbidden) {
			t.Errorf("application schema duplicates River-owned object %q", forbidden)
		}
	}
	if len(steps) != 8 || steps[1].ID != translationTerminalHistoryIndexMigrationID ||
		steps[2].ID != readerThoughtTombstoneSnapshotMigrationID || steps[3].ID != integrityRepairMigrationID ||
		steps[4].ID != historicalRepairMigrationID || steps[5].ID != conceptMergeAuditRepairMigrationID ||
		steps[6].ID != lifecycleRepairMigrationID || steps[7].ID != readerThoughtSearchTrigramMigrationID {
		t.Fatalf("migration plan IDs = %v, want fresh schema and all ordered forward repairs", stepIDs(steps))
	}
	if got := strings.Join(steps[1].SQL, "\n"); !strings.Contains(got, "idx_river_job_translation_terminal_history") {
		t.Fatalf("River tail migration SQL = %q", got)
	}
}

func stepIDs(plan []Step) []string {
	ids := make([]string, len(plan))
	for index, step := range plan {
		ids[index] = step.ID
	}
	return ids
}

func TestRepresentationRevisionTriggersIgnoreEmptyTransitionTables(t *testing.T) {
	t.Parallel()

	for _, functionName := range []string{
		"bump_feed_revision_trigger",
		"bump_global_revision_trigger",
		"bump_library_revision_trigger",
	} {
		body := schemaFunctionDefinition(t, functionName)
		for _, want := range []string{
			"IF TG_OP = 'INSERT' THEN",
			"IF EXISTS (SELECT 1 FROM new_rows) THEN",
			"ELSIF TG_OP = 'DELETE' THEN",
			"IF EXISTS (SELECT 1 FROM old_rows) THEN",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing branch %q", functionName, want)
			}
		}
	}
}

func schemaFunctionDefinition(t *testing.T, name string) string {
	t.Helper()
	startMarker := "CREATE FUNCTION public." + name + "() RETURNS trigger"
	start := strings.Index(singleInstallSchemaSQL, startMarker)
	if start < 0 {
		t.Fatalf("fresh schema missing function %s", name)
	}
	end := strings.Index(singleInstallSchemaSQL[start:], "$$;")
	if end < 0 {
		t.Fatalf("fresh schema function %s has no terminator", name)
	}
	return singleInstallSchemaSQL[start : start+end]
}

func TestGeneratedSchemaSnapshotMatchesPublicShape(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	schema := strings.ToLower(string(raw))
	for _, want := range []string{
		"create table public.installation_state",
		"create table public.links",
		"create table public.reader_thought_supersession_events",
		"create index idx_river_job_translation_terminal_history",
		"create function public.reader_cleanup_content_history(p_batch_size integer default 100, p_keep_per_link integer default 20)",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema.sql missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"tenant_id",
		"create table public.tenants",
		"create table public.api_keys",
		"create table public.usage_events",
		"create policy",
		"row level security",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("schema.sql contains forbidden fragment %q", forbidden)
		}
	}
}
