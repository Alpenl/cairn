#!/usr/bin/env bash
# 校验每个 `uses: owner/repo@<sha>  # vX.Y.Z` 的注释与 SHA 真实所属的 tag 一致。
#
# Makefile 里的 ACTION_PIN_CHECK 是离线的，它只能回答「同一个 version 有没有映射
# 到多个 SHA」。那条检查抓不到本文件存在的理由：一个从写下时就贴错版本号的
# pin——它自始至终自洽，因此永远通过。实测抓到过一个：actions/setup-java 固定的
# SHA 是 v4.9.1，注释却写着 v4.7.1，于是 Dependabot 的升级 PR 标题说 4.9.1 →
# 5.7.0，而 diff 里的注释仍是 v4.7.1，审阅者看到的两个版本号没有一个是对的。
#
# 注释不是装饰：SHA 是给机器看的，版本号是人判断「这次升级跨了多大」的唯一依据。
# 贴错的注释比没有注释更糟，因为它看起来像证据。
#
# 需要网络（要向 GitHub 解析 tag），所以不进离线的 `make gate`。
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

if ! command -v gh >/dev/null 2>&1; then
	echo "verify-action-pins: 需要 gh CLI 来解析 tag" >&2
	exit 1
fi

failures=0
checked=0

# 带注解的 tag（annotated tag）指向一个 tag 对象而不是 commit，要多解一层才能拿到
# 真正的 commit SHA。GitHub 上多数 action 用的都是这种。
resolve_tag_commit() {
	local repo=$1 tag=$2 object_sha object_type
	object_sha=$(gh api "repos/$repo/git/ref/tags/$tag" --jq '.object.sha' 2>/dev/null) || return 1
	object_type=$(gh api "repos/$repo/git/ref/tags/$tag" --jq '.object.type' 2>/dev/null) || return 1
	if [ "$object_type" = "tag" ]; then
		gh api "repos/$repo/git/tags/$object_sha" --jq '.object.sha' 2>/dev/null || return 1
	else
		printf '%s\n' "$object_sha"
	fi
}

while read -r pin comment; do
	[ -n "$pin" ] || continue
	action=${pin%@*}
	sha=${pin##*@}
	repo=$(printf '%s\n' "$action" | cut -d/ -f1,2)
	checked=$((checked + 1))

	actual=$(resolve_tag_commit "$repo" "$comment")
	if [ -z "$actual" ]; then
		echo "verify-action-pins: $action 的注释 $comment 在 $repo 上不存在这个 tag" >&2
		failures=$((failures + 1))
		continue
	fi
	if [ "$actual" != "$sha" ]; then
		echo "verify-action-pins: $action 固定在 ${sha:0:12}，但注释写的 $comment 指向 ${actual:0:12}" >&2
		failures=$((failures + 1))
	fi
done < <(
	grep -rhoE 'uses:[[:space:]]+[A-Za-z0-9._/-]+@[a-f0-9]{40}[[:space:]]+#[[:space:]]*v[0-9][0-9.]*' .github/workflows/*.yml \
		| sed -E 's/uses:[[:space:]]+//; s/[[:space:]]+#[[:space:]]*/ /' \
		| sort -u
)

if [ "$checked" -eq 0 ]; then
	echo "verify-action-pins: 没有找到任何带版本注释的 pin——扫描八成坏了，不能当成通过" >&2
	exit 1
fi

if [ "$failures" -gt 0 ]; then
	echo "verify-action-pins: $checked 个 pin 中有 $failures 个的版本注释与 SHA 不符" >&2
	exit 1
fi

echo "verify-action-pins: $checked 个 pin 的版本注释与其 SHA 实际所属的 tag 一致"
