#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <schema-dump> <install-schema>" >&2
    exit 2
fi

input=$1
output=$2
mkdir -p "$(dirname "$output")"
tmp="${output}.tmp"
trap 'rm -f "$tmp"' EXIT

{
    echo "-- 自动生成；请勿手工编辑。"
    echo "-- 来源：internal/migrate/schema.sql。"
    echo "-- 仅包含 Cairn 自管对象；River 与 migration ledger 由各自 runner 创建。"
    echo "--"
    awk '
        NR <= 5 { next }
        /^-- Name: / {
            excluded = ($0 ~ /^-- Name: (river_|schema_migrations)/)
        }
        !excluded { print }
    ' "$input"
} > "$tmp"

mv "$tmp" "$output"
trap - EXIT
