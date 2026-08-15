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
	common/DISTRIBUTION_BOUNDARY.txt \
	full/YT_DLP_LICENSE.txt \
	full/YT_DLP_SOURCE.txt; do
	test -s "$TMP/core/$material" || fail "missing generated material $material"
done

grep -Fq 'codeberg.org/readeck/go-readability/v2 v2.1.2' "$TMP/core/common/GO_WEBTAG_THIRD_PARTY.txt" ||
	fail 'webtag closure omits readability'
if grep -Fq 'codeberg.org/readeck/go-readability/v2' "$TMP/core/common/GO_MIGRATE_THIRD_PARTY.txt"; then
	fail 'migrate closure incorrectly contains webtag-only readability'
fi
# 从 Dockerfile 读取真实 pin 而不是硬编码版本号：这条断言的用意是「法律材料
# 与镜像里实际装的 yt-dlp 是同一个版本」，写死之后升级 pin 只会让它红，反过来
# 如果连同断言一起改掉，它就退化成「两个常量相等」，再也验证不了一致性。
ytdlp_pin=$(sed -n 's/^ARG YTDLP_VERSION=\([0-9.]\{1,\}\)$/\1/p' "$ROOT/Dockerfile")
[ -n "$ytdlp_pin" ] || fail 'cannot read YTDLP_VERSION from Dockerfile'
grep -Fq "Version: ${ytdlp_pin}" "$TMP/core/full/YT_DLP_SOURCE.txt" ||
	fail "yt-dlp source does not match the Dockerfile pin (${ytdlp_pin})"

printf '\nstale\n' >>"$TMP/core/common/CAIRN_LICENSE.txt"
if node "$GENERATOR" check "$TMP/core" >"$TMP/stale.log" 2>&1; then
	fail 'freshness check accepted modified legal material'
fi
grep -Fq 'common/CAIRN_LICENSE.txt' "$TMP/stale.log" ||
	fail 'freshness failure did not identify the changed file'

echo 'PASS: Core legal materials are complete, scoped, and freshness-checked'
