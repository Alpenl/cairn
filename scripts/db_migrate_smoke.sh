#!/usr/bin/env sh
set -eu

# pgvector/pgvector:pg16 (stock postgres:16 + pgvector extension) is required
# since the Phase 5 (v3.0) migration runs CREATE EXTENSION vector + builds an
# HNSW index. Plain postgres:16-alpine cannot satisfy it. Keep in sync with
# test/dbintegration/postgres.go and scripts/container_smoke.sh.
container_id="$(docker run -d --rm --name webtag-db-smoke -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=webtag -p 127.0.0.1::5432 pgvector/pgvector:pg16)"

cleanup() {
	docker rm -f "$container_id" >/dev/null 2>&1 || true
}

trap cleanup EXIT INT TERM

database_port="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$container_id")"
until docker exec "$container_id" pg_isready -U postgres >/dev/null 2>&1; do
	sleep 1
done

database_url="postgres://postgres:postgres@127.0.0.1:${database_port}/webtag?sslmode=disable"
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

# Ordinary Up installs the complete public schema. The explicit fresh target
# remains a compatibility alias and must be idempotent on the same database.
DATABASE_URL="$database_url" make migrate-fresh
expect "select count(*) from schema_migrations where version = 'f03e51d6911b'" "1"
expect "select count(*) from schema_migrations" "7"
expect "select to_regclass('public.idx_link_translations_saved_revision_unique')" "idx_link_translations_saved_revision_unique"
expect "select to_regclass('public.idx_link_translations_legacy_source_unique')" "idx_link_translations_legacy_source_unique"

# Assert the latest self-authored migration applied. Update this version every
# time the tail of internal/migrate/steps.go gains a new step so the smoke test
# actually exercises forward migrations and not just the historical schema.
# 'b671c9d2e411' builds the terminal-history index on river_job.
expect "select version from schema_migrations where version = 'b671c9d2e411'" "b671c9d2e411"
expect "select version from schema_migrations where version = 'reader2026081301'" "reader2026081301"
expect "select version from schema_migrations where version = 'integrity2026081401'" "integrity2026081401"
expect "select version from schema_migrations where version = 'historical2026081401'" "historical2026081401"
expect "select version from schema_migrations where version = 'conceptaudit2026081401'" "conceptaudit2026081401"
expect "select version from schema_migrations where version = 'lifecycle2026081401'" "lifecycle2026081401"
expect "select to_regclass('public.feed_lifecycle_repair_audit')" "feed_lifecycle_repair_audit"
# The River index is built with CREATE INDEX CONCURRENTLY. A canceled or
# failed build leaves a same-name index with indisvalid=false that IF NOT EXISTS
# would accept, so recording the migration is not evidence the index is usable —
# assert validity, not just presence. The river_job one additionally proves the
# runner applied River's own migrations before the WebTag steps that depend on
# that table.
expect "select indisvalid::text from pg_index where indexrelid = to_regclass('public.idx_river_job_translation_terminal_history')" "true"
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
expect "select (select count(*) from installation_state)+(select count(*) from library_read_revision)+(select count(*) from global_read_revision)+(select count(*) from feed_read_revision)" "4"
expect "select count(*) from feed_subscriptions" "1"
# Assert River's bundled schema migrations (river_job etc.) also ran — Phase 13
# wired rivermigrate into internal/migrate.Up so the single `migrate` entry
# point now provisions the River queue tables too. to_regclass returns empty
# when the table is absent, so an exact match on the name is the assertion.
expect "select to_regclass('public.river_job')" "river_job"
