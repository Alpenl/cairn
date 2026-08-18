#!/usr/bin/env bash
# Permission negative tests for the deployment layout of issue #41.
#
# Every assertion below is a real syscall made by the real uid, inside a Linux
# container, against the layout scripts/cairn-install.sh actually produced. None
# of it is a string match on a unit file standing in for a permission check: the
# question "can the application account delete the release tree" is answered by
# trying to delete the release tree as that account.
#
# What this cannot cover, and does not pretend to: the container has no systemd,
# so the units are installed and read but never started, `systemd-analyze
# security` never runs, and the ordering guarantees between webtag.service and
# cairn-updater.service are unexercised. Those belong to the staging VM in phase
# 2's acceptance list. What is covered here is the part that is pure uid, gid
# and mode — which is the part the whole design rests on.
#
# Usage:
#   bash scripts/cairn-install.test.sh          run the suite in a container
#   bash scripts/cairn-install.test.sh --in-container   the body, as root
set -euo pipefail
IFS=$'\n\t'

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)

# postgres:16 is chosen for what it already contains: a Debian userland with
# useradd/setpriv, a matching pg_dump/pg_restore pair for the installer's
# tooling check, and perl for the socket probe. It is also already present on
# any host that has run the existing container smoke test.
IMAGE=${CAIRN_DEPLOY_TEST_IMAGE:-postgres:16}

TOKEN='deploy-token-for-tests-0123456789abcdef'
DB_URL='postgres://webtag:secret@127.0.0.1:5432/webtag?sslmode=disable'

pass_count=0

pass() {
	pass_count=$((pass_count + 1))
	printf '  ok   %s\n' "$1"
}

fail() {
	printf '  FAIL %s\n' "$1" >&2
	exit 1
}

step() { printf '\n== %s\n' "$1"; }

# --- host side --------------------------------------------------------------

# Global rather than local: the EXIT trap below runs after this function has
# returned, and a local would already be out of scope by then.
work=''

run_on_host() {
	command -v docker >/dev/null || fail 'docker is required to run the permission negative tests'
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT

	echo "building cairn-updater for the container"
	# CGO_ENABLED=0 so one binary runs on whatever libc the image has, and so a
	# helper built here is the same helper the release ships.
	(cd "$ROOT" && CGO_ENABLED=0 go build -o "$work/cairn-updater" ./cmd/cairn-updater)

	docker run --rm \
		--entrypoint bash \
		-v "$ROOT:/src:ro" \
		-v "$work:/work" \
		"$IMAGE" /src/scripts/cairn-install.test.sh --in-container
}

# --- container side ---------------------------------------------------------

CORE_DIR=/opt/webtag
READER_DIR=/var/www/reader
STATE_DIR=/var/lib/cairn-updater
HELPER_ENV=/etc/cairn-updater.env
UNIT_DIR=/etc/systemd/system
SOCKET=/run/cairn-updater.sock
TAG=v9.9.9

webtag_uid=''
webtag_gid=''

# as_webtag runs a command with the real uid and gid of the application account
# and with every supplementary group dropped, which is what systemd's User=
# produces. Nothing here uses su or a login shell: the question is what the
# kernel permits that uid, not what a shell profile does.
as_webtag() {
	setpriv --reuid "$webtag_uid" --regid "$webtag_gid" --clear-groups -- "$@"
}

denied() {
	local what=$1
	shift
	if as_webtag "$@" >/dev/null 2>&1; then
		fail "$what -- the application account was allowed to do this"
	fi
	pass "$what"
}

allowed() {
	local what=$1
	shift
	if ! as_webtag "$@" >/dev/null 2>&1; then
		fail "$what -- the application account was refused, but this must work"
	fi
	pass "$what"
}

expect_exit_nonzero() {
	local what=$1
	shift
	if "$@" >/dev/null 2>&1; then
		fail "$what -- the command succeeded and had to fail"
	fi
	pass "$what"
}

expect_exit_zero() {
	local what=$1
	shift
	if ! "$@" >/dev/null 2>&1; then
		fail "$what -- the command failed and had to succeed"
	fi
	pass "$what"
}

install_layout() {
	bash /src/scripts/cairn-install.sh --updater-binary /work/cairn-updater
}

seed_release_trees() {
	# What a host looks like after one successful update: a release tree, the
	# two current symlinks, a pre-migration dump and a Reader build. The
	# negative tests need something real to fail to delete.
	install -d -o root -g root -m 0755 "$CORE_DIR/releases/$TAG"
	install -o root -g root -m 0755 /work/cairn-updater "$CORE_DIR/releases/$TAG/webtag"
	install -o root -g root -m 0755 /work/cairn-updater "$CORE_DIR/releases/$TAG/migrate"
	ln -sfn "$CORE_DIR/releases/$TAG" "$CORE_DIR/releases/current"

	install -d -o root -g root -m 0755 "$READER_DIR/releases/$TAG"
	printf '<!doctype html><title>reader</title>' >"$READER_DIR/releases/$TAG/index.html"
	chmod 0644 "$READER_DIR/releases/$TAG/index.html"
	ln -sfn "$READER_DIR/releases/$TAG" "$READER_DIR/current"

	# The helper runs with UMask=0077, so a dump lands owner-only. Reproduced
	# here so the "the application cannot read the backup" assertion is testing
	# the mode the unit actually produces.
	(
		umask 0077
		printf 'PGDMP fake custom-format dump' >"$CORE_DIR/backups/pre-$TAG.dump"
	)
}

fill_environment_files() {
	# A realistic application environment, so the "no deploy token in the
	# application's environment" test has something to find besides nothing.
	# Appended, not written: the installer already put a generated
	# SESSION_SIGNING_KEY in this file, and clobbering it here would make the
	# idempotency assertion below pass against a file that no longer resembles
	# what a real host carries.
	cat >>"$CORE_DIR/.env" <<-EOF
		DATABASE_URL=$DB_URL
		APP_ENV=prod
	EOF
	chown "root:webtag" "$CORE_DIR/.env"
	chmod 0640 "$CORE_DIR/.env"

	# The operator's manual step, performed here so the helper can start.
	{
		printf 'DEPLOY_AUTH_TOKEN=%s\n' "$TOKEN"
		printf 'DATABASE_URL=%s\n' "$DB_URL"
	} >>"$HELPER_ENV"
	chmod 0600 "$HELPER_ENV"
}

write_socket_probe() {
	cat >/work/probe.pl <<'PERL'
use strict;
use warnings;
use IO::Socket::UNIX;
my ($path, $token) = @ARGV;
my $sock = IO::Socket::UNIX->new(Peer => $path, Type => SOCK_STREAM())
    or die "connect $path: $!\n";
my $auth = (defined $token && length $token) ? "Authorization: Bearer $token\r\n" : '';
print {$sock} "GET /api/deploy/system/version HTTP/1.0\r\nHost: localhost\r\n${auth}\r\n";
my $status = <$sock>;
print defined($status) ? $status : "no response\n";
PERL
	chmod 0644 /work/probe.pl
}

# --- the suite --------------------------------------------------------------

test_installer_runs_clean() {
	step 'the installer produces the layout on a fresh host'
	expect_exit_zero 'a first run of the installer exits zero' install_layout
	expect_exit_zero 'its own self-check accepts what it just built' \
		bash /src/scripts/cairn-install.sh --verify-only

	local model
	model=$(stat -c '%n %U:%G %a' \
		"$CORE_DIR" "$CORE_DIR/releases" "$CORE_DIR/backups" "$CORE_DIR/.env" \
		"$READER_DIR" "$READER_DIR/releases" "$STATE_DIR" "$HELPER_ENV" \
		"$UNIT_DIR/webtag.service" "$UNIT_DIR/cairn-updater.service")
	printf '%s\n' "$model" | sed 's/^/       /'

	local expected
	expected=$(
		cat <<-EOF
			$CORE_DIR root:root 755
			$CORE_DIR/releases root:root 755
			$CORE_DIR/backups root:root 755
			$CORE_DIR/.env root:webtag 640
			$READER_DIR root:root 755
			$READER_DIR/releases root:root 755
			$STATE_DIR root:root 700
			$HELPER_ENV root:root 600
			$UNIT_DIR/webtag.service root:root 644
			$UNIT_DIR/cairn-updater.service root:root 644
		EOF
	)
	[[ $model == "$expected" ]] || fail 'the installed layout does not match the permission model of issue #41'
	pass 'every path matches the ownership and mode the issue specifies'

	# The Reader's session cookie is signed with this key. An empty value is not
	# a disabled feature, it is a key regenerated on every boot: sessions then
	# die at every restart, and session mode holds no installation token in the
	# browser to recover with. A fresh host must therefore arrive with a real
	# key, not with a blank to fill in.
	local session_key
	session_key=$(grep -E '^SESSION_SIGNING_KEY=' "$CORE_DIR/.env" | tail -n 1)
	session_key=${session_key#SESSION_SIGNING_KEY=}
	[[ -n $session_key ]] || fail 'the installer left SESSION_SIGNING_KEY empty in the core environment file'
	[[ $(printf '%s' "$session_key" | base64 -d 2>/dev/null | wc -c) -eq 32 ]] ||
		fail 'the generated SESSION_SIGNING_KEY does not decode to 32 bytes'
	pass 'the core environment file arrives with a generated 32-byte SESSION_SIGNING_KEY'
}

test_helper_selfcheck_accepts_the_layout() {
	step "the helper's own start-up self-check accepts the installed layout"
	# This is the join between the two deliverables: cmd/cairn-updater refuses to
	# serve on a host whose layout it does not recognise, and the installer's
	# whole job is to produce a layout it does.
	expect_exit_zero 'cairn-updater selfcheck exits zero against the installed tree' \
		env DEPLOY_AUTH_TOKEN="$TOKEN" DATABASE_URL="$DB_URL" /usr/local/bin/cairn-updater selfcheck
}

test_application_cannot_write_the_core_tree() {
	step 'the application account cannot alter the Core deployment tree'
	denied 'it cannot create a file in the releases directory' \
		touch "$CORE_DIR/releases/pwned"
	denied 'it cannot create a directory in the releases directory' \
		mkdir "$CORE_DIR/releases/v0.0.1"
	denied 'it cannot delete the installed release binary' \
		rm -f "$CORE_DIR/releases/$TAG/webtag"
	denied 'it cannot overwrite the installed release binary' \
		dd if=/dev/zero of="$CORE_DIR/releases/$TAG/webtag" bs=1 count=1 conv=notrunc
	denied 'it cannot rename the current symlink' \
		mv "$CORE_DIR/releases/current" "$CORE_DIR/releases/stale"
	denied 'it cannot repoint the current symlink' \
		ln -sfn /tmp/evil "$CORE_DIR/releases/current"
	denied 'it cannot delete the whole release tree' \
		rm -rf "$CORE_DIR/releases/$TAG"
	denied 'it cannot add a sibling directory beside its release tree' \
		mkdir "$CORE_DIR/evil"
}

test_application_cannot_touch_its_own_configuration() {
	step 'the application account can read its configuration and cannot change it'
	allowed 'it can read /opt/webtag/.env, which is the point of root:webtag 0640' \
		cat "$CORE_DIR/.env"
	denied 'it cannot append to /opt/webtag/.env' \
		dd if=/dev/zero of="$CORE_DIR/.env" bs=1 count=1 oflag=append conv=notrunc
	denied 'it cannot rename /opt/webtag/.env' \
		mv "$CORE_DIR/.env" "$CORE_DIR/.env.bak"
	denied 'it cannot delete /opt/webtag/.env' \
		rm -f "$CORE_DIR/.env"
}

test_application_cannot_touch_backups() {
	step 'the application account cannot reach the recovery evidence'
	denied 'it cannot delete a pre-migration dump' \
		rm -f "$CORE_DIR/backups/pre-$TAG.dump"
	denied 'it cannot truncate a pre-migration dump' \
		dd if=/dev/zero of="$CORE_DIR/backups/pre-$TAG.dump" bs=1 count=1 conv=notrunc
	denied 'it cannot read a pre-migration dump (UMask=0077 on the helper unit)' \
		cat "$CORE_DIR/backups/pre-$TAG.dump"
	denied 'it cannot write a decoy dump into the backups directory' \
		touch "$CORE_DIR/backups/pre-v9.9.8.dump"
}

test_application_cannot_touch_the_reader_tree() {
	step 'the application account cannot alter the root-domain Reader'
	denied 'it cannot overwrite the served index.html' \
		dd if=/dev/zero of="$READER_DIR/releases/$TAG/index.html" bs=1 count=1 conv=notrunc
	denied 'it cannot add a file to the Reader release tree' \
		touch "$READER_DIR/releases/$TAG/evil.js"
	denied 'it cannot add a release directory' \
		mkdir "$READER_DIR/releases/v0.0.1"
	denied 'it cannot delete a Reader release' \
		rm -rf "$READER_DIR/releases/$TAG"
	denied 'it cannot repoint the Reader current symlink' \
		ln -sfn /tmp/evil "$READER_DIR/current"
	denied 'it cannot rename the Reader current symlink' \
		mv "$READER_DIR/current" "$READER_DIR/stale"
}

test_application_cannot_touch_units_or_helper() {
	step 'the application account cannot change what the next boot runs'
	denied 'it cannot append to webtag.service' \
		dd if=/dev/zero of="$UNIT_DIR/webtag.service" bs=1 count=1 oflag=append conv=notrunc
	denied 'it cannot drop a new unit into the unit directory' \
		touch "$UNIT_DIR/evil.service"
	denied 'it cannot overwrite the helper binary' \
		dd if=/dev/zero of=/usr/local/bin/cairn-updater bs=1 count=1 conv=notrunc
	denied 'it cannot read the helper state directory' \
		ls "$STATE_DIR"
}

test_application_cannot_read_the_deploy_token() {
	step 'the deployment credential is out of the application account reach'
	denied 'it cannot read /etc/cairn-updater.env' cat "$HELPER_ENV"
	denied 'it cannot write /etc/cairn-updater.env' \
		dd if=/dev/zero of="$HELPER_ENV" bs=1 count=1 oflag=append conv=notrunc

	# The environment webtag.service actually composes, assembled from the
	# EnvironmentFile= lines of the installed unit and evaluated as the
	# application uid. There is no systemd here to do it, so it is done the way
	# systemd does it, and then checked from inside the account.
	local unit="$UNIT_DIR/webtag.service" line file composed=()
	while IFS= read -r line; do
		file=${line#EnvironmentFile=}
		file=${file#-}
		[[ -r $file ]] || continue
		while IFS= read -r entry; do
			[[ $entry =~ ^[[:space:]]*(#|$) ]] && continue
			composed+=("$entry")
		done <"$file"
	done < <(grep -E '^EnvironmentFile=' "$unit" || true)

	local rendered
	rendered=$(as_webtag env -i "${composed[@]}")
	# Printed so a failure is diagnosable from the log alone, with secret-shaped
	# values masked. This container's key is a throwaway, but a suite that
	# prints one teaches the eye to skim past a key in CI output, and the next
	# value in that position may not be disposable.
	printf '%s\n' "$rendered" |
		sed -E 's/^([A-Z0-9_]*(KEY|TOKEN|SECRET|PASSWORD)[A-Z0-9_]*=).*/\1<redacted>/' |
		sed 's/^/       /'
	grep -q '^DATABASE_URL=' <<<"$rendered" ||
		fail 'the composed application environment is empty, so finding no token in it would prove nothing'
	! grep -q 'DEPLOY_AUTH_TOKEN' <<<"$rendered" ||
		fail 'DEPLOY_AUTH_TOKEN reached the application environment'
	pass 'the composed application environment carries DATABASE_URL and no DEPLOY_AUTH_TOKEN'

	! grep -q 'DEPLOY_AUTH_TOKEN' "$unit" ||
		fail 'webtag.service mentions DEPLOY_AUTH_TOKEN'
	! grep -qF "EnvironmentFile=$HELPER_ENV" "$unit" ||
		fail "webtag.service reads $HELPER_ENV"
	pass 'webtag.service names neither the token nor the file that holds it'
}

test_socket_is_reachable_only_with_the_token() {
	step 'the deployment socket refuses the application account and demands a token'
	write_socket_probe

	env DEPLOY_AUTH_TOKEN="$TOKEN" DATABASE_URL="$DB_URL" \
		/usr/local/bin/cairn-updater serve >/work/updater.log 2>&1 &
	local helper_pid=$!
	local waited=0
	while [[ ! -S $SOCKET ]]; do
		waited=$((waited + 1))
		[[ $waited -lt 100 ]] || {
			cat /work/updater.log >&2
			fail 'the helper never created its socket'
		}
		sleep 0.1
	done

	local owner
	owner=$(stat -c '%F %U:%G %a' "$SOCKET")
	[[ $owner == 'socket root:caddy 660' ]] ||
		fail "the socket is '$owner', the model requires 'socket root:caddy 660'"
	pass 'the socket is root:caddy 0660'

	local status
	status=$(perl /work/probe.pl "$SOCKET" 2>&1 || true)
	grep -q '401' <<<"$status" ||
		fail "an unauthenticated request over the socket returned '$status', expected 401"
	pass 'a request without the token is refused even though it arrived on the local socket'

	status=$(perl /work/probe.pl "$SOCKET" "$TOKEN" 2>&1 || true)
	grep -q '200' <<<"$status" ||
		fail "an authenticated request returned '$status', expected 200"
	pass 'a request with the token is served, so the refusals above are about access and not about a dead socket'

	status=$(as_webtag perl /work/probe.pl "$SOCKET" "$TOKEN" 2>&1 || true)
	grep -qi 'permission denied' <<<"$status" ||
		fail "the application account got '$status' from the socket, expected a connect refusal"
	pass 'the application account cannot connect to the socket even holding the token'

	kill "$helper_pid" 2>/dev/null || true
	wait "$helper_pid" 2>/dev/null || true
	rm -f "$SOCKET"
}

test_account_is_fenced() {
	step 'the application account holds no privilege that would make the modes irrelevant'
	[[ $(id -u webtag) -ne 0 ]] || fail 'webtag is uid 0'
	pass 'webtag is not uid 0'
	! id -nG webtag | grep -qw caddy || fail 'webtag is in the caddy group'
	pass 'webtag is not in the socket group'
	! id -nG webtag | grep -qw docker || fail 'webtag is in the docker group'
	pass 'webtag is not in the docker group'
	[[ $(getent passwd webtag | cut -d: -f7) == /usr/sbin/nologin ]] ||
		fail 'webtag has a login shell'
	pass 'webtag has no login shell'
}

test_self_check_is_not_vacuous() {
	step 'the self-check fails on a layout that has drifted'
	chmod 0777 "$CORE_DIR/releases"
	expect_exit_nonzero 'the installer self-check rejects a group-writable releases directory' \
		bash /src/scripts/cairn-install.sh --verify-only
	expect_exit_nonzero 'the helper refuses to start on the same host' \
		env DEPLOY_AUTH_TOKEN="$TOKEN" DATABASE_URL="$DB_URL" /usr/local/bin/cairn-updater selfcheck
	# The report is captured rather than piped: the command exits non-zero by
	# design, and a pipeline under pipefail would report the exit code instead
	# of the question being asked.
	local report
	report=$(bash /src/scripts/cairn-install.sh --verify-only 2>&1 || true)
	grep -q "$CORE_DIR/releases: has mode 0777" <<<"$report" ||
		fail "the self-check did not name the path and mode that failed: $report"
	pass 'the failure names the path that broke the model and the mode it carries'

	chmod 0755 "$CORE_DIR/releases"
	expect_exit_zero 'the self-check passes again once the mode is restored' \
		bash /src/scripts/cairn-install.sh --verify-only

	# The second fence: membership of the socket group is granted in /etc/group,
	# not in the filesystem, so no mode check would ever notice it.
	gpasswd -a webtag caddy >/dev/null
	expect_exit_nonzero 'the installer self-check rejects webtag being added to the caddy group' \
		bash /src/scripts/cairn-install.sh --verify-only
	expect_exit_nonzero 'the helper refuses to start once webtag can reach its socket' \
		env DEPLOY_AUTH_TOKEN="$TOKEN" DATABASE_URL="$DB_URL" /usr/local/bin/cairn-updater selfcheck
	gpasswd -d webtag caddy >/dev/null
	expect_exit_zero 'both accept the host again once the membership is removed' \
		bash /src/scripts/cairn-install.sh --verify-only
}

# test_pg_tooling_check exercises the two answers the client-version check can
# give that a host with a matching pair never reaches. Both are checked without
# a running server, because the failure being tested is a client mismatch and
# standing up PostgreSQL to prove it would only add a second thing that can
# break.
test_pg_tooling_check() {
	step 'the installer refuses a PostgreSQL client pair it cannot dump and restore with'
	cat >/work/fake-pg-dump <<-'STUB'
		#!/bin/sh
		echo "pg_dump (PostgreSQL) 13.20 (Debian 13.20-1.pgdg13+1)"
	STUB
	chmod 0755 /work/fake-pg-dump

	local report
	report=$(CAIRN_UPDATER_PG_DUMP=/work/fake-pg-dump install_layout 2>&1 || true)
	grep -q 'pg_dump is major 13 but pg_restore is major 16' <<<"$report" ||
		fail "a mismatched client pair was accepted: $report"
	pass 'a pg_dump and pg_restore from different major lines is a hard failure'

	report=$(CAIRN_UPDATER_PG_DUMP=/nonexistent-pg-dump install_layout 2>&1 || true)
	grep -q 'pg_dump is not on PATH' <<<"$report" ||
		fail "a missing pg_dump was accepted: $report"
	pass 'a missing pg_dump is a hard failure, not a warning discovered after quiesce'

	# psql is installed here and the DATABASE_URL in /etc/cairn-updater.env
	# points at a database that is not running, which is the ordinary state of a
	# host being installed before the container is up. That must be a manual
	# step, not a refusal to install.
	report=$(install_layout 2>&1)
	grep -q 'could not connect with the configured DATABASE_URL' <<<"$report" ||
		fail "an unreachable server did not produce a manual step: $report"
	pass 'an unreachable server is reported as an open manual step and does not block the install'
}

test_installer_is_idempotent() {
	step 'a second installer run changes nothing'
	local before after
	before=$(layout_snapshot)
	local token_before env_before
	token_before=$(sha256sum "$HELPER_ENV" | cut -d' ' -f1)
	env_before=$(sha256sum "$CORE_DIR/.env" | cut -d' ' -f1)

	expect_exit_zero 'a second run of the installer exits zero' install_layout

	after=$(layout_snapshot)
	if [[ $before != "$after" ]]; then
		diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&2 || true
		fail 'the second run changed ownership, mode or the set of installed paths'
	fi
	pass 'ownership, mode and the set of installed paths are unchanged'

	[[ $(sha256sum "$HELPER_ENV" | cut -d' ' -f1) == "$token_before" ]] ||
		fail 'the second run overwrote /etc/cairn-updater.env, destroying the operator token'
	pass 'the operator DEPLOY_AUTH_TOKEN survived the second run'
	[[ $(sha256sum "$CORE_DIR/.env" | cut -d' ' -f1) == "$env_before" ]] ||
		fail 'the second run overwrote /opt/webtag/.env'
	pass 'the application configuration survived the second run'

	# Drifted modes are repaired rather than reported, which is the other half of
	# idempotency: re-running has to converge on the model from either side.
	chmod 0777 "$CORE_DIR/backups"
	chown webtag:webtag "$STATE_DIR"
	expect_exit_zero 'a run against a drifted host exits zero' install_layout
	[[ $(stat -c '%U:%G %a' "$CORE_DIR/backups") == 'root:root 755' ]] ||
		fail 'the installer did not repair the backups directory'
	[[ $(stat -c '%U:%G %a' "$STATE_DIR") == 'root:root 700' ]] ||
		fail 'the installer did not repair the state directory'
	pass 'drifted ownership and modes are repaired back to the model'

	# A host installed before the key was generated. The installer must report
	# it and change nothing: an existing .env is the operator's file, and a
	# re-run that could append to it is a re-run that could edit configuration
	# it did not write. Reporting is the whole remedy here, so its absence is
	# the failure worth catching.
	local legacy_env legacy_hash report
	legacy_env=$(grep -vE '^SESSION_SIGNING_KEY=' "$CORE_DIR/.env")
	printf '%s\n' "$legacy_env" >"$CORE_DIR/.env"
	chown root:webtag "$CORE_DIR/.env"
	chmod 0640 "$CORE_DIR/.env"
	legacy_hash=$(sha256sum "$CORE_DIR/.env" | cut -d' ' -f1)

	report=$(install_layout)
	[[ $(sha256sum "$CORE_DIR/.env" | cut -d' ' -f1) == "$legacy_hash" ]] ||
		fail 'the installer edited an existing /opt/webtag/.env instead of reporting the missing key'
	pass 'an existing core environment file is never edited, even to add the key'
	grep -q 'SESSION_SIGNING_KEY' <<<"$report" ||
		fail 'a host with no SESSION_SIGNING_KEY was not told to set one'
	pass 'a host missing SESSION_SIGNING_KEY is reported as a manual step'
}

layout_snapshot() {
	find "$CORE_DIR" "$READER_DIR" "$STATE_DIR" "$HELPER_ENV" \
		"$UNIT_DIR/webtag.service" "$UNIT_DIR/cairn-updater.service" \
		/usr/local/bin/cairn-updater \
		-printf '%p %u %g %m %y\n' | sort
}

run_in_container() {
	[[ $(id -u) -eq 0 ]] || fail 'the container body must run as root'

	test_installer_runs_clean
	fill_environment_files
	seed_release_trees

	webtag_uid=$(id -u webtag)
	webtag_gid=$(id -g webtag)

	test_account_is_fenced
	test_helper_selfcheck_accepts_the_layout
	test_application_cannot_write_the_core_tree
	test_application_cannot_touch_its_own_configuration
	test_application_cannot_touch_backups
	test_application_cannot_touch_the_reader_tree
	test_application_cannot_touch_units_or_helper
	test_application_cannot_read_the_deploy_token
	test_socket_is_reachable_only_with_the_token
	test_self_check_is_not_vacuous
	test_pg_tooling_check
	test_installer_is_idempotent

	printf '\ncairn-install.test: %d assertions passed\n' "$pass_count"
}

if [[ ${1:-} == --in-container ]]; then
	run_in_container
else
	run_on_host
fi
