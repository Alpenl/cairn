#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

python3 - "$ROOT" <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
caddy_path = root / "deploy/caddy/cairn-deploy.caddy"
installer_path = root / "scripts/cairn-install.sh"
updater_unit_path = root / "deploy/systemd/cairn-updater.service"
webtag_unit_path = root / "deploy/systemd/webtag.service"


def fail(message):
    raise SystemExit(f"deploy contract: {message}")


def read(path):
    try:
        return path.read_text(encoding="utf-8")
    except FileNotFoundError:
        fail(f"missing {path.relative_to(root)}")


caddy = read(caddy_path)
live_lines = [
    line
    for line in caddy.splitlines()
    if line.strip() and not line.lstrip().startswith("#")
]
live = "\n".join(live_lines)

if live.count("handle /api/deploy/system/* {") != 1:
    fail("Caddy fragment must define exactly one live deployment handle block")
if "handle_path /api/deploy/system/*" in live:
    fail("Caddy fragment must not strip the deployment route prefix")
if "reverse_proxy unix//run/cairn-updater.sock" not in live:
    fail("Caddy fragment must proxy deployment routes to the updater Unix socket")
if 'header_down -Cache-Control' not in live or 'header_down +Cache-Control "no-store"' not in live:
    fail("Caddy fragment must force no-store on deployment API responses")

for forbidden in ("basic_auth", "forward_auth", "header_up"):
    if re.search(rf"^\s*{forbidden}\b", live, flags=re.MULTILINE):
        fail(f"Caddy fragment must not carry deployment credentials through {forbidden}")

depth = 0
for line in live_lines:
    depth += line.count("{")
    depth -= line.count("}")
    if depth < 0:
        fail("Caddy fragment closes more blocks than it opens")
if depth != 0:
    fail("Caddy fragment has unbalanced braces")

for example in ("reader.alpenl.com {", "webtag.alpenl.com {"):
    if example not in caddy:
        fail(f"Caddy reference example is missing {example}")

installer = read(installer_path)
if "deploy/caddy/cairn-deploy.caddy" not in installer:
    fail("installer manual step must point at the tracked Caddy fragment")

updater_unit = read(updater_unit_path)
if "ReadWritePaths=/run" not in updater_unit:
    fail("updater unit must be allowed to create the Unix socket under /run")
if "EnvironmentFile=/etc/cairn-updater.env" not in updater_unit:
    fail("updater unit must read its root-only environment file")

webtag_unit = read(webtag_unit_path)
for hidden in ("/etc/cairn-updater.env", "/run/cairn-updater.sock", "/var/lib/cairn-updater"):
    if f"InaccessiblePaths=-{hidden}" not in webtag_unit:
        fail(f"webtag unit must hide {hidden}")

print("deploy contract: Caddy fragment, installer pointer, and unit fences are consistent")
PY
