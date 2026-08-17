#!/usr/bin/env bash
# Offline contract test for the signed release manifest generator.
#
# It builds a synthetic dist/ directory, then pins three things:
#
#   1. The canonical encoding, checked against an independent re-serialisation
#      in Python. A canonical form that only one implementation agrees with is
#      not a canonical form — the helper has to be able to reproduce the signed
#      bytes without linking this repository's encoder.
#   2. The default-deny compatibility classification: online updates are denied
#      until every migration step is classified, and binary-only rollback is
#      denied unless the previous release's targets were supplied.
#   3. Fail-closed signing. Missing, empty, malformed, and untrusted keys all
#      abort, and none of them leaves a manifest or signature behind.
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SCRIPT="$ROOT/scripts/core-release-manifest.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export REPOSITORY=Alpenl/cairn
export TAG=v1.2.3
export COMMIT=0123456789abcdef0123456789abcdef01234567
export BUILD_TIME=2026-08-14T01:02:03Z
VERSION=${TAG#v}

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

build_dist() {
	local dist=$1
	local extra_executable=${2:-}
	local include_reader=${3:-yes}
	python3 - "$dist" "$VERSION" "$COMMIT" "$BUILD_TIME" "$extra_executable" "$include_reader" <<'PY'
import hashlib, io, json, os, sys, tarfile

dist, version, commit, build_time, extra_executable, include_reader = sys.argv[1:]
os.makedirs(dist, exist_ok=True)


def add(tar, name, data, mode):
    info = tarfile.TarInfo(name)
    info.size = len(data)
    info.mode = mode
    info.mtime = 0
    tar.addfile(info, io.BytesIO(data))


def add_root_entry(tar):
    """Write the archive's own root the way `tar czf - -C dir .` does.

    The real Reader tarball is built that way, so a fixture that omits this
    entry cannot see a checker that refuses it — which is exactly how an
    archive every release produces reached the release pipeline unrejected
    by the local gate.
    """
    info = tarfile.TarInfo("./")
    info.type = tarfile.DIRTYPE
    info.mode = 0o755
    info.mtime = 0
    tar.addfile(info)


for arch in ("amd64", "arm64"):
    root = f"cairn_{version}_linux_{arch}"
    webtag = f"ELF-webtag-{arch}".encode()
    migrate = f"ELF-migrate-{arch}".encode()
    provenance = json.dumps({
        "version": version,
        "commit": commit,
        "build_time": build_time,
        "source_state": "clean",
        "os": "linux",
        "arch": arch,
        "binaries": {
            "webtag": {"sha256": hashlib.sha256(webtag).hexdigest()},
            "migrate": {"sha256": hashlib.sha256(migrate).hexdigest()},
        },
    }).encode()
    with tarfile.open(os.path.join(dist, f"{root}.tar.gz"), "w:gz") as tar:
        add(tar, f"{root}/BUILD-PROVENANCE.json", provenance, 0o644)
        add(tar, f"{root}/legal/CAIRN_LICENSE.txt", b"license", 0o644)
        add(tar, f"{root}/migrate", migrate, 0o755)
        add(tar, f"{root}/webtag", webtag, 0o755)
        if extra_executable:
            add(tar, f"{root}/{extra_executable}", b"ELF-extra", 0o755)

if include_reader == "yes":
    builds = {
        "embedded": {
            "embedded/index.html": b"<script src=/reader/assets/app.js></script>",
            "embedded/assets/app.js": b"embedded-bundle",
            "embedded/sw.js": b"/api",
        },
        "root": {
            "root/index.html": b"<script src=/assets/app.js></script>",
            "root/assets/app.js": b"root-bundle",
            "root/sw.js": b"/api",
        },
    }
    base_paths = {"embedded": "/reader/", "root": "/"}
    manifest = {
        "schema_version": 1,
        "artifact_kind": "reader-vnext-release-manifest",
        "release_id": f"reader-{version}",
        "commit_full_sha": commit,
        "generated_at": build_time,
        "builds": [
            {
                "name": name,
                "base_path": base_paths[name],
                "directory": name,
                "file_count": len(files),
                "total_bytes": sum(len(body) for body in files.values()),
            }
            for name, files in builds.items()
        ],
    }
    with tarfile.open(os.path.join(dist, f"cairn-reader-{version}.tar.gz"), "w:gz") as tar:
        add_root_entry(tar)
        add(tar, "reader-vnext-release-manifest.json",
            (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode(), 0o644)
        for files in builds.values():
            for name, body in files.items():
                add(tar, name, body, 0o644)
PY
}

# An independent implementation of the canonical rules: sorted keys, two space
# indent, "\n" only, one trailing newline, integers, no ASCII escaping.
assert_canonical() {
	local file=$1
	python3 - "$file" <<'PY'
import json, sys
from pathlib import Path

raw = Path(sys.argv[1]).read_bytes()
text = raw.decode("utf-8")
document = json.loads(text, parse_float=lambda value: (_ for _ in ()).throw(ValueError("float in manifest")))
expected = json.dumps(document, indent=2, sort_keys=True, ensure_ascii=False) + "\n"
if text != expected:
    raise SystemExit("manifest is not in the canonical encoding")
if b"\r" in raw:
    raise SystemExit("manifest contains a carriage return")
if not raw.endswith(b"}\n") or raw.endswith(b"\n\n"):
    raise SystemExit("manifest does not end with exactly one newline")
PY
}

DIST="$TMP/dist"
build_dist "$DIST"

# 1. preview needs no secret and must produce canonical bytes.
DIST_DIR="$DIST" bash "$SCRIPT" preview >"$TMP/manifest.json" 2>"$TMP/preview.log" ||
	{
		cat "$TMP/preview.log" >&2
		fail 'preview rejected a well formed dist directory'
	}
assert_canonical "$TMP/manifest.json" || fail 'preview output is not canonical'

python3 - "$TMP/manifest.json" "$DIST" "$COMMIT" "$BUILD_TIME" "$TAG" <<'PY'
import hashlib, json, sys
from pathlib import Path

manifest = json.loads(Path(sys.argv[1]).read_text())
dist, commit, build_time, tag = Path(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]


def fail(message):
    raise SystemExit(f"manifest contract: {message}")


if manifest["schema_version"] != 1 or manifest["artifact_kind"] != "cairn-release-manifest":
    fail("schema identity")
if manifest["tag"] != tag or manifest["version"] != tag[1:]:
    fail("tag / version")
if manifest["commit"] != commit or manifest["build_time"] != build_time:
    fail("commit / build time")
if manifest["minimum_helper_protocol"] != 1:
    fail("helper protocol floor")
if manifest["platforms"] != ["linux/amd64", "linux/arm64"]:
    fail(f"platform matrix {manifest['platforms']}")
if not manifest["schema_target"] or manifest["river_ledger_target"] < 1:
    fail("migration targets")
if manifest["online_update_compatible"] is not False:
    fail("online update classification must default to deny")
if manifest["rollback_compatible"] is not False:
    fail("rollback must default to deny without a known predecessor")
for field in ("online_update_reason", "rollback_reason"):
    if not manifest[field].strip():
        fail(f"{field} is empty")

expected_output = f"cairn {tag[1:]}\ncommit: {commit}\nbuilt: {build_time}"
for entry in manifest["core"]:
    archive = dist / entry["archive"]
    if entry["sha256"] != hashlib.sha256(archive.read_bytes()).hexdigest():
        fail(f"{entry['archive']} digest")
    if entry["size_bytes"] != archive.stat().st_size:
        fail(f"{entry['archive']} size")
    if sorted(entry["executables"]) != ["migrate", "webtag"]:
        fail(f"{entry['archive']} executables {sorted(entry['executables'])}")
    for name, executable in entry["executables"].items():
        if executable["identity"]["version_output"] != expected_output:
            fail(f"{entry['archive']} {name} identity")
        if executable["path"] != f"{entry['package_root']}/{name}":
            fail(f"{entry['archive']} {name} path")

reader = manifest["reader"]
reader_archive = dist / reader["archive"]
if reader["sha256"] != hashlib.sha256(reader_archive.read_bytes()).hexdigest():
    fail("reader digest")
if reader["commit"] != commit:
    fail("reader commit")
if [build["name"] for build in reader["builds"]] != ["embedded", "root"]:
    fail("reader builds")
print(f"schema_target={manifest['schema_target']} river_ledger_target={manifest['river_ledger_target']}")
PY

targets=$(python3 -c "import json,sys;m=json.load(open(sys.argv[1]));print(m['schema_target'],m['river_ledger_target'])" "$TMP/manifest.json")
schema_target=${targets% *}
river_target=${targets#* }

# 2. Supplying the previous release's targets is what makes binary-only
#    rollback provable. Nothing else is allowed to flip that flag.
DIST_DIR="$DIST" PREVIOUS_SCHEMA_TARGET="$schema_target" PREVIOUS_RIVER_TARGET="$river_target" \
	bash "$SCRIPT" preview >"$TMP/rollback.json" 2>"$TMP/rollback.log" ||
	{
		cat "$TMP/rollback.log" >&2
		fail 'preview with a known predecessor failed'
	}
assert_canonical "$TMP/rollback.json" || fail 'rollback manifest is not canonical'
python3 -c "
import json,sys
manifest = json.load(open(sys.argv[1]))
assert manifest['rollback_compatible'] is True, 'identical targets did not allow binary-only rollback'
assert manifest['online_update_compatible'] is False, 'online update must stay denied'
" "$TMP/rollback.json" || fail 'rollback classification is wrong'

DIST_DIR="$DIST" PREVIOUS_SCHEMA_TARGET=some-older-step PREVIOUS_RIVER_TARGET="$river_target" \
	bash "$SCRIPT" preview >"$TMP/advanced.json" 2>/dev/null || fail 'preview with an older predecessor failed'
python3 -c "
import json,sys
manifest = json.load(open(sys.argv[1]))
assert manifest['rollback_compatible'] is False, 'a schema advance still allowed binary-only rollback'
" "$TMP/advanced.json" || fail 'schema advance was treated as rollback compatible'

# 3. Fail-closed signing. Every failure must leave the asset directory clean.
expect_signing_rejected() {
	local name=$1
	local expected=$2
	shift 2
	local signing_dist="$TMP/sign-$name"
	rm -rf "$signing_dist"
	build_dist "$signing_dist"
	if env "$@" DIST_DIR="$signing_dist" bash "$SCRIPT" generate >"$TMP/$name.log" 2>&1; then
		fail "$name produced a release manifest"
	fi
	grep -Eq "$expected" "$TMP/$name.log" || {
		cat "$TMP/$name.log" >&2
		fail "$name did not report the expected signing failure"
	}
	if [[ -e $signing_dist/cairn-release-manifest.json || -e $signing_dist/cairn-release-manifest.json.sig ]]; then
		fail "$name left a release asset behind"
	fi
}

expect_signing_rejected missing-secret 'CAIRN_RELEASE_SIGNING_KEY is not set' CAIRN_RELEASE_SIGNING_KEY=
expect_signing_rejected empty-secret 'CAIRN_RELEASE_SIGNING_KEY (is not set|is empty)' CAIRN_RELEASE_SIGNING_KEY='   '
expect_signing_rejected malformed-secret 'not standard base64' CAIRN_RELEASE_SIGNING_KEY='not-base-64-!!'
expect_signing_rejected short-secret 'Ed25519 seed' CAIRN_RELEASE_SIGNING_KEY="$(printf 'short' | base64)"
expect_signing_rejected untrusted-secret 'not part of the compiled-in trust root' \
	CAIRN_RELEASE_SIGNING_KEY="$(head -c 32 /dev/zero | base64)"

# 4. Packaging rules are enforced while the manifest is produced, not only when
#    the helper unpacks. A release that would be rejected on the host must not
#    reach the host.
BROKEN="$TMP/dist-extra-executable"
build_dist "$BROKEN" tools/debug-shell
if DIST_DIR="$BROKEN" bash "$SCRIPT" preview >/dev/null 2>"$TMP/extra.log"; then
	fail 'a package carrying a third executable was described by a manifest'
fi
grep -Fq 'carries executables' "$TMP/extra.log" || {
	cat "$TMP/extra.log" >&2
	fail 'the extra executable was not the reported reason'
}

MISSING_READER="$TMP/dist-no-reader"
build_dist "$MISSING_READER" '' no
if DIST_DIR="$MISSING_READER" bash "$SCRIPT" preview >/dev/null 2>"$TMP/no-reader.log"; then
	fail 'a release without the Reader archive was described by a manifest'
fi

# 5. Identity inputs are validated before anything is read.
for name in bad-tag bad-commit bad-time bad-repo; do
	case $name in
		bad-tag) env_pair=(TAG=latest) ;;
		bad-commit) env_pair=(COMMIT=0123456789ab) ;;
		bad-time) env_pair=(BUILD_TIME=yesterday) ;;
		bad-repo) env_pair=(REPOSITORY=cairn) ;;
	esac
	if env "${env_pair[@]}" DIST_DIR="$DIST" bash "$SCRIPT" preview >/dev/null 2>"$TMP/$name.log"; then
		fail "$name identity was accepted"
	fi
done

echo 'PASS: the release manifest is canonical, default-deny, and fails closed without a trusted signing key'
