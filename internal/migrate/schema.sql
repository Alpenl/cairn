-- 自动生成；请勿手工编辑。
-- 改 schema 请改 internal/migrate/steps.go，然后跑：
--   make schema-dump
-- 源真相：internal/migrate/steps.go 中的 steps 切片
--
--
-- PostgreSQL database dump
--


-- Dumped from database version 16.14 (Debian 16.14-1.pgdg13+1)
-- Dumped by pg_dump version 16.14 (Debian 16.14-1.pgdg13+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

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
-- Name: river_job_state; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.river_job_state AS ENUM (
    'available',
    'cancelled',
    'completed',
    'discarded',
    'pending',
    'retryable',
    'running',
    'scheduled'
);


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
    END IF;
    RETURN NEW;
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
					 IF TG_OP = 'UPDATE' AND OLD.user_deleted_at IS NOT NULL THEN
						 NEW.user_deleted_at := OLD.user_deleted_at;
					 ELSE
						 NEW.user_deleted_at := COALESCE(NEW.user_deleted_at,CURRENT_TIMESTAMP);
					 END IF;
					 NEW.deleted := true; NEW.body := ''; NEW.target := '{}'::jsonb; NEW.quote := NULL; NEW.source := '';
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
			 $$;


--
-- Name: reader_scrub_user_deleted_thought_event(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reader_scrub_user_deleted_thought_event() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
			 BEGIN
				 IF EXISTS (SELECT 1 FROM public.reader_thoughts WHERE id=NEW.annotation_id AND user_deleted) THEN
					 NEW.loser := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
					 NEW.winner_at_detection := jsonb_build_object('type','user_deleted','id',NEW.annotation_id,'user_deleted',true);
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
				 IF EXISTS (SELECT 1 FROM public.reader_thoughts WHERE id=NEW.annotation_id AND user_deleted) THEN
					 NEW.target := '{}'::jsonb; NEW.payload := '{}'::jsonb;
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
-- Name: river_job_state_in_bitmask(bit, public.river_job_state); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.river_job_state_in_bitmask(bitmask bit, state public.river_job_state) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

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
    CONSTRAINT chk_feed_subscriptions_url CHECK (((char_length(url) >= 1) AND (char_length(url) <= 2048) AND (url ~ '^https?://'::text)))
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
    CONSTRAINT chk_link_translations_source_content_revision CHECK ((((source_content_revision IS NULL) AND (scope = 'selection'::text) AND (block_key = 'summary'::text)) OR ((source_content_revision > 0) AND (block_key = ANY (ARRAY['content'::text, 'content-document'::text]))))),
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
    content text,
    content_document text,
    content_format text DEFAULT 'plain'::text NOT NULL,
    library_kind text,
    library_kind_locked boolean DEFAULT false NOT NULL,
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
    parse_generation bigint DEFAULT 1 NOT NULL,
    deleted_at timestamp with time zone,
    feed_managed boolean DEFAULT false NOT NULL,
    CONSTRAINT chk_links_content_format CHECK ((content_format = ANY (ARRAY['plain'::text, 'markdown'::text, 'html'::text]))),
    CONSTRAINT chk_links_content_revision CHECK ((content_revision > 0)),
    CONSTRAINT chk_links_content_source CHECK ((content_source = ANY (ARRAY['fetched'::text, 'user'::text]))),
    CONSTRAINT chk_links_input_images_array CHECK (((input_images IS NULL) OR (jsonb_typeof(input_images) = 'array'::text))),
    CONSTRAINT chk_links_library_kind CHECK (((library_kind IS NULL) OR (library_kind = ANY (ARRAY['reading'::text, 'site'::text])))),
    CONSTRAINT chk_links_library_kind_lock CHECK (((NOT library_kind_locked) OR (library_kind IS NOT NULL))),
    CONSTRAINT chk_links_metadata_revision_safe CHECK (((metadata_revision >= 1) AND (metadata_revision <= '9007199254740991'::bigint))),
    CONSTRAINT chk_links_parse_generation_safe CHECK (((parse_generation >= 1) AND (parse_generation <= '9007199254740991'::bigint))),
    CONSTRAINT chk_links_site_has_no_content CHECK (((library_kind <> 'site'::text) OR ((summary IS NULL) AND (content IS NULL) AND (content_document IS NULL)))),
    CONSTRAINT chk_links_status CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'done'::text, 'failed'::text])))
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
-- Name: reader_feed_hides; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_feed_hides (
    item_key text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: reader_feed_saves; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reader_feed_saves (
    feed_item_id uuid NOT NULL,
    link_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
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
    identity_key text NOT NULL,
    source_kind text DEFAULT 'url'::text NOT NULL,
    title text,
    body text DEFAULT ''::text NOT NULL,
    summary text,
    suggested_tags text[] DEFAULT '{}'::text[] NOT NULL,
    tags text[] DEFAULT '{}'::text[] NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    metadata_revision bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone DEFAULT (now() + '30 days'::interval),
    deleted_at timestamp with time zone,
    note text DEFAULT ''::text NOT NULL,
    proposal_status text DEFAULT 'idle'::text NOT NULL,
    body_document text,
    body_format text DEFAULT 'plain'::text NOT NULL,
    CONSTRAINT reader_inbox_body_format_check CHECK ((body_format = ANY (ARRAY['plain'::text, 'markdown'::text, 'html'::text]))),
    CONSTRAINT reader_inbox_identity_key_check CHECK ((btrim(identity_key) <> ''::text)),
    CONSTRAINT reader_inbox_proposal_status_check CHECK ((proposal_status = ANY (ARRAY['idle'::text, 'pending'::text, 'running'::text, 'completed'::text, 'failed'::text]))),
    CONSTRAINT reader_inbox_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'confirmed'::text])))
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
    logical_clock bigint NOT NULL,
    recovery_of jsonb,
    expected_winner_key jsonb,
    CONSTRAINT reader_thought_ops_logical_clock_check CHECK (((logical_clock > 0) AND (logical_clock <= '9007199254740991'::bigint))),
    CONSTRAINT reader_thought_ops_operation_kind_check CHECK ((operation_kind = ANY (ARRAY['add'::text, 'update'::text, 'delete'::text]))),
    CONSTRAINT reader_thought_ops_target_kind_check CHECK (((target ->> 'kind'::text) = ANY (ARRAY['saved-content'::text, 'summary'::text, 'note'::text, 'inbox'::text])))
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
    winner_logical_clock bigint NOT NULL,
    winner_device_id text DEFAULT ''::text NOT NULL,
    winner_op_id text DEFAULT ''::text NOT NULL,
    user_deleted boolean DEFAULT false NOT NULL,
    user_deleted_at timestamp with time zone,
    CONSTRAINT chk_reader_thoughts_user_deleted_at CHECK (((NOT user_deleted) OR (user_deleted_at IS NOT NULL))),
    CONSTRAINT chk_reader_thoughts_user_deleted_content CHECK (((NOT user_deleted) OR (deleted AND (body = ''::text) AND (target = '{}'::jsonb) AND (quote IS NULL) AND (source = ''::text)))),
    CONSTRAINT reader_thoughts_target_kind_check CHECK (((user_deleted AND (target = '{}'::jsonb)) OR ((target ->> 'kind'::text) = ANY (ARRAY['saved-content'::text, 'summary'::text, 'note'::text, 'inbox'::text])))),
    CONSTRAINT reader_thoughts_winner_logical_clock_check CHECK (((winner_logical_clock > 0) AND (winner_logical_clock <= '9007199254740991'::bigint)))
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
-- Name: river_job; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_job (
    id bigint NOT NULL,
    state public.river_job_state DEFAULT 'available'::public.river_job_state NOT NULL,
    attempt smallint DEFAULT 0 NOT NULL,
    max_attempts smallint DEFAULT 25 NOT NULL,
    attempted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    finalized_at timestamp with time zone,
    scheduled_at timestamp with time zone DEFAULT now() NOT NULL,
    priority smallint DEFAULT 1 NOT NULL,
    args jsonb NOT NULL,
    attempted_by text[],
    errors jsonb[],
    kind text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    queue text DEFAULT 'default'::text NOT NULL,
    tags character varying(255)[] DEFAULT '{}'::character varying[] NOT NULL,
    unique_key bytea,
    unique_states bit(8),
    CONSTRAINT finalized_or_finalized_at_null CHECK ((((finalized_at IS NULL) AND (state <> ALL (ARRAY['cancelled'::public.river_job_state, 'completed'::public.river_job_state, 'discarded'::public.river_job_state]))) OR ((finalized_at IS NOT NULL) AND (state = ANY (ARRAY['cancelled'::public.river_job_state, 'completed'::public.river_job_state, 'discarded'::public.river_job_state]))))),
    CONSTRAINT kind_length CHECK (((char_length(kind) > 0) AND (char_length(kind) < 128))),
    CONSTRAINT max_attempts_is_positive CHECK ((max_attempts > 0)),
    CONSTRAINT priority_in_range CHECK (((priority >= 1) AND (priority <= 4))),
    CONSTRAINT queue_length CHECK (((char_length(queue) > 0) AND (char_length(queue) < 128)))
);


--
-- Name: river_job_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.river_job_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: river_job_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.river_job_id_seq OWNED BY public.river_job.id;


--
-- Name: river_leader; Type: TABLE; Schema: public; Owner: -
--

CREATE UNLOGGED TABLE public.river_leader (
    elected_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    leader_id text NOT NULL,
    name text DEFAULT 'default'::text NOT NULL,
    CONSTRAINT leader_id_length CHECK (((char_length(leader_id) > 0) AND (char_length(leader_id) < 128))),
    CONSTRAINT name_length CHECK ((name = 'default'::text))
);


--
-- Name: river_migration; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_migration (
    line text NOT NULL,
    version bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT line_length CHECK (((char_length(line) > 0) AND (char_length(line) < 128))),
    CONSTRAINT version_gte_1 CHECK ((version >= 1))
);


--
-- Name: river_notification; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_notification (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    payload text NOT NULL,
    topic text NOT NULL,
    CONSTRAINT topic_length CHECK (((length(topic) > 0) AND (length(topic) < 128)))
);


--
-- Name: river_notification_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.river_notification_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: river_notification_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.river_notification_id_seq OWNED BY public.river_notification.id;


--
-- Name: river_queue; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_queue (
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    paused_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);


--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_migrations (
    version text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: site_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_entries (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    site_id uuid NOT NULL,
    link_id uuid NOT NULL,
    entry_name text NOT NULL,
    purpose text DEFAULT ''::text NOT NULL,
    normalized_url text NOT NULL,
    first_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_recollected_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_site_entries_lengths CHECK (((char_length(entry_name) >= 1) AND (char_length(entry_name) <= 256) AND (char_length(purpose) <= 1000)))
);


--
-- Name: site_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_identities (
    identity_key text NOT NULL,
    site_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: site_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.site_tags (
    site_id uuid NOT NULL,
    tag text NOT NULL,
    normalized_tag text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_site_tags_lengths CHECK (((char_length(tag) >= 1) AND (char_length(tag) <= 128) AND ((char_length(normalized_tag) >= 1) AND (char_length(normalized_tag) <= 128))))
);


--
-- Name: sites; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sites (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    site_key text NOT NULL,
    name text NOT NULL,
    intro text DEFAULT ''::text NOT NULL,
    homepage_url text,
    icon_url text,
    user_note text DEFAULT ''::text NOT NULL,
    pinned boolean DEFAULT false NOT NULL,
    primary_entry_id uuid,
    revision bigint DEFAULT 1 NOT NULL,
    first_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_collected_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_sites_lengths CHECK (((char_length(name) >= 1) AND (char_length(name) <= 256) AND (char_length(intro) <= 1000) AND (char_length(user_note) <= 10000))),
    CONSTRAINT chk_sites_revision CHECK ((revision > 0))
);


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
-- Name: river_job id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_job ALTER COLUMN id SET DEFAULT nextval('public.river_job_id_seq'::regclass);


--
-- Name: river_notification id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_notification ALTER COLUMN id SET DEFAULT nextval('public.river_notification_id_seq'::regclass);


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
-- Name: feed_subscriptions feed_subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feed_subscriptions
    ADD CONSTRAINT feed_subscriptions_pkey PRIMARY KEY (id);


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
-- Name: reader_engagement reader_engagement_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_engagement
    ADD CONSTRAINT reader_engagement_pkey PRIMARY KEY (link_id);


--
-- Name: reader_feed_hides reader_feed_hides_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_hides
    ADD CONSTRAINT reader_feed_hides_pkey PRIMARY KEY (item_key);


--
-- Name: reader_feed_saves reader_feed_saves_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_feed_saves
    ADD CONSTRAINT reader_feed_saves_pkey PRIMARY KEY (feed_item_id);


--
-- Name: reader_host_purge_receipts reader_host_purge_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reader_host_purge_receipts
    ADD CONSTRAINT reader_host_purge_receipts_pkey PRIMARY KEY (host_kind, host_id);


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
-- Name: river_job river_job_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_job
    ADD CONSTRAINT river_job_pkey PRIMARY KEY (id);


--
-- Name: river_leader river_leader_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_leader
    ADD CONSTRAINT river_leader_pkey PRIMARY KEY (name);


--
-- Name: river_migration river_migration_pkey1; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_migration
    ADD CONSTRAINT river_migration_pkey1 PRIMARY KEY (line, version);


--
-- Name: river_notification river_notification_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_notification
    ADD CONSTRAINT river_notification_pkey PRIMARY KEY (id);


--
-- Name: river_queue river_queue_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_queue
    ADD CONSTRAINT river_queue_pkey PRIMARY KEY (name);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);


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
-- Name: idx_link_translations_link_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_link_translations_link_updated ON public.link_translations USING btree (link_id, updated_at DESC);


--
-- Name: idx_link_translations_saved_revision_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_link_translations_saved_revision_unique ON public.link_translations USING btree (link_id, scope, block_key, start_offset, end_offset, source_content_revision, target_language) WHERE (source_content_revision IS NOT NULL);


--
-- Name: idx_link_translations_summary_source_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_link_translations_summary_source_unique ON public.link_translations USING btree (link_id, scope, block_key, start_offset, end_offset, source_hash, target_language) WHERE (source_content_revision IS NULL);


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
-- Name: idx_reader_engagement_continue; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_engagement_continue ON public.reader_engagement USING btree (read, progress DESC, last_opened DESC NULLS LAST);


--
-- Name: idx_reader_feed_saves_link; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_feed_saves_link ON public.reader_feed_saves USING btree (link_id);


--
-- Name: idx_reader_inbox_identity_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_identity_key ON public.reader_inbox USING btree (identity_key);


--
-- Name: idx_reader_inbox_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_pending ON public.reader_inbox USING btree (status, updated_at DESC, id DESC);


--
-- Name: idx_reader_inbox_pending_expiry; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_inbox_pending_expiry ON public.reader_inbox USING btree (expires_at, id) WHERE ((status = 'pending'::text) AND (expires_at IS NOT NULL) AND (deleted_at IS NULL));


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
-- Name: idx_reader_thought_supersession_events_sequence; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_supersession_events_sequence ON public.reader_thought_supersession_events USING btree (sequence);


--
-- Name: idx_reader_thought_tombstones_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_tombstones_order ON public.reader_thought_tombstones USING btree (created_at DESC, thought_id DESC);


--
-- Name: idx_reader_thought_tombstones_search_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thought_tombstones_search_trgm ON public.reader_thought_tombstones USING gin ((((((COALESCE((snapshot ->> 'body'::text), ''::text) || ' '::text) || COALESCE((snapshot ->> 'source'::text), ''::text)) || ' '::text) || COALESCE(((snapshot -> 'quote'::text))::text, ''::text))) public.gin_trgm_ops);


--
-- Name: idx_reader_thoughts_host; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thoughts_host ON public.reader_thoughts USING btree (host_kind, host_id, deleted, updated_at DESC);


--
-- Name: idx_reader_thoughts_search_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_reader_thoughts_search_trgm ON public.reader_thoughts USING gin ((((((body || ' '::text) || source) || ' '::text) || COALESCE((quote)::text, ''::text))) public.gin_trgm_ops);


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
-- Name: river_job_args_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_args_index ON public.river_job USING gin (args);


--
-- Name: river_job_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_kind ON public.river_job USING btree (kind);


--
-- Name: river_job_metadata_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_metadata_index ON public.river_job USING gin (metadata);


--
-- Name: river_job_prioritized_fetching_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_prioritized_fetching_index ON public.river_job USING btree (state, queue, priority, scheduled_at, id);


--
-- Name: river_job_state_and_finalized_at_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_state_and_finalized_at_index ON public.river_job USING btree (state, finalized_at) WHERE (finalized_at IS NOT NULL);


--
-- Name: river_job_unique_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX river_job_unique_idx ON public.river_job USING btree (unique_key) WHERE ((unique_key IS NOT NULL) AND (unique_states IS NOT NULL) AND public.river_job_state_in_bitmask(unique_states, state));


--
-- Name: river_notification_created_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_notification_created_at_idx ON public.river_notification USING btree (created_at);


--
-- Name: river_notification_topic_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_notification_topic_id_idx ON public.river_notification USING btree (topic, id);


--
-- Name: links links_metadata_revision_bump; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER links_metadata_revision_bump BEFORE UPDATE OF title, summary, tags ON public.links FOR EACH ROW EXECUTE FUNCTION public.advance_link_metadata_revision();


--
-- Name: reader_thoughts trg_reader_enforce_user_deleted_thought; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_enforce_user_deleted_thought BEFORE INSERT OR UPDATE ON public.reader_thoughts FOR EACH ROW EXECUTE FUNCTION public.reader_enforce_user_deleted_thought();


--
-- Name: reader_thought_tombstones trg_reader_protect_user_deleted_thought_tombstone; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_protect_user_deleted_thought_tombstone BEFORE INSERT OR DELETE OR UPDATE ON public.reader_thought_tombstones FOR EACH ROW EXECUTE FUNCTION public.reader_protect_user_deleted_thought_tombstone();


--
-- Name: reader_thoughts trg_reader_scrub_user_deleted_thought; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought AFTER INSERT OR UPDATE ON public.reader_thoughts FOR EACH ROW WHEN ((new.user_deleted = true)) EXECUTE FUNCTION public.reader_scrub_user_deleted_thought();


--
-- Name: reader_thought_supersession_events trg_reader_scrub_user_deleted_thought_event; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought_event BEFORE INSERT OR UPDATE OF annotation_id, loser, winner_at_detection ON public.reader_thought_supersession_events FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_event();


--
-- Name: reader_thought_ops trg_reader_scrub_user_deleted_thought_op; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_scrub_user_deleted_thought_op BEFORE INSERT OR UPDATE OF annotation_id, payload, target ON public.reader_thought_ops FOR EACH ROW EXECUTE FUNCTION public.reader_scrub_user_deleted_thought_op();


--
-- Name: links trg_reader_tombstone_deleted_link_thoughts; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_tombstone_deleted_link_thoughts AFTER DELETE ON public.links FOR EACH ROW EXECUTE FUNCTION public.reader_tombstone_deleted_link_thoughts();


--
-- Name: links trg_reader_tombstone_trashed_link_thoughts; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_reader_tombstone_trashed_link_thoughts AFTER UPDATE OF deleted_at ON public.links FOR EACH ROW WHEN (((old.deleted_at IS NULL) AND (new.deleted_at IS NOT NULL))) EXECUTE FUNCTION public.reader_tombstone_trashed_link_thoughts();


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


--
-- PostgreSQL database dump complete
--
