#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
VERIFY="$ROOT/scripts/core-release-verify.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

VERSION=1.2.3
COMMIT=0123456789abcdef0123456789abcdef01234567
WRONG_COMMIT=89abcdef0123456789abcdef0123456789abcdef
BUILD_TIME=2026-08-14T01:02:03Z
PACKAGE_NAME="cairn_${VERSION}_linux_amd64"
PACKAGE="$TMP/$PACKAGE_NAME"
ARCHIVE="$TMP/$PACKAGE_NAME.tar.gz"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

build_binary() {
	local command=$1
	local output=$2
	local commit=$3
	local tags=${4:-}
	local args=(-mod=vendor -trimpath)
	if [[ -n $tags ]]; then
		args+=("-tags=$tags")
	fi
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build "${args[@]}" \
		-ldflags "-s -w -X webtag/internal/buildinfo.Version=$VERSION -X webtag/internal/buildinfo.Commit=$commit -X webtag/internal/buildinfo.BuildTime=$BUILD_TIME" \
		-o "$output" "./cmd/$command"
}

write_provenance() {
	local source_state=$1
	local webtag_sha migrate_sha
	webtag_sha=$(sha256sum "$PACKAGE/webtag")
	webtag_sha=${webtag_sha%% *}
	migrate_sha=$(sha256sum "$PACKAGE/migrate")
	migrate_sha=${migrate_sha%% *}
	jq -n \
		--arg version "$VERSION" \
		--arg commit "$COMMIT" \
		--arg build_time "$BUILD_TIME" \
		--arg source_state "$source_state" \
		--arg webtag_sha "$webtag_sha" \
		--arg migrate_sha "$migrate_sha" \
		'{version: $version, commit: $commit, build_time: $build_time, source_state: $source_state, os: "linux", arch: "amd64", binaries: {webtag: {sha256: $webtag_sha}, migrate: {sha256: $migrate_sha}}}' \
		>"$PACKAGE/BUILD-PROVENANCE.json"
}

pack() {
	tar -C "$TMP" -czf "$ARCHIVE" "$PACKAGE_NAME"
}

expect_rejected() {
	local name=$1
	if VERSION=$VERSION COMMIT=$COMMIT BUILD_TIME=$BUILD_TIME CORE_RELEASE_EXECUTE=true \
		bash "$VERIFY" "$ARCHIVE" >"$TMP/$name.log" 2>&1; then
		fail "$name artifact was accepted"
	fi
}

mkdir -p "$PACKAGE/legal"
cp -R "$ROOT/legal/core/common/." "$PACKAGE/legal/"
build_binary webtag "$PACKAGE/webtag" "$COMMIT" nomsgpack,sonic
build_binary migrate "$PACKAGE/migrate" "$COMMIT"
write_provenance clean
pack
VERSION=$VERSION COMMIT=$COMMIT BUILD_TIME=$BUILD_TIME CORE_RELEASE_EXECUTE=true \
	bash "$VERIFY" "$ARCHIVE"

build_binary migrate "$PACKAGE/migrate" "$WRONG_COMMIT"
write_provenance clean
pack
expect_rejected wrong-commit
grep -Eq 'does not embed provenance identity|does not match provenance' "$TMP/wrong-commit.log" ||
	fail 'wrong-commit rejection did not identify the binary identity mismatch'

build_binary migrate "$PACKAGE/migrate" "$COMMIT"
write_provenance dirty
pack
expect_rejected dirty
grep -Fq 'invalid platform or source state' "$TMP/dirty.log" ||
	fail 'dirty provenance rejection did not identify source state'

write_provenance clean
printf '\ntampered\n' >>"$PACKAGE/legal/CAIRN_LICENSE.txt"
pack
expect_rejected legal-tamper
grep -Fq 'legal materials differ from the frozen common closure' "$TMP/legal-tamper.log" ||
	fail 'legal tamper rejection did not identify the frozen closure'

echo 'PASS: Core archives bind both executables and legal materials to release provenance'
