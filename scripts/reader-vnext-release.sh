#!/usr/bin/env bash
# Build and verify Reader release artifacts for both production deployment
# shapes: the standalone root-domain Reader and the embedded /reader/ smoke
# build carried by the backend image.
set -euo pipefail
IFS=$'\n\t'

readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly PNPM_BIN=(corepack pnpm@10.13.1)
readonly RELEASE_MARKER="webtag-reader-release.json"

usage() {
  cat <<'EOF'
Usage:
  scripts/reader-vnext-release.sh build [--release-id ID] [--out-dir DIR]
  scripts/reader-vnext-release.sh verify --manifest FILE [--expected-commit FULL_SHA]

build:
  Produces two fresh Reader builds under DIR:
    root/      VITE_BASE=/        for https://reader.alpenl.com/
    embedded/  VITE_BASE=/reader/ for https://webtag.alpenl.com/reader/

  Each build contains webtag-reader-release.json, and DIR contains
  reader-vnext-release-manifest.json with file-level SHA-256 provenance.
EOF
}

fail() {
  echo "reader-vnext-release: $*" >&2
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

require_clean_tree() {
  git -C "$ROOT" diff --quiet || fail "working tree has unstaged changes; release provenance requires a clean tree"
  git -C "$ROOT" diff --cached --quiet || fail "index has staged changes; release provenance requires a clean tree"
}

validate_full_sha() {
  local value=$1
  [[ "$value" =~ ^[0-9a-f]{40}$ ]] || fail "expected a full 40-character lowercase Git SHA: $value"
}

write_release_marker() {
  local target_dir=$1
  local tree_kind=$2
  local base_path=$3
  local release_id=$4
  local commit_sha=$5
  local generated_at=$6
  python3 - "$target_dir/$RELEASE_MARKER" "$tree_kind" "$base_path" "$release_id" "$commit_sha" "$generated_at" <<'PY'
import json
import sys
from pathlib import Path

path, tree_kind, base_path, release_id, commit_sha, generated_at = sys.argv[1:]
marker = {
    "schema_version": 1,
    "artifact_kind": "webtag-reader-release",
    "release_id": release_id,
    "tree_kind": tree_kind,
    "base_path": base_path,
    "commit_full_sha": commit_sha,
    "generated_at": generated_at,
}
Path(path).write_text(json.dumps(marker, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
}

write_manifest() {
  local output_dir=$1
  local release_id=$2
  local commit_sha=$3
  local generated_at=$4
  python3 - "$output_dir" "$release_id" "$commit_sha" "$generated_at" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

output_dir, release_id, commit_sha, generated_at = sys.argv[1:]
root = Path(output_dir)

def collect(name, base_path):
    build_dir = root / name
    files = []
    for path in sorted(item for item in build_dir.rglob("*") if item.is_file()):
        files.append({
            "path": path.relative_to(build_dir).as_posix(),
            "bytes": path.stat().st_size,
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
        })
    return {
        "name": name,
        "base_path": base_path,
        "directory": name,
        "release_marker": f"{name}/webtag-reader-release.json",
        "file_count": len(files),
        "total_bytes": sum(item["bytes"] for item in files),
        "files": files,
    }

manifest = {
    "schema_version": 1,
    "artifact_kind": "reader-vnext-release-manifest",
    "release_id": release_id,
    "commit_full_sha": commit_sha,
    "generated_at": generated_at,
    "builds": [
        collect("root", "/"),
        collect("embedded", "/reader/"),
    ],
}
(root / "reader-vnext-release-manifest.json").write_text(
    json.dumps(manifest, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
}

verify_manifest() {
  local manifest=$1
  local expected_commit=$2
  python3 - "$manifest" "$expected_commit" <<'PY'
import json
import re
import sys
from pathlib import Path

manifest_path = Path(sys.argv[1])
expected_commit = sys.argv[2]
manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
root = manifest_path.parent

def fail(message):
    raise SystemExit(message)

if manifest.get("schema_version") != 1:
    fail("manifest schema_version must be 1")
if manifest.get("artifact_kind") != "reader-vnext-release-manifest":
    fail("manifest artifact_kind mismatch")
commit = manifest.get("commit_full_sha")
if not isinstance(commit, str) or not re.fullmatch(r"[0-9a-f]{40}", commit):
    fail("manifest requires a 40-character commit_full_sha")
if expected_commit and commit != expected_commit:
    fail(f"manifest commit mismatch: {commit} != {expected_commit}")

builds = manifest.get("builds")
if not isinstance(builds, list) or {build.get("name") for build in builds} != {"root", "embedded"}:
    fail("manifest must contain root and embedded builds")

for build in builds:
    name = build["name"]
    base_path = build["base_path"]
    build_dir = root / build["directory"]
    if not build_dir.is_dir():
        fail(f"{name} build directory is missing: {build_dir}")
    marker_path = root / build["release_marker"]
    if not marker_path.is_file():
        fail(f"{name} release marker is missing")
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
    if marker.get("commit_full_sha") != commit:
        fail(f"{name} release marker commit mismatch")
    if marker.get("base_path") != base_path or marker.get("tree_kind") != name:
        fail(f"{name} release marker identity mismatch")
    index = (build_dir / "index.html").read_text(encoding="utf-8")
    if name == "root":
        if "/reader/assets/" in index or "/assets/" not in index:
            fail("root build must reference /assets/ and not /reader/assets/")
    else:
        if "/reader/assets/" not in index:
            fail("embedded build must reference /reader/assets/")
    sw = build_dir / "sw.js"
    if not sw.is_file():
        fail(f"{name} build is missing sw.js")
    if "/api" not in sw.read_text(encoding="utf-8"):
        fail(f"{name} sw.js does not contain API denylist evidence")
    if not any((build_dir / "assets").glob("*")):
        fail(f"{name} build has no assets")
    if build.get("file_count", 0) <= 0 or build.get("total_bytes", 0) <= 0:
        fail(f"{name} file provenance is empty")

print(f"verified reader release manifest: {manifest_path}")
PY
}

build_release() {
  local release_id=""
  local output_dir=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --release-id)
        [[ $# -ge 2 ]] || fail "--release-id requires a value"
        release_id=$2
        shift 2
        ;;
      --out-dir)
        [[ $# -ge 2 ]] || fail "--out-dir requires a value"
        output_dir=$2
        shift 2
        ;;
      --help|-h)
        usage
        exit 0
        ;;
      *)
        fail "unknown build argument: $1"
        ;;
    esac
  done

  need_command git
  need_command corepack
  need_command python3
  require_clean_tree

  local commit_sha
  commit_sha="$(git -C "$ROOT" rev-parse HEAD)"
  validate_full_sha "$commit_sha"
  local short_sha=${commit_sha:0:12}
  local generated_at
  generated_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if [[ -z "$release_id" ]]; then
    release_id="${short_sha}-reader-${generated_at//[-:]/}"
  fi
  if [[ -z "$output_dir" ]]; then
    output_dir="artifacts/reader-vnext/release/$release_id"
  fi
  output_dir="$(absolute_path "$output_dir")"
  [[ "$output_dir" != "$ROOT/reader/dist" && "$output_dir" != "$ROOT/internal/app/assets/reader" ]] || \
    fail "release helper requires a fresh immutable artifact out-dir, not a mutable build target"
  [[ ! -e "$output_dir" ]] || fail "output directory already exists; choose a fresh release id: $output_dir"

  mkdir -p "$output_dir"
  "${PNPM_BIN[@]}" --filter webtag-reader typecheck
  VITE_BASE=/ "${PNPM_BIN[@]}" --filter webtag-reader exec vite build --outDir "$output_dir/root" --emptyOutDir
  VITE_BASE=/reader/ "${PNPM_BIN[@]}" --filter webtag-reader exec vite build --outDir "$output_dir/embedded" --emptyOutDir
  write_release_marker "$output_dir/root" root / "$release_id" "$commit_sha" "$generated_at"
  write_release_marker "$output_dir/embedded" embedded /reader/ "$release_id" "$commit_sha" "$generated_at"
  write_manifest "$output_dir" "$release_id" "$commit_sha" "$generated_at"
  verify_manifest "$output_dir/reader-vnext-release-manifest.json" "$commit_sha"
}

verify_release() {
  local manifest=""
  local expected_commit=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --manifest)
        [[ $# -ge 2 ]] || fail "--manifest requires a value"
        manifest=$2
        shift 2
        ;;
      --expected-commit)
        [[ $# -ge 2 ]] || fail "--expected-commit requires a value"
        expected_commit=$2
        shift 2
        ;;
      --help|-h)
        usage
        exit 0
        ;;
      *)
        fail "unknown verify argument: $1"
        ;;
    esac
  done
  [[ -n "$manifest" ]] || fail "--manifest is required"
  manifest="$(absolute_path "$manifest")"
  [[ -f "$manifest" ]] || fail "manifest does not exist: $manifest"
  if [[ -n "$expected_commit" ]]; then
    validate_full_sha "$expected_commit"
  fi
  need_command python3
  verify_manifest "$manifest" "$expected_commit"
}

command=${1:-}
[[ -n "$command" ]] || { usage; exit 2; }
shift

case "$command" in
  build)
    build_release "$@"
    ;;
  verify)
    verify_release "$@"
    ;;
  --help|-h)
    usage
    ;;
  *)
    fail "unknown command: $command"
    ;;
esac
