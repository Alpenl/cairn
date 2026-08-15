#!/usr/bin/env bash
# reader-vnext-lease.sh
#
# LC-01 coordination store for Reader vNext work-package leases. A lease is a
# Git commit containing one complete record.json on a dedicated remote ref.
# State changes are compare-and-swap operations against that record commit;
# promote updates the integration ref and lease ref with one atomic push.
set -euo pipefail
IFS=$'\n\t'

readonly SCRIPT_NAME="reader-vnext-lease"
readonly LEASE_REF_PREFIX="refs/heads/coord/reader-vnext-leases"
readonly INTEGRATION_REF="refs/heads/int/reader-vnext"
readonly ZERO_SHA="0000000000000000000000000000000000000000"

REMOTE="origin"
REPO="."
LEASE=""
EXPECTED_RECORD_SHA=""
EXPECTED_REVISION=""
NEXT_RECORD_FILE=""
OWNER=""
BRANCH=""
FILES_FILE=""
INTEGRATION_HEAD=""
EXPECTED_RELEASE=""
SOURCE_SERIES_FILE=""
CANDIDATE_SHA=""
REVIEWED_SHA=""
REVIEW_RECORD_FILE=""
REVIEW_RECORD=""
OUT_FILE=""
INTEGRATION_WORKTREE=""
LEASE_WORKTREE=""
VERIFICATION_ARTIFACT=""
TEMP_DIR=""

cleanup_temp() {
  if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
    rm -rf -- "$TEMP_DIR"
  fi
}

trap cleanup_temp EXIT

usage() {
  cat <<'EOF'
Usage:
  reader-vnext-lease.sh init --lease NAME --owner NAME --branch BRANCH \
    --integration-head SHA [--expected-release TEXT] [--files-file FILE]
  reader-vnext-lease.sh read --lease NAME
  reader-vnext-lease.sh cas --lease NAME --expected-record-sha SHA \
    --expected-revision N --next-record FILE
  reader-vnext-lease.sh verify --lease NAME --expected-revision N \
    --integration-worktree DIR --lease-worktree DIR \
    --expected-integration-head SHA --source-series-file FILE \
    --expected-candidate-sha SHA --expected-reviewed-sha SHA|empty \
    --review-record FILE|none --out FILE
  reader-vnext-lease.sh promote --lease NAME --expected-record-sha SHA \
    --expected-revision N --expected-integration-head SHA \
    --candidate-sha SHA --review-record FILE \
    --verification-artifact FILE

Global options may appear after the command:
  --remote NAME|URL  (default: origin)
  --repo DIR         (default: .)
EOF
}

json_error() {
  local code=$1
  local message=$2
  python3 - "$code" "$message" <<'PY'
import json
import sys

print(json.dumps({"ok": False, "error": {"code": sys.argv[1], "message": sys.argv[2]}}, separators=(",", ":")))
PY
}

fail() {
  json_error "$1" "$2" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing_command" "required command is not installed: $1"
}

validate_sha() {
  [[ "$1" =~ ^[0-9a-fA-F]{40}$ ]] || fail "invalid_sha" "expected a 40-character Git SHA: $1"
}

validate_lease() {
  [[ -n "$LEASE" ]] || fail "missing_lease" "--lease is required"
  [[ "$LEASE" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] || fail "invalid_lease" "lease contains invalid ref characters"
  [[ "$LEASE" != *..* ]] || fail "invalid_lease" "lease must not contain '..'"
  [[ "$LEASE" != *'@{'* ]] || fail "invalid_lease" "lease must not contain '@{'"
  [[ "$LEASE" != /* && "$LEASE" != */ ]] || fail "invalid_lease" "lease must not start or end with '/'"
}

lease_ref() {
  printf '%s/%s\n' "$LEASE_REF_PREFIX" "$LEASE"
}

git_repo() {
  git -C "$REPO" "$@"
}

remote_ref_sha() {
  local ref=$1
  local result
  result=$(git_repo ls-remote "$REMOTE" "$ref" | awk 'NR == 1 { print $1 }')
  if [[ -z "$result" ]]; then
    printf '%s\n' "$ZERO_SHA"
  else
    validate_sha "$result"
    printf '%s\n' "$result"
  fi
}

fetch_record() {
  local ref
  ref=$(lease_ref)
  local sha
  sha=$(remote_ref_sha "$ref")
  [[ "$sha" != "$ZERO_SHA" ]] || fail "record_not_found" "remote lease record does not exist: $LEASE"
  git_repo fetch --no-tags "$REMOTE" "$ref" >/dev/null 2>&1 \
    || fail "record_fetch_failed" "could not fetch remote lease record: $LEASE"
  printf '%s\n' "$sha"
}

record_json() {
  local commit_sha=$1
  git_repo show "$commit_sha:record.json" 2>/dev/null \
    || fail "invalid_record_commit" "remote lease commit does not contain record.json: $commit_sha"
}

json_value() {
  local file=$1
  local key=$2
  python3 - "$file" "$key" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle).get(sys.argv[2])
if isinstance(value, (dict, list)):
    print(json.dumps(value, separators=(",", ":")))
elif value is None:
    print("null")
else:
    print(value)
PY
}

validate_record_file() {
  local file=$1
  local expected_state=$2
  local next_state=$3
  local command_name=$4
  python3 - "$file" "$LEASE" "$EXPECTED_REVISION" "$expected_state" "$next_state" "$command_name" <<'PY'
import json
import re
import sys

path, lease, revision, expected_state, next_state, command_name = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as handle:
        record = json.load(handle)
except Exception as exc:
    raise SystemExit(f"record is not valid JSON: {exc}")

required = {
    "lease", "lease_revision", "state", "owner", "branch", "files",
    "acquired_at", "expected_release", "integration_head_at_acquire",
    "source_delivery_shas", "candidate_bundle_sha", "reviewed_bundle_sha",
    "review_record_sha", "integrated_sha",
}
missing = sorted(required - record.keys())
if missing:
    raise SystemExit("record is missing fields: " + ",".join(missing))
if record["lease"] != lease:
    raise SystemExit("record lease does not match --lease")
if not isinstance(record["lease_revision"], int) or str(record["lease_revision"]) != revision:
    raise SystemExit("record lease_revision does not match --expected-revision")
states = {"acquired", "candidate", "reviewed", "integrated"}
if record["state"] not in states or record["state"] != next_state:
    raise SystemExit("record state is not the requested next state")
if (expected_state, next_state) not in {("acquired", "candidate"), ("candidate", "reviewed")}:
    raise SystemExit(f"invalid lease state transition: {expected_state} -> {next_state}")
if command_name == "cas" and next_state == "integrated":
    raise SystemExit("cas may not create the integrated state; use promote")

if not isinstance(record["files"], list) or any(not isinstance(item, str) or not item for item in record["files"]):
    raise SystemExit("files must be a list of non-empty strings")
if not isinstance(record["source_delivery_shas"], list):
    raise SystemExit("source_delivery_shas must be a list")
sha_re = re.compile(r"^[0-9a-fA-F]{40}$")
for value in record["source_delivery_shas"]:
    if not isinstance(value, str) or not sha_re.fullmatch(value):
        raise SystemExit("source_delivery_shas contains an invalid SHA")
for field in ("integration_head_at_acquire", "candidate_bundle_sha", "reviewed_bundle_sha", "integrated_sha"):
    value = record[field]
    if value is not None and (not isinstance(value, str) or not sha_re.fullmatch(value)):
        raise SystemExit(f"{field} must be null or a 40-character SHA")
for field in ("review_record_sha",):
    value = record[field]
    if value is not None and (not isinstance(value, str) or not re.fullmatch(r"^[0-9a-fA-F]{64}$", value)):
        raise SystemExit(f"{field} must be null or a SHA-256")

if next_state in {"candidate", "reviewed", "integrated"}:
    if not record["source_delivery_shas"]:
        raise SystemExit("candidate records require source_delivery_shas")
    if not record["candidate_bundle_sha"]:
        raise SystemExit("candidate records require candidate_bundle_sha")
if next_state in {"reviewed", "integrated"}:
    if record["reviewed_bundle_sha"] != record["candidate_bundle_sha"]:
        raise SystemExit("reviewed_bundle_sha must equal candidate_bundle_sha")
    if not record["review_record_sha"]:
        raise SystemExit("reviewed records require review_record_sha")
if next_state == "integrated":
    if record["integrated_sha"] != record["candidate_bundle_sha"]:
        raise SystemExit("integrated_sha must equal candidate_bundle_sha")
PY
}

validate_transition_record() {
  local current_file=$1
  local next_file=$2
  python3 - "$current_file" "$next_file" <<'PY'
import json
import sys

current_path, next_path = sys.argv[1:]
with open(current_path, encoding="utf-8") as handle:
    current = json.load(handle)
with open(next_path, encoding="utf-8") as handle:
    next_record = json.load(handle)

stable_fields = (
    "lease", "lease_revision", "owner", "branch", "files", "acquired_at",
    "expected_release", "integration_head_at_acquire",
)
for field in stable_fields:
    if current.get(field) != next_record.get(field):
        raise SystemExit(f"transition changed immutable field: {field}")

if current.get("state") == "acquired":
    for field in ("reviewed_bundle_sha", "review_record_sha", "integrated_sha"):
        if next_record.get(field) is not None:
            raise SystemExit(f"acquired -> candidate must leave {field} null")
elif current.get("state") == "candidate":
    if next_record.get("source_delivery_shas") != current.get("source_delivery_shas"):
        raise SystemExit("candidate -> reviewed changed source_delivery_shas")
    if next_record.get("candidate_bundle_sha") != current.get("candidate_bundle_sha"):
        raise SystemExit("candidate -> reviewed changed candidate_bundle_sha")
    if next_record.get("integrated_sha") is not None:
        raise SystemExit("candidate -> reviewed must leave integrated_sha null")
PY
}

create_record_commit() {
  local record_file=$1
  local parent=${2:-}
  local message=$3
  local blob tree
  blob=$(git_repo hash-object -w "$record_file")
  tree=$(printf '100644 blob %s\trecord.json\n' "$blob" | git_repo mktree)
  local -a parent_args=()
  if [[ -n "$parent" ]]; then
    parent_args=(-p "$parent")
  fi
  GIT_AUTHOR_NAME=reader-vnext-lease \
  GIT_AUTHOR_EMAIL=reader-vnext-lease@localhost \
  GIT_COMMITTER_NAME=reader-vnext-lease \
  GIT_COMMITTER_EMAIL=reader-vnext-lease@localhost \
    git_repo commit-tree "$tree" "${parent_args[@]}" -m "$message"
}

emit_record_result() {
  local command_name=$1
  local commit_sha=$2
  local file=$3
  python3 - "$command_name" "$LEASE" "$commit_sha" "$file" <<'PY'
import json
import sys

with open(sys.argv[4], encoding="utf-8") as handle:
    record = json.load(handle)
print(json.dumps({
    "ok": True,
    "command": sys.argv[1],
    "lease": sys.argv[2],
    "record_commit_sha": sys.argv[3],
    "record": record,
}, separators=(",", ":")))
PY
}

write_initial_record() {
  local file=$1
  local files_json='[]'
  if [[ -n "$FILES_FILE" ]]; then
    files_json=$(python3 - "$FILES_FILE" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.dumps([line.strip() for line in handle if line.strip()], separators=(",", ":")))
PY
  )
  fi
  python3 - "$file" "$LEASE" "$OWNER" "$BRANCH" "$INTEGRATION_HEAD" "$EXPECTED_RELEASE" "$files_json" <<'PY'
import json
import sys
from datetime import datetime, timezone

path, lease, owner, branch, integration_head, expected_release, files = sys.argv[1:]
record = {
    "lease": lease,
    "lease_revision": 1,
    "state": "acquired",
    "owner": owner,
    "branch": branch,
    "files": json.loads(files),
    "acquired_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "expected_release": expected_release,
    "integration_head_at_acquire": integration_head,
    "source_delivery_shas": [],
    "candidate_bundle_sha": None,
    "reviewed_bundle_sha": None,
    "review_record_sha": None,
    "integrated_sha": None,
}
with open(path, "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
}

command_init() {
  validate_lease
  [[ -n "$OWNER" ]] || fail "missing_owner" "--owner is required"
  [[ -n "$BRANCH" ]] || fail "missing_branch" "--branch is required"
  [[ -n "$INTEGRATION_HEAD" ]] || fail "missing_integration_head" "--integration-head is required"
  validate_sha "$INTEGRATION_HEAD"
  local ref
  ref=$(lease_ref)
  [[ "$(remote_ref_sha "$ref")" == "$ZERO_SHA" ]] || fail "record_exists" "remote lease record already exists: $LEASE"
  local temp_dir record_file commit_sha
  TEMP_DIR=$(mktemp -d)
  temp_dir="$TEMP_DIR"
  record_file="$temp_dir/record.json"
  write_initial_record "$record_file"
  commit_sha=$(create_record_commit "$record_file" "" "reader-vnext lease $LEASE acquired")
  git_repo push --porcelain --force-with-lease="$ref:" "$REMOTE" "$commit_sha:$ref" >/dev/null \
    || fail "record_push_failed" "could not create remote lease record"
  [[ "$(remote_ref_sha "$ref")" == "$commit_sha" ]] || fail "record_postcondition_failed" "remote lease ref did not reach the created record"
  emit_record_result "init" "$commit_sha" "$record_file"
}

command_read() {
  validate_lease
  local commit_sha temp_dir record_file
  TEMP_DIR=$(mktemp -d)
  temp_dir="$TEMP_DIR"
  record_file="$temp_dir/record.json"
  commit_sha=$(fetch_record)
  record_json "$commit_sha" >"$record_file"
  emit_record_result "read" "$commit_sha" "$record_file"
}

command_cas() {
  validate_lease
  [[ -n "$EXPECTED_RECORD_SHA" ]] || fail "missing_expected_record_sha" "--expected-record-sha is required"
  [[ -n "$EXPECTED_REVISION" ]] || fail "missing_expected_revision" "--expected-revision is required"
  [[ "$EXPECTED_REVISION" =~ ^[1-9][0-9]*$ ]] || fail "invalid_revision" "--expected-revision must be a positive integer"
  [[ -f "$NEXT_RECORD_FILE" ]] || fail "missing_next_record" "--next-record must point to a JSON file"
  validate_sha "$EXPECTED_RECORD_SHA"
  local ref current_sha temp_dir current_file next_state current_state new_commit
  ref=$(lease_ref)
  current_sha=$(remote_ref_sha "$ref")
  [[ "$current_sha" == "$EXPECTED_RECORD_SHA" ]] || fail "stale_record" "remote lease record changed; refusing to overwrite it"
  TEMP_DIR=$(mktemp -d)
  temp_dir="$TEMP_DIR"
  current_file="$temp_dir/current.json"
  git_repo fetch --no-tags "$REMOTE" "$ref" >/dev/null 2>&1 \
    || fail "record_fetch_failed" "could not fetch remote lease record"
  record_json "$current_sha" >"$current_file"
  current_state=$(json_value "$current_file" state)
  next_state=$(json_value "$NEXT_RECORD_FILE" state)
  validate_transition_record "$current_file" "$NEXT_RECORD_FILE" \
    || fail "invalid_next_record" "next record changed an immutable lease field"
  validate_record_file "$NEXT_RECORD_FILE" "$current_state" "$next_state" "cas" \
    || fail "invalid_next_record" "next record failed schema or state transition validation"
  new_commit=$(create_record_commit "$NEXT_RECORD_FILE" "$EXPECTED_RECORD_SHA" "reader-vnext lease $LEASE CAS $current_state -> $next_state")
  git_repo push --porcelain --force-with-lease="$ref:$EXPECTED_RECORD_SHA" "$REMOTE" "$new_commit:$ref" >/dev/null \
    || fail "cas_push_failed" "CAS push rejected; lease is stale or remote refused the update"
  [[ "$(remote_ref_sha "$ref")" == "$new_commit" ]] || fail "record_postcondition_failed" "remote lease ref did not reach the CAS commit"
  emit_record_result "cas" "$new_commit" "$NEXT_RECORD_FILE"
}

verify_source_series() {
  local base_sha=$1
  local candidate_sha=$2
  local series_file=$3
  python3 - "$REPO" "$base_sha" "$candidate_sha" "$series_file" <<'PY'
import json
import subprocess
import sys

repo, base, candidate, series_path = sys.argv[1:]
with open(series_path, encoding="utf-8") as handle:
    sources = [line.strip() for line in handle if line.strip()]
if not sources:
    raise SystemExit("source series is empty")
commits = subprocess.check_output(
    ["git", "-C", repo, "rev-list", "--reverse", f"{base}..{candidate}"],
    text=True,
).splitlines()
if not commits:
    raise SystemExit("candidate bundle has no commits after integration head")
positions = []
for source in sources:
    matches = []
    for position, commit in enumerate(commits):
        message = subprocess.check_output(
            ["git", "-C", repo, "show", "-s", "--format=%B", commit], text=True,
        )
        if f"(cherry picked from commit {source})" in message:
            matches.append((position, commit))
    if len(matches) != 1:
        raise SystemExit(f"source {source} has {len(matches)} cherry-pick trailer matches")
    positions.append(matches[0])
if positions != sorted(positions):
    raise SystemExit("source delivery series is not in candidate commit order")
mapping = []
for source, (position, commit) in zip(sources, positions):
    patch = subprocess.check_output(
        ["git", "-C", repo, "show", "--format=email", "--no-ext-diff", commit], text=True,
    )
    patch_id = subprocess.check_output(
        ["git", "patch-id", "--stable"], input=patch, text=True,
    ).split()[0]
    mapping.append({"source_sha": source, "candidate_commit_sha": commit, "patch_id": patch_id})
print(json.dumps(mapping, separators=(",", ":")))
PY
}

verify_record_source_series() {
  local record_file=$1
  local series_file=$2
  python3 - "$record_file" "$series_file" <<'PY'
import json
import sys

record_path, series_path = sys.argv[1:]
with open(record_path, encoding="utf-8") as handle:
    record = json.load(handle)
with open(series_path, encoding="utf-8") as handle:
    series = [line.strip() for line in handle if line.strip()]
if record.get("source_delivery_shas") != series:
    raise SystemExit("source series does not match record source_delivery_shas")
PY
}

validate_review_record() {
  local file=$1
  local candidate_sha=$2
  [[ "$file" != "none" && -f "$file" ]] || fail "missing_review_record" "an immutable review record is required"
  local digest
  digest=$(sha256sum "$file" | awk '{print $1}')
  python3 - "$file" "$LEASE" "$EXPECTED_REVISION" "$candidate_sha" "$digest" <<'PY'
import json
import re
import sys

path, lease, revision, candidate, digest = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    record = json.load(handle)
if record.get("lease") != lease or str(record.get("lease_revision")) != revision:
    raise SystemExit("review record lease or revision mismatch")
if record.get("delivery_sha") != candidate:
    raise SystemExit("review record delivery_sha mismatch")
if record.get("verdict") != "approved":
    raise SystemExit("review record verdict must be approved")
if not isinstance(record.get("reviewer"), str) or not record["reviewer"].strip():
    raise SystemExit("review record reviewer is required")
if not re.fullmatch(r"[0-9a-f]{64}", digest):
    raise SystemExit("could not calculate review record digest")
print(digest)
PY
}

write_verification_artifact() {
  local file=$1
  local record_sha=$2
  local candidate_sha=$3
  local reviewed_sha=$4
  local review_digest=$5
  local source_mapping=$6
  mkdir -p "$(dirname "$file")"
  python3 - "$file" "$LEASE" "$EXPECTED_REVISION" "$record_sha" "$INTEGRATION_HEAD" "$candidate_sha" "$reviewed_sha" "$review_digest" "$source_mapping" <<'PY'
import json
import sys

path, lease, revision, record_sha, integration_head, candidate, reviewed, review_digest, source_mapping = sys.argv[1:]
artifact = {
    "ok": True,
    "command": "verify",
    "lease": lease,
    "lease_revision": int(revision),
    "record_commit_sha": record_sha,
    "integration_head_at_acquire": integration_head,
    "candidate_bundle_sha": candidate,
    "reviewed_bundle_sha": None if reviewed == "empty" else reviewed,
    "review_record_sha": None if review_digest == "none" else review_digest,
    "source_mapping": json.loads(source_mapping),
    "checks": {
        "record_revision": True,
        "integration_head": True,
        "source_series": True,
        "review_record": review_digest != "none",
    },
}
with open(path, "w", encoding="utf-8") as handle:
    json.dump(artifact, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
print(json.dumps(artifact, separators=(",", ":")))
PY
}

command_verify() {
  validate_lease
  [[ -n "$EXPECTED_REVISION" ]] || fail "missing_expected_revision" "--expected-revision is required"
  [[ "$EXPECTED_REVISION" =~ ^[1-9][0-9]*$ ]] || fail "invalid_revision" "--expected-revision must be a positive integer"
  [[ -n "$INTEGRATION_WORKTREE" && -d "$INTEGRATION_WORKTREE" ]] || fail "missing_integration_worktree" "--integration-worktree must be a directory"
  [[ -n "$LEASE_WORKTREE" && -d "$LEASE_WORKTREE" ]] || fail "missing_lease_worktree" "--lease-worktree must be a directory"
  [[ -n "$INTEGRATION_HEAD" ]] || fail "missing_integration_head" "--expected-integration-head is required"
  validate_sha "$INTEGRATION_HEAD"
  [[ -f "$SOURCE_SERIES_FILE" ]] || fail "missing_source_series" "--source-series-file must point to a file"
  [[ -n "$CANDIDATE_SHA" ]] || fail "missing_candidate_sha" "--expected-candidate-sha is required"
  validate_sha "$CANDIDATE_SHA"
  [[ -n "$OUT_FILE" ]] || fail "missing_output" "--out is required"
  local record_sha temp_dir record_file state record_candidate record_reviewed review_digest source_mapping
  TEMP_DIR=$(mktemp -d)
  temp_dir="$TEMP_DIR"
  record_file="$temp_dir/record.json"
  record_sha=$(fetch_record)
  record_json "$record_sha" >"$record_file"
  state=$(json_value "$record_file" state)
  [[ "$state" == "candidate" || "$state" == "reviewed" ]] || fail "invalid_verify_state" "verify requires candidate or reviewed lease state"
  [[ "$(json_value "$record_file" lease_revision)" == "$EXPECTED_REVISION" ]] || fail "stale_record" "remote record revision does not match"
  record_candidate=$(json_value "$record_file" candidate_bundle_sha)
  [[ "$record_candidate" == "$CANDIDATE_SHA" ]] || fail "candidate_mismatch" "record candidate_bundle_sha differs from expected candidate"
  record_reviewed=$(json_value "$record_file" reviewed_bundle_sha)
  if [[ "$REVIEWED_SHA" == "empty" ]]; then
    [[ "$record_reviewed" == "null" ]] || fail "reviewed_mismatch" "expected an unreviewed candidate"
  else
    validate_sha "$REVIEWED_SHA"
    [[ "$record_reviewed" == "$REVIEWED_SHA" ]] || fail "reviewed_mismatch" "record reviewed_bundle_sha differs from expected reviewed SHA"
  fi
  [[ "$(git -C "$INTEGRATION_WORKTREE" status --porcelain=v1 --untracked-files=all)" == "" ]] \
    || fail "dirty_integration_worktree" "integration worktree must be clean"
  [[ "$(git -C "$LEASE_WORKTREE" status --porcelain=v1 --untracked-files=all)" == "" ]] \
    || fail "dirty_lease_worktree" "lease worktree must be clean"
  [[ "$(git -C "$INTEGRATION_WORKTREE" rev-parse HEAD)" == "$INTEGRATION_HEAD" ]] \
    || fail "integration_head_mismatch" "integration worktree HEAD differs from expected integration head"
  git -C "$LEASE_WORKTREE" cat-file -e "$CANDIDATE_SHA^{commit}" \
    || fail "candidate_missing" "candidate bundle is not available in the lease worktree"
  git -C "$LEASE_WORKTREE" merge-base --is-ancestor "$INTEGRATION_HEAD" "$CANDIDATE_SHA" \
    || fail "candidate_not_derived" "candidate bundle is not based on the integration head"
  verify_record_source_series "$record_file" "$SOURCE_SERIES_FILE" \
    || fail "source_series_record_mismatch" "source series does not match the lease record"
  source_mapping=$(verify_source_series "$INTEGRATION_HEAD" "$CANDIDATE_SHA" "$SOURCE_SERIES_FILE") \
    || fail "source_series_invalid" "candidate does not contain the complete ordered cherry-pick series"
  review_digest="none"
  if [[ "$REVIEW_RECORD_FILE" != "none" ]]; then
    review_digest=$(validate_review_record "$REVIEW_RECORD_FILE" "$CANDIDATE_SHA") \
      || fail "review_record_invalid" "review record does not bind the candidate"
    [[ "$state" == "reviewed" ]] || fail "reviewed_state_required" "a review record requires reviewed lease state"
    [[ "$(json_value "$record_file" review_record_sha)" == "$review_digest" ]] \
      || fail "review_digest_mismatch" "record review_record_sha differs from review artifact digest"
  else
    [[ "$state" == "candidate" ]] || fail "review_record_required" "reviewed state requires --review-record"
  fi
  write_verification_artifact "$OUT_FILE" "$record_sha" "$CANDIDATE_SHA" "$REVIEWED_SHA" "$review_digest" "$source_mapping"
}

validate_verification_artifact() {
  local file=$1
  [[ -f "$file" ]] || fail "missing_verification_artifact" "verification artifact does not exist"
  python3 - "$file" "$LEASE" "$EXPECTED_REVISION" "$EXPECTED_RECORD_SHA" "$INTEGRATION_HEAD" "$CANDIDATE_SHA" "$REVIEWED_SHA" <<'PY'
import json
import sys

path, lease, revision, record_sha, integration_head, candidate, reviewed = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    artifact = json.load(handle)
if artifact.get("ok") is not True or artifact.get("command") != "verify":
    raise SystemExit("verification artifact is not a successful verify result")
if artifact.get("lease") != lease or str(artifact.get("lease_revision")) != revision:
    raise SystemExit("verification artifact lease or revision mismatch")
if artifact.get("record_commit_sha") != record_sha:
    raise SystemExit("verification artifact record commit mismatch")
if artifact.get("integration_head_at_acquire") != integration_head:
    raise SystemExit("verification artifact integration head mismatch")
if artifact.get("candidate_bundle_sha") != candidate:
    raise SystemExit("verification artifact candidate mismatch")
if artifact.get("reviewed_bundle_sha") != reviewed:
    raise SystemExit("verification artifact reviewed SHA mismatch")
checks = artifact.get("checks")
if not isinstance(checks, dict) or not all(checks.get(key) is True for key in ("record_revision", "integration_head", "source_series", "review_record")):
    raise SystemExit("verification artifact does not contain all successful checks")
mapping = artifact.get("source_mapping")
if not isinstance(mapping, list) or not mapping:
    raise SystemExit("verification artifact does not contain source mapping")
for item in mapping:
    if not isinstance(item, dict) or not all(isinstance(item.get(key), str) and item[key] for key in ("source_sha", "candidate_commit_sha", "patch_id")):
        raise SystemExit("verification artifact contains an invalid source mapping")
PY
}

command_promote() {
  validate_lease
  [[ -n "$EXPECTED_RECORD_SHA" ]] || fail "missing_expected_record_sha" "--expected-record-sha is required"
  [[ -n "$EXPECTED_REVISION" ]] || fail "missing_expected_revision" "--expected-revision is required"
  [[ "$EXPECTED_REVISION" =~ ^[1-9][0-9]*$ ]] || fail "invalid_revision" "--expected-revision must be a positive integer"
  [[ -n "$INTEGRATION_HEAD" ]] || fail "missing_integration_head" "--expected-integration-head is required"
  validate_sha "$EXPECTED_RECORD_SHA"
  validate_sha "$INTEGRATION_HEAD"
  [[ -n "$CANDIDATE_SHA" ]] || fail "missing_candidate_sha" "--candidate-sha is required"
  validate_sha "$CANDIDATE_SHA"
  [[ -n "$REVIEW_RECORD_FILE" && "$REVIEW_RECORD_FILE" != "none" ]] || fail "missing_review_record" "--review-record is required"
  [[ -n "$VERIFICATION_ARTIFACT" ]] || fail "missing_verification_artifact" "--verification-artifact is required"
  local ref current_sha temp_dir record_file state record_candidate record_reviewed record_integration_head review_digest
  ref=$(lease_ref)
  current_sha=$(remote_ref_sha "$ref")
  [[ "$current_sha" == "$EXPECTED_RECORD_SHA" ]] || fail "stale_record" "remote lease record changed; refusing to promote"
  local integration_sha
  integration_sha=$(remote_ref_sha "$INTEGRATION_REF")
  [[ "$integration_sha" == "$INTEGRATION_HEAD" ]] || fail "stale_integration" "remote integration ref changed; refusing atomic promotion"
  TEMP_DIR=$(mktemp -d)
  temp_dir="$TEMP_DIR"
  record_file="$temp_dir/record.json"
  git_repo fetch --no-tags "$REMOTE" "$ref" >/dev/null 2>&1 \
    || fail "record_fetch_failed" "could not fetch remote lease record"
  record_json "$current_sha" >"$record_file"
  record_integration_head=$(json_value "$record_file" integration_head_at_acquire)
  [[ "$record_integration_head" == "$INTEGRATION_HEAD" ]] \
    || fail "integration_head_record_mismatch" "expected integration head differs from lease acquisition head"
  state=$(json_value "$record_file" state)
  [[ "$state" == "reviewed" ]] || fail "invalid_promote_state" "promote requires reviewed lease state"
  record_candidate=$(json_value "$record_file" candidate_bundle_sha)
  [[ "$record_candidate" == "$CANDIDATE_SHA" ]] || fail "candidate_mismatch" "record candidate_bundle_sha differs from --candidate-sha"
  record_reviewed=$(json_value "$record_file" reviewed_bundle_sha)
  [[ "$record_reviewed" == "$CANDIDATE_SHA" ]] || fail "reviewed_mismatch" "record reviewed_bundle_sha must equal candidate"
  review_digest=$(validate_review_record "$REVIEW_RECORD_FILE" "$CANDIDATE_SHA") \
    || fail "review_record_invalid" "review record does not bind the candidate"
  [[ "$(json_value "$record_file" review_record_sha)" == "$review_digest" ]] \
    || fail "review_digest_mismatch" "record review_record_sha differs from review artifact digest"
  REVIEWED_SHA="$CANDIDATE_SHA"
  validate_verification_artifact "$VERIFICATION_ARTIFACT" \
    || fail "verification_artifact_invalid" "verification artifact does not bind this promotion"
  local integrated_file integrated_commit
  integrated_file="$temp_dir/integrated.json"
  python3 - "$record_file" "$integrated_file" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["state"] = "integrated"
record["integrated_sha"] = record["candidate_bundle_sha"]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
  integrated_commit=$(create_record_commit "$integrated_file" "$EXPECTED_RECORD_SHA" "reader-vnext lease $LEASE promoted")
  # One push, two force-with-lease guards, and no fallback. If either remote
  # ref rejects the update, the remote must not accept either side.
  git_repo push --porcelain --atomic \
    --force-with-lease="$INTEGRATION_REF:$INTEGRATION_HEAD" \
    --force-with-lease="$ref:$EXPECTED_RECORD_SHA" \
    "$REMOTE" "$CANDIDATE_SHA:$INTEGRATION_REF" "$integrated_commit:$ref" >/dev/null \
    || fail "atomic_promote_failed" "atomic promotion was rejected; neither ref may be treated as integrated"
  [[ "$(remote_ref_sha "$INTEGRATION_REF")" == "$CANDIDATE_SHA" ]] \
    || fail "integration_postcondition_failed" "integration ref did not reach candidate"
  [[ "$(remote_ref_sha "$ref")" == "$integrated_commit" ]] \
    || fail "record_postcondition_failed" "lease ref did not reach integrated record"
  emit_record_result "promote" "$integrated_commit" "$integrated_file"
}

parse_args() {
  local command_name=${1:-}
  [[ -n "$command_name" ]] || { usage; exit 2; }
  shift
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --remote) [[ $# -ge 2 ]] || fail "missing_value" "--remote requires a value"; REMOTE=$2; shift 2 ;;
      --repo) [[ $# -ge 2 ]] || fail "missing_value" "--repo requires a value"; REPO=$2; shift 2 ;;
      --lease) [[ $# -ge 2 ]] || fail "missing_value" "--lease requires a value"; LEASE=$2; shift 2 ;;
      --expected-record-sha) [[ $# -ge 2 ]] || fail "missing_value" "--expected-record-sha requires a value"; EXPECTED_RECORD_SHA=$2; shift 2 ;;
      --expected-revision) [[ $# -ge 2 ]] || fail "missing_value" "--expected-revision requires a value"; EXPECTED_REVISION=$2; shift 2 ;;
      --next-record) [[ $# -ge 2 ]] || fail "missing_value" "--next-record requires a value"; NEXT_RECORD_FILE=$2; shift 2 ;;
      --owner) [[ $# -ge 2 ]] || fail "missing_value" "--owner requires a value"; OWNER=$2; shift 2 ;;
      --branch) [[ $# -ge 2 ]] || fail "missing_value" "--branch requires a value"; BRANCH=$2; shift 2 ;;
      --files-file) [[ $# -ge 2 ]] || fail "missing_value" "--files-file requires a value"; FILES_FILE=$2; shift 2 ;;
      --integration-head|--expected-integration-head) [[ $# -ge 2 ]] || fail "missing_value" "$1 requires a value"; INTEGRATION_HEAD=$2; shift 2 ;;
      --expected-release) [[ $# -ge 2 ]] || fail "missing_value" "--expected-release requires a value"; EXPECTED_RELEASE=$2; shift 2 ;;
      --source-series-file) [[ $# -ge 2 ]] || fail "missing_value" "--source-series-file requires a value"; SOURCE_SERIES_FILE=$2; shift 2 ;;
      --expected-candidate-sha|--candidate-sha) [[ $# -ge 2 ]] || fail "missing_value" "$1 requires a value"; CANDIDATE_SHA=$2; shift 2 ;;
      --expected-reviewed-sha) [[ $# -ge 2 ]] || fail "missing_value" "--expected-reviewed-sha requires a value"; REVIEWED_SHA=$2; shift 2 ;;
      --review-record) [[ $# -ge 2 ]] || fail "missing_value" "--review-record requires a value"; REVIEW_RECORD_FILE=$2; shift 2 ;;
      --out) [[ $# -ge 2 ]] || fail "missing_value" "--out requires a value"; OUT_FILE=$2; shift 2 ;;
      --integration-worktree) [[ $# -ge 2 ]] || fail "missing_value" "--integration-worktree requires a value"; INTEGRATION_WORKTREE=$2; shift 2 ;;
      --lease-worktree) [[ $# -ge 2 ]] || fail "missing_value" "--lease-worktree requires a value"; LEASE_WORKTREE=$2; shift 2 ;;
      --verification-artifact) [[ $# -ge 2 ]] || fail "missing_value" "--verification-artifact requires a value"; VERIFICATION_ARTIFACT=$2; shift 2 ;;
      --help|-h) usage; exit 0 ;;
      *) fail "unknown_argument" "unknown argument: $1" ;;
    esac
  done
  case "$command_name" in
    init) command_init ;;
    read) command_read ;;
    cas) command_cas ;;
    verify) command_verify ;;
    promote) command_promote ;;
    help) usage ;;
    *) fail "unknown_command" "unknown command: $command_name" ;;
  esac
}

need_command git
need_command python3
need_command sha256sum
parse_args "$@"
