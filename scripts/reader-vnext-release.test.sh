#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT="$ROOT/scripts/reader-vnext-release.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

COMMIT=0123456789abcdef0123456789abcdef01234567

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

make_fixture() {
	python3 - "$TMP/release" "$COMMIT" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
commit = sys.argv[2]
release_id = "reader-contract-test"
generated_at = "2026-08-24T00:00:00Z"

builds = {
    "root": {
        "base_path": "/",
        "index": '<script type="module" src="/assets/app.js"></script>\n',
        "asset": 'console.log("root reader")\n',
        "sw": 'const deny = ["/api"]\n',
    },
    "embedded": {
        "base_path": "/reader/",
        "index": '<script type="module" src="/reader/assets/app.js"></script>\n',
        "asset": 'console.log("embedded reader")\n',
        "sw": 'const deny = ["/api"]\n',
    },
}

for name, data in builds.items():
    build_dir = root / name
    (build_dir / "assets").mkdir(parents=True, exist_ok=True)
    (build_dir / "index.html").write_text(data["index"], encoding="utf-8")
    (build_dir / "assets" / "app.js").write_text(data["asset"], encoding="utf-8")
    (build_dir / "sw.js").write_text(data["sw"], encoding="utf-8")
    marker = {
        "schema_version": 1,
        "artifact_kind": "webtag-reader-release",
        "release_id": release_id,
        "tree_kind": name,
        "base_path": data["base_path"],
        "commit_full_sha": commit,
        "generated_at": generated_at,
    }
    (build_dir / "webtag-reader-release.json").write_text(
        json.dumps(marker, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def collect(name, base_path):
    build_dir = root / name
    files = []
    for path in sorted(item for item in build_dir.rglob("*") if item.is_file()):
        body = path.read_bytes()
        files.append({
            "path": path.relative_to(build_dir).as_posix(),
            "bytes": len(body),
            "sha256": hashlib.sha256(body).hexdigest(),
        })
    return {
        "name": name,
        "base_path": base_path,
        "directory": name,
        "release_marker": f"{name}/webtag-reader-release.json",
        "file_count": len(files),
        "total_bytes": sum(item["bytes"] for item in files),
        "files": files,
    }


manifest = {
    "schema_version": 1,
    "artifact_kind": "reader-vnext-release-manifest",
    "release_id": release_id,
    "commit_full_sha": commit,
    "generated_at": generated_at,
    "builds": [
        collect("root", "/"),
        collect("embedded", "/reader/"),
    ],
}
(root / "reader-vnext-release-manifest.json").write_text(
    json.dumps(manifest, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
}

expect_rejected() {
	local name=$1
	local message=$2
	shift 2
	local log="$TMP/$name.log"
	if bash "$SCRIPT" "$@" >"$log" 2>&1; then
		fail "$name was accepted"
	fi
	grep -Fq "$message" "$log" || fail "$name did not report $message"
}

make_fixture

manifest="$TMP/release/reader-vnext-release-manifest.json"
bash "$SCRIPT" verify --manifest "$manifest" --expected-commit "$COMMIT" >"$TMP/ok.log"
grep -Fq 'verified reader bundle manifest' "$TMP/ok.log" || fail 'valid manifest did not verify'

expect_rejected wrong-commit 'manifest commit mismatch' \
	verify --manifest "$manifest" --expected-commit 0000000000000000000000000000000000000000

python3 - "$TMP/release/root/assets/app.js" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
path.write_text(path.read_text(encoding="utf-8").replace("root", "ROOT", 1), encoding="utf-8")
PY
expect_rejected tampered-file 'digest mismatch' \
	verify --manifest "$manifest" --expected-commit "$COMMIT"

echo 'PASS: Reader bundle manifest verifies dual artifacts and rejects tampering'
