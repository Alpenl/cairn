#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

fail() {
	echo "core release verify: $*" >&2
	exit 1
}

[[ $# -gt 0 ]] || fail 'usage: core-release-verify.sh <archive>...'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for archive in "$@"; do
	name=$(basename "$archive" .tar.gz)
	[[ $name =~ ^cairn_([0-9]+\.[0-9]+\.[0-9]+)_linux_(amd64|arm64)$ ]] ||
		fail "$archive does not use the canonical Core archive name"
	archive_version=${BASH_REMATCH[1]}
	arch=${BASH_REMATCH[2]}
	[[ $archive_version != 0.0.0 ]] || fail "$archive uses the development placeholder version"
	destination="$work/$name"
	mkdir -p "$destination"
	tar -xzf "$archive" -C "$destination"
	root="$destination/$name"

	for binary in webtag migrate; do
		test -x "$root/$binary" || fail "$archive is missing executable $binary"
	done
	test -r "$root/BUILD-PROVENANCE.json" || fail "$archive is missing BUILD-PROVENANCE.json"
	test -d "$root/legal" || fail "$archive is missing its legal directory"
	if ! diff -qr "$ROOT/legal/core/common" "$root/legal" >"$work/legal.diff"; then
		cat "$work/legal.diff" >&2
		fail "$archive legal materials differ from the frozen common closure"
	fi
	while IFS= read -r -d '' material; do
		test -r "$material" || fail "$archive contains unreadable legal material ${material#"$root/"}"
	done < <(find "$root/legal" -type f -print0)

	jq -e --arg arch "$arch" --arg version "$archive_version" \
		'.version == $version and
		 (.commit | type == "string" and test("^[0-9a-f]{40}$")) and
		 (.build_time | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$")) and
		 .source_state == "clean" and .os == "linux" and .arch == $arch' \
		"$root/BUILD-PROVENANCE.json" >/dev/null || fail "$archive provenance has invalid platform or source state"
	provenance_version=$(jq -er '.version' "$root/BUILD-PROVENANCE.json")
	provenance_commit=$(jq -er '.commit' "$root/BUILD-PROVENANCE.json")
	provenance_build_time=$(jq -er '.build_time' "$root/BUILD-PROVENANCE.json")
	if [[ -n ${VERSION:-} ]]; then
		jq -e --arg version "$VERSION" '.version == $version' "$root/BUILD-PROVENANCE.json" >/dev/null ||
			fail "$archive provenance does not contain version $VERSION"
	fi
	if [[ -n ${COMMIT:-} ]]; then
		[[ $COMMIT =~ ^[0-9a-f]{40}$ ]] || fail 'COMMIT must be a full lowercase Git revision'
		jq -e --arg commit "$COMMIT" '.commit == $commit' "$root/BUILD-PROVENANCE.json" >/dev/null ||
			fail "$archive provenance does not contain target revision $COMMIT"
	fi
	if [[ -n ${BUILD_TIME:-} ]]; then
		jq -e --arg build_time "$BUILD_TIME" '.build_time == $build_time' "$root/BUILD-PROVENANCE.json" >/dev/null ||
			fail "$archive provenance does not contain build time $BUILD_TIME"
	fi
	for binary in webtag migrate; do
		metadata=$(go version -m "$root/$binary")
		grep -Fq $'path\twebtag/cmd/'"$binary" <<<"$metadata" || fail "$archive $binary is not cmd/$binary"
		grep -Fq 'GOOS=linux' <<<"$metadata" || fail "$archive $binary is not linux"
		grep -Fq "GOARCH=$arch" <<<"$metadata" || fail "$archive $binary is not $arch"
		for value in "$provenance_version" "$provenance_commit" "$provenance_build_time"; do
			grep -aFq -- "$value" "$root/$binary" || fail "$archive $binary does not embed provenance identity value $value"
		done
		hash=$(sha256sum "$root/$binary")
		hash=${hash%% *}
		jq -e --arg binary "$binary" --arg hash "$hash" '.binaries[$binary].sha256 == $hash' \
			"$root/BUILD-PROVENANCE.json" >/dev/null || fail "$archive provenance hash does not match $binary"
	done

	execute=${CORE_RELEASE_EXECUTE:-auto}
	if [[ $execute == true ]] ||
		[[ $execute == auto && $(go env GOOS) == linux && $(go env GOARCH) == "$arch" ]]; then
		expected=$(printf 'cairn %s\ncommit: %s\nbuilt: %s' \
			"$provenance_version" "$provenance_commit" "$provenance_build_time")
		for binary in webtag migrate; do
			stderr="$work/${name}-${binary}.stderr"
			if ! actual=$("$root/$binary" --version 2>"$stderr"); then
				cat "$stderr" >&2
				fail "$archive $binary --version could not execute"
			fi
			[[ ! -s $stderr ]] || fail "$archive $binary --version wrote to stderr"
			[[ $actual == "$expected" ]] || fail "$archive $binary --version does not match provenance"
		done
	elif [[ $execute != auto && $execute != false ]]; then
		fail 'CORE_RELEASE_EXECUTE must be auto, true, or false'
	fi
done

echo "PASS: verified $# Core release archive(s)"
