package migrate

const (
	// TranslationSourceContractMigrationID remains the startup compatibility
	// marker for translation source identity. In the public fresh-install
	// history it records the complete installation schema rather than an
	// expand/contract upgrade phase.
	TranslationSourceContractMigrationID = "f03e51d6911b"

	translationTerminalHistoryIndexMigrationID = "b671c9d2e411"
	readerThoughtTombstoneSnapshotMigrationID  = "reader2026081301"
	integrityRepairMigrationID                 = "integrity2026081401"
	historicalRepairMigrationID                = "historical2026081401"
	conceptMergeAuditRepairMigrationID         = "conceptaudit2026081401"
	lifecycleRepairMigrationID                 = "lifecycle2026081401"
)

// singleInstallSchemaSQL is the complete application-owned schema for a new
// Cairn installation. River owns its tables, types, functions, and sequences;
// maybeRunRiverMigrations installs those before this SQL runs.
const singleInstallSchemaSQL = `--
-- PostgreSQL database dump
--


-- Dumped from database version 16.14 (Debian 16.14-1.pgdg12+1)
-- Dumped by pg_dump version 16.14 (Debian 16.14-1.pgdg12+1)

SET LOCAL standard_conforming_strings = on;
SET LOCAL search_path = '';
SET LOCAL check_function_bodies = false;

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: EXTENSION pgcrypto; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


--
-- Name: vector; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;


--
-- Name: EXTENSION vector; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION vector IS 'vector data type and ivfflat and hnsw access methods';



--
-- Name: advance_link_metadata_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.advance_link_metadata_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.title IS DISTINCT FROM NEW.title OR OLD.summary IS DISTINCT FROM NEW.summary OR OLD.tags IS DISTINCT FROM NEW.tags THEN
        IF OLD.metadata_revision >= 9007199254740991 THEN
            RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='chk_links_metadata_revision_safe',
                MESSAGE='link metadata revision has reached the JavaScript-safe maximum';
        END IF;
        NEW.metadata_revision := OLD.metadata_revision + 1;
        IF OLD.title IS DISTINCT FROM NEW.title OR OLD.summary IS DISTINCT FROM NEW.summary THEN
            NEW.embedding := NULL;
            NEW.embedding_model := NULL;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: bump_concept_global_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_concept_global_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(n.primary_name,n.display_name)
          IS DISTINCT FROM ROW(o.primary_name,o.display_name)
    ) THEN PERFORM bump_global_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_feed_folders_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_feed_folders_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(n.name,n.created_at,n.updated_at)
          IS DISTINCT FROM ROW(o.name,o.created_at,o.updated_at)
    ) THEN PERFORM bump_feed_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_feed_items_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_feed_items_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(
            n.subscription_id,n.url,n.title,n.author,n.summary,n.content_text,
            n.content_html,n.published_at,n.read_at,n.starred,n.read_later,
            n.link_id,n.created_at
        ) IS DISTINCT FROM ROW(
            o.subscription_id,o.url,o.title,o.author,o.summary,o.content_text,
            o.content_html,o.published_at,o.read_at,o.starred,o.read_later,
            o.link_id,o.created_at
        )
    ) THEN PERFORM bump_feed_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_feed_read_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_feed_read_revision() RETURNS void
    LANGUAGE sql
    AS $$
    UPDATE feed_read_revision SET revision=revision+1, updated_at=now() WHERE singleton;
$$;


--
-- Name: bump_feed_revision_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_feed_revision_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
	IF TG_OP = 'INSERT' THEN
		IF EXISTS (SELECT 1 FROM new_rows) THEN
			PERFORM bump_feed_read_revision();
		END IF;
	ELSIF TG_OP = 'DELETE' THEN
		IF EXISTS (SELECT 1 FROM old_rows) THEN
			PERFORM bump_feed_read_revision();
		END IF;
	END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_feed_subscriptions_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_feed_subscriptions_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(n.folder_id,n.title,n.active)
          IS DISTINCT FROM ROW(o.folder_id,o.title,o.active)
    ) THEN PERFORM bump_feed_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_global_read_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_global_read_revision() RETURNS void
    LANGUAGE sql
    AS $$
    UPDATE global_read_revision SET revision=revision+1, updated_at=now() WHERE singleton;
$$;


--
-- Name: bump_global_revision_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_global_revision_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
	IF TG_OP = 'INSERT' THEN
		IF EXISTS (SELECT 1 FROM new_rows) THEN
			PERFORM bump_global_read_revision();
		END IF;
	ELSIF TG_OP = 'DELETE' THEN
		IF EXISTS (SELECT 1 FROM old_rows) THEN
			PERFORM bump_global_read_revision();
		END IF;
	END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_library_read_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_library_read_revision() RETURNS void
    LANGUAGE sql
    AS $$
    UPDATE library_read_revision SET revision=revision+1, updated_at=now() WHERE singleton;
$$;


--
-- Name: bump_library_revision_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_library_revision_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
	IF TG_OP = 'INSERT' THEN
		IF EXISTS (SELECT 1 FROM new_rows) THEN
			PERFORM bump_library_read_revision();
		END IF;
	ELSIF TG_OP = 'DELETE' THEN
		IF EXISTS (SELECT 1 FROM old_rows) THEN
			PERFORM bump_library_read_revision();
		END IF;
	END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_link_concept_read_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_link_concept_read_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o
          ON o.link_id=n.link_id AND o.concept_id=n.concept_id
        WHERE n.link_id IS NULL OR o.link_id IS NULL OR n.surface_tag IS DISTINCT FROM o.surface_tag
    ) THEN PERFORM bump_library_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_links_read_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_links_read_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(
            n.url,n.title,n.summary,n.tags,n.fetcher_type,n.status,n.error_msg,
            n.created_at,n.updated_at,n.domain,n.content_type,n.path_depth,
            n.parent_path,n.parent_id,n.description,n.is_low_confidence,
            n.low_confidence_reason,n.content,n.content_document,n.content_format,
            n.library_kind,n.library_kind_source,n.library_kind_locked,
            n.predicted_library_kind,n.classification_confidence,
            n.classification_reason,n.classification_explanation,n.classifier_version,
            n.content_revision,n.content_source,n.content_cjk_chars,n.content_words
        ) IS DISTINCT FROM ROW(
            o.url,o.title,o.summary,o.tags,o.fetcher_type,o.status,o.error_msg,
            o.created_at,o.updated_at,o.domain,o.content_type,o.path_depth,
            o.parent_path,o.parent_id,o.description,o.is_low_confidence,
            o.low_confidence_reason,o.content,o.content_document,o.content_format,
            o.library_kind,o.library_kind_source,o.library_kind_locked,
            o.predicted_library_kind,o.classification_confidence,
            o.classification_reason,o.classification_explanation,o.classifier_version,
            o.content_revision,o.content_source,o.content_cjk_chars,o.content_words
        )
    ) THEN
        PERFORM bump_library_read_revision();
    END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_site_entries_read_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_site_entries_read_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(n.site_id,n.normalized_url)
          IS DISTINCT FROM ROW(o.site_id,o.normalized_url)
    ) THEN PERFORM bump_library_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_site_tags_read_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_site_tags_read_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o
          ON o.site_id=n.site_id AND o.normalized_tag=n.normalized_tag
        WHERE n.site_id IS NULL OR o.site_id IS NULL OR n.tag IS DISTINCT FROM o.tag
    ) THEN PERFORM bump_library_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: bump_sites_read_revision_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.bump_sites_read_revision_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM new_rows n FULL JOIN old_rows o ON o.id=n.id
        WHERE n.id IS NULL OR o.id IS NULL OR ROW(
            n.site_key,n.name,n.intro,n.homepage_url,n.icon_url,n.pinned,
            n.primary_entry_id,n.needs_review,n.revision,n.first_collected_at,
            n.last_collected_at,n.updated_at
        ) IS DISTINCT FROM ROW(
            o.site_key,o.name,o.intro,o.homepage_url,o.icon_url,o.pinned,
            o.primary_entry_id,o.needs_review,o.revision,o.first_collected_at,
            o.last_collected_at,o.updated_at
        )
    ) THEN PERFORM bump_library_read_revision(); END IF;
    RETURN NULL;
END;
$$;


--
-- Name: guard_representation_write_gate(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.guard_representation_write_gate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NOT pg_try_advisory_xact_lock_shared(4, 0) THEN
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
END;
$$;


--
-- Name: lock_library_feed_revisions(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.lock_library_feed_revisions() RETURNS void
    LANGUAGE sql
    AS $$
    SELECT lock_representation_revisions(false, true, true);
$$;


--
-- Name: lock_library_global_revisions(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.lock_library_global_revisions() RETURNS void
    LANGUAGE sql
    AS $$
    SELECT lock_representation_revisions(true, true, false);
$$;


--
-- Name: lock_representation_revisions(boolean, boolean, boolean); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.lock_representation_revisions(lock_global boolean, lock_library boolean, lock_feed boolean) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM lock_representation_write_gate_exclusive();
    IF lock_global THEN
        PERFORM revision FROM global_read_revision WHERE singleton FOR UPDATE;
    END IF;
    IF lock_library THEN
        PERFORM revision FROM library_read_revision WHERE singleton FOR UPDATE;
    END IF;
    IF lock_feed THEN
        PERFORM revision FROM feed_read_revision WHERE singleton FOR UPDATE;
    END IF;
END;
$$;


--
-- Name: lock_representation_write_gate_exclusive(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.lock_representation_write_gate_exclusive() RETURNS void
    LANGUAGE sql
    AS $$
    SELECT pg_advisory_xact_lock(4, 0);
$$;


--
-- Name: lock_representation_write_gate_shared(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.lock_representation_write_gate_shared() RETURNS void
    LANGUAGE sql
    AS $$
    SELECT pg_advisory_xact_lock_shared(4, 0);
$$;


--
-- Name: reader_capture_content_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_capture_content_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.content IS DISTINCT FROM NEW.content OR OLD.content_document IS DISTINCT FROM NEW.content_document OR OLD.content_revision IS DISTINCT FROM NEW.content_revision THEN
        INSERT INTO reader_content_history (link_id,revision,content,content_document,content_format,content_source)
        VALUES (OLD.id,OLD.content_revision,OLD.content,OLD.content_document,OLD.content_format,OLD.content_source)
        ON CONFLICT (link_id,revision) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_cleanup_content_history(integer, integer); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_cleanup_content_history(p_batch_size integer DEFAULT 100, p_keep_per_link integer DEFAULT 20) RETURNS integer
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
    bounded_batch integer := LEAST(GREATEST(COALESCE(p_batch_size,100),1),1000);
    bounded_keep integer := LEAST(GREATEST(COALESCE(p_keep_per_link,20),1),100);
    deleted_count integer;
BEGIN
    WITH ranked AS (
        SELECT h.id,h.revision,
            ROW_NUMBER() OVER (PARTITION BY h.link_id ORDER BY h.revision DESC) AS history_rank,
            MIN(h.revision) OVER (PARTITION BY h.link_id) AS oldest_revision
        FROM reader_content_history h
        JOIN links l ON l.id=h.link_id
        WHERE h.revision < l.content_revision
    ), candidates AS (
        SELECT id,revision,oldest_revision FROM ranked
        WHERE history_rank > bounded_keep AND revision <> oldest_revision
        ORDER BY revision ASC,id ASC LIMIT bounded_batch
    )
    DELETE FROM reader_content_history h USING candidates WHERE h.id=candidates.id;
    GET DIAGNOSTICS deleted_count=ROW_COUNT;
    RETURN deleted_count;
END;
$$;


--
-- Name: reader_enforce_user_deleted_thought(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_enforce_user_deleted_thought() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.user_deleted := NEW.deleted OR NEW.user_deleted OR OLD.user_deleted;
    ELSE
        NEW.user_deleted := NEW.deleted OR NEW.user_deleted;
    END IF;
    IF NEW.user_deleted THEN
        NEW.user_deleted := true;
        IF TG_OP = 'UPDATE' AND OLD.user_deleted_at IS NOT NULL THEN
            NEW.user_deleted_at := OLD.user_deleted_at;
        ELSE
            NEW.user_deleted_at := COALESCE(NEW.user_deleted_at, CURRENT_TIMESTAMP);
        END IF;
        NEW.deleted := true;
        NEW.body := '';
        NEW.target := '{}'::jsonb;
        NEW.quote := NULL;
        NEW.source := '';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_scrub_user_deleted_thought(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_scrub_user_deleted_thought() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    IF NEW.user_deleted THEN
        UPDATE reader_thought_ops
        SET target = '{}'::jsonb, payload = '{}'::jsonb
        WHERE annotation_id = NEW.id
          AND (target IS DISTINCT FROM '{}'::jsonb OR payload IS DISTINCT FROM '{}'::jsonb);
        UPDATE reader_thought_supersession_events
        SET loser = jsonb_build_object('type','user_deleted','id',NEW.id,'user_deleted',true),
            winner_at_detection = jsonb_build_object('type','user_deleted','id',NEW.id,'user_deleted',true)
        WHERE annotation_id = NEW.id;
        UPDATE reader_thought_tombstones
        SET reason = 'user_deleted',
            snapshot = jsonb_build_object(
            'snapshot_version', 1,
            'id', NEW.id,
            'host_kind', NEW.host_kind,
            'host_id', NEW.host_id,
            'type', 'user_deleted',
            'user_deleted', true,
            'deleted_at', NEW.user_deleted_at)
        WHERE thought_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_scrub_user_deleted_thought_op(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_scrub_user_deleted_thought_op() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM reader_thoughts WHERE id = NEW.annotation_id AND user_deleted) THEN
        NEW.target := '{}'::jsonb;
        NEW.payload := '{}'::jsonb;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_scrub_user_deleted_thought_event(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_scrub_user_deleted_thought_event() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM reader_thoughts WHERE id = NEW.annotation_id AND user_deleted) THEN
        NEW.loser := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
        NEW.winner_at_detection := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_protect_user_deleted_thought_tombstone(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_protect_user_deleted_thought_tombstone() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
    terminal_deleted_at timestamp with time zone;
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM reader_thoughts WHERE id = OLD.thought_id AND user_deleted) THEN
            RETURN NULL;
        END IF;
        RETURN OLD;
    END IF;
    SELECT user_deleted_at INTO terminal_deleted_at
    FROM reader_thoughts WHERE id = NEW.thought_id AND user_deleted;
    IF FOUND THEN
        NEW.reason := 'user_deleted';
        NEW.snapshot := jsonb_build_object(
            'snapshot_version',1,'id',NEW.thought_id,'host_kind',NEW.host_kind,'host_id',NEW.host_id,
            'type','user_deleted','user_deleted',true,'deleted_at',terminal_deleted_at);
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reader_tombstone_deleted_link_thoughts(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_tombstone_deleted_link_thoughts() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
    SELECT thought.id,thought.host_kind,thought.host_id,'link_deleted',
        jsonb_build_object(
            'snapshot_version',1,
            'id',thought.id,'host_kind',thought.host_kind,'host_id',thought.host_id,'link_id',thought.link_id,
            'type','thought','body',thought.body,'target',thought.target,'quote',thought.quote,'source',thought.source,
            'created_at',thought.created_at,'updated_at',thought.updated_at,
            'original_host_snapshot',to_jsonb(COALESCE(OLD.content_document,OLD.content,'')),
            'original_host_identity',jsonb_build_object('kind','link','id',OLD.id,'url',OLD.url,'content_revision',OLD.content_revision),
            'frozen_at',CURRENT_TIMESTAMP)
    FROM reader_thoughts thought
    WHERE thought.host_kind='link' AND thought.host_id=OLD.id::text AND thought.deleted=false
    ON CONFLICT (thought_id) DO NOTHING;
    RETURN OLD;
END;
$$;


--
-- Name: reader_tombstone_trashed_link_thoughts(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_tombstone_trashed_link_thoughts() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
    SELECT thought.id,thought.host_kind,thought.host_id,'link_deleted',
        jsonb_build_object(
            'snapshot_version',1,
            'id',thought.id,'host_kind',thought.host_kind,'host_id',thought.host_id,'link_id',thought.link_id,
            'type','thought','body',thought.body,'target',thought.target,'quote',thought.quote,'source',thought.source,
            'created_at',thought.created_at,'updated_at',thought.updated_at,
            'original_host_snapshot',to_jsonb(COALESCE(NEW.content_document,NEW.content,'')),
            'original_host_identity',jsonb_build_object('kind','link','id',NEW.id,'url',NEW.url,'content_revision',NEW.content_revision),
            'frozen_at',CURRENT_TIMESTAMP)
    FROM reader_thoughts thought
    WHERE thought.host_kind='link' AND thought.host_id=NEW.id::text AND thought.deleted=false
    ON CONFLICT (thought_id) DO NOTHING;
    RETURN NEW;
END;
$$;





--
-- Name: concept; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.concept (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    primary_name text NOT NULL,
    wikidata_qid text,
    use_count integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    display_name text,
    embedding public.vector(1536),
    embedding_model text
);


--
-- Name: concept_alias; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.concept_alias (
    alias text NOT NULL,
    concept_id uuid NOT NULL,
    lang text,
    source text NOT NULL,
    confidence real,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: concept_merge_proposal; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.concept_merge_proposal (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    winner_id uuid NOT NULL,
    loser_id uuid NOT NULL,
    score real NOT NULL,
    llm_reason text,
    status text DEFAULT 'pending'::text NOT NULL,
    decided_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    decided_at timestamp with time zone,
    CONSTRAINT chk_merge_proposal_distinct CHECK ((winner_id <> loser_id)),
    CONSTRAINT chk_merge_proposal_decision_audit CHECK ((((status = 'pending'::text) AND (decided_by IS NULL) AND (decided_at IS NULL)) OR ((status = ANY (ARRAY['approved'::text, 'rejected'::text])) AND (decided_by IS NOT NULL) AND (btrim(decided_by) <> ''::text) AND (decided_at IS NOT NULL)))),
    CONSTRAINT concept_merge_proposal_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text])))
);


--
-- Name: feed_folders; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.feed_folders (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_feed_folders_name CHECK (((char_length(name) >= 1) AND (char_length(name) <= 128)))
);


--
-- Name: feed_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.feed_items (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    subscription_id uuid NOT NULL,
    external_id text NOT NULL,
    url text DEFAULT ''::text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    author text,
    summary text,
    content_text text,
    content_html text,
    published_at timestamp with time zone,
    read_at timestamp with time zone,
    starred boolean DEFAULT false NOT NULL,
    read_later boolean DEFAULT false NOT NULL,
    link_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: feed_read_revision; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.feed_read_revision (
    singleton boolean DEFAULT true NOT NULL,
    revision bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT feed_read_revision_revision_check CHECK ((revision >= 0)),
    CONSTRAINT feed_read_revision_singleton_check CHECK (singleton)
);


--
-- Name: feed_subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.feed_subscriptions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    folder_id uuid,
    url text NOT NULL,
    site_url text,
    title text NOT NULL,
    description text,
    active boolean DEFAULT true NOT NULL,
    etag text,
    last_modified text,
    sync_boundary_external_id text,
    last_fetched_at timestamp with time zone,
    last_success_at timestamp with time zone,
    next_fetch_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    failure_count integer DEFAULT 0 NOT NULL,
    refresh_claim_token uuid,
    refresh_claimed_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    canonical_url text,
    CONSTRAINT chk_feed_subscriptions_failure_count CHECK ((failure_count >= 0)),
    CONSTRAINT chk_feed_subscriptions_url CHECK ((((char_length(url) >= 1) AND (char_length(url) <= 2048)) AND (url ~ '^https?://'::text)))
);


--
-- Name: global_read_revision; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.global_read_revision (
    singleton boolean DEFAULT true NOT NULL,
    revision bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT global_read_revision_revision_check CHECK ((revision >= 0)),
    CONSTRAINT global_read_revision_singleton_check CHECK (singleton)
);


--
-- Name: idempotency_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.idempotency_keys (
    key text NOT NULL,
    status integer DEFAULT 0 NOT NULL,
    body bytea,
    content_type text DEFAULT ''::text NOT NULL,
    in_flight boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    owner_token text DEFAULT (gen_random_uuid())::text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    CONSTRAINT chk_idempotency_keys_generation CHECK ((generation >= 1))
);


--
-- Name: installation_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.installation_state (
    singleton boolean DEFAULT true NOT NULL,
    representation_namespace uuid DEFAULT gen_random_uuid() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT installation_state_singleton_check CHECK (singleton)
);


--
-- Name: library_classification_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.library_classification_rules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    host text NOT NULL,
    identity_adapter text,
    path_prefix text,
    target_kind text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_library_classification_rule_host CHECK (((char_length(host) >= 1) AND (char_length(host) <= 253))),
    CONSTRAINT chk_library_classification_rule_path CHECK (((path_prefix IS NULL) OR ((char_length(path_prefix) >= 1) AND (char_length(path_prefix) <= 2048)))),
    CONSTRAINT chk_library_classification_rule_target CHECK ((target_kind = ANY (ARRAY['reading'::text, 'site'::text])))
);


--
-- Name: library_read_revision; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.library_read_revision (
    singleton boolean DEFAULT true NOT NULL,
    revision bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT library_read_revision_revision_check CHECK ((revision >= 0)),
    CONSTRAINT library_read_revision_singleton_check CHECK (singleton)
);


--
-- Name: library_review_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.library_review_items (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    kind text NOT NULL,
    link_id uuid,
    site_id uuid,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    revision bigint DEFAULT 1 NOT NULL,
    CONSTRAINT chk_library_review_kind CHECK ((kind = ANY (ARRAY['classification_uncertain'::text, 'migration_suggestion'::text, 'note_conflict'::text, 'merge_conflict'::text]))),
    CONSTRAINT chk_library_review_payload_object CHECK ((jsonb_typeof(payload) = 'object'::text)),
    CONSTRAINT chk_library_review_status CHECK ((status = ANY (ARRAY['pending'::text, 'applied'::text, 'dismissed'::text])))
);


--
-- Name: link_concept; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.link_concept (
    link_id uuid NOT NULL,
    concept_id uuid NOT NULL,
    surface_tag text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: link_translations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.link_translations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    link_id uuid NOT NULL,
    scope text NOT NULL,
    block_key text NOT NULL,
    start_offset integer NOT NULL,
    end_offset integer NOT NULL,
    source_text text NOT NULL,
    translated_text text,
    source_format text DEFAULT 'plain'::text NOT NULL,
    target_language text DEFAULT 'zh-CN'::text NOT NULL,
    source_hash text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    model text,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    attempt_generation bigint DEFAULT 0 NOT NULL,
    current_river_job_id bigint,
    source_content_revision bigint,
    CONSTRAINT chk_link_translations_attempt_generation CHECK ((attempt_generation >= 0)),
    CONSTRAINT chk_link_translations_format CHECK ((source_format = ANY (ARRAY['plain'::text, 'markdown'::text]))),
    CONSTRAINT chk_link_translations_offsets CHECK (((start_offset >= 0) AND (end_offset > start_offset))),
    CONSTRAINT chk_link_translations_scope CHECK ((scope = ANY (ARRAY['selection'::text, 'full'::text]))),
    CONSTRAINT chk_link_translations_source_content_revision CHECK (((source_content_revision IS NULL) OR (source_content_revision > 0))),
    CONSTRAINT chk_link_translations_source_hash CHECK ((char_length(source_hash) = 64)),
    CONSTRAINT chk_link_translations_status CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'done'::text, 'failed'::text]))),
    CONSTRAINT chk_link_translations_target CHECK ((target_language = 'zh-CN'::text))
);


--
-- Name: link_url_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.link_url_identities (
    normalized_url text NOT NULL,
    link_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.links (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    url text NOT NULL,
    title text,
    summary text,
    tags text[] DEFAULT '{}'::text[] NOT NULL,
    fetcher_type text,
    status text DEFAULT 'pending'::text NOT NULL,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    domain text,
    content_type text DEFAULT 'unknown'::text,
    path_depth integer,
    parent_path text,
    parent_id uuid,
    description text,
    is_low_confidence boolean DEFAULT false NOT NULL,
    low_confidence_reason text,
    source_kind text DEFAULT 'url'::text NOT NULL,
    source_key text NOT NULL,
    input_title text,
    input_text text,
    input_html text,
    input_images jsonb,
    source_metadata jsonb,
    embedding public.vector(1536),
    embedding_model text,
    content text,
    content_document text,
    content_format text DEFAULT 'plain'::text NOT NULL,
    library_kind text,
    library_kind_source text,
    library_kind_locked boolean DEFAULT false NOT NULL,
    predicted_library_kind text,
    classification_confidence real,
    classification_reason text,
    classification_explanation text,
    classifier_version text,
    content_revision bigint DEFAULT 1 NOT NULL,
    first_collected_at timestamp with time zone NOT NULL,
    last_recollected_at timestamp with time zone,
    payload_purge_due_at timestamp with time zone,
    payload_purged_at timestamp with time zone,
    has_content boolean GENERATED ALWAYS AS ((content IS NOT NULL)) STORED,
    content_cjk_chars integer DEFAULT 0 NOT NULL,
    content_words integer DEFAULT 0 NOT NULL,
    content_source text DEFAULT 'fetched'::text NOT NULL,
    metadata_revision bigint DEFAULT 1 NOT NULL,
    deleted_at timestamp with time zone,
    feed_managed boolean DEFAULT false NOT NULL,
    requested_library_kind text DEFAULT 'auto'::text NOT NULL,
    requested_library_kind_source text DEFAULT 'auto'::text NOT NULL,
    CONSTRAINT chk_links_classification_confidence CHECK (((classification_confidence IS NULL) OR ((classification_confidence >= (0)::double precision) AND (classification_confidence <= (1)::double precision)))),
    CONSTRAINT chk_links_content_format CHECK ((content_format = ANY (ARRAY['plain'::text, 'markdown'::text, 'html'::text]))),
    CONSTRAINT chk_links_content_revision CHECK ((content_revision > 0)),
    CONSTRAINT chk_links_content_source CHECK ((content_source = ANY (ARRAY['fetched'::text, 'user'::text]))),
    CONSTRAINT chk_links_input_images_array CHECK (((input_images IS NULL) OR (jsonb_typeof(input_images) = 'array'::text))),
    CONSTRAINT chk_links_library_kind CHECK (((library_kind IS NULL) OR (library_kind = ANY (ARRAY['reading'::text, 'site'::text])))),
    CONSTRAINT chk_links_library_kind_pair CHECK (((library_kind IS NULL) = (library_kind_source IS NULL))),
    CONSTRAINT chk_links_library_kind_source CHECK (((library_kind_source IS NULL) OR (library_kind_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text])))),
    CONSTRAINT chk_links_metadata_revision_safe CHECK (((metadata_revision >= 1) AND (metadata_revision <= '9007199254740991'::bigint))),
    CONSTRAINT chk_links_predicted_library_kind CHECK (((predicted_library_kind IS NULL) OR (predicted_library_kind = ANY (ARRAY['reading'::text, 'site'::text])))),
    CONSTRAINT chk_links_requested_library_intent CHECK (((requested_library_kind_source <> 'user'::text) OR (requested_library_kind = ANY (ARRAY['reading'::text, 'site'::text])))),
    CONSTRAINT chk_links_requested_library_kind CHECK ((requested_library_kind = ANY (ARRAY['auto'::text, 'reading'::text, 'site'::text]))),
    CONSTRAINT chk_links_requested_library_kind_source CHECK ((requested_library_kind_source = ANY (ARRAY['auto'::text, 'user'::text]))),
    CONSTRAINT chk_links_site_has_no_content CHECK (((library_kind <> 'site'::text) OR ((summary IS NULL) AND (content IS NULL) AND (content_document IS NULL)))),
    CONSTRAINT chk_links_status CHECK ((status = ANY (ARRAY['skeleton'::text, 'pending'::text, 'processing'::text, 'done'::text, 'failed'::text])))
);


--
-- Name: parse_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.parse_jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    link_id uuid NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    error_msg text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expected_metadata_revision bigint DEFAULT 0 NOT NULL,
    CONSTRAINT chk_parse_jobs_expected_metadata_revision CHECK ((expected_metadata_revision >= 0)),
    CONSTRAINT chk_parse_jobs_status CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'done'::text, 'failed'::text])))
);


--
-- Name: reader_categories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_categories (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: reader_categorizables; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_categorizables (
    category_id uuid NOT NULL,
    host_kind text NOT NULL,
    host_id text NOT NULL
);


--
-- Name: reader_content_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_content_history (
    id bigint NOT NULL,
    link_id uuid NOT NULL,
    revision bigint NOT NULL,
    content text,
    content_document text,
    content_format text DEFAULT 'plain'::text NOT NULL,
    content_source text DEFAULT 'fetched'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_content_history_content_format_check CHECK ((content_format = ANY (ARRAY['plain'::text, 'markdown'::text, 'html'::text]))),
    CONSTRAINT reader_content_history_content_source_check CHECK ((content_source = ANY (ARRAY['fetched'::text, 'user'::text]))),
    CONSTRAINT reader_content_history_revision_check CHECK ((revision > 0))
);


--
-- Name: reader_content_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.reader_content_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: reader_content_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.reader_content_history_id_seq OWNED BY public.reader_content_history.id;


--
-- Name: reader_domain_activity; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_domain_activity (
    domain text NOT NULL,
    last_at timestamp with time zone NOT NULL,
    last_link_id uuid
);


--
-- Name: reader_engagement; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_engagement (
    link_id uuid NOT NULL,
    read boolean DEFAULT false NOT NULL,
    progress real DEFAULT 0 NOT NULL,
    read_later boolean DEFAULT false NOT NULL,
    last_opened timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_engagement_progress_check CHECK (((progress >= (0)::double precision) AND (progress <= (1)::double precision)))
);


--
-- Name: reader_feed_feedback; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_feed_feedback (
    item_key text NOT NULL,
    action text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_feed_feedback_action_check CHECK ((action = ANY (ARRAY['not_interested'::text, 'hide'::text, 'save'::text, 'unsave'::text])))
);


--
-- Name: reader_feed_saves; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_feed_saves (
    feed_item_id uuid NOT NULL,
    link_id uuid NOT NULL,
    created_link boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: reader_feed_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_feed_snapshots (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    mode text NOT NULL,
    items jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_feed_snapshots_mode_check CHECK ((mode = ANY (ARRAY['recommended'::text, 'chronological'::text])))
);


--
-- Name: reader_host_purge_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_host_purge_receipts (
    host_kind text NOT NULL,
    host_id uuid NOT NULL,
    operation_id uuid NOT NULL,
    outcome text NOT NULL,
    CONSTRAINT reader_host_purge_receipts_host_kind_check CHECK ((host_kind = ANY (ARRAY['link'::text, 'inbox'::text, 'note'::text]))),
    CONSTRAINT reader_host_purge_receipts_outcome_check CHECK ((outcome = 'purged'::text))
);


--
-- Name: reader_inbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_inbox (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    url text NOT NULL,
    identity_key text,
    source_kind text DEFAULT 'url'::text NOT NULL,
    title text,
    body text DEFAULT ''::text NOT NULL,
    summary text,
    suggested_tags text[] DEFAULT '{}'::text[] NOT NULL,
    tags text[] DEFAULT '{}'::text[] NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    metadata_revision bigint DEFAULT 1 NOT NULL,
    job_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone DEFAULT (now() + '30 days'::interval),
    expired_at timestamp with time zone,
    expiry_lease_id uuid,
    expiry_lease_until timestamp with time zone,
    deleted_at timestamp with time zone,
    note text DEFAULT ''::text NOT NULL,
    proposal_signals jsonb DEFAULT '{}'::jsonb NOT NULL,
    proposal_status text DEFAULT 'pending'::text NOT NULL,
    CONSTRAINT reader_inbox_proposal_status_check CHECK ((proposal_status = ANY (ARRAY['pending'::text, 'running'::text, 'completed'::text, 'failed'::text]))),
    CONSTRAINT reader_inbox_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'confirmed'::text, 'discarded'::text])))
);


--
-- Name: reader_inbox_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_inbox_jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    inbox_id uuid NOT NULL,
    expected_metadata_revision bigint NOT NULL,
    status text DEFAULT 'queued'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    CONSTRAINT reader_inbox_jobs_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT reader_inbox_jobs_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'completed'::text, 'failed'::text])))
);


--
-- Name: reader_note_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_note_history (
    id bigint NOT NULL,
    note_id uuid NOT NULL,
    revision bigint NOT NULL,
    title text NOT NULL,
    content text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    reanchor_ops jsonb DEFAULT '[]'::jsonb NOT NULL
);


--
-- Name: reader_note_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.reader_note_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: reader_note_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.reader_note_history_id_seq OWNED BY public.reader_note_history.id;


--
-- Name: reader_notes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_notes (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    title text DEFAULT '未命名笔记'::text NOT NULL,
    published_content text DEFAULT ''::text NOT NULL,
    published_revision bigint DEFAULT 0 NOT NULL,
    draft_content text,
    draft_revision bigint DEFAULT 0 NOT NULL,
    draft_updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: reader_tag_activity; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_tag_activity (
    tag text NOT NULL,
    last_at timestamp with time zone NOT NULL,
    last_link_id uuid
);


--
-- Name: reader_thought_ops; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_thought_ops (
    sequence bigint NOT NULL,
    op_id text NOT NULL,
    device_id text NOT NULL,
    operation_kind text NOT NULL,
    annotation_id text NOT NULL,
    host_kind text NOT NULL,
    host_id text NOT NULL,
    target jsonb NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    logical_clock bigint DEFAULT 0 NOT NULL,
    recovery_of jsonb,
    expected_winner_key jsonb,
    CONSTRAINT reader_thought_ops_logical_clock_check CHECK (((logical_clock >= 0) AND (logical_clock <= '9007199254740991'::bigint))),
    CONSTRAINT reader_thought_ops_operation_kind_check CHECK ((operation_kind = ANY (ARRAY['add'::text, 'update'::text, 'delete'::text])))
);


--
-- Name: reader_thought_ops_sequence_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.reader_thought_ops_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: reader_thought_ops_sequence_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.reader_thought_ops_sequence_seq OWNED BY public.reader_thought_ops.sequence;


--
-- Name: reader_thought_supersession_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_thought_supersession_events (
    sequence bigint NOT NULL,
    annotation_id text NOT NULL,
    loser_sequence bigint NOT NULL,
    winner_sequence bigint NOT NULL,
    loser jsonb NOT NULL,
    winner_at_detection jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: reader_thought_supersession_events_sequence_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.reader_thought_supersession_events_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: reader_thought_supersession_events_sequence_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.reader_thought_supersession_events_sequence_seq OWNED BY public.reader_thought_supersession_events.sequence;


--
-- Name: reader_thought_tombstones; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_thought_tombstones (
    thought_id text NOT NULL,
    host_kind text NOT NULL,
    host_id text NOT NULL,
    reason text NOT NULL,
    snapshot jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_thought_tombstones_host_id_check CHECK ((btrim(host_id) <> ''::text)),
    CONSTRAINT reader_thought_tombstones_host_kind_check CHECK ((host_kind = ANY (ARRAY['link'::text, 'inbox'::text, 'note'::text]))),
    CONSTRAINT reader_thought_tombstones_reason_check CHECK ((btrim(reason) <> ''::text)),
    CONSTRAINT reader_thought_tombstones_snapshot_check CHECK ((jsonb_typeof(snapshot) = 'object'::text))
);


--
-- Name: reader_thoughts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_thoughts (
    id text NOT NULL,
    host_kind text NOT NULL,
    host_id text NOT NULL,
    link_id uuid,
    target jsonb NOT NULL,
    quote jsonb,
    body text DEFAULT ''::text NOT NULL,
    source text DEFAULT 'user'::text NOT NULL,
    deleted boolean DEFAULT false NOT NULL,
    last_sequence bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    winner_logical_clock bigint DEFAULT 0 NOT NULL,
    winner_device_id text DEFAULT ''::text NOT NULL,
    winner_op_id text DEFAULT ''::text NOT NULL,
    user_deleted boolean DEFAULT false NOT NULL,
    user_deleted_at timestamp with time zone,
    CONSTRAINT chk_reader_thoughts_user_deleted_at CHECK (((NOT user_deleted) OR (user_deleted_at IS NOT NULL))),
    CONSTRAINT chk_reader_thoughts_user_deleted_content CHECK (((NOT user_deleted) OR (deleted AND (body = ''::text) AND (target = '{}'::jsonb) AND (quote IS NULL) AND (source = ''::text)))),
    CONSTRAINT reader_thoughts_winner_logical_clock_check CHECK (((winner_logical_clock >= 0) AND (winner_logical_clock <= '9007199254740991'::bigint)))
);


--
-- Name: reader_todos; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_todos (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    text text NOT NULL,
    due_at timestamp with time zone,
    done boolean DEFAULT false NOT NULL,
    origin_kind text NOT NULL,
    origin_host_kind text,
    origin_host_id text,
    origin_ref jsonb,
    host_revision bigint DEFAULT 0 NOT NULL,
    completed_at timestamp with time zone,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT reader_todos_origin_kind_check CHECK ((origin_kind = ANY (ARRAY['standalone'::text, 'thought'::text, 'note'::text])))
);



--
-- Name: site_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_entries (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    site_id uuid NOT NULL,
    link_id uuid NOT NULL,
    entry_name text NOT NULL,
    entry_name_source text NOT NULL,
    purpose text DEFAULT ''::text NOT NULL,
    purpose_source text NOT NULL,
    normalized_url text NOT NULL,
    first_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_recollected_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_site_entries_lengths CHECK ((((char_length(entry_name) >= 1) AND (char_length(entry_name) <= 256)) AND (char_length(purpose) <= 1000))),
    CONSTRAINT chk_site_entries_sources CHECK (((entry_name_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text])) AND (purpose_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text]))))
);


--
-- Name: site_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_identities (
    identity_key text NOT NULL,
    site_id uuid NOT NULL,
    source text NOT NULL,
    locked boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_site_identities_source CHECK ((source = ANY (ARRAY['auto'::text, 'manual_merge'::text, 'manual_split'::text, 'migration'::text])))
);


--
-- Name: site_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_tags (
    site_id uuid NOT NULL,
    tag text NOT NULL,
    normalized_tag text NOT NULL,
    source text NOT NULL,
    concept_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_site_tags_lengths CHECK ((((char_length(tag) >= 1) AND (char_length(tag) <= 128)) AND ((char_length(normalized_tag) >= 1) AND (char_length(normalized_tag) <= 128)))),
    CONSTRAINT chk_site_tags_source CHECK ((source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text])))
);


--
-- Name: sites; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sites (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    site_key text NOT NULL,
    name text NOT NULL,
    name_source text NOT NULL,
    intro text DEFAULT ''::text NOT NULL,
    intro_source text NOT NULL,
    homepage_url text,
    homepage_source text,
    icon_url text,
    icon_source text,
    user_note text DEFAULT ''::text NOT NULL,
    pinned boolean DEFAULT false NOT NULL,
    primary_entry_id uuid,
    primary_source text DEFAULT 'auto'::text NOT NULL,
    grouping_locked boolean DEFAULT false NOT NULL,
    needs_review boolean DEFAULT false NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    first_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    embedding public.vector(1536),
    embedding_model text,
    CONSTRAINT chk_sites_lengths CHECK ((((char_length(name) >= 1) AND (char_length(name) <= 256)) AND (char_length(intro) <= 1000) AND (char_length(user_note) <= 10000))),
    CONSTRAINT chk_sites_optional_sources CHECK ((((homepage_source IS NULL) OR (homepage_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text]))) AND ((icon_source IS NULL) OR (icon_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text]))))),
    CONSTRAINT chk_sites_revision CHECK ((revision > 0)),
    CONSTRAINT chk_sites_sources CHECK (((name_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text])) AND (intro_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text])) AND (primary_source = ANY (ARRAY['auto'::text, 'user'::text, 'migration'::text]))))
);


--
-- Name: reader_content_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_content_history ALTER COLUMN id SET DEFAULT nextval('public.reader_content_history_id_seq'::regclass);


--
-- Name: reader_note_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_note_history ALTER COLUMN id SET DEFAULT nextval('public.reader_note_history_id_seq'::regclass);


--
-- Name: reader_thought_ops sequence; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_ops ALTER COLUMN sequence SET DEFAULT nextval('public.reader_thought_ops_sequence_seq'::regclass);


--
-- Name: reader_thought_supersession_events sequence; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_supersession_events ALTER COLUMN sequence SET DEFAULT nextval('public.reader_thought_supersession_events_sequence_seq'::regclass);


--
-- Name: concept_alias concept_alias_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.concept_alias
    ADD CONSTRAINT concept_alias_pkey PRIMARY KEY (alias, concept_id);


--
-- Name: concept_merge_proposal concept_merge_proposal_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.concept_merge_proposal
    ADD CONSTRAINT concept_merge_proposal_pkey PRIMARY KEY (id);


--
-- Name: concept concept_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.concept
    ADD CONSTRAINT concept_pkey PRIMARY KEY (id);


--
-- Name: feed_folders feed_folders_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_folders
    ADD CONSTRAINT feed_folders_pkey PRIMARY KEY (id);


--
-- Name: feed_items feed_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_items
    ADD CONSTRAINT feed_items_pkey PRIMARY KEY (id);


--
-- Name: feed_read_revision feed_read_revision_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_read_revision
    ADD CONSTRAINT feed_read_revision_pkey PRIMARY KEY (singleton);


--
-- Name: feed_subscriptions feed_subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_subscriptions
    ADD CONSTRAINT feed_subscriptions_pkey PRIMARY KEY (id);


--
-- Name: global_read_revision global_read_revision_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.global_read_revision
    ADD CONSTRAINT global_read_revision_pkey PRIMARY KEY (singleton);


--
-- Name: idempotency_keys idempotency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key);


--
-- Name: installation_state installation_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.installation_state
    ADD CONSTRAINT installation_state_pkey PRIMARY KEY (singleton);


--
-- Name: installation_state installation_state_representation_namespace_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.installation_state
    ADD CONSTRAINT installation_state_representation_namespace_key UNIQUE (representation_namespace);


--
-- Name: library_classification_rules library_classification_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.library_classification_rules
    ADD CONSTRAINT library_classification_rules_pkey PRIMARY KEY (id);


--
-- Name: library_read_revision library_read_revision_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.library_read_revision
    ADD CONSTRAINT library_read_revision_pkey PRIMARY KEY (singleton);


--
-- Name: library_review_items library_review_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.library_review_items
    ADD CONSTRAINT library_review_items_pkey PRIMARY KEY (id);


--
-- Name: link_concept link_concept_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_concept
    ADD CONSTRAINT link_concept_pkey PRIMARY KEY (link_id, concept_id);


--
-- Name: link_translations link_translations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_translations
    ADD CONSTRAINT link_translations_pkey PRIMARY KEY (id);


--
-- Name: link_url_identities link_url_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_url_identities
    ADD CONSTRAINT link_url_identities_pkey PRIMARY KEY (normalized_url);


--
-- Name: links links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.links
    ADD CONSTRAINT links_pkey PRIMARY KEY (id);


--
-- Name: parse_jobs parse_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.parse_jobs
    ADD CONSTRAINT parse_jobs_pkey PRIMARY KEY (id);


--
-- Name: reader_categories reader_categories_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_categories
    ADD CONSTRAINT reader_categories_name_key UNIQUE (name);


--
-- Name: reader_categories reader_categories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_categories
    ADD CONSTRAINT reader_categories_pkey PRIMARY KEY (id);


--
-- Name: reader_categorizables reader_categorizables_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_categorizables
    ADD CONSTRAINT reader_categorizables_pkey PRIMARY KEY (category_id, host_kind, host_id);


--
-- Name: reader_content_history reader_content_history_link_revision_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_content_history
    ADD CONSTRAINT reader_content_history_link_revision_key UNIQUE (link_id, revision);


--
-- Name: reader_content_history reader_content_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_content_history
    ADD CONSTRAINT reader_content_history_pkey PRIMARY KEY (id);


--
-- Name: reader_domain_activity reader_domain_activity_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_domain_activity
    ADD CONSTRAINT reader_domain_activity_pkey PRIMARY KEY (domain);


--
-- Name: reader_engagement reader_engagement_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_engagement
    ADD CONSTRAINT reader_engagement_pkey PRIMARY KEY (link_id);


--
-- Name: reader_feed_feedback reader_feed_feedback_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_feedback
    ADD CONSTRAINT reader_feed_feedback_pkey PRIMARY KEY (item_key);


--
-- Name: reader_feed_saves reader_feed_saves_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_saves
    ADD CONSTRAINT reader_feed_saves_pkey PRIMARY KEY (feed_item_id);


--
-- Name: reader_feed_snapshots reader_feed_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_snapshots
    ADD CONSTRAINT reader_feed_snapshots_pkey PRIMARY KEY (id);


--
-- Name: reader_host_purge_receipts reader_host_purge_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_host_purge_receipts
    ADD CONSTRAINT reader_host_purge_receipts_pkey PRIMARY KEY (host_kind, host_id);


--
-- Name: reader_inbox_jobs reader_inbox_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_inbox_jobs
    ADD CONSTRAINT reader_inbox_jobs_pkey PRIMARY KEY (id);


--
-- Name: reader_inbox reader_inbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_inbox
    ADD CONSTRAINT reader_inbox_pkey PRIMARY KEY (id);


--
-- Name: reader_note_history reader_note_history_note_revision_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_note_history
    ADD CONSTRAINT reader_note_history_note_revision_key UNIQUE (note_id, revision);


--
-- Name: reader_note_history reader_note_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_note_history
    ADD CONSTRAINT reader_note_history_pkey PRIMARY KEY (id);


--
-- Name: reader_notes reader_notes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_notes
    ADD CONSTRAINT reader_notes_pkey PRIMARY KEY (id);


--
-- Name: reader_tag_activity reader_tag_activity_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_tag_activity
    ADD CONSTRAINT reader_tag_activity_pkey PRIMARY KEY (tag);


--
-- Name: reader_thought_ops reader_thought_ops_op_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_ops
    ADD CONSTRAINT reader_thought_ops_op_id_key UNIQUE (op_id);


--
-- Name: reader_thought_ops reader_thought_ops_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_ops
    ADD CONSTRAINT reader_thought_ops_pkey PRIMARY KEY (sequence);


--
-- Name: reader_thought_supersession_events reader_thought_supersession_events_loser_sequence_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_supersession_events
    ADD CONSTRAINT reader_thought_supersession_events_loser_sequence_key UNIQUE (loser_sequence);


--
-- Name: reader_thought_supersession_events reader_thought_supersession_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_supersession_events
    ADD CONSTRAINT reader_thought_supersession_events_pkey PRIMARY KEY (sequence);


--
-- Name: reader_thought_tombstones reader_thought_tombstones_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thought_tombstones
    ADD CONSTRAINT reader_thought_tombstones_pkey PRIMARY KEY (thought_id);


--
-- Name: reader_thoughts reader_thoughts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thoughts
    ADD CONSTRAINT reader_thoughts_pkey PRIMARY KEY (id);


--
-- Name: reader_todos reader_todos_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_todos
    ADD CONSTRAINT reader_todos_pkey PRIMARY KEY (id);


--
-- Name: site_entries site_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_entries
    ADD CONSTRAINT site_entries_pkey PRIMARY KEY (id);


--
-- Name: site_identities site_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_identities
    ADD CONSTRAINT site_identities_pkey PRIMARY KEY (identity_key);


--
-- Name: sites sites_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sites
    ADD CONSTRAINT sites_pkey PRIMARY KEY (id);


--
-- Name: idx_concept_alias_by_concept; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_concept_alias_by_concept ON public.concept_alias USING btree (concept_id);


--
-- Name: idx_concept_alias_lookup; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_concept_alias_lookup ON public.concept_alias USING btree (alias);


--
-- Name: idx_concept_embedding_hnsw; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_concept_embedding_hnsw ON public.concept USING hnsw (embedding public.vector_cosine_ops);


--
-- Name: idx_concept_primary_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_concept_primary_name ON public.concept USING btree (lower(primary_name));


--
-- Name: idx_concept_primary_name_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_concept_primary_name_trgm ON public.concept USING gin (lower(primary_name) public.gin_trgm_ops);


--
-- Name: idx_concept_use_count; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_concept_use_count ON public.concept USING btree (use_count DESC);


--
-- Name: idx_concept_wikidata_qid; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_concept_wikidata_qid ON public.concept USING btree (wikidata_qid);


--
-- Name: idx_feed_folders_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_feed_folders_name ON public.feed_folders USING btree (lower(name));


--
-- Name: idx_feed_items_later; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_items_later ON public.feed_items USING btree (published_at DESC NULLS LAST) WHERE read_later;


--
-- Name: idx_feed_items_starred; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_items_starred ON public.feed_items USING btree (published_at DESC NULLS LAST) WHERE starred;


--
-- Name: idx_feed_items_subscription_effective_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_items_subscription_effective_time ON public.feed_items USING btree (subscription_id, COALESCE(published_at, created_at) DESC);


--
-- Name: idx_feed_items_subscription_external; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_feed_items_subscription_external ON public.feed_items USING btree (subscription_id, external_id);


--
-- Name: idx_feed_items_subscription_published; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_items_subscription_published ON public.feed_items USING btree (subscription_id, published_at DESC NULLS LAST, created_at DESC);


--
-- Name: idx_feed_items_unread; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_items_unread ON public.feed_items USING btree (published_at DESC NULLS LAST) WHERE (read_at IS NULL);


--
-- Name: idx_feed_subscriptions_canonical_url; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_feed_subscriptions_canonical_url ON public.feed_subscriptions USING btree (canonical_url) WHERE (canonical_url IS NOT NULL);


--
-- Name: idx_feed_subscriptions_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_subscriptions_due ON public.feed_subscriptions USING btree (next_fetch_at, refresh_claimed_until) WHERE active;


--
-- Name: idx_feed_subscriptions_folder; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_feed_subscriptions_folder ON public.feed_subscriptions USING btree (folder_id);


--
-- Name: idx_feed_subscriptions_url; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_feed_subscriptions_url ON public.feed_subscriptions USING btree (url);


--
-- Name: idx_idempotency_keys_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_idempotency_keys_expires ON public.idempotency_keys USING btree (expires_at);


--
-- Name: idx_library_classification_rules_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_library_classification_rules_enabled ON public.library_classification_rules USING btree (host) WHERE enabled;


--
-- Name: idx_library_classification_rules_scope; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_library_classification_rules_scope ON public.library_classification_rules USING btree (host, COALESCE(identity_adapter, ''::text), COALESCE(path_prefix, ''::text));


--
-- Name: idx_library_review_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_library_review_pending ON public.library_review_items USING btree (created_at DESC) WHERE (status = 'pending'::text);


--
-- Name: idx_library_review_pending_link_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_library_review_pending_link_kind ON public.library_review_items USING btree (link_id, kind) WHERE ((status = 'pending'::text) AND (link_id IS NOT NULL));


--
-- Name: idx_library_review_pending_site_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_library_review_pending_site_kind ON public.library_review_items USING btree (site_id, kind) WHERE ((status = 'pending'::text) AND (site_id IS NOT NULL));


--
-- Name: idx_link_concept_by_concept; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_link_concept_by_concept ON public.link_concept USING btree (concept_id);


--
-- Name: idx_link_translations_legacy_source_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_link_translations_legacy_source_unique ON public.link_translations USING btree (link_id, scope, block_key, start_offset, end_offset, source_hash, target_language) WHERE (source_content_revision IS NULL);


--
-- Name: idx_link_translations_link_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_link_translations_link_updated ON public.link_translations USING btree (link_id, updated_at DESC);


--
-- Name: idx_link_translations_missing_reconcile; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_link_translations_missing_reconcile ON public.link_translations USING btree (updated_at, id) INCLUDE (current_river_job_id) WHERE ((status = ANY (ARRAY['pending'::text, 'processing'::text])) AND (current_river_job_id IS NOT NULL));


--
-- Name: idx_link_translations_saved_revision_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_link_translations_saved_revision_unique ON public.link_translations USING btree (link_id, scope, block_key, start_offset, end_offset, source_content_revision, target_language) WHERE (source_content_revision IS NOT NULL);


--
-- Name: idx_link_url_identities_link; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_link_url_identities_link ON public.link_url_identities USING btree (link_id);


--
-- Name: idx_links_content_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_content_type ON public.links USING btree (content_type);


--
-- Name: idx_links_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_created_at ON public.links USING btree (created_at DESC);


--
-- Name: idx_links_domain_depth_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_domain_depth_status ON public.links USING btree (domain, path_depth, status);


--
-- Name: idx_links_done_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_done_created ON public.links USING btree (created_at) WHERE (status = 'done'::text);


--
-- Name: idx_links_embedding_hnsw; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_embedding_hnsw ON public.links USING hnsw (embedding public.vector_cosine_ops);


--
-- Name: idx_links_library_kind_done_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_library_kind_done_created ON public.links USING btree (library_kind, created_at DESC) WHERE (status = 'done'::text);


--
-- Name: idx_links_parent_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_parent_id ON public.links USING btree (parent_id);


--
-- Name: idx_links_site_payload_purge_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_site_payload_purge_due ON public.links USING btree (payload_purge_due_at) WHERE ((payload_purge_due_at IS NOT NULL) AND (payload_purged_at IS NULL));


--
-- Name: idx_links_source_key_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_links_source_key_unique ON public.links USING btree (source_key);


--
-- Name: idx_links_tags; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_tags ON public.links USING gin (tags);


--
-- Name: idx_links_trash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_trash ON public.links USING btree (deleted_at DESC, id DESC) WHERE (deleted_at IS NOT NULL);


--
-- Name: idx_links_url; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_links_url ON public.links USING btree (url);


--
-- Name: idx_merge_proposal_loser; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_merge_proposal_loser ON public.concept_merge_proposal USING btree (loser_id) WHERE (status = 'pending'::text);


--
-- Name: idx_merge_proposal_pair; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_merge_proposal_pair ON public.concept_merge_proposal USING btree (winner_id, loser_id);


--
-- Name: idx_merge_proposal_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_merge_proposal_pending ON public.concept_merge_proposal USING btree (created_at DESC) WHERE (status = 'pending'::text);


--
-- Name: idx_parse_jobs_link_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_parse_jobs_link_created ON public.parse_jobs USING btree (link_id, created_at DESC);


--
-- Name: idx_parse_jobs_missing_reconcile; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_parse_jobs_missing_reconcile ON public.parse_jobs USING btree (updated_at, id) INCLUDE (link_id) WHERE (status = ANY (ARRAY['pending'::text, 'processing'::text]));


--
-- Name: idx_reader_content_history_link; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_content_history_link ON public.reader_content_history USING btree (link_id, revision DESC);


--
-- Name: idx_reader_engagement_continue; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_engagement_continue ON public.reader_engagement USING btree (read, progress DESC, last_opened DESC NULLS LAST);


--
-- Name: idx_reader_feed_saves_link; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_feed_saves_link ON public.reader_feed_saves USING btree (link_id);


--
-- Name: idx_reader_feed_snapshots_expiry; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_feed_snapshots_expiry ON public.reader_feed_snapshots USING btree (created_at);


--
-- Name: idx_reader_inbox_identity_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_identity_key ON public.reader_inbox USING btree (identity_key);


--
-- Name: idx_reader_inbox_jobs_inbox; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_jobs_inbox ON public.reader_inbox_jobs USING btree (inbox_id, created_at DESC);


--
-- Name: idx_reader_inbox_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_pending ON public.reader_inbox USING btree (status, updated_at DESC, id DESC);


--
-- Name: idx_reader_inbox_pending_expiry; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_pending_expiry ON public.reader_inbox USING btree (expires_at, id) WHERE ((status = 'pending'::text) AND (expires_at IS NOT NULL) AND (expired_at IS NULL));


--
-- Name: idx_reader_inbox_trash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_trash ON public.reader_inbox USING btree (deleted_at DESC, id DESC) WHERE (deleted_at IS NOT NULL);


--
-- Name: idx_reader_inbox_url; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_url ON public.reader_inbox USING btree (url);


--
-- Name: idx_reader_note_history_note; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_note_history_note ON public.reader_note_history USING btree (note_id, revision DESC);


--
-- Name: idx_reader_notes_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_notes_updated ON public.reader_notes USING btree (deleted_at, updated_at DESC, id DESC);


--
-- Name: idx_reader_thought_ops_sequence; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_ops_sequence ON public.reader_thought_ops USING btree (sequence);


--
-- Name: idx_reader_thought_search; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_search ON public.reader_thoughts USING gin (to_tsvector('simple'::regconfig, body));


--
-- Name: idx_reader_thought_supersession_events_sequence; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_supersession_events_sequence ON public.reader_thought_supersession_events USING btree (sequence);


--
-- Name: idx_reader_thought_tombstones_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_tombstones_order ON public.reader_thought_tombstones USING btree (created_at DESC, thought_id DESC);


--
-- Name: idx_reader_thoughts_host; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thoughts_host ON public.reader_thoughts USING btree (host_kind, host_id, deleted, updated_at DESC);


--
-- Name: idx_reader_todos_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_todos_order ON public.reader_todos USING btree (deleted_at, done, due_at, created_at DESC, id DESC);


--
-- Name: idx_reader_todos_projection; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_reader_todos_projection ON public.reader_todos USING btree (origin_kind, origin_host_id, ((origin_ref ->> 'block_ref'::text)), COALESCE((origin_ref ->> 'occurrence'::text), '1'::text)) WHERE ((origin_kind <> 'standalone'::text) AND (deleted_at IS NULL));


--
-- Name: idx_site_entries_link; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_site_entries_link ON public.site_entries USING btree (link_id);


--
-- Name: idx_site_entries_site; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_site_entries_site ON public.site_entries USING btree (site_id);


--
-- Name: idx_site_entries_site_url; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_site_entries_site_url ON public.site_entries USING btree (site_id, normalized_url);


--
-- Name: idx_site_identities_site; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_site_identities_site ON public.site_identities USING btree (site_id);


--
-- Name: idx_site_tags_normalized; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_site_tags_normalized ON public.site_tags USING btree (normalized_tag);


--
-- Name: idx_site_tags_site_normalized; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_site_tags_site_normalized ON public.site_tags USING btree (site_id, normalized_tag);


--
-- Name: idx_sites_embedding_hnsw; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sites_embedding_hnsw ON public.sites USING hnsw (embedding public.vector_cosine_ops) WHERE (embedding IS NOT NULL);


--
-- Name: idx_sites_last_collected; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sites_last_collected ON public.sites USING btree (last_collected_at DESC);


--
-- Name: idx_sites_pinned_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sites_pinned_updated ON public.sites USING btree (pinned, updated_at DESC);


--
-- Name: idx_sites_site_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_sites_site_key ON public.sites USING btree (site_key);


--
-- Name: links links_metadata_revision_bump; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER links_metadata_revision_bump BEFORE UPDATE OF title, summary, tags ON public.links FOR EACH ROW EXECUTE FUNCTION public.advance_link_metadata_revision();


--
-- Name: concept trg_concept_bump_global_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_concept_bump_global_revision_del AFTER DELETE ON public.concept REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_global_revision_trigger();


--
-- Name: concept trg_concept_bump_global_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_concept_bump_global_revision_ins AFTER INSERT ON public.concept REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_global_revision_trigger();


--
-- Name: concept trg_concept_bump_global_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_concept_bump_global_revision_upd AFTER UPDATE ON public.concept REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_concept_global_revision_update();


--
-- Name: concept trg_concept_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_concept_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.concept FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: concept trg_concept_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_concept_representation_write_gate_upd BEFORE UPDATE OF display_name, id, primary_name ON public.concept FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_folders trg_feed_folders_bump_feed_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_folders_bump_feed_revision_del AFTER DELETE ON public.feed_folders REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_folders trg_feed_folders_bump_feed_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_folders_bump_feed_revision_ins AFTER INSERT ON public.feed_folders REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_folders trg_feed_folders_bump_feed_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_folders_bump_feed_revision_upd AFTER UPDATE ON public.feed_folders REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_folders_revision_update();


--
-- Name: feed_folders trg_feed_folders_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_folders_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.feed_folders FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_folders trg_feed_folders_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_folders_representation_write_gate_upd BEFORE UPDATE OF created_at, id, name, updated_at ON public.feed_folders FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_items trg_feed_items_bump_feed_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_items_bump_feed_revision_del AFTER DELETE ON public.feed_items REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_items trg_feed_items_bump_feed_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_items_bump_feed_revision_ins AFTER INSERT ON public.feed_items REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_items trg_feed_items_bump_feed_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_items_bump_feed_revision_upd AFTER UPDATE ON public.feed_items REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_items_revision_update();


--
-- Name: feed_items trg_feed_items_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_items_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.feed_items FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_items trg_feed_items_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_items_representation_write_gate_upd BEFORE UPDATE OF author, content_html, content_text, created_at, id, link_id, published_at, read_at, read_later, starred, subscription_id, summary, title, url ON public.feed_items FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_subscriptions trg_feed_subscriptions_bump_feed_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_subscriptions_bump_feed_revision_del AFTER DELETE ON public.feed_subscriptions REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_subscriptions trg_feed_subscriptions_bump_feed_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_subscriptions_bump_feed_revision_ins AFTER INSERT ON public.feed_subscriptions REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_revision_trigger();


--
-- Name: feed_subscriptions trg_feed_subscriptions_bump_feed_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_subscriptions_bump_feed_revision_upd AFTER UPDATE ON public.feed_subscriptions REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_feed_subscriptions_revision_update();


--
-- Name: feed_subscriptions trg_feed_subscriptions_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_subscriptions_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.feed_subscriptions FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: feed_subscriptions trg_feed_subscriptions_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_feed_subscriptions_representation_write_gate_upd BEFORE UPDATE OF active, folder_id, id, title ON public.feed_subscriptions FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: link_concept trg_link_concept_bump_read_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_link_concept_bump_read_revision_del AFTER DELETE ON public.link_concept REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: link_concept trg_link_concept_bump_read_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_link_concept_bump_read_revision_ins AFTER INSERT ON public.link_concept REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: link_concept trg_link_concept_bump_read_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_link_concept_bump_read_revision_upd AFTER UPDATE ON public.link_concept REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_link_concept_read_revision_update();


--
-- Name: link_concept trg_link_concept_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_link_concept_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.link_concept FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: link_concept trg_link_concept_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_link_concept_representation_write_gate_upd BEFORE UPDATE OF concept_id, link_id, surface_tag ON public.link_concept FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: links trg_links_bump_read_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_links_bump_read_revision_del AFTER DELETE ON public.links REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: links trg_links_bump_read_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_links_bump_read_revision_ins AFTER INSERT ON public.links REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: links trg_links_bump_read_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_links_bump_read_revision_upd AFTER UPDATE ON public.links REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_links_read_revision_update();


--
-- Name: links trg_links_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_links_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.links FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: links trg_links_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_links_representation_write_gate_upd BEFORE UPDATE OF classification_confidence, classification_explanation, classification_reason, classifier_version, content, content_cjk_chars, content_document, content_format, content_revision, content_source, content_type, content_words, created_at, description, domain, error_msg, fetcher_type, id, is_low_confidence, library_kind, library_kind_locked, library_kind_source, low_confidence_reason, parent_id, parent_path, path_depth, predicted_library_kind, requested_library_kind, requested_library_kind_source, status, summary, tags, title, updated_at, url ON public.links FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: links trg_reader_capture_content_history; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_capture_content_history BEFORE UPDATE OF content, content_document, content_revision ON public.links FOR EACH ROW EXECUTE FUNCTION public.reader_capture_content_history();


--
-- Name: links trg_reader_tombstone_deleted_link_thoughts; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_tombstone_deleted_link_thoughts AFTER DELETE ON public.links FOR EACH ROW EXECUTE FUNCTION public.reader_tombstone_deleted_link_thoughts();


--
-- Name: links trg_reader_tombstone_trashed_link_thoughts; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_tombstone_trashed_link_thoughts AFTER UPDATE OF deleted_at ON public.links FOR EACH ROW WHEN (((old.deleted_at IS NULL) AND (new.deleted_at IS NOT NULL))) EXECUTE FUNCTION public.reader_tombstone_trashed_link_thoughts();


--
-- Name: reader_thought_ops trg_reader_scrub_user_deleted_thought_op; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought_op BEFORE INSERT OR UPDATE OF annotation_id, payload, target ON public.reader_thought_ops FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_op();


--
-- Name: reader_thought_supersession_events trg_reader_scrub_user_deleted_thought_event; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought_event BEFORE INSERT OR UPDATE OF annotation_id, loser, winner_at_detection ON public.reader_thought_supersession_events FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_event();


--
-- Name: reader_thought_tombstones trg_reader_protect_user_deleted_thought_tombstone; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_protect_user_deleted_thought_tombstone BEFORE INSERT OR UPDATE OR DELETE ON public.reader_thought_tombstones FOR EACH ROW EXECUTE FUNCTION public.reader_protect_user_deleted_thought_tombstone();


--
-- Name: reader_thoughts trg_reader_enforce_user_deleted_thought; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_enforce_user_deleted_thought BEFORE INSERT OR UPDATE ON public.reader_thoughts FOR EACH ROW EXECUTE FUNCTION public.reader_enforce_user_deleted_thought();


--
-- Name: reader_thoughts trg_reader_scrub_user_deleted_thought; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought AFTER INSERT OR UPDATE ON public.reader_thoughts FOR EACH ROW WHEN ((new.user_deleted = true)) EXECUTE FUNCTION public.reader_scrub_user_deleted_thought();


--
-- Name: site_entries trg_site_entries_bump_read_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_entries_bump_read_revision_del AFTER DELETE ON public.site_entries REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: site_entries trg_site_entries_bump_read_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_entries_bump_read_revision_ins AFTER INSERT ON public.site_entries REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: site_entries trg_site_entries_bump_read_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_entries_bump_read_revision_upd AFTER UPDATE ON public.site_entries REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_site_entries_read_revision_update();


--
-- Name: site_entries trg_site_entries_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_entries_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.site_entries FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: site_entries trg_site_entries_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_entries_representation_write_gate_upd BEFORE UPDATE OF id, normalized_url, site_id ON public.site_entries FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: site_tags trg_site_tags_bump_read_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_tags_bump_read_revision_del AFTER DELETE ON public.site_tags REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: site_tags trg_site_tags_bump_read_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_tags_bump_read_revision_ins AFTER INSERT ON public.site_tags REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: site_tags trg_site_tags_bump_read_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_tags_bump_read_revision_upd AFTER UPDATE ON public.site_tags REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_site_tags_read_revision_update();


--
-- Name: site_tags trg_site_tags_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_tags_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.site_tags FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: site_tags trg_site_tags_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_site_tags_representation_write_gate_upd BEFORE UPDATE OF normalized_tag, site_id, tag ON public.site_tags FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: sites trg_sites_bump_read_revision_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sites_bump_read_revision_del AFTER DELETE ON public.sites REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: sites trg_sites_bump_read_revision_ins; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sites_bump_read_revision_ins AFTER INSERT ON public.sites REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_library_revision_trigger();


--
-- Name: sites trg_sites_bump_read_revision_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sites_bump_read_revision_upd AFTER UPDATE ON public.sites REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION public.bump_sites_read_revision_update();


--
-- Name: sites trg_sites_representation_write_gate_ins_del; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sites_representation_write_gate_ins_del BEFORE INSERT OR DELETE ON public.sites FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: sites trg_sites_representation_write_gate_upd; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_sites_representation_write_gate_upd BEFORE UPDATE OF first_collected_at, homepage_url, icon_url, id, intro, last_collected_at, name, needs_review, pinned, primary_entry_id, revision, site_key, updated_at ON public.sites FOR EACH STATEMENT EXECUTE FUNCTION public.guard_representation_write_gate();


--
-- Name: concept_alias concept_alias_concept_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.concept_alias
    ADD CONSTRAINT concept_alias_concept_id_fkey FOREIGN KEY (concept_id) REFERENCES public.concept(id) ON DELETE CASCADE;


--
-- Name: feed_items feed_items_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_items
    ADD CONSTRAINT feed_items_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE SET NULL;


--
-- Name: feed_items feed_items_subscription_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_items
    ADD CONSTRAINT feed_items_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES public.feed_subscriptions(id) ON DELETE CASCADE;


--
-- Name: feed_subscriptions feed_subscriptions_folder_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_subscriptions
    ADD CONSTRAINT feed_subscriptions_folder_id_fkey FOREIGN KEY (folder_id) REFERENCES public.feed_folders(id) ON DELETE SET NULL;


--
-- Name: links fk_links_parent_id; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.links
    ADD CONSTRAINT fk_links_parent_id FOREIGN KEY (parent_id) REFERENCES public.links(id) ON DELETE SET NULL;


--
-- Name: library_review_items library_review_items_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.library_review_items
    ADD CONSTRAINT library_review_items_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: library_review_items library_review_items_site_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.library_review_items
    ADD CONSTRAINT library_review_items_site_id_fkey FOREIGN KEY (site_id) REFERENCES public.sites(id) ON DELETE CASCADE;


--
-- Name: link_concept link_concept_concept_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_concept
    ADD CONSTRAINT link_concept_concept_id_fkey FOREIGN KEY (concept_id) REFERENCES public.concept(id) ON DELETE CASCADE;


--
-- Name: link_concept link_concept_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_concept
    ADD CONSTRAINT link_concept_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: link_translations link_translations_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_translations
    ADD CONSTRAINT link_translations_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: link_url_identities link_url_identities_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.link_url_identities
    ADD CONSTRAINT link_url_identities_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: parse_jobs parse_jobs_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.parse_jobs
    ADD CONSTRAINT parse_jobs_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: reader_categorizables reader_categorizables_category_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_categorizables
    ADD CONSTRAINT reader_categorizables_category_id_fkey FOREIGN KEY (category_id) REFERENCES public.reader_categories(id) ON DELETE CASCADE;


--
-- Name: reader_content_history reader_content_history_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_content_history
    ADD CONSTRAINT reader_content_history_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: reader_engagement reader_engagement_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_engagement
    ADD CONSTRAINT reader_engagement_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: reader_feed_saves reader_feed_saves_feed_item_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_saves
    ADD CONSTRAINT reader_feed_saves_feed_item_id_fkey FOREIGN KEY (feed_item_id) REFERENCES public.feed_items(id) ON DELETE CASCADE;


--
-- Name: reader_feed_saves reader_feed_saves_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_saves
    ADD CONSTRAINT reader_feed_saves_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: reader_inbox_jobs reader_inbox_jobs_inbox_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_inbox_jobs
    ADD CONSTRAINT reader_inbox_jobs_inbox_id_fkey FOREIGN KEY (inbox_id) REFERENCES public.reader_inbox(id) ON DELETE CASCADE;


--
-- Name: reader_thoughts reader_thoughts_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_thoughts
    ADD CONSTRAINT reader_thoughts_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE SET NULL;


--
-- Name: site_entries site_entries_link_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_entries
    ADD CONSTRAINT site_entries_link_id_fkey FOREIGN KEY (link_id) REFERENCES public.links(id) ON DELETE CASCADE;


--
-- Name: site_entries site_entries_site_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_entries
    ADD CONSTRAINT site_entries_site_id_fkey FOREIGN KEY (site_id) REFERENCES public.sites(id) ON DELETE CASCADE;


--
-- Name: site_identities site_identities_site_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_identities
    ADD CONSTRAINT site_identities_site_id_fkey FOREIGN KEY (site_id) REFERENCES public.sites(id) ON DELETE CASCADE;


--
-- Name: site_tags site_tags_site_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.site_tags
    ADD CONSTRAINT site_tags_site_id_fkey FOREIGN KEY (site_id) REFERENCES public.sites(id) ON DELETE CASCADE;


--
-- Name: sites sites_primary_entry_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sites
    ADD CONSTRAINT sites_primary_entry_id_fkey FOREIGN KEY (primary_entry_id) REFERENCES public.site_entries(id) DEFERRABLE INITIALLY DEFERRED;

-- Installation-scoped singleton state is data, so pg_dump --schema-only does
-- not carry it. These rows are part of the fresh-install contract.
SET LOCAL search_path = public, pg_catalog;
INSERT INTO public.installation_state (singleton) VALUES (true);
INSERT INTO public.library_read_revision (singleton) VALUES (true);
INSERT INTO public.global_read_revision (singleton) VALUES (true);
INSERT INTO public.feed_read_revision (singleton) VALUES (true);

-- Preserve the product's fresh-install starter subscription without a
-- separate bootstrap marker or provisioning trigger.
INSERT INTO public.feed_subscriptions (url, canonical_url, title)
VALUES (
    'https://www.ruanyifeng.com/blog/atom.xml',
    'https://www.ruanyifeng.com/blog/atom.xml',
    '阮一峰的网络日志'
);
UPDATE public.feed_read_revision SET revision = 0, updated_at = now() WHERE singleton;
`

var steps = []Step{
	{
		ID:  TranslationSourceContractMigrationID,
		SQL: []string{singleInstallSchemaSQL},
	},
	{
		// This is the only application index on a River-owned table. It must
		// follow River's migrations and run outside a transaction.
		ID:                    translationTerminalHistoryIndexMigrationID,
		NonTransactional:      true,
		RecoverInvalidIndexes: []string{"public.idx_river_job_translation_terminal_history"},
		SQL: []string{
			`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_river_job_translation_terminal_history
			 ON public.river_job (finalized_at, id)
			 WHERE kind IN ('translate_link_v2', 'translate_link_content')
			   AND state IN ('cancelled', 'completed', 'discarded')
			   AND finalized_at IS NOT NULL`,
		},
	},
	{
		// Upgrade existing installations to the immutable, self-contained
		// Thought snapshot contract used by history replay and reattachment.
		ID: readerThoughtTombstoneSnapshotMigrationID,
		SQL: []string{
			`CREATE OR REPLACE FUNCTION reader_tombstone_deleted_link_thoughts() RETURNS TRIGGER AS $$
			BEGIN
				INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
				SELECT thought.id,thought.host_kind,thought.host_id,'link_deleted',
					jsonb_build_object(
						'snapshot_version',1,
						'id',thought.id,'host_kind',thought.host_kind,'host_id',thought.host_id,'link_id',thought.link_id,
						'type','thought','body',thought.body,'target',thought.target,'quote',thought.quote,'source',thought.source,
						'created_at',thought.created_at,'updated_at',thought.updated_at,
						'original_host_snapshot',to_jsonb(COALESCE(OLD.content_document,OLD.content,'')),
						'original_host_identity',jsonb_build_object('kind','link','id',OLD.id,'url',OLD.url,'content_revision',OLD.content_revision),
						'frozen_at',CURRENT_TIMESTAMP)
				FROM reader_thoughts thought
				WHERE thought.host_kind='link' AND thought.host_id=OLD.id::text AND thought.deleted=false
				ON CONFLICT (thought_id) DO NOTHING;
				RETURN OLD;
			END;
			$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public`,
			`CREATE OR REPLACE FUNCTION reader_tombstone_trashed_link_thoughts() RETURNS TRIGGER AS $$
			BEGIN
				INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
				SELECT thought.id,thought.host_kind,thought.host_id,'link_deleted',
					jsonb_build_object(
						'snapshot_version',1,
						'id',thought.id,'host_kind',thought.host_kind,'host_id',thought.host_id,'link_id',thought.link_id,
						'type','thought','body',thought.body,'target',thought.target,'quote',thought.quote,'source',thought.source,
						'created_at',thought.created_at,'updated_at',thought.updated_at,
						'original_host_snapshot',to_jsonb(COALESCE(NEW.content_document,NEW.content,'')),
						'original_host_identity',jsonb_build_object('kind','link','id',NEW.id,'url',NEW.url,'content_revision',NEW.content_revision),
						'frozen_at',CURRENT_TIMESTAMP)
				FROM reader_thoughts thought
				WHERE thought.host_kind='link' AND thought.host_id=NEW.id::text AND thought.deleted=false
				ON CONFLICT (thought_id) DO NOTHING;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql SET search_path = pg_catalog, public`,
		},
	},
	{
		// Cross-cutting forward repair for pre-upgrade rows. Every statement is
		// transactional with the ledger insert so a failed repair is retryable.
		ID: integrityRepairMigrationID,
		SQL: []string{
			// 这一步会 UPDATE links 的 classifier_version / updated_at，正是
			// trg_links_representation_write_gate_upd 监听的列。该触发器用
			// 非阻塞的 pg_try_advisory_xact_lock_shared，拿不到就直接 RAISE
			// 40001；运行时写入方都先阻塞式预取 gate，迁移也必须这样做，
			// 否则任何一个并发在线事务都会让整个迁移失败退出。
			`SELECT public.lock_library_feed_revisions()`,
			`ALTER TABLE public.idempotency_keys ADD COLUMN IF NOT EXISTS owner_token text;
			 UPDATE public.idempotency_keys SET owner_token = gen_random_uuid()::text WHERE owner_token IS NULL;
			 ALTER TABLE public.idempotency_keys ALTER COLUMN owner_token SET DEFAULT (gen_random_uuid())::text;
			 ALTER TABLE public.idempotency_keys ALTER COLUMN owner_token SET NOT NULL;
			 ALTER TABLE public.idempotency_keys ADD COLUMN IF NOT EXISTS generation bigint;
			 UPDATE public.idempotency_keys SET generation = 1 WHERE generation IS NULL OR generation < 1;
			 ALTER TABLE public.idempotency_keys ALTER COLUMN generation SET DEFAULT 1;
			 ALTER TABLE public.idempotency_keys ALTER COLUMN generation SET NOT NULL;
			 DO $migration$
			 BEGIN
				 IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conname='chk_idempotency_keys_generation' AND conrelid='public.idempotency_keys'::regclass) THEN
					 ALTER TABLE public.idempotency_keys ADD CONSTRAINT chk_idempotency_keys_generation CHECK (generation >= 1);
				 END IF;
			 END;
			 $migration$;`,
			`ALTER TABLE public.links ADD COLUMN IF NOT EXISTS feed_managed boolean;
			 UPDATE public.links AS link
			 SET feed_managed = true
			 FROM public.reader_feed_saves AS save
			 WHERE save.link_id = link.id
			   AND save.created_link
			   AND link.source_kind = 'subscription'
			   AND link.feed_managed IS NULL;
			 UPDATE public.links SET feed_managed = false WHERE feed_managed IS NULL;
			 ALTER TABLE public.links ALTER COLUMN feed_managed SET DEFAULT false;
			 ALTER TABLE public.links ALTER COLUMN feed_managed SET NOT NULL`,
			`UPDATE public.parse_jobs AS attempt
			 SET status = 'failed', error_msg = 'link_deleted', updated_at = CURRENT_TIMESTAMP
			 FROM public.links AS link
			 WHERE link.id = attempt.link_id
			   AND link.deleted_at IS NOT NULL
			   AND attempt.status IN ('pending', 'processing')`,
			`UPDATE public.concept_merge_proposal
			 SET decided_by = NULL, decided_at = NULL
			 WHERE status = 'pending' AND (decided_by IS NOT NULL OR decided_at IS NOT NULL);
			 UPDATE public.concept_merge_proposal
			 SET decided_by = COALESCE(decided_by, 'legacy-migration'),
			     decided_at = COALESCE(decided_at, created_at)
			 WHERE status IN ('approved', 'rejected') AND (decided_by IS NULL OR decided_at IS NULL);
			 ALTER TABLE public.concept_merge_proposal DROP CONSTRAINT IF EXISTS concept_merge_proposal_loser_id_fkey;
			 ALTER TABLE public.concept_merge_proposal DROP CONSTRAINT IF EXISTS concept_merge_proposal_winner_id_fkey;
			 DO $migration$
			 BEGIN
				 IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conname='chk_merge_proposal_decision_audit' AND conrelid='public.concept_merge_proposal'::regclass) THEN
					 ALTER TABLE public.concept_merge_proposal ADD CONSTRAINT chk_merge_proposal_decision_audit CHECK (
						 (status='pending' AND decided_by IS NULL AND decided_at IS NULL)
						 OR (status IN ('approved','rejected') AND decided_by IS NOT NULL AND decided_at IS NOT NULL));
				 END IF;
			 END;
			 $migration$;`,
			`ALTER TABLE public.reader_thoughts ADD COLUMN IF NOT EXISTS user_deleted boolean;
			 ALTER TABLE public.reader_thoughts ADD COLUMN IF NOT EXISTS user_deleted_at timestamp with time zone;
			 UPDATE public.reader_thoughts
			 SET user_deleted=true, user_deleted_at=COALESCE(user_deleted_at,updated_at),
			     deleted=true, body='', target='{}'::jsonb, quote=NULL, source=''
			 WHERE deleted OR COALESCE(user_deleted,false);
			 UPDATE public.reader_thoughts SET user_deleted=false WHERE user_deleted IS NULL;
			 ALTER TABLE public.reader_thoughts ALTER COLUMN user_deleted SET DEFAULT false;
			 ALTER TABLE public.reader_thoughts ALTER COLUMN user_deleted SET NOT NULL;
			 UPDATE public.reader_thought_ops AS op
			 SET target='{}'::jsonb, payload='{}'::jsonb
			 FROM public.reader_thoughts AS thought
			 WHERE thought.id=op.annotation_id AND thought.user_deleted
			   AND (op.target IS DISTINCT FROM '{}'::jsonb OR op.payload IS DISTINCT FROM '{}'::jsonb);
			 UPDATE public.reader_thought_supersession_events AS event
			 SET loser=jsonb_build_object('type','user_deleted','id',thought.id,'user_deleted',true),
			     winner_at_detection=jsonb_build_object('type','user_deleted','id',thought.id,'user_deleted',true)
			 FROM public.reader_thoughts AS thought
			 WHERE thought.id=event.annotation_id AND thought.user_deleted;
			 UPDATE public.reader_thought_tombstones AS tombstone
			 SET reason='user_deleted',snapshot=jsonb_build_object(
				 'snapshot_version',1,'id',thought.id,'host_kind',thought.host_kind,'host_id',thought.host_id,
				 'type','user_deleted','user_deleted',true,'deleted_at',thought.user_deleted_at)
			 FROM public.reader_thoughts AS thought
			 WHERE thought.id=tombstone.thought_id AND thought.user_deleted;
			 DO $migration$
			 BEGIN
				 IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conname='chk_reader_thoughts_user_deleted_at' AND conrelid='public.reader_thoughts'::regclass) THEN
					 ALTER TABLE public.reader_thoughts ADD CONSTRAINT chk_reader_thoughts_user_deleted_at CHECK (NOT user_deleted OR user_deleted_at IS NOT NULL);
				 END IF;
				 IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conname='chk_reader_thoughts_user_deleted_content' AND conrelid='public.reader_thoughts'::regclass) THEN
					 ALTER TABLE public.reader_thoughts ADD CONSTRAINT chk_reader_thoughts_user_deleted_content CHECK (
						 NOT user_deleted OR (deleted AND body='' AND target='{}'::jsonb AND quote IS NULL AND source=''));
				 END IF;
			 END;
			 $migration$;`,
			`CREATE OR REPLACE FUNCTION public.reader_enforce_user_deleted_thought() RETURNS trigger AS $function$
			 BEGIN
				 IF TG_OP = 'UPDATE' THEN
					 NEW.user_deleted := NEW.deleted OR NEW.user_deleted OR OLD.user_deleted;
				 ELSE
					 NEW.user_deleted := NEW.deleted OR NEW.user_deleted;
				 END IF;
				 IF NEW.user_deleted THEN
					 IF TG_OP = 'UPDATE' AND OLD.user_deleted_at IS NOT NULL THEN
						 NEW.user_deleted_at := OLD.user_deleted_at;
					 ELSE
						 NEW.user_deleted_at := COALESCE(NEW.user_deleted_at,CURRENT_TIMESTAMP);
					 END IF;
					 NEW.deleted := true; NEW.body := ''; NEW.target := '{}'::jsonb; NEW.quote := NULL; NEW.source := '';
				 END IF;
				 RETURN NEW;
			 END;
			 $function$ LANGUAGE plpgsql SET search_path=pg_catalog,public;
			 CREATE OR REPLACE FUNCTION public.reader_scrub_user_deleted_thought() RETURNS trigger AS $function$
			 BEGIN
				 IF NEW.user_deleted THEN
					 UPDATE public.reader_thought_ops SET target='{}'::jsonb,payload='{}'::jsonb
					 WHERE annotation_id=NEW.id AND (target IS DISTINCT FROM '{}'::jsonb OR payload IS DISTINCT FROM '{}'::jsonb);
					 UPDATE public.reader_thought_supersession_events
					 SET loser=jsonb_build_object('type','user_deleted','id',NEW.id,'user_deleted',true),
					     winner_at_detection=jsonb_build_object('type','user_deleted','id',NEW.id,'user_deleted',true)
					 WHERE annotation_id=NEW.id;
					 UPDATE public.reader_thought_tombstones SET reason='user_deleted',snapshot=jsonb_build_object(
						 'snapshot_version',1,'id',NEW.id,'host_kind',NEW.host_kind,'host_id',NEW.host_id,
						 'type','user_deleted','user_deleted',true,'deleted_at',NEW.user_deleted_at)
					 WHERE thought_id=NEW.id;
				 END IF;
				 RETURN NEW;
			 END;
			 $function$ LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public;
			 CREATE OR REPLACE FUNCTION public.reader_scrub_user_deleted_thought_op() RETURNS trigger AS $function$
			 BEGIN
				 IF EXISTS (SELECT 1 FROM public.reader_thoughts WHERE id=NEW.annotation_id AND user_deleted) THEN
					 NEW.target := '{}'::jsonb; NEW.payload := '{}'::jsonb;
				 END IF;
				 RETURN NEW;
			 END;
			 $function$ LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public;
			 CREATE OR REPLACE FUNCTION public.reader_scrub_user_deleted_thought_event() RETURNS trigger AS $function$
			 BEGIN
				 IF EXISTS (SELECT 1 FROM public.reader_thoughts WHERE id=NEW.annotation_id AND user_deleted) THEN
					 NEW.loser := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
					 NEW.winner_at_detection := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
				 END IF;
				 RETURN NEW;
			 END;
			 $function$ LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public;
			 CREATE OR REPLACE FUNCTION public.reader_protect_user_deleted_thought_tombstone() RETURNS trigger AS $function$
			 DECLARE terminal_deleted_at timestamp with time zone;
			 BEGIN
				 IF TG_OP='DELETE' THEN
					 IF EXISTS (SELECT 1 FROM public.reader_thoughts WHERE id=OLD.thought_id AND user_deleted) THEN RETURN NULL; END IF;
					 RETURN OLD;
				 END IF;
				 SELECT user_deleted_at INTO terminal_deleted_at FROM public.reader_thoughts WHERE id=NEW.thought_id AND user_deleted;
				 IF FOUND THEN
					 NEW.reason := 'user_deleted';
					 NEW.snapshot := jsonb_build_object(
						 'snapshot_version',1,'id',NEW.thought_id,'host_kind',NEW.host_kind,'host_id',NEW.host_id,
						 'type','user_deleted','user_deleted',true,'deleted_at',terminal_deleted_at);
				 END IF;
				 RETURN NEW;
			 END;
			 $function$ LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public;
			 DROP TRIGGER IF EXISTS trg_reader_enforce_user_deleted_thought ON public.reader_thoughts;
			 CREATE TRIGGER trg_reader_enforce_user_deleted_thought BEFORE INSERT OR UPDATE ON public.reader_thoughts
			 FOR EACH ROW EXECUTE FUNCTION public.reader_enforce_user_deleted_thought();
			 DROP TRIGGER IF EXISTS trg_reader_scrub_user_deleted_thought ON public.reader_thoughts;
			 CREATE TRIGGER trg_reader_scrub_user_deleted_thought AFTER INSERT OR UPDATE ON public.reader_thoughts
			 FOR EACH ROW WHEN (NEW.user_deleted=true) EXECUTE FUNCTION public.reader_scrub_user_deleted_thought();
			 DROP TRIGGER IF EXISTS trg_reader_scrub_user_deleted_thought_op ON public.reader_thought_ops;
			 CREATE TRIGGER trg_reader_scrub_user_deleted_thought_op BEFORE INSERT OR UPDATE OF annotation_id,payload,target ON public.reader_thought_ops
			 FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_op();
			 DROP TRIGGER IF EXISTS trg_reader_scrub_user_deleted_thought_event ON public.reader_thought_supersession_events;
			 CREATE TRIGGER trg_reader_scrub_user_deleted_thought_event BEFORE INSERT OR UPDATE OF annotation_id,loser,winner_at_detection ON public.reader_thought_supersession_events
			 FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_event();
			 DROP TRIGGER IF EXISTS trg_reader_protect_user_deleted_thought_tombstone ON public.reader_thought_tombstones;
			 CREATE TRIGGER trg_reader_protect_user_deleted_thought_tombstone BEFORE INSERT OR UPDATE OR DELETE ON public.reader_thought_tombstones
			 FOR EACH ROW EXECUTE FUNCTION public.reader_protect_user_deleted_thought_tombstone()`,
			`UPDATE public.library_review_items AS review
			 SET status='dismissed',revision=revision+1,resolved_at=CURRENT_TIMESTAMP
			 WHERE review.kind='migration_suggestion' AND review.status='pending'
			   AND NOT EXISTS (
				 SELECT 1 FROM public.links AS link
				 WHERE link.id=review.link_id AND link.library_kind='reading'
				   AND review.payload @> jsonb_build_object('content_revision',link.content_revision));
			 UPDATE public.links AS link
			 SET classifier_version=NULL,updated_at=CURRENT_TIMESTAMP
			 WHERE link.classifier_version='historical-migration-v1'
			   AND link.library_kind='reading' AND link.library_kind_source='migration' AND NOT link.library_kind_locked
			   AND link.predicted_library_kind='site'
			   AND NOT EXISTS (
				 SELECT 1 FROM public.library_review_items AS review
				 WHERE review.link_id=link.id AND review.kind='migration_suggestion' AND review.status='pending'
				   AND review.payload @> jsonb_build_object('content_revision',link.content_revision))`,
		},
	},
	{
		// Replay the historical-migration repair after rolling upgrades: an old
		// replica can persist the assessment and fail before its final action even
		// after integrity2026081401 has already run. The new runtime commits both
		// phases atomically; this tail repairs split writes created during rollout.
		ID: historicalRepairMigrationID,
		SQL: []string{
			// 同 integrity2026081401：本步骤同样 UPDATE links 的
			// classifier_version / updated_at，必须先阻塞式预取 revision gate，
			// 否则会撞上写入门的非阻塞 try-lock 而 RAISE 40001。
			`SELECT public.lock_library_feed_revisions()`,
			`UPDATE public.library_review_items AS review
			 SET status='dismissed',revision=revision+1,resolved_at=CURRENT_TIMESTAMP
			 WHERE review.kind='migration_suggestion' AND review.status='pending'
			   AND NOT EXISTS (
				 SELECT 1 FROM public.links AS link
				 WHERE link.id=review.link_id
				   AND link.status='done' AND link.deleted_at IS NULL
				   AND link.library_kind='reading' AND link.library_kind_source='migration'
				   AND NOT link.library_kind_locked
				   AND link.classifier_version='historical-migration-v1'
				   AND link.predicted_library_kind='site'
				   AND (link.content IS NOT NULL OR link.content_document IS NOT NULL
				        OR EXISTS (SELECT 1 FROM public.link_translations AS translation WHERE translation.link_id=link.id))
				   AND review.payload @> jsonb_build_object('content_revision',link.content_revision));
			 UPDATE public.links AS link
			 SET classifier_version=NULL,updated_at=CURRENT_TIMESTAMP
			 WHERE link.status='done' AND link.deleted_at IS NULL
			   AND link.library_kind='reading' AND link.library_kind_source='migration'
			   AND NOT link.library_kind_locked
			   AND link.classifier_version='historical-migration-v1'
			   AND link.predicted_library_kind='site'
			   AND NOT EXISTS (
				 SELECT 1 FROM public.library_review_items AS review
				 WHERE review.link_id=link.id AND review.kind='migration_suggestion' AND review.status='pending'
				   AND (link.content IS NOT NULL OR link.content_document IS NOT NULL
				        OR EXISTS (SELECT 1 FROM public.link_translations AS translation WHERE translation.link_id=link.id))
				   AND review.payload @> jsonb_build_object('content_revision',link.content_revision))`,
		},
	},
	{
		// A dedicated tail makes the concept audit repair replay for installations
		// that already recorded integrity2026081401 during a rolling upgrade. The
		// UUID columns remain immutable history rather than nullable live pointers;
		// already-cascaded rows cannot be reconstructed here and require backup/PITR
		// or an external audit source.
		ID: conceptMergeAuditRepairMigrationID,
		SQL: []string{
			`UPDATE public.concept_merge_proposal
			 SET decided_by = NULL, decided_at = NULL
			 WHERE status = 'pending' AND (decided_by IS NOT NULL OR decided_at IS NOT NULL);
			 UPDATE public.concept_merge_proposal
			 SET decided_by = CASE
			       WHEN decided_by IS NULL OR btrim(decided_by) = '' THEN 'legacy-migration'
			       ELSE decided_by
			     END,
			     decided_at = COALESCE(decided_at, created_at)
			 WHERE status IN ('approved', 'rejected')
			   AND (decided_by IS NULL OR btrim(decided_by) = '' OR decided_at IS NULL);
			 ALTER TABLE public.concept_merge_proposal DROP CONSTRAINT IF EXISTS concept_merge_proposal_loser_id_fkey;
			 ALTER TABLE public.concept_merge_proposal DROP CONSTRAINT IF EXISTS concept_merge_proposal_winner_id_fkey;
			 ALTER TABLE public.concept_merge_proposal DROP CONSTRAINT IF EXISTS chk_merge_proposal_decision_audit;
			 ALTER TABLE public.concept_merge_proposal ADD CONSTRAINT chk_merge_proposal_decision_audit CHECK (
			   (status='pending' AND decided_by IS NULL AND decided_at IS NULL)
				   OR (status IN ('approved','rejected') AND decided_by IS NOT NULL
				       AND btrim(decided_by) <> '' AND decided_at IS NOT NULL));`,
		},
	},
	{
		// Lifecycle defects can survive an already-recorded integrity repair:
		// old replicas may leave deleted Links with runnable parse attempts or
		// Feed-exclusive Links with no remaining association. Keep this repair
		// in its own ledger entry so every upgraded installation executes it.
		ID: lifecycleRepairMigrationID,
		SQL: []string{
			`SELECT public.lock_library_feed_revisions()`,
			`CREATE TABLE IF NOT EXISTS public.feed_lifecycle_repair_audit (
				link_id uuid NOT NULL,
				classification text NOT NULL,
				details jsonb DEFAULT '{}'::jsonb NOT NULL,
				first_detected_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
				last_observed_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
				CONSTRAINT feed_lifecycle_repair_audit_pkey PRIMARY KEY (link_id, classification),
				CONSTRAINT chk_feed_lifecycle_repair_audit_classification CHECK (
					classification IN ('repaired_feed_managed_orphan', 'ambiguous_subscription_orphan'))
			)`,
			`ALTER TABLE public.links ADD COLUMN IF NOT EXISTS feed_managed boolean;
			 UPDATE public.links AS link
			 SET feed_managed=true,updated_at=CURRENT_TIMESTAMP
			 FROM public.reader_feed_saves AS save
			 WHERE save.link_id=link.id
			   AND save.created_link
			   AND link.source_kind='subscription'
			   AND link.feed_managed IS NULL;
			 UPDATE public.links SET feed_managed=false WHERE feed_managed IS NULL;
			 ALTER TABLE public.links ALTER COLUMN feed_managed SET DEFAULT false;
			 ALTER TABLE public.links ALTER COLUMN feed_managed SET NOT NULL`,
			// 「feed_managed 且没有任何 save」**不能**证明这是个孤儿：
			// reader_feed_saves 对 feed_items 是 ON DELETE CASCADE，而保留策略
			// trimOrdinaryFeedItems 会裁掉已读的普通项，于是一条用户真实保存过、
			// 读完后被裁剪的文章也会落到完全相同的状态。原先据此软删，会静默销毁
			// 用户保存的正文。改为只记录、不销毁，与下面 NOT feed_managed 变体
			// 采用的非破坏性分类保持一致。
			`INSERT INTO public.feed_lifecycle_repair_audit
				(link_id,classification,details)
			 SELECT link.id,'ambiguous_subscription_orphan',
				jsonb_build_object('source_kind',link.source_kind,'action','retain',
					'reason','feed_managed_without_save')
			 FROM public.links AS link
			 WHERE link.deleted_at IS NULL AND link.feed_managed
			   AND NOT EXISTS (
				 SELECT 1 FROM public.reader_feed_saves AS save WHERE save.link_id=link.id)
			 ON CONFLICT (link_id,classification) DO UPDATE
			 SET last_observed_at=CURRENT_TIMESTAMP`,
			// 只对真正已删除的 Link 收尾。未经证实的 feed_managed 伪孤儿不再
			// 参与取消任务 / 置失败 / 软删（见上方审计语句的说明）。
			`SELECT link.id
			 FROM public.links AS link
			 WHERE link.deleted_at IS NOT NULL
			 ORDER BY link.id
			 FOR UPDATE`,
			`WITH target_links AS (
				SELECT link.id
				FROM public.links AS link
				WHERE link.deleted_at IS NOT NULL
			), locked_job AS (
				SELECT job.id,job.queue,job.state,job.finalized_at
				FROM public.river_job AS job
				WHERE job.state IN ('available','pending','retryable','running','scheduled')
				  AND (
					(job.kind='parse_link' AND job.args->>'link_id' IN (SELECT id::text FROM target_links))
					OR (job.kind IN ('translate_link_content','translate_link_v2') AND EXISTS (
						SELECT 1 FROM public.link_translations AS translation
						JOIN target_links AS target ON target.id=translation.link_id
						WHERE translation.id::text=job.args->>'translation_id')))
				ORDER BY job.id
				FOR UPDATE
			), notification AS (
				SELECT id,pg_notify(
					'public.river_control',
					json_build_object('action','cancel','job_id',id,'queue',queue)::text)
				FROM locked_job
				WHERE state NOT IN ('cancelled','completed','discarded') AND finalized_at IS NULL
			), updated_job AS (
				UPDATE public.river_job AS job
				SET state=CASE WHEN job.state='running' THEN job.state ELSE 'cancelled' END,
					finalized_at=CASE WHEN job.state='running' THEN job.finalized_at ELSE COALESCE(job.finalized_at,CURRENT_TIMESTAMP) END,
					metadata=jsonb_set(COALESCE(job.metadata,'{}'::jsonb),'{cancel_attempted_at}',to_jsonb(CURRENT_TIMESTAMP),true)
				FROM notification
				WHERE job.id=notification.id
				RETURNING job.id
			)
			SELECT count(*) FROM updated_job`,
			`UPDATE public.parse_jobs AS attempt
			 SET status='failed',error_msg='link_deleted',updated_at=CURRENT_TIMESTAMP
			 FROM public.links AS link
			 WHERE link.id=attempt.link_id
			   AND link.deleted_at IS NOT NULL
			   AND attempt.status IN ('pending','processing')`,
			`UPDATE public.link_translations AS attempt
			 SET status='failed',error_msg='link_deleted',current_river_job_id=NULL,updated_at=CURRENT_TIMESTAMP
			 FROM public.links AS link
			 WHERE link.id=attempt.link_id
			   AND link.deleted_at IS NOT NULL
			   AND attempt.status IN ('pending','processing')`,
			`INSERT INTO public.feed_lifecycle_repair_audit
				(link_id,classification,details)
			 SELECT link.id,'ambiguous_subscription_orphan',
				jsonb_build_object('source_kind',link.source_kind,'action','report_only')
			 FROM public.links AS link
			 WHERE link.deleted_at IS NULL
			   AND link.source_kind='subscription'
			   AND NOT link.feed_managed
			   AND NOT EXISTS (
				 SELECT 1 FROM public.reader_feed_saves AS save WHERE save.link_id=link.id)
				 ON CONFLICT (link_id,classification) DO UPDATE
				 SET last_observed_at=CURRENT_TIMESTAMP`,
		},
	},
}
