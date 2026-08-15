#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
LEASE_SCRIPT="$ROOT/scripts/reader-vnext-lease.sh"
ZERO_SHA=0000000000000000000000000000000000000000
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_eq() {
  [[ "$1" == "$2" ]] || fail "expected [$1], got [$2]"
}

assert_failed() {
  if "$@" >"$TMP/failure.out" 2>"$TMP/failure.err"; then
    fail "command unexpectedly succeeded: $*"
  fi
}

git init --bare "$TMP/remote.git" >/dev/null
git init "$TMP/repo" >/dev/null
git -C "$TMP/repo" config user.name test
git -C "$TMP/repo" config user.email test@example.invalid
printf 'base\n' >"$TMP/repo/article.txt"
git -C "$TMP/repo" add article.txt
git -C "$TMP/repo" commit -m base >/dev/null
BASE=$(git -C "$TMP/repo" rev-parse HEAD)
git -C "$TMP/repo" remote add origin "$TMP/remote.git"
git -C "$TMP/repo" push origin "$BASE:refs/heads/int/reader-vnext" >/dev/null

INIT_JSON="$TMP/init.json"
"$LEASE_SCRIPT" init --repo "$TMP/repo" --remote origin --lease w01 --owner test-agent \
  --branch agent/reader-w01 --integration-head "$BASE" --expected-release 'test' >"$INIT_JSON"
INITIAL_RECORD=$(python3 - "$INIT_JSON" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)

git -C "$TMP/repo" checkout -b source "$BASE" >/dev/null
printf 'source\n' >>"$TMP/repo/article.txt"
git -C "$TMP/repo" add article.txt
git -C "$TMP/repo" commit -m source >/dev/null
SOURCE_SHA=$(git -C "$TMP/repo" rev-parse HEAD)
git -C "$TMP/repo" checkout -b candidate "$BASE" >/dev/null
git -C "$TMP/repo" cherry-pick -x "$SOURCE_SHA" >/dev/null
CANDIDATE_SHA=$(git -C "$TMP/repo" rev-parse HEAD)
printf '%s\n' "$SOURCE_SHA" >"$TMP/source-series.txt"
git -C "$TMP/repo" checkout -b integration "$BASE" >/dev/null

python3 - "$INIT_JSON" "$TMP/candidate.json" "$SOURCE_SHA" "$CANDIDATE_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)["record"]
record["state"] = "candidate"
record["source_delivery_shas"] = [sys.argv[3]]
record["candidate_bundle_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY

"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$INITIAL_RECORD" --expected-revision 1 \
  --next-record "$TMP/candidate.json" >"$TMP/candidate-result.json"
CANDIDATE_RECORD=$(python3 - "$TMP/candidate-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)

# A candidate cannot change its lease epoch or immutable owner metadata.
python3 - "$TMP/candidate.json" "$TMP/candidate-revision.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["lease_revision"] = 2
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
assert_failed "$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$CANDIDATE_RECORD" --expected-revision 1 \
  --next-record "$TMP/candidate-revision.json"
python3 - "$TMP/candidate.json" "$TMP/candidate-owner.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["owner"] = "different-agent"
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
assert_failed "$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$CANDIDATE_RECORD" --expected-revision 1 \
  --next-record "$TMP/candidate-owner.json"

# A stale expected record cannot overwrite a candidate.
assert_failed "$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$INITIAL_RECORD" --expected-revision 1 \
  --next-record "$TMP/candidate.json"

cat >"$TMP/review.json" <<EOF
{"lease":"w01","lease_revision":1,"delivery_sha":"$CANDIDATE_SHA","verdict":"approved","reviewer":"test-reviewer","reviewed_at":"2026-08-09T00:00:00Z"}
EOF
REVIEW_SHA=$(sha256sum "$TMP/review.json" | awk '{print $1}')
python3 - "$TMP/candidate.json" "$TMP/reviewed.json" "$CANDIDATE_SHA" "$REVIEW_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["state"] = "reviewed"
record["reviewed_bundle_sha"] = sys.argv[3]
record["review_record_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$CANDIDATE_RECORD" --expected-revision 1 \
  --next-record "$TMP/reviewed.json" >"$TMP/reviewed-result.json"
REVIEWED_RECORD=$(python3 - "$TMP/reviewed-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)

# A review record with the wrong lease revision must be rejected by verify.
cat >"$TMP/wrong-review.json" <<EOF
{"lease":"w01","lease_revision":2,"delivery_sha":"$CANDIDATE_SHA","verdict":"approved","reviewer":"test-reviewer","reviewed_at":"2026-08-09T00:00:00Z"}
EOF
assert_failed "$LEASE_SCRIPT" verify --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-revision 1 --integration-worktree "$TMP/repo" --lease-worktree "$TMP/repo" \
  --expected-integration-head "$BASE" --source-series-file "$TMP/source-series.txt" \
  --expected-candidate-sha "$CANDIDATE_SHA" --expected-reviewed-sha "$CANDIDATE_SHA" \
  --review-record "$TMP/wrong-review.json" --out "$TMP/wrong-verification.json"

# A source list that is not the record's ordered delivery list must be rejected.
printf '%s\n' "$CANDIDATE_SHA" >"$TMP/wrong-source-series.txt"
assert_failed "$LEASE_SCRIPT" verify --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-revision 1 --integration-worktree "$TMP/repo" --lease-worktree "$TMP/repo" \
  --expected-integration-head "$BASE" --source-series-file "$TMP/wrong-source-series.txt" \
  --expected-candidate-sha "$CANDIDATE_SHA" --expected-reviewed-sha "$CANDIDATE_SHA" \
  --review-record "$TMP/review.json" --out "$TMP/wrong-source-verification.json"

"$LEASE_SCRIPT" verify --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-revision 1 --integration-worktree "$TMP/repo" --lease-worktree "$TMP/repo" \
  --expected-integration-head "$BASE" --source-series-file "$TMP/source-series.txt" \
  --expected-candidate-sha "$CANDIDATE_SHA" --expected-reviewed-sha "$CANDIDATE_SHA" \
  --review-record "$TMP/review.json" --out "$TMP/verification.json" >"$TMP/verification-result.json"

# A direct CAS attempt to enter integrated is forbidden.
python3 - "$TMP/reviewed.json" "$TMP/integrated-by-cas.json" "$CANDIDATE_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["state"] = "integrated"
record["integrated_sha"] = sys.argv[3]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
assert_failed "$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$REVIEWED_RECORD" --expected-revision 1 \
  --next-record "$TMP/integrated-by-cas.json"

"$LEASE_SCRIPT" promote --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$REVIEWED_RECORD" --expected-revision 1 \
  --expected-integration-head "$BASE" --candidate-sha "$CANDIDATE_SHA" \
  --review-record "$TMP/review.json" --verification-artifact "$TMP/verification.json" \
  >"$TMP/promote-result.json"
assert_eq "$(git ls-remote "$TMP/remote.git" refs/heads/int/reader-vnext | awk '{print $1}')" "$CANDIDATE_SHA"
INTEGRATED_RECORD=$(git -C "$TMP/repo" ls-remote origin refs/heads/coord/reader-vnext-leases/w01 | awk '{print $1}')
git -C "$TMP/repo" fetch --no-tags origin refs/heads/coord/reader-vnext-leases/w01 >/dev/null
assert_eq "$(git -C "$TMP/repo" show "$INTEGRATED_RECORD:record.json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')" integrated

# An integrated record cannot be moved backward through CAS.
assert_failed "$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w01 \
  --expected-record-sha "$INTEGRATED_RECORD" --expected-revision 1 \
  --next-record "$TMP/candidate.json"

# A failed atomic promotion must leave both refs unchanged. Use a second lease
# and an intentionally stale integration expected SHA.
"$LEASE_SCRIPT" init --repo "$TMP/repo" --remote origin --lease w02 --owner test-agent \
  --branch agent/reader-w02 --integration-head "$BASE" --expected-release test >"$TMP/init-w02.json"
INITIAL_W02=$(python3 - "$TMP/init-w02.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)
python3 - "$TMP/init-w02.json" "$TMP/candidate-w02.json" "$SOURCE_SHA" "$CANDIDATE_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)["record"]
record["state"] = "candidate"
record["source_delivery_shas"] = [sys.argv[3]]
record["candidate_bundle_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w02 --expected-record-sha "$INITIAL_W02" --expected-revision 1 --next-record "$TMP/candidate-w02.json" >"$TMP/candidate-w02-result.json"
CANDIDATE_W02=$(python3 - "$TMP/candidate-w02-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)
python3 - "$TMP/candidate-w02.json" "$TMP/reviewed-w02.json" "$CANDIDATE_SHA" "$REVIEW_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["state"] = "reviewed"
record["reviewed_bundle_sha"] = sys.argv[3]
record["review_record_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w02 --expected-record-sha "$CANDIDATE_W02" --expected-revision 1 --next-record "$TMP/reviewed-w02.json" >"$TMP/reviewed-w02-result.json"
REVIEWED_W02=$(python3 - "$TMP/reviewed-w02-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)

assert_failed "$LEASE_SCRIPT" promote --repo "$TMP/repo" --remote origin --lease w02 \
  --expected-record-sha "$REVIEWED_W02" --expected-revision 1 \
  --expected-integration-head "$ZERO_SHA" --candidate-sha "$CANDIDATE_SHA" \
  --review-record "$TMP/review.json" --verification-artifact "$TMP/verification.json"
assert_eq "$(git ls-remote "$TMP/remote.git" refs/heads/int/reader-vnext | awk '{print $1}')" "$CANDIDATE_SHA"
W02_RECORD=$(git -C "$TMP/repo" ls-remote origin refs/heads/coord/reader-vnext-leases/w02 | awk '{print $1}')
git -C "$TMP/repo" fetch --no-tags origin refs/heads/coord/reader-vnext-leases/w02 >/dev/null
assert_eq "$(git -C "$TMP/repo" show "$W02_RECORD:record.json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')" reviewed

# A hook rejection of one promotion ref must leave both refs unchanged.
"$LEASE_SCRIPT" init --repo "$TMP/repo" --remote origin --lease w03 --owner test-agent \
  --branch agent/reader-w03 --integration-head "$CANDIDATE_SHA" --expected-release test >"$TMP/init-w03.json"
INITIAL_W03=$(python3 - "$TMP/init-w03.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)
git -C "$TMP/repo" checkout -b source-w03 "$CANDIDATE_SHA" >/dev/null
printf 'source-w03\n' >>"$TMP/repo/article.txt"
git -C "$TMP/repo" add article.txt
git -C "$TMP/repo" commit -m source-w03 >/dev/null
SOURCE_W03_SHA=$(git -C "$TMP/repo" rev-parse HEAD)
git -C "$TMP/repo" checkout -b candidate-w03 "$CANDIDATE_SHA" >/dev/null
git -C "$TMP/repo" cherry-pick -x "$SOURCE_W03_SHA" >/dev/null
CANDIDATE_W03_SHA=$(git -C "$TMP/repo" rev-parse HEAD)
printf '%s\n' "$SOURCE_W03_SHA" >"$TMP/source-series-w03.txt"
git -C "$TMP/repo" checkout integration >/dev/null
git -C "$TMP/repo" reset --hard "$CANDIDATE_SHA" >/dev/null
python3 - "$TMP/init-w03.json" "$TMP/candidate-w03.json" "$SOURCE_W03_SHA" "$CANDIDATE_W03_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)["record"]
record["state"] = "candidate"
record["source_delivery_shas"] = [sys.argv[3]]
record["candidate_bundle_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w03 \
  --expected-record-sha "$INITIAL_W03" --expected-revision 1 \
  --next-record "$TMP/candidate-w03.json" >"$TMP/candidate-w03-result.json"
CANDIDATE_RECORD_W03=$(python3 - "$TMP/candidate-w03-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)
cat >"$TMP/review-w03.json" <<EOF
{"lease":"w03","lease_revision":1,"delivery_sha":"$CANDIDATE_W03_SHA","verdict":"approved","reviewer":"test-reviewer","reviewed_at":"2026-08-09T00:00:00Z"}
EOF
REVIEW_W03_SHA=$(sha256sum "$TMP/review-w03.json" | awk '{print $1}')
python3 - "$TMP/candidate-w03.json" "$TMP/reviewed-w03.json" "$CANDIDATE_W03_SHA" "$REVIEW_W03_SHA" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    record = json.load(handle)
record["state"] = "reviewed"
record["reviewed_bundle_sha"] = sys.argv[3]
record["review_record_sha"] = sys.argv[4]
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
"$LEASE_SCRIPT" cas --repo "$TMP/repo" --remote origin --lease w03 \
  --expected-record-sha "$CANDIDATE_RECORD_W03" --expected-revision 1 \
  --next-record "$TMP/reviewed-w03.json" >"$TMP/reviewed-w03-result.json"
REVIEWED_RECORD_W03=$(python3 - "$TMP/reviewed-w03-result.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    print(json.load(handle)["record_commit_sha"])
PY
)
"$LEASE_SCRIPT" verify --repo "$TMP/repo" --remote origin --lease w03 \
  --expected-revision 1 --integration-worktree "$TMP/repo" --lease-worktree "$TMP/repo" \
  --expected-integration-head "$CANDIDATE_SHA" --source-series-file "$TMP/source-series-w03.txt" \
  --expected-candidate-sha "$CANDIDATE_W03_SHA" --expected-reviewed-sha "$CANDIDATE_W03_SHA" \
  --review-record "$TMP/review-w03.json" --out "$TMP/verification-w03.json" \
  >"$TMP/verification-w03-result.json"
BEFORE_INTEGRATION=$(git ls-remote "$TMP/remote.git" refs/heads/int/reader-vnext | awk '{print $1}')
BEFORE_W03_RECORD=$(git -C "$TMP/repo" ls-remote origin refs/heads/coord/reader-vnext-leases/w03 | awk '{print $1}')
cat >"$TMP/remote.git/hooks/pre-receive" <<'EOF'
#!/usr/bin/env bash
while read -r _old _new ref; do
  if [[ "$ref" == "refs/heads/coord/reader-vnext-leases/w03" ]]; then
    exit 1
  fi
done
exit 0
EOF
chmod +x "$TMP/remote.git/hooks/pre-receive"
assert_failed "$LEASE_SCRIPT" promote --repo "$TMP/repo" --remote origin --lease w03 \
  --expected-record-sha "$REVIEWED_RECORD_W03" --expected-revision 1 \
  --expected-integration-head "$CANDIDATE_SHA" --candidate-sha "$CANDIDATE_W03_SHA" \
  --review-record "$TMP/review-w03.json" --verification-artifact "$TMP/verification-w03.json"
assert_eq "$(git ls-remote "$TMP/remote.git" refs/heads/int/reader-vnext | awk '{print $1}')" "$BEFORE_INTEGRATION"
assert_eq "$(git -C "$TMP/repo" ls-remote origin refs/heads/coord/reader-vnext-leases/w03 | awk '{print $1}')" "$BEFORE_W03_RECORD"

echo 'PASS: LC-01 init/read/CAS/verify/promote and atomic failure fences'
