package migrate

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMigrationPlanHasOneCurrentHead(t *testing.T) {
	t.Parallel()

	plan := Steps()
	if len(plan) != 1 || plan[0].ID != CurrentSchemaMigrationID {
		t.Fatalf("migration plan IDs = %v, want [%s]", stepIDs(plan), CurrentSchemaMigrationID)
	}
	if len(plan[0].SQL) != 4 {
		t.Fatalf("current migration has %d statements, want schema cleanup plus two seed statements", len(plan[0].SQL))
	}
	if plan[0].SQL[0] != currentInstallSchemaSQL {
		t.Fatal("current migration does not install the generated application schema")
	}
	if plan[0].OnlineUpdate.Compatibility != OnlineUpdateIncompatible ||
		strings.TrimSpace(plan[0].OnlineUpdate.Note) == "" {
		t.Fatalf("current migration online review = %+v, want an explained incompatibility", plan[0].OnlineUpdate)
	}
}

func TestFreshSchemaContainsCurrentContracts(t *testing.T) {
	t.Parallel()

	body := strings.Join(steps[0].SQL, "\n")
	for _, want := range []string{
		"CREATE TABLE public.installation_state",
		"CREATE TABLE public.links",
		"CREATE TABLE public.link_translations",
		"CREATE TABLE public.link_url_identities",
		"CREATE TABLE public.feed_subscriptions",
		"CREATE TABLE public.sites",
		"CREATE TABLE public.reader_thought_supersession_events",
		"CREATE FUNCTION public.advance_link_metadata_revision() RETURNS trigger",
		"CONSTRAINT chk_links_metadata_revision_safe",
		"parse_generation bigint DEFAULT 1 NOT NULL",
		"CONSTRAINT chk_links_parse_generation_safe",
		"library_kind_locked boolean DEFAULT false NOT NULL",
		"CONSTRAINT chk_links_library_kind_lock",
		"CREATE TRIGGER links_metadata_revision_bump",
		"CREATE INDEX idx_reader_inbox_pending_expiry",
		"CREATE UNIQUE INDEX idx_link_translations_saved_revision_unique",
		"CREATE UNIQUE INDEX idx_link_translations_summary_source_unique",
		"CREATE INDEX idx_reader_thoughts_search_trgm",
		"CREATE INDEX idx_reader_thought_tombstones_search_trgm",
		"INSERT INTO public.installation_state (singleton) VALUES (true)",
		"https://www.ruanyifeng.com/blog/atom.xml",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fresh schema missing %q", want)
		}
	}
}

func TestFreshSchemaExcludesRetiredAndExternallyOwnedObjects(t *testing.T) {
	t.Parallel()

	lowered := strings.ToLower(currentInstallSchemaSQL)
	for _, forbidden := range []string{
		"schema_migrations",
		"create type public.river_job_state",
		"create function public.river_job_state_in_bitmask",
		"create sequence public.river_job_id_seq",
		"create table public.river_job",
		"tenant_id",
		"create table public.tenants",
		"create table public.api_keys",
		"create table public.usage_events",
		"enable row level security",
		"force row level security",
		"create policy",
		"stripe",
		"create extension if not exists vector",
		"public.vector(1536)",
		"vector_cosine_ops",
		"create table public.reader_inbox_jobs",
		"create table public.parse_jobs",
		"create table public.reader_categories",
		"create table public.reader_categorizables",
		"create table public.concept",
		"create table public.concept_alias",
		"create table public.concept_merge_proposal",
		"create table public.link_concept",
		"create table public.library_classification_rules",
		"create table public.library_review_items",
		"create table public.reader_content_history",
		"create table public.feed_lifecycle_repair_audit",
		"create table public.reader_tag_activity",
		"create table public.reader_domain_activity",
		"create table public.library_read_revision",
		"create table public.global_read_revision",
		"create table public.feed_read_revision",
		"idx_link_translations_missing_reconcile",
		"idx_river_job_translation_terminal_history",
		"idx_reader_thought_search ",
		"to_tsvector('simple'::regconfig, body)",
		"proposal_signals",
		"expiry_lease_id",
		"expiry_lease_until",
		"'discarded'::text",
		"library_kind_source",
		"predicted_library_kind",
		"classification_confidence",
		"requested_library_kind",
		"name_source",
		"intro_source",
		"homepage_source",
		"icon_source",
		"primary_source",
		"grouping_locked",
		"entry_name_source",
		"purpose_source",
		"guard_representation_write_gate",
		"lock_representation_write_gate_shared",
		"lock_representation_write_gate_exclusive",
	} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("fresh schema contains retired fragment %q", forbidden)
		}
	}
}

func TestProductionUpgradeIsOneOrderedEdge(t *testing.T) {
	t.Parallel()

	wantNames := []string{
		"obsolete subsystems and protocol constraints",
		"Reader Inbox state",
		"parse state",
		"representation revisions and write gate",
		"Site provenance",
		"Reader Category",
	}
	gotNames := make([]string, 0, len(productionUpgradeSegments))
	for _, segment := range productionUpgradeSegments {
		gotNames = append(gotNames, segment.Name)
		if len(segment.SQL) == 0 {
			t.Errorf("production upgrade segment %q has no SQL", segment.Name)
		}
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("production upgrade segments = %v, want %v", gotNames, wantNames)
	}

	body := strings.ToLower(strings.Join(productionUpgradeSQL(), "\n"))
	for _, want := range []string{
		"drop extension if exists vector",
		"drop table if exists public.reader_inbox_jobs",
		"drop table if exists public.parse_jobs",
		"public.library_read_revision",
		"drop function if exists public.guard_representation_write_gate",
		"drop column if exists name_source",
		"drop table if exists public.reader_categories",
		"delete from public.schema_migrations",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("production upgrade missing %q", want)
		}
	}
	for _, version := range ProductionBaselineVersions() {
		if !strings.Contains(body, "'"+strings.ToLower(version)+"'") {
			t.Errorf("production upgrade does not retire ledger version %q", version)
		}
	}
}

func TestProductionBaselineIsExactAndDefensivelyCopied(t *testing.T) {
	t.Parallel()

	want := []string{
		"f03e51d6911b",
		"b671c9d2e411",
		"reader2026081301",
		"integrity2026081401",
		"historical2026081401",
		"conceptaudit2026081401",
		"lifecycle2026081401",
		"readersearch2026081701",
		"readertodoprojection2026081701",
		ProductionBaselineMigrationID,
	}
	got := ProductionBaselineVersions()
	if !slices.Equal(got, want) {
		t.Fatalf("production baseline = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if slices.Equal(got, ProductionBaselineVersions()) {
		t.Fatal("ProductionBaselineVersions returned mutable package state")
	}
}

func TestGeneratedSchemaSnapshotsHaveSeparateOwnership(t *testing.T) {
	t.Parallel()

	fullRaw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	installRaw, err := os.ReadFile("install_schema.sql")
	if err != nil {
		t.Fatalf("read install_schema.sql: %v", err)
	}
	full := strings.ToLower(string(fullRaw))
	install := strings.ToLower(string(installRaw))
	for _, want := range []string{
		"create table public.installation_state",
		"create table public.links",
		"create table public.reader_thought_supersession_events",
	} {
		if !strings.Contains(full, want) || !strings.Contains(install, want) {
			t.Errorf("generated snapshots do not both contain %q", want)
		}
	}
	for _, ownedElsewhere := range []string{
		"create table public.schema_migrations",
		"create table public.river_job",
		"create type public.river_job_state",
	} {
		if !strings.Contains(full, ownedElsewhere) {
			t.Errorf("full schema missing externally managed object %q", ownedElsewhere)
		}
		if strings.Contains(install, ownedElsewhere) {
			t.Errorf("application install schema duplicates externally managed object %q", ownedElsewhere)
		}
	}
}

func stepIDs(plan []Step) []string {
	ids := make([]string, len(plan))
	for index, step := range plan {
		ids[index] = step.ID
	}
	return ids
}
