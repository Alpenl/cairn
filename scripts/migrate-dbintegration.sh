#!/usr/bin/env bash
set -euo pipefail

repository=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repository"

image=${PG_IMAGE:-postgres:16}
container_name="cairn-migrate-dbintegration-$$"
database_name=cairn_migrate_test
database_password=migrate_test_pw

cleanup() {
    docker rm -f "$container_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --rm \
    --name "$container_name" \
    -e POSTGRES_PASSWORD="$database_password" \
    -e POSTGRES_DB="$database_name" \
    -p 127.0.0.1::5432 \
    "$image" >/dev/null

binding=$(docker port "$container_name" 5432/tcp)
port=${binding##*:}
database_url="postgres://postgres:${database_password}@127.0.0.1:${port}/${database_name}?sslmode=disable"

for attempt in $(seq 1 30); do
    ready_count=$(docker logs "$container_name" 2>&1 \
        | awk '/database system is ready to accept connections/ { count++ } END { print count + 0 }')
    if [ "$ready_count" -ge 2 ] \
        && docker exec "$container_name" pg_isready -U postgres -d "$database_name" >/dev/null 2>&1 \
        && DATABASE_URL="$database_url" go run ./cmd/migrate --plan-json >/dev/null 2>&1; then
        break
    fi
    if [ "$attempt" -eq 30 ]; then
        docker logs "$container_name" >&2 || true
        echo "migration dbintegration postgres did not become ready" >&2
        exit 1
    fi
    sleep 1
done

echo ">> applying migrations for internal/migrate dbintegration"
DATABASE_URL="$database_url" go run ./cmd/migrate

echo ">> running internal/migrate dbintegration"
WEBTAG_TEST_DATABASE_URL="$database_url" \
    go test -tags=dbintegration -count=1 -timeout=120s ./internal/migrate
