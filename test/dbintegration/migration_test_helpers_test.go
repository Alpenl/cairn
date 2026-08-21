package dbintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/migrate"
)

type migrationRowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func assertMigrationRecorded(t *testing.T, db migrationRowQuerier, id string, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE version = $1
	)`, id).Scan(&got); err != nil {
		t.Fatalf("read migration %s state: %v", id, err)
	}
	if got != want {
		t.Fatalf("migration %s recorded = %v, want %v", id, got, want)
	}
}

func assertIndexExists(t *testing.T, db migrationRowQuerier, name string, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(t.Context(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&got); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("index %s exists = %v, want %v", name, got, want)
	}
}

func isolatedMigrationDatabase(t *testing.T) string {
	t.Helper()
	_ = StartPostgres(t)

	baseURL, err := url.Parse(DSN(t))
	if err != nil {
		t.Fatalf("parse shared Postgres DSN: %v", err)
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	adminPool, err := pgxpool.New(t.Context(), adminURL.String())
	if err != nil {
		t.Fatalf("open Postgres admin pool: %v", err)
	}

	databaseName := "migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(t.Context(), `CREATE DATABASE `+quotedName); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated migration database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, databaseName)
		if _, err := adminPool.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+quotedName); err != nil {
			t.Errorf("drop isolated migration database: %v", err)
		}
		adminPool.Close()
	})

	targetURL := *baseURL
	targetURL.Path = "/" + databaseName
	return targetURL.String()
}

// prepareProductionUpgradeFixture converts a current fresh schema back to the
// structural subset that the sole supported v0.1.17 upgrade edge requires.
// Individual tests add the retired objects whose data movement they exercise;
// this helper owns only the common, non-optional baseline and exact ledger.
func prepareProductionUpgradeFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatalf("install current schema before production upgrade fixture: %v", err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin production upgrade fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `
		ALTER TABLE public.link_translations
			ADD COLUMN current_river_job_id bigint,
			DROP CONSTRAINT chk_link_translations_source_content_revision,
			ADD CONSTRAINT chk_link_translations_source_content_revision
			CHECK (source_content_revision IS NULL OR source_content_revision > 0);
		DROP INDEX public.idx_link_translations_summary_source_unique;
		CREATE UNIQUE INDEX idx_link_translations_legacy_source_unique
			ON public.link_translations
			(link_id,scope,block_key,start_offset,end_offset,source_hash,target_language)
			WHERE source_content_revision IS NULL;

		ALTER TABLE public.reader_inbox
			DROP CONSTRAINT reader_inbox_identity_key_check,
			DROP CONSTRAINT reader_inbox_status_check,
			ALTER COLUMN identity_key DROP NOT NULL;
		ALTER TABLE public.reader_inbox
			ADD CONSTRAINT reader_inbox_status_check
			CHECK (status IN ('pending','confirmed','discarded'));
		ALTER TABLE public.reader_thought_ops
			DROP CONSTRAINT reader_thought_ops_target_kind_check,
			ALTER COLUMN logical_clock SET DEFAULT 0;
		ALTER TABLE public.reader_thoughts
			DROP CONSTRAINT reader_thoughts_target_kind_check,
			ALTER COLUMN winner_logical_clock SET DEFAULT 0;

		ALTER TABLE public.reader_feed_hides
			RENAME CONSTRAINT reader_feed_hides_pkey TO reader_feed_feedback_pkey;
		ALTER TABLE public.reader_feed_hides RENAME TO reader_feed_feedback;
		ALTER TABLE public.reader_feed_feedback
			ADD COLUMN action text DEFAULT 'hide' NOT NULL,
			ADD CONSTRAINT reader_feed_feedback_action_check
			CHECK (action IN ('hide','save','unsave'));
		ALTER TABLE public.reader_feed_saves
			ADD COLUMN created_link boolean DEFAULT false NOT NULL;
		CREATE INDEX idx_river_job_translation_terminal_history
			ON public.river_job (finalized_at,id)
			WHERE kind IN ('translate_link_v2','translate_link_content')
			  AND state IN ('cancelled','completed','discarded')
			  AND finalized_at IS NOT NULL;

		CREATE FUNCTION public.guard_representation_write_gate() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NOT pg_try_advisory_xact_lock_shared(4,0) THEN
				IF NOT EXISTS (
					SELECT 1 FROM pg_locks
					WHERE locktype='advisory' AND pid=pg_backend_pid()
					  AND classid=4 AND objid=0 AND objsubid=2
					  AND mode='ExclusiveLock' AND granted
				) THEN
					RAISE EXCEPTION USING ERRCODE='40001',
						MESSAGE='representation write conflicts with an exclusive revision operation',
						HINT='retry the transaction';
				END IF;
			END IF;
			RETURN NULL;
		END
		$$;
		CREATE FUNCTION public.lock_representation_write_gate_exclusive() RETURNS void
		LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(4,0) $$;
		CREATE FUNCTION public.lock_representation_write_gate_shared() RETURNS void
		LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock_shared(4,0) $$;

		CREATE TRIGGER trg_feed_folders_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.feed_folders FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_feed_folders_representation_write_gate_upd
		BEFORE UPDATE OF created_at,id,name,updated_at ON public.feed_folders FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_feed_items_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.feed_items FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_feed_items_representation_write_gate_upd
		BEFORE UPDATE OF author,content_html,content_text,created_at,id,link_id,published_at,
			read_at,read_later,starred,subscription_id,summary,title,url
		ON public.feed_items FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_feed_subscriptions_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.feed_subscriptions FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_feed_subscriptions_representation_write_gate_upd
		BEFORE UPDATE OF active,folder_id,id,title ON public.feed_subscriptions FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_links_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.links FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_links_representation_write_gate_upd
		BEFORE UPDATE OF content,content_cjk_chars,content_document,content_format,
			content_revision,content_source,content_type,content_words,created_at,description,
			domain,error_msg,fetcher_type,id,is_low_confidence,library_kind,library_kind_locked,
			low_confidence_reason,parent_id,parent_path,path_depth,status,summary,tags,title,
			updated_at,url
		ON public.links FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_site_entries_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.site_entries FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_site_entries_representation_write_gate_upd
		BEFORE UPDATE OF id,normalized_url,site_id ON public.site_entries FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_site_tags_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.site_tags FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_site_tags_representation_write_gate_upd
		BEFORE UPDATE OF normalized_tag,site_id,tag ON public.site_tags FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_sites_representation_write_gate_ins_del
		BEFORE INSERT OR DELETE ON public.sites FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();
		CREATE TRIGGER trg_sites_representation_write_gate_upd
		BEFORE UPDATE OF first_collected_at,homepage_url,icon_url,id,intro,last_collected_at,
			name,pinned,primary_entry_id,revision,site_key,updated_at
		ON public.sites FOR EACH STATEMENT
		EXECUTE FUNCTION public.guard_representation_write_gate();

		DELETE FROM public.schema_migrations;
	`); err != nil {
		t.Fatalf("restore common v0.1.17 schema shape: %v", err)
	}
	for _, version := range migrate.ProductionBaselineVersions() {
		if _, err := tx.Exec(t.Context(),
			`INSERT INTO public.schema_migrations(version) VALUES ($1)`, version); err != nil {
			t.Fatalf("record production baseline migration %s: %v", version, err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit production upgrade fixture: %v", err)
	}
	assertProductionBaselineLedger(t, pool)
}

func assertProductionBaselineLedger(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	assertMigrationRecorded(t, pool, migrate.CurrentSchemaMigrationID, false)
	for _, version := range migrate.ProductionBaselineVersions() {
		assertMigrationRecorded(t, pool, version, true)
	}
}

func assertCurrentSchemaLedger(t *testing.T, pool migrationRowQuerier) {
	t.Helper()
	assertMigrationRecorded(t, pool, migrate.CurrentSchemaMigrationID, true)
	for _, version := range migrate.ProductionBaselineVersions() {
		assertMigrationRecorded(t, pool, version, false)
	}
}

func installLegacyLinkClassificationSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `ALTER TABLE public.links
		ADD COLUMN library_kind_source text,
		ADD COLUMN predicted_library_kind text,
		ADD COLUMN classification_confidence real,
		ADD COLUMN classification_reason text,
		ADD COLUMN classification_explanation text,
		ADD COLUMN classifier_version text,
		ADD COLUMN requested_library_kind text DEFAULT 'auto' NOT NULL,
		ADD COLUMN requested_library_kind_source text DEFAULT 'auto' NOT NULL,
		ADD CONSTRAINT chk_links_classification_confidence CHECK (
			classification_confidence IS NULL OR classification_confidence BETWEEN 0 AND 1),
		ADD CONSTRAINT chk_links_library_kind_pair CHECK (
			(library_kind IS NULL) = (library_kind_source IS NULL)),
		ADD CONSTRAINT chk_links_library_kind_source CHECK (
			library_kind_source IS NULL OR library_kind_source IN ('auto','user','migration')),
		ADD CONSTRAINT chk_links_predicted_library_kind CHECK (
			predicted_library_kind IS NULL OR predicted_library_kind IN ('reading','site')),
		ADD CONSTRAINT chk_links_requested_library_intent CHECK (
			requested_library_kind_source <> 'user' OR requested_library_kind IN ('reading','site')),
		ADD CONSTRAINT chk_links_requested_library_kind CHECK (
			requested_library_kind IN ('auto','reading','site')),
		ADD CONSTRAINT chk_links_requested_library_kind_source CHECK (
			requested_library_kind_source IN ('auto','user'))`); err != nil {
		t.Fatalf("install legacy Link classification schema: %v", err)
	}
}

func runMigrateCommand(t *testing.T, dsn, target string, wantError bool) string {
	t.Helper()
	stdout, stderr := runMigrate(t, migrateInvocation{dsn: dsn, target: target, wantError: wantError})
	return stdout + stderr
}

// migrateInvocation is one cmd/migrate run. It exists so the exact-target and
// fail-closed tests can drive the real command with the real environment the
// deploy helper would use, rather than reaching into the package internals.
type migrateInvocation struct {
	dsn       string
	target    string
	extraEnv  []string
	args      []string
	wantError bool
}

// runMigrate returns stdout and stderr separately. With --report-json the
// contract is that stdout holds exactly one JSON object and every human line
// moves to stderr, so a combined stream would not be parsable.
func runMigrate(t *testing.T, invocation migrateInvocation) (string, string) {
	t.Helper()
	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	args := append([]string{"run", "./cmd/migrate"}, invocation.args...)
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = repositoryRoot
	cmd.Env = append(migrationCommandEnvironment(invocation.dsn, invocation.target), invocation.extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if invocation.wantError && err == nil {
		t.Fatalf("cmd/migrate target %q unexpectedly succeeded\nstdout=%s\nstderr=%s",
			invocation.target, stdout.String(), stderr.String())
	}
	if !invocation.wantError && err != nil {
		t.Fatalf("cmd/migrate target %q failed: %v\nstdout=%s\nstderr=%s",
			invocation.target, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// migrationReport mirrors the JSON contract cmd/migrate emits for the deploy
// helper. Decoding it here is the mechanical check that the contract survives:
// a renamed field shows up as a zero value the assertions reject.
type migrationReport struct {
	SchemaVersion int      `json:"schema_version"`
	Tool          string   `json:"tool"`
	OK            bool     `json:"ok"`
	Mode          string   `json:"mode"`
	Target        string   `json:"target"`
	AllowManual   bool     `json:"allow_manual"`
	StartVersion  string   `json:"start_version"`
	EndVersion    string   `json:"end_version"`
	Applied       []string `json:"applied"`

	AlreadyAtTarget bool `json:"already_at_target"`
	StoppedAtManual bool `json:"-"`

	OnlineUpdate *struct {
		Target   string   `json:"target"`
		Pending  []string `json:"pending"`
		Allowed  bool     `json:"allowed"`
		Blockers []struct {
			StepID string `json:"step_id"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"blockers"`
	} `json:"online_update"`

	Ledgers *struct {
		OK     bool `json:"ok"`
		Schema struct {
			Present  bool     `json:"present"`
			Target   string   `json:"target"`
			Head     string   `json:"head"`
			Applied  []string `json:"applied"`
			Missing  []string `json:"missing"`
			Extra    []string `json:"extra"`
			AtTarget bool     `json:"at_target"`
		} `json:"schema"`
		River struct {
			Present  bool   `json:"present"`
			Line     string `json:"line"`
			Target   int    `json:"target"`
			Head     int    `json:"head"`
			Applied  []int  `json:"applied"`
			Missing  []int  `json:"missing"`
			Extra    []int  `json:"extra"`
			AtTarget bool   `json:"at_target"`
		} `json:"river"`
		Problems []struct {
			Ledger string `json:"ledger"`
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
		} `json:"problems"`
	} `json:"ledgers"`

	Error *struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeMigrationReport(t *testing.T, stdout string) migrationReport {
	t.Helper()
	var report migrationReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("cmd/migrate --report-json stdout is not a single JSON object: %v\n%s", err, stdout)
	}
	if report.SchemaVersion != 1 {
		t.Fatalf("report schema_version = %d, want 1", report.SchemaVersion)
	}
	if report.Tool != "cairn-migrate" {
		t.Fatalf("report tool = %q, want cairn-migrate", report.Tool)
	}
	return report
}

func migrationTargetPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open pool on isolated migration database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func schemaLedgerVersions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	versions := make([]string, 0, 16)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	return versions
}

func migrationCommandEnvironment(dsn, target string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DATABASE_URL=") || strings.HasPrefix(entry, "MIGRATION_TARGET=") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "DATABASE_URL="+dsn)
	if target != "" {
		environment = append(environment, "MIGRATION_TARGET="+target)
	}
	return environment
}
