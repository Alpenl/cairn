#!/usr/bin/env sh
set -eu

# A formal version exists only on an exact Core release tag. Development
# builds retain the nearest release, commit distance, SHA, and dirty state so
# ordinary commits never consume a future patch version.
stable_tags="$(git tag --list | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/')"
description=""
if [ -n "$stable_tags" ]; then
	set -- git describe --tags --long --abbrev=12 --dirty --candidates=10000
	while IFS= read -r tag; do
		set -- "$@" --match "$tag"
	done <<EOF
$stable_tags
EOF
	description="$("$@" 2>/dev/null || true)"
fi
if [ -n "$description" ] && [ "${description%-dirty}" = "$description" ] &&
	[ -n "$(git status --porcelain --untracked-files=normal)" ]; then
	description="${description}-dirty"
fi

if [ -n "$description" ]; then
	case "$description" in
		v*-0-g????????????)
			printf '%s\n' "${description#v}" | sed 's/-0-g[0-9a-f]*$//'
			;;
		*)
			printf '%s\n' "${description#v}"
			;;
	esac
	exit 0
fi

commits_since_root="$(git rev-list --count HEAD)"
commit="$(git rev-parse --short=12 HEAD)"
dirty=""
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
	dirty="-dirty"
fi
printf '0.0.0-%s-g%s%s\n' "$commits_since_root" "$commit" "$dirty"
