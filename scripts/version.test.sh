#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
VERSION_SCRIPT="$ROOT/scripts/version.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_version() {
	local repository=$1
	local expected=$2
	local actual

	actual=$(cd "$repository" && sh "$VERSION_SCRIPT")
	[[ "$actual" == "$expected" ]] || fail "expected version [$expected], got [$actual]"
}

describe_from() {
	local repository=$1
	local version=$2
	local distance=$3
	local suffix=${4:-}
	local commit_sha

	commit_sha=$(git -C "$repository" rev-parse --short=12 HEAD)
	printf '%s-%s-g%s%s\n' "$version" "$distance" "$commit_sha" "$suffix"
}

commit() {
	local repository=$1
	local message=$2

	printf '%s\n' "$message" >>"$repository/history.txt"
	git -C "$repository" add history.txt
	git -C "$repository" commit -m "$message" >/dev/null
}

REPOSITORY="$TMP/repository"
git init -q "$REPOSITORY"
git -C "$REPOSITORY" config user.name test
git -C "$REPOSITORY" config user.email test@example.invalid

# Without a release tag, use a neutral development identity.
commit "$REPOSITORY" root
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.0.0 1)"
commit "$REPOSITORY" second
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.0.0 2)"

# A tag on HEAD is emitted exactly.
git -C "$REPOSITORY" tag v0.1.496
assert_version "$REPOSITORY" 0.1.496

# The nearest tag in the commit graph wins even when an older tag is
# numerically larger. This permits the first public release to reset the
# private version line without leaking the private version number.
commit "$REPOSITORY" public-release
git -C "$REPOSITORY" tag v0.1.0
assert_version "$REPOSITORY" 0.1.0

# A closer prerelease or malformed tag is not a stable Core version.
commit "$REPOSITORY" non-core-tags
git -C "$REPOSITORY" tag v9.9.9-rc.1
git -C "$REPOSITORY" tag v9.9.9.9
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.1.0 1)"

# Commits after the nearest release keep their distance and commit identity.
commit "$REPOSITORY" post-release-one
commit "$REPOSITORY" post-release-two
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.1.0 3)"

# A modified tracked file is never reported as a formal release.
printf 'dirty\n' >>"$REPOSITORY/history.txt"
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.1.0 3 -dirty)"

# Non-ignored untracked sources can affect a local build and are dirty too.
git -C "$REPOSITORY" restore history.txt
printf 'untracked\n' >"$REPOSITORY/untracked.txt"
assert_version "$REPOSITORY" "$(describe_from "$REPOSITORY" 0.1.0 3 -dirty)"

echo "PASS: scripts/version.sh separates formal releases from development builds"
