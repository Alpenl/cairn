#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT="$ROOT/scripts/core-release-build.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

expect_rejected() {
	local name=$1
	local version=$2
	local commit=$3
	local build_time=$4
	local log="$TMP/$name.log"

	if VERSION=$version COMMIT=$commit BUILD_TIME=$build_time OUT_DIR="$TMP/out" TARGET_ARCHES=amd64 \
		bash "$SCRIPT" >"$log" 2>&1; then
		fail "$name formal identity was accepted"
	fi
	grep -Eq 'development placeholder|full lowercase Git revision|RFC3339 timestamp' "$log" ||
		fail "$name did not report an identity validation error"
}

full_commit=0123456789abcdef0123456789abcdef01234567
expect_rejected placeholder-version 0.0.0 "$full_commit" 2026-08-14T01:02:03Z
expect_rejected short-commit 1.2.3 0123456789ab 2026-08-14T01:02:03Z
expect_rejected missing-build-time 1.2.3 "$full_commit" unknown

head_commit=$(git -C "$ROOT" rev-parse HEAD)
wrong_commit=0000000000000000000000000000000000000000
if VERSION=1.2.3 COMMIT=$wrong_commit BUILD_TIME=2026-08-14T01:02:03Z OUT_DIR="$TMP/out" TARGET_ARCHES=amd64 \
	bash "$SCRIPT" >"$TMP/wrong-commit.log" 2>&1; then
	fail 'source HEAD mismatch was accepted'
fi
grep -Fq 'does not match source HEAD' "$TMP/wrong-commit.log" || fail 'source HEAD mismatch was not reported'

dirty_file="$ROOT/.core-release-dirty-test"
trap 'rm -f "$dirty_file"; rm -rf "$TMP"' EXIT
printf 'dirty\n' >"$dirty_file"
if VERSION=1.2.3 COMMIT=$head_commit BUILD_TIME=2026-08-14T01:02:03Z OUT_DIR="$TMP/out" TARGET_ARCHES=amd64 \
	bash "$SCRIPT" >"$TMP/dirty.log" 2>&1; then
	fail 'dirty formal source tree was accepted'
fi
grep -Fq 'source tree must be clean' "$TMP/dirty.log" || fail 'dirty source failure was not reported'
rm -f "$dirty_file"

echo 'PASS: formal Core builds reject placeholder and incomplete identity metadata'
