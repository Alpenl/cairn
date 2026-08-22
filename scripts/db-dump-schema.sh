#!/usr/bin/env bash
# scripts/db-dump-schema.sh —— 把 fresh install migration 应用后的完整 schema
# 用 pg_dump 落到 internal/migrate/schema.sql。
#
# 用途：
#   - schema.sql 是 *生成产物*，不是 source of truth。源真相是
#     internal/migrate/install_schema.sql 里的 fresh-install baseline，
#     以及 River / migration runner 自己创建的 ledger 对象。
#   - 这份 dump 主要给：
#       1. PR reviewer 一眼看清"这次迁移到底改了 schema 哪几张表"；
#       2. DBA / 运维定位字段、索引时不用临时连 prod 跑 \d；
#       3. CI 可以 diff 这份文件，发现 install baseline 没有被同步 dump
#          就让构建失败。
#   - 每次修改 fresh baseline 后必须重新运行本脚本并审查 schema.sql diff。
#
# 运行方式：
#   ./scripts/db-dump-schema.sh        # 默认 image=postgres:16，随机 localhost 端口
#   PG_IMAGE=postgres:15 ./scripts/db-dump-schema.sh
#   PG_PORT=55432 ./scripts/db-dump-schema.sh  # 调试时固定端口
#
# 退出码：
#   0 成功；非 0 = docker 不可用 / migrate 失败 / pg_dump 失败。
#
# 依赖：docker；本地构建 cmd/migrate 由脚本接管，无需提前 make build。
set -euo pipefail

PG_IMAGE="${PG_IMAGE:-postgres:16}"
CONTAINER_NAME="webtag-schema-dump-$$"
PG_PASSWORD="schema_dump_pw"
PG_DB="webtag_schema_dump"
PG_PORT="${PG_PORT:-}"
OUT_FILE="${OUT_FILE:-internal/migrate/schema.sql}"

cleanup() {
    # 永远尝试清理容器；忽略 not-found / already-removed 的退出码。
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cd "$(dirname "$0")/.."

# 0. 确认 docker 可用。
if ! command -v docker >/dev/null 2>&1; then
    echo "docker not found; install Docker Desktop or the docker CLI" >&2
    exit 1
fi

publish_arg="127.0.0.1::5432"
if [ -n "$PG_PORT" ]; then
    publish_arg="127.0.0.1:$PG_PORT:5432"
    echo ">> starting $PG_IMAGE on 127.0.0.1:$PG_PORT"
else
    echo ">> starting $PG_IMAGE on a random localhost port"
fi

# 1. 启动一次性 postgres 容器。-p 暴露到 host 端口供 migrate 工具连。
docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -e POSTGRES_PASSWORD="$PG_PASSWORD" \
    -e POSTGRES_DB="$PG_DB" \
    -p "$publish_arg" \
    "$PG_IMAGE" >/dev/null

if [ -z "$PG_PORT" ]; then
    PG_PORT="$(docker port "$CONTAINER_NAME" 5432/tcp | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -n 1)"
    if [ -z "$PG_PORT" ]; then
        echo "could not discover published postgres port" >&2
        docker logs "$CONTAINER_NAME" >&2 || true
        exit 1
    fi
    echo ">> published postgres on 127.0.0.1:$PG_PORT"
fi

# 2. 等待 postgres 就绪。pg_isready 在容器内跑，最多等 30 秒。
echo ">> waiting for postgres to accept connections"
for i in $(seq 1 30); do
    if docker exec "$CONTAINER_NAME" pg_isready -U postgres -d "$PG_DB" >/dev/null 2>&1; then
        break
    fi
    sleep 1
    if [ "$i" -eq 30 ]; then
        echo "postgres did not become ready in 30s" >&2
        docker logs "$CONTAINER_NAME" >&2 || true
        exit 1
    fi
done

# 3. 用真正迁移工具会使用的 host DATABASE_URL 做有界连接探测。
export DATABASE_URL="postgres://postgres:$PG_PASSWORD@127.0.0.1:$PG_PORT/$PG_DB?sslmode=disable"
for i in $(seq 1 30); do
    if go run ./cmd/migrate --plan-json >/dev/null 2>&1; then
        break
    fi
    sleep 1
    if [ "$i" -eq 30 ]; then
        echo "postgres accepted container-local probes but not the published endpoint" >&2
        docker logs "$CONTAINER_NAME" >&2 || true
        exit 1
    fi
done

# 4. 跑迁移。用 go run 而非 make build → bin/migrate，避免对本机构建链
#    施加额外要求；CI 上 go 必然可用。
echo ">> applying fresh-install migration plan via cmd/migrate"
go run ./cmd/migrate

expect() {
    local sql="$1"
    local want="$2"
    local got
    got="$(docker exec "$CONTAINER_NAME" psql -U postgres -d "$PG_DB" -tAc "$sql" | tr -d '[:space:]')"
    if [ "$got" != "$want" ]; then
        echo "schema dump migration assertion failed" >&2
        echo "  SQL:  $sql" >&2
        echo "  want: '$want'" >&2
        echo "  got:  '$got'" >&2
        exit 1
    fi
}

expect "SELECT version FROM schema_migrations" "schema2026082201"
expect "SELECT count(*) FROM schema_migrations" "1"
expect "SELECT to_regclass('public.idx_reader_thoughts_search_trgm')" "idx_reader_thoughts_search_trgm"
expect "SELECT to_regclass('public.idx_reader_thought_tombstones_search_trgm')" "idx_reader_thought_tombstones_search_trgm"
# The tsvector index that no `%query%` ILIKE predicate could ever use must be gone.
expect "SELECT coalesce(to_regclass('public.idx_reader_thought_search')::text,'none')" "none"
# Both trigram indexes must be valid: CREATE INDEX CONCURRENTLY can leave an
# indisvalid=false relation behind, and to_regclass would still resolve it.
expect "SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname IN ('idx_reader_thoughts_search_trgm','idx_reader_thought_tombstones_search_trgm') AND i.indisvalid AND i.indisready" "2"
expect "SELECT coalesce(to_regclass('public.feed_lifecycle_repair_audit')::text,'none')" "none"
expect "SELECT coalesce(to_regclass('public.reader_todo_projection_backfills')::text,'none')" "none"
expect "SELECT count(*) FROM pg_extension WHERE extname='vector'" "0"
expect "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('concept','concept_alias','concept_merge_proposal','link_concept','library_classification_rules','library_review_items','reader_content_history','reader_todo_projection_backfills','feed_lifecycle_repair_audit')" "0"
expect "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='links' AND column_name IN ('embedding','embedding_model')) OR (table_name='sites' AND column_name IN ('needs_review','embedding','embedding_model')) OR (table_name='site_tags' AND column_name='concept_id'))" "0"
expect "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='sites' AND column_name IN ('name_source','intro_source','homepage_source','icon_source','primary_source','grouping_locked')) OR (table_name='site_entries' AND column_name IN ('entry_name_source','purpose_source')) OR (table_name='site_tags' AND column_name='source') OR (table_name='site_identities' AND column_name IN ('source','locked')))" "0"
expect "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('reader_categories','reader_categorizables')" "0"
# 收件箱必须能带着采集到的结构走到确认入库那一步，否则 content_document 只能
# 拿压平的纯文本冒充 markdown。
expect "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='reader_inbox' AND column_name IN ('body_document','body_format')" "2"
expect "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='reader_inbox_jobs'" "0"
expect "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='reader_inbox' AND column_name IN ('job_id','proposal_signals','expired_at','expiry_lease_id','expiry_lease_until')" "0"
expect "SELECT count(*) FROM pg_constraint WHERE conname='reader_inbox_body_format_check' AND conrelid='public.reader_inbox'::regclass" "1"
expect "SELECT to_regclass('public.idx_link_translations_saved_revision_unique')" "idx_link_translations_saved_revision_unique"
expect "SELECT to_regclass('public.idx_link_translations_summary_source_unique')" "idx_link_translations_summary_source_unique"
# Translation terminal projection is worker-owned. The two indexes used only by
# the retired history/missing-row reconciler must not survive the cleanup step.
expect "SELECT coalesce(to_regclass('public.idx_link_translations_missing_reconcile')::text,'none')" "none"
expect "SELECT coalesce(to_regclass('public.idx_river_job_translation_terminal_history')::text,'none')" "none"
expect "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND column_name='tenant_id'" "0"
expect "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('tenants','api_keys','usage_events','feed_bootstraps','tenant_read_revision','tenant_feed_revision')" "0"
expect "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' AND (c.relrowsecurity OR c.relforcerowsecurity)" "0"
expect "SELECT count(*) FROM pg_policies WHERE schemaname='public'" "0"
expect "SELECT count(*) FROM installation_state" "1"
expect "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('library_read_revision','global_read_revision','feed_read_revision')" "0"
expect "SELECT count(*) FROM pg_trigger AS trg JOIN pg_class AS rel ON rel.oid=trg.tgrelid JOIN pg_namespace AS ns ON ns.oid=rel.relnamespace JOIN pg_proc AS proc ON proc.oid=trg.tgfoid WHERE NOT trg.tgisinternal AND ns.nspname='public' AND proc.proname LIKE 'bump_%'" "0"
expect "SELECT count(*) FROM pg_proc AS proc JOIN pg_namespace AS ns ON ns.oid=proc.pronamespace WHERE ns.nspname='public' AND proc.proname IN ('lock_library_feed_revisions','lock_library_global_revisions','lock_representation_revisions')" "0"
expect "SELECT count(*) FROM pg_proc AS proc JOIN pg_namespace AS ns ON ns.oid=proc.pronamespace WHERE ns.nspname='public' AND proc.proname IN ('guard_representation_write_gate','lock_representation_write_gate_shared','lock_representation_write_gate_exclusive')" "0"
expect "SELECT count(*) FROM pg_trigger AS trg JOIN pg_class AS rel ON rel.oid=trg.tgrelid JOIN pg_namespace AS ns ON ns.oid=rel.relnamespace JOIN pg_proc AS proc ON proc.oid=trg.tgfoid WHERE NOT trg.tgisinternal AND ns.nspname='public' AND proc.proname='guard_representation_write_gate'" "0"
expect "SELECT count(*) FROM feed_subscriptions" "1"

# 5. 导出 schema。--schema-only 跳过数据；--no-owner / --no-privileges
#    去掉 OWNER TO / GRANT 噪音，让 dump 在不同环境之间稳定（不会因为
#    本地 postgres 用户名不同而每次 diff 都变化）。
echo ">> dumping schema to $OUT_FILE"
mkdir -p "$(dirname "$OUT_FILE")"
docker exec "$CONTAINER_NAME" pg_dump \
    -U postgres \
    -d "$PG_DB" \
    --schema-only \
    --no-owner \
    --no-privileges \
    | sed '/^\\restrict /d; /^\\unrestrict /d' \
    > "$OUT_FILE.tmp"
# 过滤 \restrict / \unrestrict 行：PG16 引入的随机 session-id 标记，每次
# dump 都会变。保留它会让 schema.sql 在没改 migration 时也 diff，破坏
# "schema.sql 稳定 + 改了再变" 的语义。

# pg_dump versions can emit more than one blank line after the completion
# marker. Strip only trailing empty lines so the tracked snapshot is stable
# across supported PostgreSQL images while preserving every SQL statement.
NORMALIZED_FILE="$OUT_FILE.normalized"
awk '{ lines[NR] = $0 } END {
    last = NR
    while (last > 0 && lines[last] == "") last--
    for (i = 1; i <= last; i++) print lines[i]
}' "$OUT_FILE.tmp" > "$NORMALIZED_FILE"

# 在最前面加一段 banner，提示这是生成产物，避免有人手动改了再 commit。
{
    echo "-- 自动生成；请勿手工编辑。"
    echo "-- 改 fresh schema 请改 internal/migrate/install_schema.sql，然后跑："
    echo "--   make schema-dump"
    echo "-- 源真相：internal/migrate/install_schema.sql 与 River / migration ledger runner"
    echo "--"
    cat "$NORMALIZED_FILE"
} > "$OUT_FILE"
rm -f "$OUT_FILE.tmp" "$NORMALIZED_FILE"

echo ">> done: $OUT_FILE ($(wc -l < "$OUT_FILE") lines)"
