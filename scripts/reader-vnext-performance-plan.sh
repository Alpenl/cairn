#!/usr/bin/env bash
# Generate a real PostgreSQL EXPLAIN artifact for the Reader vNext performance
# harness from a temporary migrated database and a fixed small seed.
set -euo pipefail
IFS=$'\n\t'

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
PG_IMAGE="${PG_IMAGE:-pgvector/pgvector:pg16}"
PG_PORT="${PG_PORT:-55438}"
OUT_FILE=""

usage() {
  cat <<'EOF'
Usage:
  scripts/reader-vnext-performance-plan.sh --out FILE [--pg-port PORT]
EOF
}

fail() {
  echo "reader-vnext-performance-plan: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      [[ $# -ge 2 ]] || fail "--out requires a value"
      OUT_FILE=$2
      shift 2
      ;;
    --pg-port)
      [[ $# -ge 2 ]] || fail "--pg-port requires a value"
      PG_PORT=$2
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$OUT_FILE" ]] || fail "--out is required"
[[ "$PG_PORT" =~ ^[0-9]+$ ]] || fail "--pg-port must be numeric"

command -v docker >/dev/null 2>&1 || fail "docker is required"
command -v psql >/dev/null 2>&1 || fail "psql is required"
command -v go >/dev/null 2>&1 || fail "go is required"

CONTAINER_NAME="webtag-reader-perf-$$"
PG_PASSWORD="reader_perf_pw"
PG_DB="webtag_reader_perf"

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cd "$ROOT"

mkdir -p "$(dirname "$OUT_FILE")"

docker run -d --rm \
  --name "$CONTAINER_NAME" \
  -e POSTGRES_PASSWORD="$PG_PASSWORD" \
  -e POSTGRES_DB="$PG_DB" \
  -p "$PG_PORT:5432" \
  "$PG_IMAGE" >/dev/null

for i in $(seq 1 30); do
  if docker exec "$CONTAINER_NAME" pg_isready -U postgres -d "$PG_DB" >/dev/null 2>&1; then
    break
  fi
  sleep 1
  if [[ "$i" -eq 30 ]]; then
    docker logs "$CONTAINER_NAME" >&2 || true
    fail "postgres did not become ready"
  fi
done

export DATABASE_URL="postgres://postgres:$PG_PASSWORD@localhost:$PG_PORT/$PG_DB?sslmode=disable"
MIGRATION_TARGET=f03e51d6911b go run ./cmd/migrate >/dev/null
go run ./cmd/migrate >/dev/null

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
INSERT INTO links (
  id, url, title, summary, tags, fetcher_type, status, domain,
  path_depth, source_key, library_kind, library_kind_source, content,
  content_format, content_source, first_collected_at, created_at, updated_at
)
SELECT
  gen_random_uuid(),
  format('https://perf.example.test/article-%s', n),
  format('Performance article %s', n),
  'Seeded summary',
  ARRAY['perf','reader'],
  'stored',
  'done',
  'perf.example.test',
  1,
  format('perf-seed-%s', n),
  'reading',
  'user',
  'Seeded body',
  'plain',
  'fetched',
  now() - (n || ' minutes')::interval,
  now() - (n || ' minutes')::interval,
  now() - (n || ' minutes')::interval
FROM generate_series(1,10) AS n;

INSERT INTO reader_inbox (
  url, source_kind, title, body, summary, suggested_tags, tags,
  created_at, updated_at
)
SELECT
  format('https://capture.example.test/pending-%s', n),
  'browser_capture',
  format('Pending capture %s', n),
  'Captured body',
  'Captured summary',
  ARRAY['capture'],
  ARRAY['capture'],
  now() - (n || ' minutes')::interval,
  now() - (n || ' minutes')::interval
FROM generate_series(1,3) AS n;

INSERT INTO reader_notes (title,published_content,published_revision,created_at,updated_at)
SELECT format('Performance note %s', n), '# Note', 1, now(), now()
FROM generate_series(1,2) AS n;

INSERT INTO reader_thoughts (
  id, host_kind, host_id, link_id, target, quote, body, source,
  last_sequence, created_at, updated_at
)
SELECT
  format('perf-thought-%s', n),
  'link',
  (SELECT id::text FROM links ORDER BY created_at DESC LIMIT 1),
  (SELECT id FROM links ORDER BY created_at DESC LIMIT 1),
  '{}'::jsonb,
  '{}'::jsonb,
  format('Performance thought %s', n),
  'user',
  n,
  now(),
  now()
FROM generate_series(1,3) AS n;

INSERT INTO reader_todos (text,due_at,origin_kind,created_at,updated_at)
SELECT format('Performance todo %s', n), now() + (n || ' days')::interval, 'standalone', now(), now()
FROM generate_series(1,3) AS n;

INSERT INTO reader_engagement (link_id,read,progress,read_later,last_opened)
SELECT id, false, 0.5, false, now()
FROM links
ORDER BY created_at DESC
LIMIT 3;

INSERT INTO reader_feed_snapshots (mode,items)
VALUES ('recommended','[{"key":"seed","source":"reading"}]'::jsonb);
SQL

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -tA -o "$OUT_FILE" <<'SQL'
EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT
  snapshot.id,
  snapshot.mode,
  snapshot.created_at,
  (SELECT count(*) FROM reader_inbox WHERE status = 'pending') AS pending_count,
  (SELECT count(*) FROM reader_todos WHERE NOT done AND deleted_at IS NULL) AS open_todo_count
FROM reader_feed_snapshots snapshot
ORDER BY snapshot.created_at DESC
LIMIT 1;
SQL

[[ -s "$OUT_FILE" ]] || fail "EXPLAIN output was not written"
printf 'postgres plan written: %s\n' "$OUT_FILE"
