#!/usr/bin/env bash
# Install the systemd deployment layout for Cairn Core (issue #41, phase 2).
#
# The script creates the accounts, directories, files and units of one fixed
# permission model, and then proves the model holds before it exits. The proof
# is the point: the installer and cmd/cairn-updater's start-up self-check are
# separate deliverables that drift, and a host where /opt/webtag/releases
# quietly became group-writable would keep serving updates while the guarantee
# it exists to provide is already gone.
#
# It is idempotent. Every directory goes through `install -d`, which fixes mode
# and ownership on an existing path; every account is created only when getent
# cannot find it; the two files that can hold operator secrets are written only
# when absent and are never rewritten; the units and the helper binary are
# replaced unconditionally because they are code, not configuration.
#
# It writes exactly one secret, and the line between that one and the rest is
# whether the value has a counterparty. DEPLOY_AUTH_TOKEN is shared with the
# clients that call the helper, so the installer must not choose it:
# /etc/cairn-updater.env arrives empty, the helper refuses to start that way,
# and the final report tells the operator what is left to do by hand. The
# Reader session signing key has no counterparty — nobody ever types it, quotes
# it, or copies it to a second host — so an empty value buys no operator
# control and costs a real guarantee: see ensure_core_env.
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

SERVICE_USER=${CAIRN_UPDATER_SERVICE_USER:-webtag}
SERVICE_GROUP=${CAIRN_INSTALL_SERVICE_GROUP:-$SERVICE_USER}
SOCKET_GROUP=${CAIRN_UPDATER_SOCKET_GROUP:-caddy}
SOCKET_PATH=${CAIRN_UPDATER_SOCKET:-/run/cairn-updater.sock}
CORE_DIR=${CAIRN_UPDATER_CORE_DIR:-/opt/webtag}
READER_DIR=${CAIRN_UPDATER_READER_DIR:-/var/www/reader}
STATE_DIR=${CAIRN_UPDATER_STATE_DIR:-/var/lib/cairn-updater}
HELPER_ENV=${CAIRN_UPDATER_ENV_FILE:-/etc/cairn-updater.env}
UNIT_DIR=${CAIRN_INSTALL_UNIT_DIR:-/etc/systemd/system}
BIN_DIR=${CAIRN_INSTALL_BIN_DIR:-/usr/local/bin}
ASSET_DIR=${CAIRN_INSTALL_ASSET_DIR:-$ROOT/deploy}

CORE_ENV="$CORE_DIR/.env"
RELEASES_DIR="$CORE_DIR/releases"
BACKUPS_DIR="$CORE_DIR/backups"
READER_RELEASES_DIR="$READER_DIR/releases"
JOBS_DIR="$STATE_DIR/jobs"
HELPER_BIN="$BIN_DIR/cairn-updater"

updater_binary=''
verify_only=no

# manual_steps collects everything the operator still has to do by hand. It is
# printed last so a successful run ends with the list rather than with a wall of
# progress lines the reader has already stopped following.
manual_steps=()
# violations collects failed requirements. The self-check reports all of them,
# because an operator fixing a fresh host should get the whole list in one pass
# instead of one wrong mode per run.
violations=()

usage() {
	cat <<'USAGE'
Usage: cairn-install.sh [options]

  --updater-binary PATH  install PATH as the cairn-updater helper
  --verify-only          check the permission model and change nothing
  -h, --help             this text

Paths follow the cmd/cairn-updater environment variables
(CAIRN_UPDATER_CORE_DIR, CAIRN_UPDATER_READER_DIR, CAIRN_UPDATER_STATE_DIR,
CAIRN_UPDATER_ENV_FILE, CAIRN_UPDATER_SOCKET, CAIRN_UPDATER_SOCKET_GROUP,
CAIRN_UPDATER_SERVICE_USER) so the installer and the helper cannot disagree
about where the installation is.
USAGE
}

fail() {
	echo "cairn-install: $*" >&2
	exit 1
}

note() { echo "cairn-install: $*"; }

step() { echo "cairn-install: == $*"; }

violate() { violations+=("$1"); }

manual() { manual_steps+=("$1"); }

parse_args() {
	while [[ $# -gt 0 ]]; do
		case $1 in
		--updater-binary)
			[[ $# -ge 2 ]] || fail '--updater-binary needs a path'
			updater_binary=$2
			shift 2
			;;
		--updater-binary=*)
			updater_binary=${1#--updater-binary=}
			shift
			;;
		--verify-only)
			verify_only=yes
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) fail "unknown option $1" ;;
		esac
	done
}

require_host() {
	[[ $(uname -s) == Linux ]] || fail 'this installer only supports Linux hosts'
	[[ $(id -u) -eq 0 ]] || fail 'this installer must run as root'
}

# --- accounts ---------------------------------------------------------------

ensure_socket_group() {
	if getent group "$SOCKET_GROUP" >/dev/null; then
		return
	fi
	# Normally the Caddy package owns this group. Creating it when it is absent
	# keeps a fresh host installable in either order; the package manager reuses
	# an existing system group rather than making a second one.
	groupadd --system "$SOCKET_GROUP"
	note "created system group $SOCKET_GROUP (Caddy is not installed yet, or uses a different group)"
	manual "Confirm Caddy runs as group $SOCKET_GROUP; it is the only account allowed to reach $SOCKET_PATH."
}

ensure_service_account() {
	if ! getent group "$SERVICE_GROUP" >/dev/null; then
		groupadd --system "$SERVICE_GROUP"
		note "created system group $SERVICE_GROUP"
	fi
	if getent passwd "$SERVICE_USER" >/dev/null; then
		return
	fi
	# No home directory, no login shell, no supplementary groups. The account
	# exists to own a process and to be told no by the filesystem.
	useradd --system \
		--gid "$SERVICE_GROUP" \
		--no-create-home \
		--home-dir "$CORE_DIR" \
		--shell /usr/sbin/nologin \
		--comment 'Cairn Core service account' \
		"$SERVICE_USER"
	note "created system account $SERVICE_USER"
}

# --- filesystem -------------------------------------------------------------

ensure_directories() {
	# install -d is the idempotent form: it creates what is missing and applies
	# the ownership and mode to what is already there, so a host whose modes
	# drifted is repaired by re-running rather than by hand.
	install -d -o root -g root -m 0755 "$CORE_DIR"
	install -d -o root -g root -m 0755 "$RELEASES_DIR"
	install -d -o root -g root -m 0755 "$BACKUPS_DIR"
	install -d -o root -g root -m 0755 "$READER_DIR"
	install -d -o root -g root -m 0755 "$READER_RELEASES_DIR"
	install -d -o root -g root -m 0700 "$STATE_DIR"
	install -d -o root -g root -m 0700 "$JOBS_DIR"
}

# SESSION_SIGNING_KEY signs the Reader's httpOnly session cookie. Left empty,
# the application generates one per process, which means the key dies with the
# process: every restart — including every automated update — invalidates every
# session at once. Session mode deliberately keeps no installation token in the
# browser, so at that moment the Reader holds nothing it can re-authenticate
# with and the user gets "无法确认当前身份：unauthorized" with no way forward
# but retyping the key. Nothing in that chain is visible while it is cheap to
# fix, which is why the value is minted here rather than added to the list of
# things the operator is asked to remember.
#
# 32 bytes from the kernel CSPRNG. No openssl dependency: head and base64 are
# coreutils, present on any host that has systemd.
generate_session_signing_key() {
	head -c 32 /dev/urandom | base64 | tr -d '\n'
}

ensure_core_env() {
	if [[ ! -e $CORE_ENV ]]; then
		# An empty file, not a copy of .env.example: the example carries
		# placeholder AI endpoints and a placeholder database URL, and a host
		# that starts with plausible-looking wrong values is worse than one that
		# refuses to start. The application will fail on the missing
		# DATABASE_URL, loudly, which is the intended outcome.
		install -o root -g "$SERVICE_GROUP" -m 0640 /dev/null "$CORE_ENV"
		cat >"$CORE_ENV" <<-EOF
			# Cairn Core configuration, read by webtag.service as an EnvironmentFile.
			# Owned root:$SERVICE_GROUP 0640: the application reads it and cannot rewrite it.
			# Fill in from .env.example in the repository; DATABASE_URL must be the
			# host loopback mapping of the PostgreSQL container.

			# Generated by the installer, once, on this host. It signs the Reader's
			# session cookie; keeping it stable is what lets a session survive a
			# restart. Replacing it signs every Reader out — that is also the only
			# way to revoke a stateless session, so it is the lever to pull if one
			# leaks. Never copy it to another host.
			SESSION_SIGNING_KEY=$(generate_session_signing_key)
		EOF
		chown "root:$SERVICE_GROUP" "$CORE_ENV"
		chmod 0640 "$CORE_ENV"
		note "created $CORE_ENV with a generated SESSION_SIGNING_KEY"
		manual "Fill in $CORE_ENV (DATABASE_URL, AI_*, CURSOR_SIGNING_KEY); webtag.service will not start until you do."
	else
		# Existing content is never touched, only its ownership and mode. An
		# existing file is the operator's, and appending to it would make a
		# re-run capable of editing configuration it did not write. So a host
		# that predates the generated key is reported rather than repaired —
		# the application's own start-up WARN says the same thing every boot.
		chown "root:$SERVICE_GROUP" "$CORE_ENV"
		chmod 0640 "$CORE_ENV"
		if [[ -z $(read_env_value "$CORE_ENV" SESSION_SIGNING_KEY) ]]; then
			manual "Set SESSION_SIGNING_KEY in $CORE_ENV (openssl rand -base64 32); while it is empty every restart signs every Reader out and users must re-enter the installation token."
		fi
	fi
}

ensure_helper_env() {
	local template="$ASSET_DIR/cairn-updater.env.example"
	[[ -r $template ]] || fail "missing $template"
	if [[ ! -e $HELPER_ENV ]]; then
		install -o root -g root -m 0600 "$template" "$HELPER_ENV"
		note "installed the $HELPER_ENV template with an empty DEPLOY_AUTH_TOKEN"
	else
		chown root:root "$HELPER_ENV"
		chmod 0600 "$HELPER_ENV"
	fi
	# The template ships an empty token and this file must never gain one from
	# version control, so the check is on the installed copy every run.
	local token
	token=$(read_env_value "$HELPER_ENV" DEPLOY_AUTH_TOKEN)
	if [[ -z $token ]]; then
		manual "Generate DEPLOY_AUTH_TOKEN into $HELPER_ENV (>= 32 chars, e.g. openssl rand -base64 48); the helper is fail-closed and will not start while it is empty."
	fi
	if [[ -z $(read_env_value "$HELPER_ENV" DATABASE_URL) ]]; then
		manual "Set DATABASE_URL in $HELPER_ENV to the host loopback URL of the PostgreSQL container."
	fi
}

# read_env_value pulls one value out of a systemd EnvironmentFile without
# sourcing it. Sourcing a file to read one variable executes everything else in
# it, and this particular file is the one that holds the deployment token.
read_env_value() {
	local file=$1 key=$2 line=''
	[[ -r $file ]] || return 0
	line=$(grep -E "^[[:space:]]*${key}=" "$file" | tail -n 1 || true)
	line=${line#*=}
	line=${line%\"}
	line=${line#\"}
	printf '%s' "$line"
}

# --- units and binary -------------------------------------------------------

install_helper_binary() {
	if [[ -n $updater_binary ]]; then
		[[ -f $updater_binary ]] || fail "--updater-binary $updater_binary is not a file"
		install -d -o root -g root -m 0755 "$BIN_DIR"
		install -o root -g root -m 0755 "$updater_binary" "$HELPER_BIN"
		note "installed $HELPER_BIN"
		return
	fi
	[[ -x $HELPER_BIN ]] || fail "$HELPER_BIN is missing; build it with 'go build -o cairn-updater ./cmd/cairn-updater' and pass --updater-binary"
}

# install_units copies the two units verbatim. They are not templated, because a
# templated unit is a unit nobody can read and diff against the repository. What
# the installer does instead is refuse to install a unit whose fixed paths
# disagree with the paths this run is using, so an overridden CAIRN_UPDATER_*
# variable produces an error rather than a service pointing at nothing.
install_units() {
	local core_unit="$ASSET_DIR/systemd/webtag.service"
	local helper_unit="$ASSET_DIR/systemd/cairn-updater.service"
	[[ -r $core_unit ]] || fail "missing $core_unit"
	[[ -r $helper_unit ]] || fail "missing $helper_unit"

	assert_unit_line "$core_unit" "ExecStart=$RELEASES_DIR/current/webtag"
	assert_unit_line "$core_unit" "EnvironmentFile=$CORE_ENV"
	assert_unit_line "$core_unit" "User=$SERVICE_USER"
	assert_unit_line "$helper_unit" "ExecStart=$HELPER_BIN serve"
	assert_unit_line "$helper_unit" "EnvironmentFile=$HELPER_ENV"

	install -d -o root -g root -m 0755 "$UNIT_DIR"
	install -o root -g root -m 0644 "$core_unit" "$UNIT_DIR/webtag.service"
	install -o root -g root -m 0644 "$helper_unit" "$UNIT_DIR/cairn-updater.service"
	note "installed webtag.service and cairn-updater.service into $UNIT_DIR"
}

assert_unit_line() {
	local unit=$1 line=$2
	grep -qxF "$line" "$unit" ||
		fail "$(basename "$unit") does not contain '$line'; the unit and this installer disagree about the layout"
}

reload_systemd() {
	if [[ ! -d /run/systemd/system ]]; then
		note 'systemd is not running on this host; skipping daemon-reload'
		manual "Run 'systemctl daemon-reload' on the real host, then 'systemctl enable --now cairn-updater.service'."
		return
	fi
	systemctl daemon-reload
	note 'reloaded the systemd unit cache'
	manual "Enable the units when you are ready to cut over: 'systemctl enable --now cairn-updater.service' and 'systemctl enable webtag.service'."
}

# --- PostgreSQL client tooling ---------------------------------------------

# check_pg_tooling proves a verifiable backup can be taken before an update ever
# stops the service. The helper repeats the existence check in preflight; this
# one also compares major versions, which is the failure that does not show up
# until the dump is attempted: pg_dump refuses a server newer than itself, and a
# pg_restore from a different major line cannot list the archive the helper
# insists on listing before it migrates.
check_pg_tooling() {
	local dump_bin restore_bin dump_major restore_major
	dump_bin=$(command -v "${CAIRN_UPDATER_PG_DUMP:-pg_dump}" || true)
	restore_bin=$(command -v "${CAIRN_UPDATER_PG_RESTORE:-pg_restore}" || true)
	[[ -n $dump_bin ]] || fail 'pg_dump is not on PATH; the helper cannot take a pre-migration dump without it'
	[[ -n $restore_bin ]] || fail 'pg_restore is not on PATH; the helper cannot verify a dump without it'

	dump_major=$(pg_major "$dump_bin")
	restore_major=$(pg_major "$restore_bin")
	[[ -n $dump_major ]] || fail "could not read a major version out of '$dump_bin --version'"
	[[ -n $restore_major ]] || fail "could not read a major version out of '$restore_bin --version'"
	[[ $dump_major == "$restore_major" ]] ||
		fail "pg_dump is major $dump_major but pg_restore is major $restore_major; a dump this host takes is not one it can list"
	note "found pg_dump and pg_restore, major version $dump_major"

	local database_url server_major psql_bin
	database_url=$(read_env_value "$HELPER_ENV" DATABASE_URL)
	[[ -n $database_url ]] || database_url=${DATABASE_URL:-}
	psql_bin=$(command -v psql || true)
	if [[ -z $database_url || -z $psql_bin ]]; then
		manual "Confirm the PostgreSQL server major version is <= $dump_major; this run could not reach the server to check (psql or DATABASE_URL missing)."
		return
	fi
	server_major=$("$psql_bin" "$database_url" -tAc 'SHOW server_version_num' 2>/dev/null | tr -dc '0-9' || true)
	if [[ -z $server_major ]]; then
		manual "Confirm the PostgreSQL server major version is <= $dump_major; this run could not connect with the configured DATABASE_URL."
		return
	fi
	server_major=$((server_major / 10000))
	[[ $dump_major -ge $server_major ]] ||
		fail "pg_dump is major $dump_major but the server is major $server_major; pg_dump refuses to dump a newer server"
	note "PostgreSQL server is major $server_major, within reach of the installed client"
}

# pg_major reads the major version out of a client's --version banner, which
# reads "pg_dump (PostgreSQL) 16.14 (Debian 16.14-1.pgdg13+1)" on a packaged
# host and "pg_dump (PostgreSQL) 17devel" on a source build.
pg_major() {
	local output
	output=$("$1" --version 2>/dev/null || true)
	if [[ $output =~ \(PostgreSQL\)[[:space:]]+([0-9]+) ]]; then
		printf '%s' "${BASH_REMATCH[1]}"
	fi
}

# --- self-check -------------------------------------------------------------

# expect_path is one line of the permission model from issue #41.
expect_path() {
	local path=$1 kind=$2 owner=$3 group=$4 mode=$5 why=$6
	local actual
	if ! actual=$(stat -c '%F|%U|%G|%a' "$path" 2>/dev/null); then
		violate "$path: does not exist ($why)"
		return
	fi
	local found_kind=${actual%%|*}
	local rest=${actual#*|}
	local found_owner=${rest%%|*}
	rest=${rest#*|}
	local found_group=${rest%%|*}
	local found_mode=${rest#*|}

	# stat does not dereference, so a directory that became a symlink is caught
	# here rather than silently passing on whatever it points at.
	case $kind in
	dir) [[ $found_kind == directory ]] || violate "$path: is a $found_kind, the model requires a directory ($why)" ;;
	file) [[ $found_kind == 'regular file' || $found_kind == 'regular empty file' ]] || violate "$path: is a $found_kind, the model requires a regular file ($why)" ;;
	socket) [[ $found_kind == socket ]] || violate "$path: is a $found_kind, the model requires a socket ($why)" ;;
	esac
	[[ $found_owner == "$owner" ]] || violate "$path: is owned by $found_owner, the model requires $owner ($why)"
	[[ $found_group == "$group" ]] || violate "$path: belongs to group $found_group, the model requires $group ($why)"
	[[ $found_mode == "$mode" ]] || violate "$path: has mode 0$found_mode, the model requires 0$mode ($why)"
}

verify_layout() {
	violations=()

	expect_path "$CORE_DIR" dir root root 755 \
		'the application must not be able to add or rename anything beside its own release tree'
	expect_path "$RELEASES_DIR" dir root root 755 \
		'a writable releases directory lets an application compromise choose the program that runs after the next restart'
	expect_path "$BACKUPS_DIR" dir root root 755 \
		'a writable backups directory lets the same compromise destroy the dump the recovery runbook depends on'
	expect_path "$CORE_ENV" file root "$SERVICE_GROUP" 640 \
		'the application reads its own configuration and must not be able to rewrite it'
	expect_path "$READER_DIR" dir root root 755 \
		'the root-domain Reader is served straight from disk, so write access to it is stored XSS on the deployment origin'
	expect_path "$READER_RELEASES_DIR" dir root root 755 \
		'the Reader switch target must not be writable by the application'
	expect_path "$STATE_DIR" dir root root 700 \
		'job records are the helper own memory and survive the Core being stopped'
	expect_path "$JOBS_DIR" dir root root 700 \
		'job status must be readable only by the helper'
	expect_path "$HELPER_ENV" file root root 600 \
		'this file holds DEPLOY_AUTH_TOKEN; anything wider than owner-only hands out deployment authority'
	expect_path "$UNIT_DIR/webtag.service" file root root 644 \
		'a writable unit file is a way to change what the next boot runs and as whom'
	expect_path "$UNIT_DIR/cairn-updater.service" file root root 644 \
		'a writable unit file is a way to change what the next boot runs and as whom'
	if [[ -x $HELPER_BIN ]]; then
		expect_path "$HELPER_BIN" file root root 755 \
			'a writable helper binary is root code execution on the next restart'
	fi
	if [[ -e $SOCKET_PATH ]]; then
		expect_path "$SOCKET_PATH" socket root "$SOCKET_GROUP" 660 \
			'only Caddy may reach the deployment API; the socket group is the whole access control'
	fi

	verify_account_fences
	verify_unit_claims
}

# verify_account_fences checks the two grants that are made somewhere other than
# the filesystem and are therefore invisible to every mode check above.
verify_account_fences() {
	if ! getent passwd "$SERVICE_USER" >/dev/null; then
		violate "user $SERVICE_USER: does not exist (the application account is the subject of this whole model)"
		return
	fi
	local uid
	uid=$(id -u "$SERVICE_USER")
	[[ $uid -ne 0 ]] ||
		violate "user $SERVICE_USER: is uid 0 (an application running as root has nothing to be fenced from)"

	# id -nG is space separated and the script's IFS is newline/tab, so the read
	# gets its own IFS. Without it the whole list arrives as one element and the
	# comparison below silently never matches — which is the failure mode of a
	# fence check that reports success on a host that has already lost.
	local groups=() group
	IFS=' ' read -r -a groups <<<"$(id -nG "$SERVICE_USER")"
	for group in "${groups[@]}"; do
		[[ $group != "$SOCKET_GROUP" ]] ||
			violate "user $SERVICE_USER: is a member of $SOCKET_GROUP, so it can connect to the deployment socket"
		[[ $group != docker ]] ||
			violate "user $SERVICE_USER: is a member of docker, which is root on this host by another name"
		[[ $group != root ]] ||
			violate "user $SERVICE_USER: is a member of root"
	done
}

# verify_unit_claims re-reads the installed units. The two mistakes it looks for
# are the ones that would pass every filesystem check: handing the deployment
# tree back to the application through ReadWritePaths, and putting the
# deployment token into the application's environment.
verify_unit_claims() {
	local unit="$UNIT_DIR/webtag.service"
	[[ -r $unit ]] || return 0

	local rw
	while IFS= read -r rw; do
		violate "$unit: grants the application write access through '$rw'; ProtectSystem=strict does not protect a path that is listed in ReadWritePaths"
	done < <(grep -E "^ReadWritePaths=.*($CORE_DIR|$READER_DIR|/etc|$STATE_DIR)" "$unit" || true)

	! grep -qE '(^|[^A-Z_])DEPLOY_AUTH_TOKEN' "$unit" ||
		violate "$unit: mentions DEPLOY_AUTH_TOKEN; the application must never hold the deployment credential"
	! grep -qF "EnvironmentFile=$HELPER_ENV" "$unit" ||
		violate "$unit: reads $HELPER_ENV, which is the helper's token file"
	grep -qxF "User=$SERVICE_USER" "$unit" ||
		violate "$unit: does not run as $SERVICE_USER"
	grep -qxF 'NoNewPrivileges=yes' "$unit" ||
		violate "$unit: does not set NoNewPrivileges=yes"
	grep -qxF 'ProtectSystem=strict' "$unit" ||
		violate "$unit: does not set ProtectSystem=strict"
}

report_layout() {
	if [[ ${#violations[@]} -eq 0 ]]; then
		note "the permission model holds under $CORE_DIR, $READER_DIR, $STATE_DIR and $UNIT_DIR"
		return 0
	fi
	echo "cairn-install: the deployment layout does not satisfy its permission model:" >&2
	local violation
	for violation in "${violations[@]}"; do
		echo "  $violation" >&2
	done
	return 1
}

report_manual_steps() {
	echo
	if [[ ${#manual_steps[@]} -eq 0 ]]; then
		note 'nothing is left to do by hand'
		return
	fi
	echo "cairn-install: the operator still has to do this by hand:"
	local index=1 entry
	for entry in "${manual_steps[@]}"; do
		echo "  $index. $entry"
		index=$((index + 1))
	done
}

main() {
	parse_args "$@"
	require_host

	if [[ $verify_only == yes ]]; then
		step 'verifying the permission model'
		verify_layout
		report_layout
		return
	fi

	step 'accounts'
	ensure_socket_group
	ensure_service_account

	step 'directories and files'
	ensure_directories
	ensure_core_env
	ensure_helper_env

	step 'helper binary and units'
	install_helper_binary
	install_units
	reload_systemd

	step 'PostgreSQL client tooling'
	check_pg_tooling

	step 'self-check'
	verify_layout
	report_layout

	manual "Point Caddy at the socket: paste deploy/caddy/cairn-deploy.caddy into both site blocks and reload Caddy."
	report_manual_steps
}

main "$@"
