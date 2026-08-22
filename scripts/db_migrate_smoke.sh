#!/usr/bin/env sh
set -eu

container_name="webtag-db-smoke-$$"
container_id="$(docker run -d --rm --name "$container_name" -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=webtag -p 127.0.0.1::5432 postgres:16)"

cleanup() {
	docker rm -f "$container_id" >/dev/null 2>&1 || true
}

trap cleanup EXIT INT TERM

binding="$(docker port "$container_id" 5432/tcp)"
database_port="${binding##*:}"
database_url="postgres://postgres:postgres@127.0.0.1:${database_port}/webtag?sslmode=disable"

for attempt in $(seq 1 30); do
	ready_count="$(docker logs "$container_id" 2>&1 |
		awk '/database system is ready to accept connections/ { count++ } END { print count + 0 }')"
	if [ "$ready_count" -ge 2 ] &&
		docker exec "$container_id" pg_isready -U postgres -d webtag >/dev/null 2>&1 &&
		DATABASE_URL="$database_url" go run ./cmd/migrate --plan-json >/dev/null 2>&1; then
		break
	fi
	if [ "$attempt" -eq 30 ]; then
		docker logs "$container_id" >&2 || true
		echo "migration smoke postgres did not become ready through the published endpoint" >&2
		exit 1
	fi
	sleep 1
done

DATABASE_URL="$database_url" go run ./cmd/migrate

# expect <sql> <期望值> —— 精确比较，不是子串匹配。
#
# 原先每条断言写作 `psql ... | grep -q 1`，有两个毛病：
#   1. grep 是**子串**匹配。`grep -q 3` 对 '13'/'23'/'30' 全部命中，count 类
#      断言因此可能在数值错误时照样通过。
#   2. 失败时 grep 什么都不打印，只留一个退出码，排查得重新手跑 SQL。
#
# 这两点此前都无所谓——go-verify 里这个脚本的退出码被 `| tee` 吞掉了，断言
# 写成什么样都不影响结果。加上 shell: bash（pipefail）之后它才开始真正承重，
# 所以一并改成精确比较 + 失败时打印期望/实际。
expect() {
	_sql="$1"
	_want="$2"
	_got="$(docker exec "$container_id" psql -U postgres -d webtag -tAc "$_sql" | tr -d '[:space:]')"
	if [ "$_got" != "$_want" ]; then
		echo "迁移 smoke 断言失败" >&2
		echo "  SQL:  $_sql" >&2
		echo "  期望: '$_want'" >&2
		echo "  实际: '$_got'" >&2
		exit 1
	fi
}

# Ordinary Up installs the complete public schema and rerunning it is a no-op.
# The explicit fresh target intentionally rejects every non-empty ledger.
DATABASE_URL="$database_url" go run ./cmd/migrate
expect "select version from schema_migrations" "schema2026082201"
expect "select count(*) from schema_migrations" "1"
expect "select to_regclass('public.idx_link_translations_saved_revision_unique')" "idx_link_translations_saved_revision_unique"
expect "select to_regclass('public.idx_link_translations_summary_source_unique')" "idx_link_translations_summary_source_unique"

# Thought search is a `%query%` ILIKE contract, so the two trigram indexes are
# the ones the planner can actually consume; the old tsvector index must be gone.
expect "select indisvalid::text from pg_index where indexrelid = to_regclass('public.idx_reader_thoughts_search_trgm')" "true"
expect "select indisvalid::text from pg_index where indexrelid = to_regclass('public.idx_reader_thought_tombstones_search_trgm')" "true"
expect "select coalesce(to_regclass('public.idx_reader_thought_search')::text,'none')" "none"
expect "select coalesce(to_regclass('public.feed_lifecycle_repair_audit')::text,'none')" "none"
expect "select coalesce(to_regclass('public.reader_todo_projection_backfills')::text,'none')" "none"
# 收件箱要能把采集到的 Markdown 结构一路带到确认入库，否则 links.content_document
# 只剩压平的纯文本可写，还会被贴上 content_format='markdown' 的假标签。
expect "select count(*) from pg_extension where extname = 'vector'" "0"
expect "select count(*) from information_schema.tables where table_schema = 'public' and table_name in ('concept','concept_alias','concept_merge_proposal','link_concept','library_classification_rules','library_review_items','reader_content_history','reader_todo_projection_backfills','feed_lifecycle_repair_audit')" "0"
expect "select count(*) from information_schema.columns where table_schema = 'public' and ((table_name = 'links' and column_name in ('embedding','embedding_model')) or (table_name = 'sites' and column_name in ('needs_review','embedding','embedding_model')) or (table_name = 'site_tags' and column_name = 'concept_id'))" "0"
expect "select count(*) from information_schema.columns where table_schema='public' and ((table_name='sites' and column_name in ('name_source','intro_source','homepage_source','icon_source','primary_source','grouping_locked')) or (table_name='site_entries' and column_name in ('entry_name_source','purpose_source')) or (table_name='site_tags' and column_name='source') or (table_name='site_identities' and column_name in ('source','locked')))" "0"
expect "select count(*) from information_schema.tables where table_schema='public' and table_name in ('reader_categories','reader_categorizables')" "0"
expect "select count(*) from information_schema.columns where table_schema='public' and table_name='reader_inbox' and column_name in ('body_document','body_format')" "2"
expect "select count(*) from information_schema.tables where table_schema='public' and table_name='reader_inbox_jobs'" "0"
expect "select count(*) from information_schema.columns where table_schema='public' and table_name='reader_inbox' and column_name in ('job_id','proposal_signals','expired_at','expiry_lease_id','expiry_lease_until')" "0"
# Both indexes existed only for the retired terminal reconciler.
expect "select coalesce(to_regclass('public.idx_link_translations_missing_reconcile')::text,'none')" "none"
expect "select coalesce(to_regclass('public.idx_river_job_translation_terminal_history')::text,'none')" "none"
expect "select to_regclass('public.link_translations')" "link_translations"
# Saved content keeps the legacy plain-text projection while adding an optional
# reading document and a rollback-compatible explicit format default.
expect "select count(*) from information_schema.columns where table_schema = 'public' and table_name = 'links' and ((column_name = 'content_document' and data_type = 'text' and is_nullable = 'YES') or (column_name = 'content_format' and data_type = 'text' and is_nullable = 'NO' and column_default = '''plain''::text'))" "2"
expect "select count(*) from pg_constraint where conname = 'chk_links_content_format'" "1"
expect "select count(*) from information_schema.columns where table_schema = 'public' and table_name = 'links' and column_name = 'content_source' and data_type = 'text' and is_nullable = 'NO' and column_default = '''fetched''::text'" "1"
expect "select count(*) from pg_constraint where conrelid = 'links'::regclass and conname = 'chk_links_content_source'" "1"
# Deep Research 下线（迁移 f4d88b703004）：表与 links 上那三列 CAS 声明字段
# 都必须消失。留一条反向断言，防止哪天历史迁移被改动又把它们带回来。
expect "select count(*) from information_schema.tables where table_schema = 'public' and table_name = 'link_deep_research'" "0"
expect "select count(*) from information_schema.columns where table_schema = 'public' and table_name = 'links' and column_name like 'deep_research%'" "0"
# Single-install negative guarantees: no tenant namespace, commercial tables,
# row-level isolation, or policies may be reintroduced by migration drift.
expect "select count(*) from information_schema.columns where table_schema='public' and column_name='tenant_id'" "0"
expect "select count(*) from information_schema.tables where table_schema='public' and table_name in ('tenants','api_keys','usage_events','feed_bootstraps','tenant_read_revision','tenant_feed_revision')" "0"
expect "select count(*) from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname='public' and c.relkind='r' and (c.relrowsecurity or c.relforcerowsecurity)" "0"
expect "select count(*) from pg_policies where schemaname='public'" "0"
expect "select count(*) from installation_state" "1"
expect "select count(*) from information_schema.tables where table_schema='public' and table_name in ('library_read_revision','global_read_revision','feed_read_revision')" "0"
expect "select count(*) from pg_trigger as trg join pg_class as rel on rel.oid=trg.tgrelid join pg_namespace as ns on ns.oid=rel.relnamespace join pg_proc as proc on proc.oid=trg.tgfoid where not trg.tgisinternal and ns.nspname='public' and proc.proname like 'bump_%'" "0"
expect "select count(*) from pg_proc as proc join pg_namespace as ns on ns.oid=proc.pronamespace where ns.nspname='public' and proc.proname in ('lock_library_feed_revisions','lock_library_global_revisions','lock_representation_revisions')" "0"
expect "select count(*) from pg_proc as proc join pg_namespace as ns on ns.oid=proc.pronamespace where ns.nspname='public' and proc.proname in ('guard_representation_write_gate','lock_representation_write_gate_shared','lock_representation_write_gate_exclusive')" "0"
expect "select count(*) from pg_trigger as trg join pg_class as rel on rel.oid=trg.tgrelid join pg_namespace as ns on ns.oid=rel.relnamespace join pg_proc as proc on proc.oid=trg.tgfoid where not trg.tgisinternal and ns.nspname='public' and proc.proname='guard_representation_write_gate'" "0"
expect "select count(*) from feed_subscriptions" "1"
# Assert River's bundled schema migrations (river_job etc.) also ran — Phase 13
# wired rivermigrate into internal/migrate.Up so the single `migrate` entry
# point now provisions the River queue tables too. to_regclass returns empty
# when the table is absent, so an exact match on the name is the assertion.
expect "select to_regclass('public.river_job')" "river_job"
