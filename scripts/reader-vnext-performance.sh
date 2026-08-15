#!/usr/bin/env bash
# reader-vnext-performance.sh
#
# Candidate-evidence harness for PERF-BASE-01. It deliberately refuses to
# produce a baseline report unless the run has a named fixture, an isolated
# browser port, a real PostgreSQL EXPLAIN JSON artifact, Reader build stats,
# and a Chromium trace.
set -euo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly PNPM_BIN=(corepack pnpm@10.13.1)

COMMAND=""
FIXTURE_MANIFEST=""
READER_SPEC=""
ARTIFACT_DIR=""
REPORT=""

usage() {
  cat <<'EOF'
Usage:
  READER_E2E_PORT=PORT reader-vnext-performance.sh baseline \
    --fixture-manifest FILE --reader-spec FILE --artifact-dir DIR --report FILE

Required environment for baseline:
  READER_E2E_PORT                 Isolated, registered Chromium / Vite port.
  READER_PERF_POSTGRES_PLAN_FILE A real EXPLAIN (FORMAT JSON) artifact for the fixture.
EOF
}

fail() {
  echo "reader-vnext-performance: $*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is not installed: $1"
}

absolute_path() {
  local value=$1
  if [[ "$value" == /* ]]; then
    printf '%s\n' "$value"
  else
    printf '%s/%s\n' "$ROOT" "$value"
  fi
}

require_file() {
  local label=$1
  local file=$2
  [[ -f "$file" ]] || fail "$label does not exist: $file"
}

require_new_path() {
  local label=$1
  local path=$2
  [[ ! -e "$path" ]] || fail "$label already exists; use a fresh run id: $path"
}

validate_port() {
  local port=${READER_E2E_PORT:-}
  [[ "$port" =~ ^[0-9]+$ ]] || fail "READER_E2E_PORT must be an explicitly assigned integer"
  ((port >= 1024 && port <= 65535)) || fail "READER_E2E_PORT must be between 1024 and 65535"

  if command -v ss >/dev/null 2>&1; then
    local listeners
    listeners=$(ss -H -ltn "sport = :$port" 2>/dev/null || true)
    [[ -z "$listeners" ]] || fail "READER_E2E_PORT is already listening: $port"
    return
  fi
  if command -v lsof >/dev/null 2>&1; then
    if lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | grep -q .; then
      fail "READER_E2E_PORT is already listening: $port"
    fi
    return
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    exec 3>&-
    fail "READER_E2E_PORT is already accepting connections: $port"
  fi
}

validate_manifest() {
  local file=$1
  python3 - "$file" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    manifest = json.load(handle)

if manifest.get("schema_version") != 1:
    raise SystemExit("fixture manifest schema_version must be 1")
status = manifest.get("fixture_status")
if status not in {"candidate-seeded", "baseline-reviewed"}:
    raise SystemExit("fixture manifest must be candidate-seeded or baseline-reviewed")
for key in ("fixture_id", "seed", "reader_journey", "postgres_plan_artifact_env"):
    if not isinstance(manifest.get(key), str) or not manifest[key].strip():
        raise SystemExit(f"fixture manifest requires non-empty {key}")
if manifest["postgres_plan_artifact_env"] != "READER_PERF_POSTGRES_PLAN_FILE":
    raise SystemExit("fixture manifest must bind the standard PostgreSQL plan environment")
counts = manifest.get("data_scale")
if not isinstance(counts, dict) or not counts:
    raise SystemExit("fixture manifest requires data_scale")
for key, value in counts.items():
    if not isinstance(value, int) or value < 0:
        raise SystemExit(f"data_scale.{key} must be a non-negative integer")
for key in ("link_count", "thought_count", "note_count", "inbox_item_count", "feed_item_count"):
    if counts.get(key, 0) <= 0:
        raise SystemExit(f"data_scale.{key} must be positive for a seeded performance fixture")
PY
}

validate_postgres_plan() {
  local file=$1
  python3 - "$file" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    plan = json.load(handle)

def contains_plan(value):
    if isinstance(value, dict):
        return "Plan" in value or any(contains_plan(item) for item in value.values())
    if isinstance(value, list):
        return any(contains_plan(item) for item in value)
    return False

if not contains_plan(plan):
    raise SystemExit("PostgreSQL artifact must contain EXPLAIN (FORMAT JSON) Plan data")
PY
}

write_build_stats() {
  local dist_dir=$1
  local output=$2
  python3 - "$dist_dir" "$output" <<'PY'
import hashlib
import json
import os
import sys
from pathlib import Path

dist, output = sys.argv[1:]
root = Path(dist)
if not root.is_dir():
    raise SystemExit(f"Reader build directory does not exist: {dist}")
files = []
for path in sorted(item for item in root.rglob("*") if item.is_file()):
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    files.append({
        "path": path.relative_to(root).as_posix(),
        "bytes": path.stat().st_size,
        "sha256": digest,
    })
artifact = {
    "schema_version": 1,
    "file_count": len(files),
    "total_bytes": sum(item["bytes"] for item in files),
    "files": files,
}
with open(output, "w", encoding="utf-8") as handle:
    json.dump(artifact, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
}

write_report() {
  local report=$1
  local manifest=$2
  local build_stats=$3
  local postgres_plan=$4
  local trace=$5
  local metrics=$6
  local source_sha=$7
  local port=$8
  local started_at=$9
  local finished_at=${10}
  python3 - "$report" "$manifest" "$build_stats" "$postgres_plan" "$trace" "$metrics" "$source_sha" "$port" "$started_at" "$finished_at" <<'PY'
import hashlib
import json
import platform
import sys
from datetime import datetime, timezone
from pathlib import Path

(report_path, manifest_path, build_path, plan_path, trace_path, metrics_path,
 source_sha, port, started_at, finished_at) = sys.argv[1:]

def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()

with open(manifest_path, encoding="utf-8") as handle:
    manifest = json.load(handle)
with open(build_path, encoding="utf-8") as handle:
    build = json.load(handle)
with open(metrics_path, encoding="utf-8") as handle:
    metrics = json.load(handle)

status = "reviewed" if manifest["fixture_status"] == "baseline-reviewed" else "candidate"
review_line = (
    "- Status: `reviewed`; `PERF-BASE-01` has an independent review marker in the fixture manifest."
    if status == "reviewed"
    else "- Status: `candidate`; `PERF-BASE-01` is not ready without independent review."
)

report = f"""# Reader vNext Performance Baseline Candidate

{review_line}
- Fixture: `{manifest['fixture_id']}`
- Seed: `{manifest['seed']}`
- Fixture status: `{manifest['fixture_status']}`
- Source delivery: `{source_sha}`
- Started: `{started_at}`
- Finished: `{finished_at}`
- Registered browser port: `{port}`
- Machine: `{platform.platform()}`
- Python: `{platform.python_version()}`

## Data Scale

```json
{json.dumps(manifest['data_scale'], indent=2, sort_keys=True)}
```

## Evidence

| Artifact | SHA-256 |
|---|---|
| fixture manifest | `{digest(manifest_path)}` |
| PostgreSQL EXPLAIN JSON | `{digest(plan_path)}` |
| Reader build stats | `{digest(build_path)}` |
| Chromium trace | `{digest(trace_path)}` |
| browser metrics | `{digest(metrics_path)}` |

PostgreSQL plan path: `{plan_path}`
Reader build stats path: `{build_path}`
Chromium trace path: `{trace_path}`
Browser metrics path: `{metrics_path}`

## Reader Build

- Files: `{build['file_count']}`
- Total bytes: `{build['total_bytes']}`

## Browser Journey

```json
{json.dumps(metrics, indent=2, sort_keys=True)}
```

The report intentionally contains no pass/fail threshold. Thresholds and any
budget exception require an independent performance review and a product /
release decision; this file is only reproducible candidate evidence.
"""
Path(report_path).parent.mkdir(parents=True, exist_ok=True)
with open(report_path, "w", encoding="utf-8") as handle:
    handle.write(report)
PY
}

parse_args() {
  COMMAND=${1:-}
  [[ -n "$COMMAND" ]] || { usage; exit 2; }
  shift
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --fixture-manifest)
        [[ $# -ge 2 ]] || fail "--fixture-manifest requires a value"
        FIXTURE_MANIFEST=$2
        shift 2
        ;;
      --reader-spec)
        [[ $# -ge 2 ]] || fail "--reader-spec requires a value"
        READER_SPEC=$2
        shift 2
        ;;
      --artifact-dir)
        [[ $# -ge 2 ]] || fail "--artifact-dir requires a value"
        ARTIFACT_DIR=$2
        shift 2
        ;;
      --report)
        [[ $# -ge 2 ]] || fail "--report requires a value"
        REPORT=$2
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
  [[ "$COMMAND" == "baseline" ]] || fail "only the baseline command is currently supported"
}

parse_args "$@"
need_command git
need_command python3
need_command sha256sum
need_command corepack
need_command find
validate_port

[[ -n "$FIXTURE_MANIFEST" ]] || fail "--fixture-manifest is required"
[[ -n "$READER_SPEC" ]] || fail "--reader-spec is required"
[[ -n "$ARTIFACT_DIR" ]] || fail "--artifact-dir is required"
[[ -n "$REPORT" ]] || fail "--report is required"

FIXTURE_MANIFEST=$(absolute_path "$FIXTURE_MANIFEST")
READER_SPEC=$(absolute_path "$READER_SPEC")
ARTIFACT_DIR=$(absolute_path "$ARTIFACT_DIR")
REPORT=$(absolute_path "$REPORT")
require_file "fixture manifest" "$FIXTURE_MANIFEST"
require_file "Reader performance spec" "$READER_SPEC"
validate_manifest "$FIXTURE_MANIFEST" || fail "fixture manifest validation failed"
require_new_path "artifact directory" "$ARTIFACT_DIR"
require_new_path "report" "$REPORT"

POSTGRES_PLAN_FILE=${READER_PERF_POSTGRES_PLAN_FILE:-}
[[ -n "$POSTGRES_PLAN_FILE" ]] || fail "READER_PERF_POSTGRES_PLAN_FILE must point to a real EXPLAIN JSON artifact"
POSTGRES_PLAN_FILE=$(absolute_path "$POSTGRES_PLAN_FILE")
require_file "PostgreSQL plan artifact" "$POSTGRES_PLAN_FILE"
validate_postgres_plan "$POSTGRES_PLAN_FILE" || fail "PostgreSQL plan artifact validation failed"

mkdir -p "$ARTIFACT_DIR/playwright"
STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
SOURCE_SHA=$(git -C "$ROOT" rev-parse HEAD)
cp "$POSTGRES_PLAN_FILE" "$ARTIFACT_DIR/postgres-plan.json"

BUILD_LOG="$ARTIFACT_DIR/reader-build.log"
if ! (cd "$ROOT" && "${PNPM_BIN[@]}" --filter webtag-reader build) >"$BUILD_LOG" 2>&1; then
  cat "$BUILD_LOG" >&2
  fail "Reader production build failed; candidate evidence is incomplete"
fi
write_build_stats "$ROOT/reader/dist" "$ARTIFACT_DIR/reader-build-stats.json"

PLAYWRIGHT_LOG="$ARTIFACT_DIR/playwright.log"
if ! (
  cd "$ROOT" && \
  READER_E2E_PORT="$READER_E2E_PORT" \
  READER_PERF_FIXTURE_MANIFEST="$FIXTURE_MANIFEST" \
  READER_PERF_OUTPUT_DIR="$ARTIFACT_DIR/playwright" \
  "${PNPM_BIN[@]}" --filter webtag-reader exec playwright test "$READER_SPEC"
) >"$PLAYWRIGHT_LOG" 2>&1; then
  cat "$PLAYWRIGHT_LOG" >&2
  fail "Chromium performance journey failed; candidate evidence is incomplete"
fi

TRACE_FILE=$(find "$ARTIFACT_DIR/playwright" -type f -name 'reader-vnext-performance-trace.zip' -print -quit)
METRICS_FILE=$(find "$ARTIFACT_DIR/playwright" -type f -name 'reader-vnext-performance-metrics.json' -print -quit)
[[ -n "$TRACE_FILE" ]] || fail "Chromium trace artifact was not produced"
[[ -n "$METRICS_FILE" ]] || fail "browser metrics artifact was not produced"

FINISHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
write_report "$REPORT" "$FIXTURE_MANIFEST" \
  "$ARTIFACT_DIR/reader-build-stats.json" "$ARTIFACT_DIR/postgres-plan.json" \
  "$TRACE_FILE" "$METRICS_FILE" "$SOURCE_SHA" "$READER_E2E_PORT" \
  "$STARTED_AT" "$FINISHED_AT"

printf 'performance candidate written: %s\n' "$REPORT"
