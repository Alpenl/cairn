package migrate

const (
	// CurrentSchemaMigrationID is the only schema target emitted by new release
	// manifests. Fresh installs write it after installing install_schema.sql;
	// the sole supported upgrade writes it after converting the verified
	// v0.1.17 production schema.
	CurrentSchemaMigrationID = "schema2026082201"

	// ProductionBaselineMigrationID names the head of the exact v0.1.17 ledger
	// currently deployed on tokyo-arm-1. It is not a runnable migration target.
	ProductionBaselineMigrationID = "readerinboxdocument2026081801"
)

const dropRepresentationWriteGateSQL = `DO $$
DECLARE
	trigger_record record;
BEGIN
	FOR trigger_record IN
		SELECT ns.nspname AS schema_name, rel.relname AS table_name, trg.tgname
		FROM pg_trigger AS trg
		JOIN pg_class AS rel ON rel.oid = trg.tgrelid
		JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
		JOIN pg_proc AS proc ON proc.oid = trg.tgfoid
		WHERE NOT trg.tgisinternal
		  AND ns.nspname = 'public'
		  AND proc.proname = 'guard_representation_write_gate'
	LOOP
		EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I.%I',
			trigger_record.tgname, trigger_record.schema_name, trigger_record.table_name);
	END LOOP;
END
$$;
DROP FUNCTION IF EXISTS public.guard_representation_write_gate();
DROP FUNCTION IF EXISTS public.lock_representation_write_gate_shared();
DROP FUNCTION IF EXISTS public.lock_representation_write_gate_exclusive()`

var productionBaselineLedger = []string{
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

// upgradeSegment keeps the one supported production upgrade readable without
// turning each concern back into a durable migration target.
type upgradeSegment struct {
	Name string
	SQL  []string
}

var productionUpgradeSegments = []upgradeSegment{
	{
		// Most removed subsystems were never used by the production installation,
		// but their tables, triggers, vector columns, extension, and translation
		// reconciliation indexes remained part of every write and deployment. The
		// Activity tables are different: they held only a periodically rebuilt copy
		// of links, so the new binary reads the source rows directly. Upgrades can
		// remove all of this storage in one transaction. The current translation
		// worker owns final failure projection and only the translate_link_v2
		// protocol remains. These one-time updates retire old
		// translate_link_content work and settle attempts whose bound River row is
		// gone or already terminal.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release still reads and writes these tables, columns, and
		// indexes. Once this migration commits, that binary cannot safely serve or
		// be rolled back against the upgraded database.
		Name: "obsolete subsystems and protocol constraints",
		SQL: []string{
			`DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM public.reader_inbox
		WHERE identity_key IS NULL OR btrim(identity_key)=''
	) THEN
		RAISE EXCEPTION 'reader_inbox contains rows without canonical identity_key';
	END IF;
	IF EXISTS (
		SELECT 1 FROM public.reader_thought_ops
		WHERE logical_clock <= 0 OR target->>'kind'='legacy-stale'
	) OR EXISTS (
		SELECT 1 FROM public.reader_thoughts
		WHERE winner_logical_clock <= 0 OR target->>'kind'='legacy-stale'
	) OR EXISTS (
		SELECT 1 FROM public.reader_thought_supersession_events
		WHERE loser #>> '{target,kind}'='legacy-stale'
		   OR winner_at_detection #>> '{target,kind}'='legacy-stale'
	) OR EXISTS (
		SELECT 1 FROM public.reader_thought_tombstones
		WHERE snapshot #>> '{target,kind}'='legacy-stale'
	) THEN
		RAISE EXCEPTION 'Reader Thoughts contain retired legacy or zero-clock records';
	END IF;
END
$$`,
			`DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM public.link_translations
		WHERE (
			(source_content_revision IS NULL AND scope='selection' AND block_key='summary')
			OR (source_content_revision > 0 AND block_key IN ('content','content-document'))
		) IS NOT TRUE
	) THEN
		RAISE EXCEPTION 'link_translations contains retired or unverified source identities';
	END IF;
END
$$`,
			`ALTER TABLE public.link_translations
			 DROP CONSTRAINT IF EXISTS chk_link_translations_source_content_revision,
			 ADD CONSTRAINT chk_link_translations_source_content_revision CHECK (
				(source_content_revision IS NULL AND scope='selection' AND block_key='summary')
				OR (source_content_revision > 0 AND block_key IN ('content','content-document'))
			 )`,
			`DROP INDEX IF EXISTS public.idx_link_translations_legacy_source_unique`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_link_translations_summary_source_unique
			 ON public.link_translations USING btree
			 (link_id,scope,block_key,start_offset,end_offset,source_hash,target_language)
			 WHERE source_content_revision IS NULL`,
			`ALTER TABLE public.reader_inbox
			 ALTER COLUMN identity_key SET NOT NULL,
			 ADD CONSTRAINT reader_inbox_identity_key_check CHECK (btrim(identity_key) <> '')`,
			`ALTER TABLE public.reader_thought_ops
			 DROP CONSTRAINT IF EXISTS reader_thought_ops_logical_clock_check,
			 ALTER COLUMN logical_clock DROP DEFAULT,
			 ADD CONSTRAINT reader_thought_ops_logical_clock_check
			 CHECK (logical_clock > 0 AND logical_clock <= 9007199254740991),
			 ADD CONSTRAINT reader_thought_ops_target_kind_check
			 CHECK (target->>'kind' = ANY (ARRAY['saved-content'::text,'summary'::text,'note'::text,'inbox'::text]))`,
			`ALTER TABLE public.reader_thoughts
			 DROP CONSTRAINT IF EXISTS reader_thoughts_winner_logical_clock_check,
			 ALTER COLUMN winner_logical_clock DROP DEFAULT,
			 ADD CONSTRAINT reader_thoughts_winner_logical_clock_check
			 CHECK (winner_logical_clock > 0 AND winner_logical_clock <= 9007199254740991),
			 ADD CONSTRAINT reader_thoughts_target_kind_check
			 CHECK ((user_deleted AND target='{}'::jsonb) OR target->>'kind' = ANY (ARRAY['saved-content'::text,'summary'::text,'note'::text,'inbox'::text]))`,
			`UPDATE public.river_job
			 SET state='cancelled',
			     finalized_at=COALESCE(finalized_at,CURRENT_TIMESTAMP)
			 WHERE kind='translate_link_content'
			   AND state IN ('available','pending','retryable','running','scheduled')`,
			`UPDATE public.link_translations AS translation
			 SET status='failed',
			     error_msg='翻译服务暂时不可用，请重试',
			     current_river_job_id=NULL,
			     updated_at=CURRENT_TIMESTAMP
			 WHERE translation.status IN ('pending','processing')
			   AND translation.current_river_job_id IS NOT NULL
			   AND NOT EXISTS (
			       SELECT 1
			       FROM public.river_job AS job
			       WHERE job.id=translation.current_river_job_id
			         AND job.state IN ('available','pending','retryable','running','scheduled')
			   )`,
			`UPDATE public.link_translations
			 SET status='failed',
			     error_msg='翻译服务暂时不可用，请重试',
			     current_river_job_id=NULL,
			     updated_at=CURRENT_TIMESTAMP
			 WHERE status IN ('pending','processing')
			   AND attempt_generation=0`,
			`ALTER TABLE public.link_translations
			 DROP COLUMN IF EXISTS current_river_job_id`,
			`DROP INDEX IF EXISTS public.idx_link_translations_missing_reconcile`,
			`DROP INDEX IF EXISTS public.idx_river_job_translation_terminal_history`,
			`DELETE FROM public.reader_feed_feedback WHERE action IN ('save','unsave')`,
			`ALTER TABLE public.reader_feed_feedback
				 DROP CONSTRAINT IF EXISTS reader_feed_feedback_action_check,
				 DROP COLUMN action`,
			`ALTER TABLE public.reader_feed_feedback RENAME TO reader_feed_hides`,
			`ALTER TABLE public.reader_feed_hides
				 RENAME CONSTRAINT reader_feed_feedback_pkey TO reader_feed_hides_pkey`,
			`ALTER TABLE public.reader_feed_saves DROP COLUMN created_link`,
			`DROP TRIGGER IF EXISTS trg_reader_capture_content_history ON public.links`,
			`DROP TRIGGER IF EXISTS trg_links_representation_write_gate_upd ON public.links`,
			`DROP TRIGGER IF EXISTS trg_sites_representation_write_gate_upd ON public.sites`,
			`DROP TABLE IF EXISTS
			 public.concept_alias,
			 public.concept_merge_proposal,
			 public.link_concept,
			 public.concept,
			 public.library_classification_rules,
			 public.library_review_items,
			 public.reader_content_history,
			 public.reader_todo_projection_backfills,
			 public.feed_lifecycle_repair_audit,
			 public.reader_tag_activity,
			 public.reader_domain_activity,
			 public.reader_feed_snapshots
			 CASCADE`,
			`UPDATE public.links SET library_kind_locked=false WHERE library_kind IS NULL AND library_kind_locked`,
			`DELETE FROM public.links WHERE status='skeleton'`,
			`ALTER TABLE public.links
			 DROP CONSTRAINT IF EXISTS chk_links_status,
			 DROP CONSTRAINT IF EXISTS chk_links_classification_confidence,
			 DROP CONSTRAINT IF EXISTS chk_links_library_kind_pair,
			 DROP CONSTRAINT IF EXISTS chk_links_library_kind_lock,
			 DROP CONSTRAINT IF EXISTS chk_links_library_kind_source,
			 DROP CONSTRAINT IF EXISTS chk_links_predicted_library_kind,
			 DROP CONSTRAINT IF EXISTS chk_links_requested_library_intent,
			 DROP CONSTRAINT IF EXISTS chk_links_requested_library_kind,
			 DROP CONSTRAINT IF EXISTS chk_links_requested_library_kind_source,
			 DROP COLUMN IF EXISTS embedding,
			 DROP COLUMN IF EXISTS embedding_model,
			 DROP COLUMN IF EXISTS library_kind_source,
			 DROP COLUMN IF EXISTS predicted_library_kind,
			 DROP COLUMN IF EXISTS classification_confidence,
			 DROP COLUMN IF EXISTS classification_reason,
			 DROP COLUMN IF EXISTS classification_explanation,
			 DROP COLUMN IF EXISTS classifier_version,
			 DROP COLUMN IF EXISTS requested_library_kind,
			 DROP COLUMN IF EXISTS requested_library_kind_source,
			 ADD CONSTRAINT chk_links_library_kind_lock CHECK (NOT library_kind_locked OR library_kind IS NOT NULL),
			 ADD CONSTRAINT chk_links_status CHECK (status = ANY (ARRAY['pending'::text, 'processing'::text, 'done'::text, 'failed'::text]))`,
			`ALTER TABLE public.sites
			 DROP COLUMN IF EXISTS needs_review,
			 DROP COLUMN IF EXISTS embedding,
			 DROP COLUMN IF EXISTS embedding_model`,
			`ALTER TABLE public.site_tags DROP COLUMN IF EXISTS concept_id`,
			`DROP FUNCTION IF EXISTS
			 public.bump_concept_global_revision_update(),
			 public.bump_link_concept_read_revision_update(),
			 public.reader_capture_content_history(),
			 public.reader_cleanup_content_history(integer, integer)`,
			`DROP EXTENSION IF EXISTS vector`,
		},
	},
	{
		// River already persists every proposal attempt and terminal outcome. The
		// reader_inbox_jobs mirror and its repair worker therefore created a second
		// state machine without adding durable information. Expiry likewise needs no
		// materialized timestamp or lease because no work occurs when time passes;
		// PostgreSQL can partition rows directly from expires_at.
		//
		// Existing queued and running River rows retain their inbox_id and
		// expected_metadata_revision arguments, so the new worker can finish them.
		// Historical discarded rows move to the existing trash representation before
		// the redundant status value is removed.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release reads and writes reader_inbox_jobs and the removed
		// reader_inbox columns, so it cannot serve against this schema or be rolled
		// back after the migration commits.
		Name: "Reader Inbox state",
		SQL: []string{
			`UPDATE public.reader_inbox
			 SET deleted_at=COALESCE(deleted_at,CURRENT_TIMESTAMP),status='pending',updated_at=CURRENT_TIMESTAMP
			 WHERE status='discarded'`,
			`DROP TABLE IF EXISTS public.reader_inbox_jobs CASCADE`,
			`DROP INDEX IF EXISTS public.idx_reader_inbox_pending_expiry`,
			`ALTER TABLE public.reader_inbox
			 DROP CONSTRAINT IF EXISTS reader_inbox_status_check,
			 DROP CONSTRAINT IF EXISTS reader_inbox_proposal_status_check,
			 ALTER COLUMN proposal_status SET DEFAULT 'idle'::text,
			 DROP COLUMN IF EXISTS job_id,
			 DROP COLUMN IF EXISTS proposal_signals,
			 DROP COLUMN IF EXISTS expired_at,
			 DROP COLUMN IF EXISTS expiry_lease_id,
			 DROP COLUMN IF EXISTS expiry_lease_until`,
			`ALTER TABLE public.reader_inbox
			 ADD CONSTRAINT reader_inbox_status_check
			 CHECK ((status = ANY (ARRAY['pending'::text, 'confirmed'::text]))),
			 ADD CONSTRAINT reader_inbox_proposal_status_check
			 CHECK ((proposal_status = ANY (ARRAY['idle'::text, 'pending'::text, 'running'::text, 'completed'::text, 'failed'::text])))`,
			`CREATE INDEX IF NOT EXISTS idx_reader_inbox_pending_expiry
			 ON public.reader_inbox USING btree (expires_at,id)
			 WHERE status='pending' AND expires_at IS NOT NULL AND deleted_at IS NULL`,
		},
	},
	{
		// parse_jobs mirrored River's execution lifecycle and forced every parse
		// transition to update two state machines. The Link now carries only the
		// immutable generation needed to reject stale output; River remains the
		// queue, retry, attempt, and terminal-history ledger.
		//
		// Pending work is preserved across the protocol change. Every old
		// parse_link row is terminalized first, each visible in-flight Link is reset
		// to pending, and exactly one generation-aware River row is inserted for it
		// before the mirror table is dropped.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release requires parse_jobs and emits parse_job_id-only River
		// arguments. It must be stopped before this migration; otherwise it can race
		// the cancellation and cannot execute the replacement protocol.
		Name: "parse state",
		SQL: []string{
			`SELECT public.lock_representation_write_gate_exclusive()`,
			`ALTER TABLE public.links ADD COLUMN IF NOT EXISTS parse_generation bigint;
			 UPDATE public.links SET parse_generation=1 WHERE parse_generation IS NULL;
			 ALTER TABLE public.links
				ALTER COLUMN parse_generation SET DEFAULT 1,
				ALTER COLUMN parse_generation SET NOT NULL,
				DROP CONSTRAINT IF EXISTS chk_links_parse_generation_safe;
			 ALTER TABLE public.links ADD CONSTRAINT chk_links_parse_generation_safe
				CHECK ((parse_generation >= 1) AND (parse_generation <= 9007199254740991))`,
			`SELECT id
			 FROM public.links
			 WHERE deleted_at IS NULL AND status IN ('pending','processing')
			 ORDER BY id
			 FOR UPDATE`,
			`WITH locked_job AS (
				SELECT id
				FROM public.river_job
				WHERE kind='parse_link'
				  AND state IN ('available','pending','retryable','running','scheduled')
				ORDER BY id
				FOR UPDATE
			)
			UPDATE public.river_job AS job
			SET state='cancelled',
				finalized_at=CURRENT_TIMESTAMP,
				metadata=jsonb_set(
					jsonb_set(COALESCE(job.metadata,'{}'::jsonb),'{cancel_attempted_at}',to_jsonb(CURRENT_TIMESTAMP),true),
					'{cairn_cancel_reason}',to_jsonb('parse_generation_protocol_migration'::text),true)
			FROM locked_job
			WHERE job.id=locked_job.id`,
			`UPDATE public.links
			 SET status='pending',error_msg=NULL,updated_at=CURRENT_TIMESTAMP
			 WHERE deleted_at IS NULL AND status IN ('pending','processing')`,
			`INSERT INTO public.river_job (args,kind,max_attempts,state)
			 SELECT jsonb_build_object(
				'link_id',link.id,
				'parse_generation',link.parse_generation,
				'expected_metadata_revision',link.metadata_revision),
				'parse_link',3,'available'
			 FROM public.links AS link
			 WHERE link.deleted_at IS NULL AND link.status='pending'
			 ORDER BY link.id`,
			`DROP TABLE IF EXISTS public.parse_jobs CASCADE`,
		},
	},
	{
		// The component read-revision values were never consumed by the runtime.
		// Their advisory write gate no longer protects a read-side invariant, so it
		// only serializes otherwise independent Link, Feed, and Site writes. Normal
		// transaction visibility, row locks, and compare-and-swap revisions remain
		// the coordination mechanisms for live business state.
		//
		// The trigger cleanup is dynamic because earlier migrations already remove
		// some of the tables those triggers originally belonged to. Looking up the
		// trigger functions in pg_proc makes this step idempotent across both fresh
		// installs and databases upgraded through every historical shape. The write
		// gate cleanup uses the same approach because its statement triggers span
		// several tables.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release still references the revision tables and wrapper
		// lock functions. Stop the old Core before applying this step; after it
		// commits, rollback to that binary is not schema-safe.
		Name: "representation revisions and write gate",
		SQL: []string{
			`SELECT public.lock_representation_write_gate_exclusive()`,
			`DO $$
DECLARE
	trigger_record record;
BEGIN
	FOR trigger_record IN
		SELECT ns.nspname AS schema_name, rel.relname AS table_name, trg.tgname
		FROM pg_trigger AS trg
		JOIN pg_class AS rel ON rel.oid = trg.tgrelid
		JOIN pg_namespace AS ns ON ns.oid = rel.relnamespace
		JOIN pg_proc AS proc ON proc.oid = trg.tgfoid
		WHERE NOT trg.tgisinternal
		  AND ns.nspname = 'public'
		  AND proc.proname LIKE 'bump_%'
	LOOP
		EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I.%I',
			trigger_record.tgname, trigger_record.schema_name, trigger_record.table_name);
	END LOOP;
END
$$`,
			`DROP FUNCTION IF EXISTS
			 public.bump_concept_global_revision_update(),
			 public.bump_feed_folders_revision_update(),
			 public.bump_feed_items_revision_update(),
			 public.bump_feed_revision_trigger(),
			 public.bump_feed_subscriptions_revision_update(),
			 public.bump_global_revision_trigger(),
			 public.bump_library_revision_trigger(),
			 public.bump_link_concept_read_revision_update(),
			 public.bump_links_read_revision_update(),
			 public.bump_site_entries_read_revision_update(),
			 public.bump_site_tags_read_revision_update(),
			 public.bump_sites_read_revision_update()`,
			`DROP FUNCTION IF EXISTS
			 public.bump_feed_read_revision(),
			 public.bump_global_read_revision(),
			 public.bump_library_read_revision()`,
			`DROP FUNCTION IF EXISTS
			 public.lock_library_feed_revisions(),
			 public.lock_library_global_revisions(),
			 public.lock_representation_revisions(boolean, boolean, boolean)`,
			`DROP TABLE IF EXISTS
				 public.feed_read_revision,
				 public.global_read_revision,
				 public.library_read_revision`,
			dropRepresentationWriteGateSQL,
		},
	},
	{
		// Site metadata is written once when an aggregate or entry is created;
		// later automatic collection only touches timestamps. Per-field source
		// values therefore do not protect user edits. Identity source/lock and
		// grouping lock are likewise never consulted, while tag source prevents
		// the user from deleting an automatically copied tag. Keep the actual
		// profile, entry, tag, identity, and revision facts and remove this unused
		// provenance protocol.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release selects and writes every removed column. Stop old
		// Core before applying this step; rollback to it is not schema-safe.
		Name: "Site provenance",
		SQL: []string{
			`ALTER TABLE public.sites
				 DROP CONSTRAINT IF EXISTS chk_sites_optional_sources,
				 DROP CONSTRAINT IF EXISTS chk_sites_sources,
				 DROP COLUMN IF EXISTS name_source,
				 DROP COLUMN IF EXISTS intro_source,
				 DROP COLUMN IF EXISTS homepage_source,
				 DROP COLUMN IF EXISTS icon_source,
				 DROP COLUMN IF EXISTS primary_source,
				 DROP COLUMN IF EXISTS grouping_locked`,
			`ALTER TABLE public.site_entries
				 DROP CONSTRAINT IF EXISTS chk_site_entries_sources,
				 DROP COLUMN IF EXISTS entry_name_source,
				 DROP COLUMN IF EXISTS purpose_source`,
			`ALTER TABLE public.site_tags
				 DROP CONSTRAINT IF EXISTS chk_site_tags_source,
				 DROP COLUMN IF EXISTS source`,
			`ALTER TABLE public.site_identities
				 DROP CONSTRAINT IF EXISTS chk_site_identities_source,
				 DROP COLUMN IF EXISTS source,
				 DROP COLUMN IF EXISTS locked`,
		},
	},
	{
		// Category is exposed only in the Inbox editor beside the existing tags
		// field. The first-party client never categorizes Links or Notes directly,
		// and confirmation moves Inbox memberships to Link rows that no runtime
		// query ever reads. Preserve reachable names as ordinary tags, which are
		// already copied into the Library and remain searchable, then remove the
		// speculative polymorphic API and its unenforced text identity.
		//
		// Unexpected host kinds and orphan memberships abort the transaction. They
		// can only come from out-of-contract direct API/SQL use and have no lossless
		// target in the current product, so silently dropping them is not allowed.
		//
		// Online-update review: INCOMPATIBLE.
		//
		// The previous release serves and writes the Category API and selects
		// category_ids with Inbox detail rows. Stop old Core before applying this
		// step; rollback to it is not schema-safe.
		Name: "Reader Category",
		SQL: []string{
			`DO $$
DECLARE
	categories_table regclass := to_regclass('public.reader_categories');
	memberships_table regclass := to_regclass('public.reader_categorizables');
BEGIN
	IF (categories_table IS NULL) <> (memberships_table IS NULL) THEN
		RAISE EXCEPTION 'Reader Category schema is partial; both legacy tables are required for data migration';
	END IF;
	IF categories_table IS NULL THEN
		RETURN;
	END IF;

	IF EXISTS (
		SELECT 1
		FROM public.reader_categorizables
		WHERE host_kind NOT IN ('inbox','link')
	) THEN
		RAISE EXCEPTION 'Reader Category cleanup found an unsupported host kind; export and resolve it before retrying';
	END IF;

	IF EXISTS (
		SELECT 1
		FROM public.reader_categorizables AS membership
		WHERE (membership.host_kind='inbox' AND NOT EXISTS (
			SELECT 1 FROM public.reader_inbox AS inbox WHERE inbox.id::text=membership.host_id
		)) OR (membership.host_kind='link' AND NOT EXISTS (
			SELECT 1 FROM public.links AS link WHERE link.id::text=membership.host_id
		))
	) THEN
		RAISE EXCEPTION 'Reader Category cleanup found an orphan membership; resolve it before retrying';
	END IF;

	IF EXISTS (
		SELECT 1
		FROM public.reader_categorizables AS membership
		JOIN public.reader_categories AS category ON category.id=membership.category_id
		WHERE btrim(category.name)=''
	) THEN
		RAISE EXCEPTION 'Reader Category cleanup found a blank attached category name; resolve it before retrying';
	END IF;

	WITH category_tags AS (
		SELECT membership.host_id,
			array_agg(DISTINCT category.name ORDER BY category.name) AS tags
		FROM public.reader_categorizables AS membership
		JOIN public.reader_categories AS category ON category.id=membership.category_id
		WHERE membership.host_kind='inbox'
		GROUP BY membership.host_id
	), merged AS (
		SELECT inbox.id,
			ARRAY(
				SELECT DISTINCT tag
				FROM unnest(COALESCE(inbox.tags,'{}'::text[]) || category_tags.tags) AS tag
				ORDER BY tag
			) AS tags
		FROM public.reader_inbox AS inbox
		JOIN category_tags ON category_tags.host_id=inbox.id::text
	)
	UPDATE public.reader_inbox AS inbox
	SET tags=merged.tags,
		metadata_revision=CASE
			WHEN inbox.metadata_revision < 9007199254740991 THEN inbox.metadata_revision+1
			ELSE inbox.metadata_revision
		END,
		updated_at=CURRENT_TIMESTAMP
	FROM merged
	WHERE inbox.id=merged.id AND inbox.tags IS DISTINCT FROM merged.tags;

	WITH category_tags AS (
		SELECT membership.host_id,
			array_agg(DISTINCT category.name ORDER BY category.name) AS tags
		FROM public.reader_categorizables AS membership
		JOIN public.reader_categories AS category ON category.id=membership.category_id
		WHERE membership.host_kind='link'
		GROUP BY membership.host_id
	), merged AS (
		SELECT link.id,
			ARRAY(
				SELECT DISTINCT tag
				FROM unnest(COALESCE(link.tags,'{}'::text[]) || category_tags.tags) AS tag
				ORDER BY tag
			) AS tags
		FROM public.links AS link
		JOIN category_tags ON category_tags.host_id=link.id::text
	)
	UPDATE public.links AS link
	SET tags=merged.tags,updated_at=CURRENT_TIMESTAMP
	FROM merged
	WHERE link.id=merged.id AND link.tags IS DISTINCT FROM merged.tags;
END
$$`,
			`DROP TABLE IF EXISTS public.reader_categorizables`,
			`DROP TABLE IF EXISTS public.reader_categories`,
		},
	},
}

func productionUpgradeSQL() []string {
	statementCount := 1
	for _, segment := range productionUpgradeSegments {
		statementCount += len(segment.SQL)
	}
	statements := make([]string, 0, statementCount)
	for _, segment := range productionUpgradeSegments {
		statements = append(statements, segment.SQL...)
	}
	statements = append(statements, `DELETE FROM public.schema_migrations
		WHERE version IN (
			'f03e51d6911b',
			'b671c9d2e411',
			'reader2026081301',
			'integrity2026081401',
			'historical2026081401',
			'conceptaudit2026081401',
			'lifecycle2026081401',
			'readersearch2026081701',
			'readertodoprojection2026081701',
			'readerinboxdocument2026081801'
		)`)
	return statements
}

var steps = []Step{
	{
		ID: CurrentSchemaMigrationID,
		OnlineUpdate: OnlineIncompatible(
			"replaces the v0.1.17 schema and removes objects still required by that binary; stop old Core, bind a verified dump, then run the exact target",
		),
		SQL: []string{
			currentInstallSchemaSQL,
			dropRepresentationWriteGateSQL,
			`INSERT INTO public.installation_state (singleton) VALUES (true)`,
			`INSERT INTO public.feed_subscriptions (url,canonical_url,title)
			 VALUES (
				'https://www.ruanyifeng.com/blog/atom.xml',
				'https://www.ruanyifeng.com/blog/atom.xml',
				'阮一峰的网络日志'
			 )`,
		},
	},
}
