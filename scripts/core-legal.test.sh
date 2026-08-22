#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
GENERATOR="$ROOT/scripts/core-legal.mjs"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

node "$GENERATOR" generate "$TMP/core"
node "$GENERATOR" check "$TMP/core"

for material in \
	common/CAIRN_LICENSE.txt \
	common/OPENCC_LICENSE.txt \
	common/OPENCC_SOURCE.txt \
	common/GO_WEBTAG_THIRD_PARTY.txt \
	common/GO_MIGRATE_THIRD_PARTY.txt \
	common/READER_THIRD_PARTY.txt \
	common/DISTRIBUTION_BOUNDARY.txt; do
	test -s "$TMP/core/$material" || fail "missing generated material $material"
done

grep -Fq 'codeberg.org/readeck/go-readability/v2 v2.1.2' "$TMP/core/common/GO_WEBTAG_THIRD_PARTY.txt" ||
	fail 'webtag closure omits readability'
if grep -Fq 'codeberg.org/readeck/go-readability/v2' "$TMP/core/common/GO_MIGRATE_THIRD_PARTY.txt"; then
	fail 'migrate closure incorrectly contains webtag-only readability'
fi

printf '\nstale\n' >>"$TMP/core/common/CAIRN_LICENSE.txt"
if node "$GENERATOR" check "$TMP/core" >"$TMP/stale.log" 2>&1; then
	fail 'freshness check accepted modified legal material'
fi
grep -Fq 'common/CAIRN_LICENSE.txt' "$TMP/stale.log" ||
	fail 'freshness failure did not identify the changed file'

echo 'PASS: Core legal materials are complete, scoped, and freshness-checked'
