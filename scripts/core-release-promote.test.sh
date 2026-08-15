#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT="$ROOT/scripts/core-release-promote.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

mkdir -p "$TMP/bin" "$TMP/state/images" "$TMP/state/assets" "$TMP/assets"

cat >"$TMP/bin/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
key() { printf '%s' "$1" | tr '/:@' '____'; }
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
	file="$FAKE_STATE/images/$(key "$4")"
	[[ -s $file ]] || exit 1
	printf '{"digest":"%s"}\n' "$(cat "$file")"
elif [[ $1 == buildx && $2 == imagetools && $3 == create && $4 == --tag ]]; then
	reference=$5
	source=$6
	printf '%s\n' "${source##*@}" >"$FAKE_STATE/images/$(key "$reference")"
else
	exit 2
fi
FAKE_DOCKER

cat >"$TMP/bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == release && $2 == view ]]; then
	[[ -s $FAKE_STATE/draft ]] || exit 1
	if [[ " $* " == *' isDraft '* ]]; then
		cat "$FAKE_STATE/draft"
	else
		find "$FAKE_STATE/assets" -maxdepth 1 -type f -printf '%f\n' | sort
	fi
elif [[ $1 == release && $2 == create ]]; then
	printf 'true\n' >"$FAKE_STATE/draft"
elif [[ $1 == release && $2 == upload ]]; then
	cp "$4" "$FAKE_STATE/assets/$(basename "$4")"
elif [[ $1 == release && $2 == download ]]; then
	pattern=
	directory=
	shift 3
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--pattern) pattern=$2; shift 2 ;;
			--dir) directory=$2; shift 2 ;;
			*) shift ;;
		esac
	done
	cp "$FAKE_STATE/assets/$pattern" "$directory/$pattern"
elif [[ $1 == release && $2 == edit ]]; then
	printf 'false\n' >"$FAKE_STATE/draft"
else
	exit 2
fi
FAKE_GH
chmod +x "$TMP/bin/docker" "$TMP/bin/gh"

export DOCKER_BIN="$TMP/bin/docker"
export GH_BIN="$TMP/bin/gh"
export FAKE_STATE="$TMP/state"
export TAG=v1.2.3
export VERSION=1.2.3
export IMAGE=ghcr.io/example/cairn
export REPOSITORY=example/cairn
export CORE_RELEASE_HIGHEST_TAG=$TAG
export COMMIT=0123456789abcdef0123456789abcdef01234567
export BUILD_TIME=2026-08-14T01:02:03Z
export FULL_INDEX_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export SLIM_INDEX_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export FULL_AMD64_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export FULL_ARM64_DIGEST=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export SLIM_AMD64_DIGEST=sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
export SLIM_ARM64_DIGEST=sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff

old_latest=sha256:1111111111111111111111111111111111111111111111111111111111111111
old_full=sha256:2222222222222222222222222222222222222222222222222222222222222222
old_slim=sha256:3333333333333333333333333333333333333333333333333333333333333333
printf '%s\n' "$old_latest" >"$TMP/state/images/ghcr.io_example_cairn_latest"
printf '%s\n' "$old_full" >"$TMP/state/images/ghcr.io_example_cairn_full"
printf '%s\n' "$old_slim" >"$TMP/state/images/ghcr.io_example_cairn_slim"

printf 'amd64 archive\n' >"$TMP/assets/cairn_1.2.3_linux_amd64.tar.gz"
printf 'arm64 archive\n' >"$TMP/assets/cairn_1.2.3_linux_arm64.tar.gz"
# 客户端交付物与 Core 同 tag 发布，asset 集合是严格比对的，夹具必须一并提供。
printf 'chrome zip\n' >"$TMP/assets/cairn-extension-chrome-1.2.3.zip"
printf 'firefox zip\n' >"$TMP/assets/cairn-extension-firefox-1.2.3.zip"
printf 'android apk\n' >"$TMP/assets/cairn-android-1.2.3-debug.apk"
jq -n \
	--arg tag "$TAG" --arg commit "$COMMIT" --arg build_time "$BUILD_TIME" --arg image "$IMAGE" \
	--arg full_index "$FULL_INDEX_DIGEST" --arg full_amd64 "$FULL_AMD64_DIGEST" --arg full_arm64 "$FULL_ARM64_DIGEST" \
	--arg slim_index "$SLIM_INDEX_DIGEST" --arg slim_amd64 "$SLIM_AMD64_DIGEST" --arg slim_arm64 "$SLIM_ARM64_DIGEST" \
	'{tag: $tag, commit: $commit, build_time: $build_time, image: $image,
	  full: {index: $full_index, children: {"linux/amd64": $full_amd64, "linux/arm64": $full_arm64}},
	  slim: {index: $slim_index, children: {"linux/amd64": $slim_amd64, "linux/arm64": $slim_arm64}}}' \
	>"$TMP/assets/IMAGE-DIGESTS.json"

mkdir -p "$TMP/security-evidence"
for variant in full slim; do
	for arch in amd64 arm64; do
		platform="linux/$arch"
		case "$variant/$arch" in
			full/amd64) index=$FULL_INDEX_DIGEST; child=$FULL_AMD64_DIGEST ;;
			full/arm64) index=$FULL_INDEX_DIGEST; child=$FULL_ARM64_DIGEST ;;
			slim/amd64) index=$SLIM_INDEX_DIGEST; child=$SLIM_AMD64_DIGEST ;;
			slim/arm64) index=$SLIM_INDEX_DIGEST; child=$SLIM_ARM64_DIGEST ;;
		esac
		directory="$TMP/security-evidence/core-image-evidence-${variant}-${arch}"
		mkdir -p "$directory"
		jq -n --arg commit "$COMMIT" --arg image "$IMAGE" --arg variant "$variant" \
			--arg platform "$platform" --arg index "$index" --arg child "$child" \
			'{commit: $commit, image: $image, variant: $variant, platform: $platform, index_digest: $index, child_digest: $child}' \
			>"$directory/coordinates.json"
		jq -n --arg arch "$arch" --arg child "$child" \
			'{manifests: [{platform: {os: "linux", architecture: $arch}, digest: $child}]}' \
			>"$directory/index-manifest.json"
		jq -n --arg arch "$arch" '{os: "linux", architecture: $arch}' >"$directory/child-image.json"
		jq -n '{Results: [{Class: "os-pkgs", Type: "alpine"}]}' >"$directory/trivy.json"
		jq -n '{components: [{purl: "pkg:apk/alpine/ca-certificates@1"}]}' >"$directory/sbom.cdx.json"
		if [[ $variant == full ]]; then
			printf '[]\n' >"$directory/yt-dlp-advisories.json"
			# 版本取自 Dockerfile 而不是写死：promote 会拿 coverage 里的版本与
			# ARG YTDLP_VERSION 对账（core-release-promote.sh 的 yt_dlp_version），
			# 写死会让每次升级 pin 都在这里假失败。
			jq -n --arg version "$(sed -n 's/^ARG YTDLP_VERSION=//p' "$ROOT/Dockerfile")" \
				'{version: $version, coverage: "test advisory boundary"}' >"$directory/yt-dlp-coverage.json"
		fi
	done
done
tar -C "$TMP" -czf "$TMP/assets/core-security-evidence-1.2.3.tar.gz" security-evidence

"$SCRIPT" prepare-channel-record "$TMP/assets/CHANNEL-ROLLBACK.json"
(cd "$TMP/assets" && sha256sum \
	cairn_*.tar.gz core-security-evidence-*.tar.gz \
	cairn-extension-*.zip cairn-android-*.apk \
	CHANNEL-ROLLBACK.json IMAGE-DIGESTS.json >SHA256SUMS)

"$SCRIPT" prepare-draft "$TMP/assets"
"$SCRIPT" prepare-draft "$TMP/assets"
printf 'different\n' >"$TMP/assets/SHA256SUMS"
if "$SCRIPT" prepare-draft "$TMP/assets" >/dev/null 2>&1; then
	fail 'draft preparation accepted a same-name asset with different bytes'
fi
(cd "$TMP/assets" && sha256sum \
	cairn_*.tar.gz core-security-evidence-*.tar.gz \
	cairn-extension-*.zip cairn-android-*.apk \
	CHANNEL-ROLLBACK.json IMAGE-DIGESTS.json >SHA256SUMS)
mv "$TMP/assets/cairn_1.2.3_linux_arm64.tar.gz" "$TMP/missing-arm64.tar.gz"
if "$SCRIPT" prepare-draft "$TMP/assets" >/dev/null 2>&1; then
	fail 'draft preparation accepted a missing architecture archive'
fi
mv "$TMP/missing-arm64.tar.gz" "$TMP/assets/cairn_1.2.3_linux_arm64.tar.gz"

CAIRN_RELEASE_FAIL_AT=version-slim "$SCRIPT" promote-versions >/dev/null 2>&1 &&
	fail 'version-slim injection did not fail'
[[ $(cat "$TMP/state/images/ghcr.io_example_cairn_1.2.3") == "$FULL_INDEX_DIGEST" ]] ||
	fail 'full version digest was not promoted before injected slim failure'
"$SCRIPT" promote-versions

printf '%s\n' sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
	>"$TMP/state/images/ghcr.io_example_cairn_1.2.3-slim"
if "$SCRIPT" promote-versions >/dev/null 2>&1; then
	fail 'promotion overwrote an existing version tag with a different digest'
fi
printf '%s\n' "$SLIM_INDEX_DIGEST" >"$TMP/state/images/ghcr.io_example_cairn_1.2.3-slim"

CAIRN_RELEASE_FAIL_AT=release-publish "$SCRIPT" publish-release >/dev/null 2>&1 &&
	fail 'Release publish injection did not fail'
[[ $(cat "$TMP/state/draft") == true ]] || fail 'publish failure exposed the draft'
"$SCRIPT" publish-release
[[ $(cat "$TMP/state/draft") == false ]] || fail 'Release was not published on retry'

printf '%s\n' "$FULL_INDEX_DIGEST" >"$TMP/state/images/ghcr.io_example_cairn_latest"
rm "$TMP/assets/CHANNEL-ROLLBACK.json"
"$SCRIPT" prepare-channel-record "$TMP/assets/CHANNEL-ROLLBACK.json"
jq -e --arg digest "$old_latest" '.previous.latest == $digest' "$TMP/assets/CHANNEL-ROLLBACK.json" >/dev/null ||
	fail 'channel retry did not reuse the rollback record sealed before publication'
printf '%s\n' "$old_latest" >"$TMP/state/images/ghcr.io_example_cairn_latest"

CAIRN_RELEASE_FAIL_AT=channel-full "$SCRIPT" promote-channels >/dev/null 2>&1 &&
	fail 'partial channel injection did not fail'
[[ $(cat "$TMP/state/images/ghcr.io_example_cairn_latest") == "$FULL_INDEX_DIGEST" ]] ||
	fail 'partial update did not reach latest before failure'
[[ $(cat "$TMP/state/images/ghcr.io_example_cairn_full") == "$old_full" ]] ||
	fail 'partial failure unexpectedly changed full'
"$SCRIPT" promote-channels
[[ $(cat "$TMP/state/images/ghcr.io_example_cairn_full") == "$FULL_INDEX_DIGEST" ]] ||
	fail 'retry did not recover full channel'
[[ $(cat "$TMP/state/images/ghcr.io_example_cairn_slim") == "$SLIM_INDEX_DIGEST" ]] ||
	fail 'retry did not recover slim channel'

for point in candidate-full candidate-slim candidate-verify channel-record draft-assets release-publish channel-slim; do
	if CAIRN_RELEASE_FAIL_AT=$point "$SCRIPT" fault "$point" >/dev/null 2>&1; then
		fail "fault point $point did not fail closed"
	fi
done

echo 'PASS: Core draft and digest promotion are idempotent and recoverable'
