#!/usr/bin/env bash
# Produce and check the signed canonical release manifest the cairn-updater
# helper trusts (issue #41 stage 0).
#
# The manifest is an independent trust root. SHA256SUMS stays in the Release for
# corruption checks, but it lives in the same Release as the archives it
# describes, so it cannot answer "did the project publish this". This script's
# output can: it is signed with a key whose public half is compiled into the
# helper.
#
# Without CAIRN_RELEASE_SIGNING_KEY, generate fails and writes nothing. There is
# deliberately no "unsigned but marked" manifest: that would hand the helper a
# judgement call at exactly the moment it is running as root.
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
DIST_DIR=${DIST_DIR:-$ROOT/dist}
OUT_DIR=${OUT_DIR:-$DIST_DIR}

fail() {
	echo "core release manifest: $*" >&2
	exit 1
}

require_identity() {
	[[ ${REPOSITORY:-} =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || fail 'REPOSITORY must be owner/name'
	[[ ${TAG:-} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'TAG must be a formal vX.Y.Z release tag'
	[[ ${TAG#v} != 0.0.0 ]] || fail 'TAG v0.0.0 is a development placeholder'
	[[ ${COMMIT:-} =~ ^[0-9a-f]{40}$ ]] || fail 'COMMIT must be a full lowercase Git revision'
	[[ ${BUILD_TIME:-} =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([+-][0-9]{2}:[0-9]{2}|Z)$ ]] ||
		fail 'BUILD_TIME must be an RFC3339 timestamp'
}

identity_args() {
	printf '%s\n' \
		--dist "$DIST_DIR" \
		--repo "$REPOSITORY" \
		--tag "$TAG" \
		--commit "$COMMIT" \
		--build-time "$BUILD_TIME"
	if [[ -n ${PREVIOUS_SCHEMA_TARGET:-} ]]; then
		printf '%s\n' --previous-schema-target "$PREVIOUS_SCHEMA_TARGET"
	fi
	if [[ -n ${PREVIOUS_RIVER_TARGET:-} ]]; then
		printf '%s\n' --previous-river-target "$PREVIOUS_RIVER_TARGET"
	fi
}

command=${1:-generate}
cd "$ROOT"

case "$command" in
	generate)
		require_identity
		# Checked before anything runs so a missing secret cannot leave a
		# half-written asset directory behind. The value itself is never echoed.
		[[ -n ${CAIRN_RELEASE_SIGNING_KEY:-} ]] ||
			fail 'CAIRN_RELEASE_SIGNING_KEY is not set: a release manifest is signed or it is not produced'
		mapfile -t args < <(identity_args)
		go run ./cmd/release-manifest generate "${args[@]}" --out "$OUT_DIR"
		;;
	preview)
		require_identity
		mapfile -t args < <(identity_args)
		go run ./cmd/release-manifest preview "${args[@]}"
		;;
	verify)
		require_identity
		go run ./cmd/release-manifest verify --dist "$DIST_DIR" --repo "$REPOSITORY" --tag "$TAG"
		;;
	*)
		fail "usage: core-release-manifest.sh [generate|preview|verify]"
		;;
esac
