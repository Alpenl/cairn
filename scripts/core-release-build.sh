#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
OUT_DIR=${OUT_DIR:-$ROOT/dist}
TARGET_ARCHES=${TARGET_ARCHES:-amd64,arm64}

fail() {
	echo "core release build: $*" >&2
	exit 1
}

require_identity() {
	[[ ${VERSION:-} =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'VERSION must be a stable X.Y.Z release'
	[[ ${VERSION} != 0.0.0 ]] || fail 'VERSION 0.0.0 is a development placeholder'
	[[ ${COMMIT:-} =~ ^[0-9a-f]{40}$ ]] || fail 'COMMIT must be a full lowercase Git revision'
	[[ ${BUILD_TIME:-} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([+-][0-9]{2}:[0-9]{2}|Z)$ ]] ||
		fail 'BUILD_TIME must be an RFC3339 timestamp'
}

require_source() {
	local actual_commit status
	actual_commit=$(git -C "$ROOT" rev-parse HEAD)
	[[ $actual_commit == "$COMMIT" ]] || fail "COMMIT $COMMIT does not match source HEAD $actual_commit"
	status=$(git -C "$ROOT" status --porcelain --untracked-files=normal)
	[[ -z $status ]] || fail 'formal source tree must be clean, including untracked files'
}

verify_build_info() {
	local binary=$1
	local arch=$2
	local command=$3
	local metadata

	metadata=$(go version -m "$binary")
	grep -Fq $'path\twebtag/cmd/'"$command" <<<"$metadata" || fail "$binary is not cmd/$command"
	grep -Fq "GOOS=linux" <<<"$metadata" || fail "$binary does not target linux"
	grep -Fq "GOARCH=$arch" <<<"$metadata" || fail "$binary does not target $arch"
	for value in "$VERSION" "$COMMIT" "$BUILD_TIME"; do
		grep -aFq -- "$value" "$binary" || fail "$binary does not embed release identity value $value"
	done
}

verify_native_identity() {
	local binary=$1
	local stderr_file=$2
	local expected actual

	expected=$(printf 'cairn %s\ncommit: %s\nbuilt: %s' "$VERSION" "$COMMIT" "$BUILD_TIME")
	actual=$("$binary" --version 2>"$stderr_file")
	[[ ! -s $stderr_file ]] || fail "$binary --version wrote to stderr"
	[[ $actual == "$expected" ]] || fail "$binary --version reported unexpected identity"
}

build_arch() {
	local arch=$1
	local package_name="cairn_${VERSION}_linux_${arch}"
	local package_dir="$work/$package_name"
	local ldflags

	case "$arch" in amd64 | arm64) ;; *) fail "unsupported release architecture: $arch" ;; esac
	mkdir -p "$package_dir/legal"
	cp -R "$ROOT/legal/core/common/." "$package_dir/legal/"
	ldflags="-s -w -X webtag/internal/buildinfo.Version=$VERSION -X webtag/internal/buildinfo.Commit=$COMMIT -X webtag/internal/buildinfo.BuildTime=$BUILD_TIME"

	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=vendor -buildvcs=true -trimpath -tags=nomsgpack,sonic \
		-ldflags "$ldflags" -o "$package_dir/webtag" ./cmd/webtag
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=vendor -buildvcs=true -trimpath \
		-ldflags "$ldflags" -o "$package_dir/migrate" ./cmd/migrate

	verify_build_info "$package_dir/webtag" "$arch" webtag
	verify_build_info "$package_dir/migrate" "$arch" migrate
	if [[ $(go env GOOS) == linux && $(go env GOARCH) == "$arch" ]]; then
		verify_native_identity "$package_dir/webtag" "$work/webtag.stderr"
		verify_native_identity "$package_dir/migrate" "$work/migrate.stderr"
	fi

	webtag_sha=$(sha256sum "$package_dir/webtag")
	webtag_sha=${webtag_sha%% *}
	migrate_sha=$(sha256sum "$package_dir/migrate")
	migrate_sha=${migrate_sha%% *}
	jq -n \
		--arg version "$VERSION" \
		--arg commit "$COMMIT" \
		--arg build_time "$BUILD_TIME" \
		--arg arch "$arch" \
		--arg webtag_sha "$webtag_sha" \
		--arg migrate_sha "$migrate_sha" \
		'{version: $version, commit: $commit, build_time: $build_time, source_state: "clean", os: "linux", arch: $arch, binaries: {webtag: {sha256: $webtag_sha}, migrate: {sha256: $migrate_sha}}}' \
		>"$package_dir/BUILD-PROVENANCE.json"

	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="$BUILD_TIME" \
		-C "$work" -cf - "$package_name" | gzip -n >"$OUT_DIR/${package_name}.tar.gz"
}

require_identity
cd "$ROOT"
require_source
pnpm install --frozen-lockfile
node scripts/core-legal.mjs check
make reader-build

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

IFS=',' read -r -a target_arches <<<"$TARGET_ARCHES"
for arch in "${target_arches[@]}"; do
	build_arch "$arch"
done

(cd "$OUT_DIR" && sha256sum cairn_*.tar.gz >SHA256SUMS)
echo "Core release archives written to $OUT_DIR"
