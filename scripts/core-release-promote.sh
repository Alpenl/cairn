#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
DOCKER_BIN=${DOCKER_BIN:-docker}
GH_BIN=${GH_BIN:-gh}
CORE_RELEASE_SERIES=$(sh "$ROOT/scripts/core-release-series.sh")

fail() {
	echo "core release promotion: $*" >&2
	exit 1
}

maybe_fail() {
	local point=$1
	if [[ ${CAIRN_RELEASE_FAIL_AT:-} == "$point" ]]; then
		fail "injected failure at $point"
	fi
}

require_common() {
	local release_major_minor
	[[ ${TAG:-} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'TAG must be vX.Y.Z'
	[[ ${VERSION:-} == "${TAG#v}" ]] || fail 'VERSION must match TAG'
	[[ $CORE_RELEASE_SERIES =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
		fail 'scripts/core-release-series.sh must print a major.minor pair'
	release_major_minor=${VERSION%.*}
	[[ $release_major_minor == "$CORE_RELEASE_SERIES" ]] ||
		fail "Core releases are pinned to v${CORE_RELEASE_SERIES}.x until the user explicitly approves a new minor line"
	[[ ${IMAGE:-} == */* ]] || fail 'IMAGE must be a registry repository'
}

require_digest() {
	local name=$1
	local value=$2
	[[ $value =~ ^sha256:[0-9a-f]{64}$ ]] || fail "$name is not a sha256 digest"
}

digest_of_ref() {
	local reference=$1
	local manifest
	if ! manifest=$("$DOCKER_BIN" buildx imagetools inspect "$reference" --format '{{json .Manifest}}' 2>/dev/null); then
		return 1
	fi
	jq -er '.digest | select(test("^sha256:[0-9a-f]{64}$"))' <<<"$manifest"
}

ensure_ref() {
	local reference=$1
	local digest=$2
	local existing

	require_digest "digest for $reference" "$digest"
	if existing=$(digest_of_ref "$reference"); then
		[[ $existing == "$digest" ]] ||
			fail "$reference already points to $existing, refusing to replace it with $digest"
		echo "$reference already points to verified digest $digest"
		return
	fi

	"$DOCKER_BIN" buildx imagetools create --tag "$reference" "$IMAGE@$digest"
	existing=$(digest_of_ref "$reference") || fail "unable to resolve promoted reference $reference"
	[[ $existing == "$digest" ]] || fail "$reference resolved to $existing after promotion, expected $digest"
}

move_channel_ref() {
	local reference=$1
	local digest=$2
	local existing

	require_digest "digest for $reference" "$digest"
	if existing=$(digest_of_ref "$reference") && [[ $existing == "$digest" ]]; then
		echo "$reference already points to verified digest $digest"
		return
	fi
	"$DOCKER_BIN" buildx imagetools create --tag "$reference" "$IMAGE@$digest"
	existing=$(digest_of_ref "$reference") || fail "unable to resolve promoted channel $reference"
	[[ $existing == "$digest" ]] || fail "$reference resolved to $existing after channel promotion, expected $digest"
}

release_is_draft() {
	"$GH_BIN" release view "$TAG" --repo "$REPOSITORY" --json isDraft --jq .isDraft
}

release_assets() {
	"$GH_BIN" release view "$TAG" --repo "$REPOSITORY" --json assets --jq '.assets[].name'
}

verify_remote_asset() {
	local asset=$1
	local temporary=$2
	local name
	name=$(basename "$asset")
	rm -rf "$temporary"
	mkdir -p "$temporary"
	"$GH_BIN" release download "$TAG" --repo "$REPOSITORY" --pattern "$name" --dir "$temporary"
	cmp -s "$asset" "$temporary/$name" || fail "draft asset $name exists with different content"
}

release_asset_names() {
	# Core 交付物：服务端二进制、镜像证据，以及独立 Reader 站的发布包——后者是
	# 前端的另一种部署形态（静态站），与内嵌 /reader/ 同源同版本，必须随 Core
	# 一起发布，否则后端升级会让站点因 representation_contract 不符而连不上。
	#
	# 浏览器扩展与 Android 各自是独立发布单元，有自己的 tag 前缀与 workflow——
	# 把它们塞进 Core 会让「改一行 Android 代码」触发整套镜像构建与频道 promote。
	# 这个集合是严格的：多一个或少一个文件都会让 prepare-draft 失败。
	#
	# cairn-release-manifest.json 与它的 detached 签名是受控页面更新（#41）的
	# 信任根：helper 只认这两个文件，不认 SHA256SUMS——后者与 tarball 同属一个
	# Release，能被同时替换。它们由 prepare-draft 里的签名步骤产出，缺失即整条
	# 发布失败，不存在「未签名但标记过」的中间态。
	printf '%s\n' \
		"cairn_${VERSION}_linux_amd64.tar.gz" \
		"cairn_${VERSION}_linux_arm64.tar.gz" \
		"core-security-evidence-${VERSION}.tar.gz" \
		"cairn-reader-${VERSION}.tar.gz" \
		cairn-release-manifest.json \
		cairn-release-manifest.json.sig \
		CHANNEL-ROLLBACK.json \
		IMAGE-DIGESTS.json \
		SHA256SUMS | sort
}

checksum_asset_names() {
	release_asset_names | grep -Fvx SHA256SUMS
}

validate_channel_record() {
	local record=$1
	jq -e --arg image "$IMAGE" '
		.image == $image and
		(.previous | type == "object" and has("latest") and has("slim")) and
		([.previous.latest, .previous.slim] |
		 all(. == null or (type == "string" and test("^sha256:[0-9a-f]{64}$"))))
	' "$record" >/dev/null || fail "invalid channel rollback record: $record"
}

validate_digest_manifest() {
	local manifest=$1
	for name in SLIM_INDEX_DIGEST SLIM_AMD64_DIGEST SLIM_ARM64_DIGEST; do
		require_digest "$name" "${!name:-}"
	done
	[[ ${COMMIT:-} =~ ^[0-9a-f]{40}$ ]] || fail 'COMMIT must be a full lowercase Git revision'
	[[ ${BUILD_TIME:-} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([+-][0-9]{2}:[0-9]{2}|Z)$ ]] ||
		fail 'BUILD_TIME must be an RFC3339 timestamp'

	jq -e \
		--arg tag "$TAG" --arg commit "$COMMIT" --arg build_time "$BUILD_TIME" --arg image "$IMAGE" \
		--arg slim_index "$SLIM_INDEX_DIGEST" --arg slim_amd64 "$SLIM_AMD64_DIGEST" --arg slim_arm64 "$SLIM_ARM64_DIGEST" '
		.tag == $tag and .commit == $commit and .build_time == $build_time and .image == $image and
		.slim.index == $slim_index and .slim.children["linux/amd64"] == $slim_amd64 and
		.slim.children["linux/arm64"] == $slim_arm64 and (has("full") | not)
	' "$manifest" >/dev/null || fail 'IMAGE-DIGESTS.json does not match the verified candidate coordinates'
}

validate_security_evidence() {
	local archive=$1
	local temporary listing arch platform index child directory
	temporary=$(mktemp -d)
	listing="$temporary/members.txt"
	tar -tzf "$archive" >"$listing"
	if grep -Eq '(^/|(^|/)\.\.(/|$))' "$listing" || grep -Ev '^security-evidence/' "$listing" | grep -q .; then
		rm -rf "$temporary"
		fail 'security evidence archive contains an unsafe member path'
	fi
	tar -xzf "$archive" -C "$temporary"
	for arch in amd64 arm64; do
		platform="linux/$arch"
		case "$arch" in
			amd64) index=$SLIM_INDEX_DIGEST; child=$SLIM_AMD64_DIGEST ;;
			arm64) index=$SLIM_INDEX_DIGEST; child=$SLIM_ARM64_DIGEST ;;
		esac
		directory="$temporary/security-evidence/core-image-evidence-slim-${arch}"
		for evidence in coordinates.json index-manifest.json child-image.json trivy.json sbom.cdx.json; do
			test -s "$directory/$evidence" || {
				rm -rf "$temporary"
				fail "security evidence is missing slim/$arch $evidence"
			}
		done
		jq -e --arg commit "$COMMIT" --arg image "$IMAGE" \
			--arg platform "$platform" --arg index "$index" --arg child "$child" '
			.commit == $commit and .image == $image and .variant == "slim" and .platform == $platform and
			.index_digest == $index and .child_digest == $child
		' "$directory/coordinates.json" >/dev/null || {
			rm -rf "$temporary"
			fail "security coordinates do not match slim/$arch"
		}
		jq -e --arg arch "$arch" --arg child "$child" '
			[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest] == [$child]
		' "$directory/index-manifest.json" >/dev/null || {
			rm -rf "$temporary"
			fail "security index evidence does not bind slim/$arch"
		}
		jq -e --arg arch "$arch" '.os == "linux" and .architecture == $arch' \
			"$directory/child-image.json" >/dev/null || {
			rm -rf "$temporary"
			fail "security child evidence has the wrong platform for slim/$arch"
		}
		jq -e '[.Results[]? | select(.Class == "os-pkgs" and .Type == "alpine")] | length > 0' \
			"$directory/trivy.json" >/dev/null || {
			rm -rf "$temporary"
			fail "Trivy evidence omits Alpine runtime packages for slim/$arch"
		}
		jq -e 'any(.components[]?; ((.purl // "") | startswith("pkg:apk/alpine/")))' \
			"$directory/sbom.cdx.json" >/dev/null || {
			rm -rf "$temporary"
			fail "SBOM evidence omits Alpine packages for slim/$arch"
		}
	done
	rm -rf "$temporary"
}

validate_release_assets() {
	local asset_dir=$1
	local expected actual checksum_names line
	local -a listed=()

	expected=$(release_asset_names)
	actual=$(find "$asset_dir" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)
	[[ $actual == "$expected" ]] || fail "release asset set is incomplete or contains unexpected files"
	while IFS= read -r line; do
		[[ $line =~ ^[0-9a-f]{64}\ \ ([A-Za-z0-9._-]+)$ ]] || fail 'SHA256SUMS contains an invalid entry'
		listed+=("${BASH_REMATCH[1]}")
	done <"$asset_dir/SHA256SUMS"
	checksum_names=$(printf '%s\n' "${listed[@]}" | sort)
	[[ $checksum_names == "$(checksum_asset_names)" ]] || fail 'SHA256SUMS does not cover the exact release asset set'
	(cd "$asset_dir" && sha256sum --strict --check SHA256SUMS >/dev/null) || fail 'release asset checksum verification failed'
	validate_channel_record "$asset_dir/CHANNEL-ROLLBACK.json"
	validate_digest_manifest "$asset_dir/IMAGE-DIGESTS.json"
	validate_security_evidence "$asset_dir/core-security-evidence-${VERSION}.tar.gz"
}

prepare_draft() {
	local asset_dir=$1
	local draft assets temporary asset name expected remote

	[[ -d $asset_dir ]] || fail "asset directory does not exist: $asset_dir"
	validate_release_assets "$asset_dir"
	maybe_fail draft-prepare
	if draft=$(release_is_draft 2>/dev/null); then
		[[ $draft == true || $draft == false ]] || fail 'GitHub returned an invalid draft state'
	else
		# 中文说明正文 + GitHub 自动生成的变更列表。--generate-notes 单用会
		# 得到一份纯英文的 PR 列表，对本项目的读者没有意义；两者同时给出时
		# GitHub 会把自动生成的内容接在 notes 之后。
		local notes_file
		notes_file=$(mktemp)
		sed "s/__VERSION__/${VERSION}/g" "$ROOT/scripts/release/notes-zh.md.tmpl" >"$notes_file"
		"$GH_BIN" release create "$TAG" --repo "$REPOSITORY" --verify-tag --draft \
			--notes-file "$notes_file" --generate-notes --title "Cairn $TAG"
		rm -f "$notes_file"
		draft=true
	fi

	assets=$(release_assets)
	temporary=$(mktemp -d)
	trap 'rm -rf "$temporary"' RETURN
	while IFS= read -r asset; do
		[[ -n $asset ]] || continue
		name=$(basename "$asset")
		if grep -Fxq "$name" <<<"$assets"; then
			verify_remote_asset "$asset" "$temporary"
			continue
		fi
		[[ $draft == true ]] || fail "published Release is missing required asset $name"
		maybe_fail draft-assets
		"$GH_BIN" release upload "$TAG" "$asset" --repo "$REPOSITORY"
		verify_remote_asset "$asset" "$temporary"
	done < <(find "$asset_dir" -maxdepth 1 -type f -print | sort)
	expected=$(release_asset_names)
	remote=$(release_assets | sort)
	[[ $remote == "$expected" ]] || fail 'draft Release asset set differs from the sealed local set'
}

promote_versions() {
	require_digest SLIM_INDEX_DIGEST "${SLIM_INDEX_DIGEST:-}"
	maybe_fail version
	ensure_ref "$IMAGE:$VERSION" "$SLIM_INDEX_DIGEST"
	maybe_fail version-slim
	ensure_ref "$IMAGE:$VERSION-slim" "$SLIM_INDEX_DIGEST"
}

publish_release() {
	local draft expected remote
	maybe_fail release-publish
	draft=$(release_is_draft) || fail "Release $TAG does not exist"
	expected=$(release_asset_names)
	remote=$(release_assets | sort)
	[[ $remote == "$expected" ]] || fail 'refusing to publish an incomplete draft asset set'
	if [[ $draft == false ]]; then
		echo "Release $TAG is already published"
		return
	fi
	[[ $draft == true ]] || fail 'GitHub returned an invalid draft state'
	"$GH_BIN" release edit "$TAG" --repo "$REPOSITORY" --verify-tag --draft=false --latest=false
}

capture_channels() {
	local output=$1
	local latest slim
	if [[ -s $output ]]; then
		validate_channel_record "$output"
		echo "channel rollback state already exists at $output"
		return
	fi
	latest=$(digest_of_ref "$IMAGE:latest" || true)
	slim=$(digest_of_ref "$IMAGE:slim" || true)
	jq -n \
		--arg image "$IMAGE" \
		--arg latest "$latest" \
		--arg slim "$slim" \
		'{image: $image, previous: {latest: ($latest | if length > 0 then . else null end), slim: ($slim | if length > 0 then . else null end)}}' \
		>"$output"
	validate_channel_record "$output"
}

prepare_channel_record() {
	local output=$1
	local name assets temporary
	name=$(basename "$output")
	maybe_fail channel-record
	assets=$(release_assets 2>/dev/null || true)
	if grep -Fxq "$name" <<<"$assets"; then
		temporary=$(mktemp -d)
		"$GH_BIN" release download "$TAG" --repo "$REPOSITORY" --pattern "$name" --dir "$temporary"
		cp "$temporary/$name" "$output"
		rm -rf "$temporary"
		validate_channel_record "$output"
		echo "reused sealed channel rollback state $name"
		return
	fi
	capture_channels "$output"
}

highest_stable_tag() {
	if [[ -n ${CORE_RELEASE_HIGHEST_TAG:-} ]]; then
		printf '%s\n' "$CORE_RELEASE_HIGHEST_TAG"
		return
	fi
	git tag --list "v${CORE_RELEASE_SERIES}.*" --sort=-version:refname |
		grep -E "^v${CORE_RELEASE_SERIES//./\\.}\.[0-9]+$" | head -n 1
}

promote_channels() {
	local highest
	require_digest SLIM_INDEX_DIGEST "${SLIM_INDEX_DIGEST:-}"
	highest=$(highest_stable_tag)
	[[ $highest == "$TAG" ]] || fail "refusing to move stable channels for $TAG because highest Core tag is $highest"
	maybe_fail channel-latest
	move_channel_ref "$IMAGE:latest" "$SLIM_INDEX_DIGEST"
	maybe_fail channel-slim
	move_channel_ref "$IMAGE:slim" "$SLIM_INDEX_DIGEST"
}

command=${1:-}
case "$command" in
	fault)
		[[ $# == 2 ]] || fail 'usage: core-release-promote.sh fault <point>'
		maybe_fail "$2"
		;;
	prepare-draft)
		[[ $# == 2 ]] || fail 'usage: core-release-promote.sh prepare-draft <asset-dir>'
		require_common
		[[ -n ${REPOSITORY:-} ]] || fail 'REPOSITORY is required'
		prepare_draft "$2"
		;;
	prepare-channel-record)
		[[ $# == 2 ]] || fail 'usage: core-release-promote.sh prepare-channel-record <output>'
		require_common
		[[ -n ${REPOSITORY:-} ]] || fail 'REPOSITORY is required'
		prepare_channel_record "$2"
		;;
	promote-versions)
		require_common
		promote_versions
		;;
	publish-release)
		require_common
		[[ -n ${REPOSITORY:-} ]] || fail 'REPOSITORY is required'
		publish_release
		;;
	capture-channels)
		[[ $# == 2 ]] || fail 'usage: core-release-promote.sh capture-channels <output>'
		require_common
		capture_channels "$2"
		;;
	promote-channels)
		require_common
		promote_channels
		;;
	*)
		fail 'commands: fault, prepare-draft, prepare-channel-record, promote-versions, publish-release, capture-channels, promote-channels'
		;;
esac
