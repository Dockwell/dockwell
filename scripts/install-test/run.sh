#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# The install script's test scenario. It runs as root on the system under
# test, which is either a container (prepare.sh sets it up) or a VM:
#
#   1. A first server: install an older release, refuse a tampered archive and
#      a bad signature, upgrade in place, re-run, uninstall.
#   2. A server that joins: install with a ticket from a control plane that
#      runs beside it, check it runs only the agent and connects, uninstall.
#
# When systemd or OpenRC is running, the install script starts the service and
# this checks it. In a container where systemd is only installed, not running,
# this starts the binary the way the unit says, and checks the unit with
# systemd-analyze verify. The script is piped to sh, as people run it.
#
#   INSTALL=<install.sh> RELEASES=<url> KEY=<cosign.pub> OLD=<v> NEW=<v> run.sh
#
# RELEASES holds the trees releases.sh makes. COSIGN, when set, is a copy of
# the pinned cosign to use after the first install, which downloads its own.
set -eu

: "${INSTALL:?}" "${RELEASES:?}" "${KEY:?}" "${OLD:?}" "${NEW:?}"
BIN=/usr/local/bin/dockwell
DATA=/var/lib/dockwell
UNIT=/etc/systemd/system/dockwell.service
INITD=/etc/init.d/dockwell
PANEL=http://127.0.0.1:7600
FIXTURE=http://127.0.0.1:7601
MEMORY_LIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$INSTALL")
[ -n "$MEMORY_LIMIT" ] || { echo "can't read MEMORY_LIMIT from $INSTALL" >&2; exit 1; }
LIMIT=GOMEMLIMIT=$MEMORY_LIMIT
log=$(mktemp -d)
case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64) arch=arm64 ;;
*) arch=$(uname -m) ;;
esac

step() { printf '\n== %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }
die() {
	printf 'FAIL: %s\n' "$*" >&2
	for f in "$log/out" "$log/dockwell.log" /var/log/dockwell.log; do
		[ -s "$f" ] || continue
		printf -- '--- %s\n' "$f" >&2
		tail -n 40 "$f" >&2
	done
	if [ "$mode" = systemd ]; then journalctl -u dockwell -n 40 --no-pager >&2 || true; fi
	exit 1
}

if [ -d /run/systemd/system ]; then
	mode=systemd
elif [ -e /run/openrc/softlevel ]; then
	mode=openrc
else
	mode=direct
fi

# get prints what a URL answers, with curl or, on Alpine, busybox wget.
get() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 5 "$1"
	else
		wget -q -T 5 -O - "$1"
	fi
}

get_html() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 5 -H 'Accept: text/html' "$1"
	else
		wget -q -T 5 -O - --header 'Accept: text/html' "$1"
	fi
}

# connected_now counts the servers connected now, by a control plane's log:
# each server's last "server connected" or "server disconnected" line says
# whether it is. The API would say so too, but it needs a session, and a
# session needs a passkey, which a script doesn't have.
connected_now() {
	awk '
		function server(s) { s = $0; sub(/.*"server":"/, "", s); sub(/".*/, "", s); return s }
		/"msg":"server connected"/ { up[server()] = 1 }
		/"msg":"server disconnected"/ { up[server()] = 0 }
		END { n = 0; for (s in up) n += up[s]; print n }
	' "$1"
}

# run_install runs the install script from a release tree, piped to sh, and
# keeps what it printed in $log/out.
run_install() {
	tree=$1
	shift
	# shellcheck disable=SC2002 # piped on purpose, as curl pipes it
	if cat "$INSTALL" | DOCKWELL_RELEASES_URL="$RELEASES/$tree" DOCKWELL_VERIFY_KEY="$KEY" sh -s -- "$@" >"$log/out" 2>&1; then
		status=0
	else
		status=$?
	fi
	cat "$log/out"
	return "$status"
}

said() {
	grep -qF -- "$1" "$log/out" || die "the install script didn't say \"$1\""
}

version_is() {
	got=$("$BIN" version | sed -n 's/^dockwell  *//p')
	[ "$got" = "v$1" ] || die "$BIN is $got, not v$1"
}

server_id() {
	sed -n 's/.*"server_id": *"\([^"]*\)".*/\1/p' "$DATA/agent/agent.json"
}

pid() {
	case $mode in
	systemd) systemctl show -p MainPID --value dockwell ;;
	openrc)
		# The installed binary's process, not the control plane it joins.
		for p in $(pidof dockwell); do
			case $(readlink "/proc/$p/exe") in "$BIN"*) echo "$p" ;; esac
		done
		;;
	direct) cat "$log/pid" ;;
	esac
}

# start_direct runs the binary as the unit would, when no init system can.
start_direct() {
	[ "$mode" = direct ] || return 0
	envs=$(sed -n 's/^Environment=//p' "$UNIT")
	cmd=$(sed -n 's/^ExecStart=//p' "$UNIT")
	[ -n "$cmd" ] || die "$UNIT has no ExecStart"
	# shellcheck disable=SC2086 # split as systemd splits them
	env $envs $cmd >"$log/dockwell.log" 2>&1 &
	echo $! >"$log/pid"
}

stop_direct() {
	[ "$mode" = direct ] || return 0
	kill "$(cat "$log/pid")" 2>/dev/null || true
	wait "$(cat "$log/pid")" 2>/dev/null || true
}

wait_panel() {
	i=0
	until get "$PANEL/healthz" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 100 ] || die "the panel didn't answer on $PANEL within 10 s"
		sleep 0.1
	done
	get_html "$PANEL/" | grep -q '<div id="root"></div>' || die "$PANEL/ doesn't serve the panel"
	ok "the panel answers on $PANEL"
}

running() {
	case $mode in
	systemd) systemctl is-active --quiet dockwell ;;
	openrc) rc-service dockwell status >/dev/null 2>&1 ;;
	direct) kill -0 "$(cat "$log/pid")" 2>/dev/null ;;
	esac
}

check_service() {
	if [ -f "$UNIT" ]; then
		grep -qx "Environment=$LIMIT" "$UNIT" || die "$UNIT doesn't set $LIMIT"
		[ -L /etc/systemd/system/multi-user.target.wants/dockwell.service ] || die "dockwell.service isn't enabled"
		if command -v systemd-analyze >/dev/null 2>&1; then
			systemd-analyze verify "$UNIT" || die "systemd-analyze verify found problems in $UNIT"
			ok "systemd-analyze verify accepts $UNIT"
		fi
	elif [ -f "$INITD" ]; then
		grep -q "GOMEMLIMIT:=$MEMORY_LIMIT" "$INITD" || die "$INITD doesn't set $LIMIT"
		[ -L /etc/runlevels/default/dockwell ] || die "dockwell isn't in the default runlevel"
	else
		die "no service was installed"
	fi
	if [ "$mode" != direct ]; then
		running || die "the service isn't running"
		tr '\0' '\n' <"/proc/$(pid)/environ" | grep -qx "$LIMIT" || die "the running service doesn't have $LIMIT"
		ok "the $mode service runs with $LIMIT"
	fi
}

step "Help and usage errors"
sh "$INSTALL" --help | grep -q '^Usage:' || die "--help doesn't print the usage"
if sh "$INSTALL" --frobnicate >"$log/out" 2>&1; then die "an unknown option succeeded"; fi
said "There's no option --frobnicate"
ok "--help and unknown options"

step "Install v$OLD on a first server ($mode)"
run_install good --version "$OLD" || die "installing v$OLD failed"
said "The release signature checks out"
said "Installed $BIN."
version_is "$OLD"
start_direct
check_service
wait_panel
id=$(server_id)
[ -n "$id" ] || die "the control server has no agent identity"
ok "this server is $id"
[ -z "${COSIGN:-}" ] || export DOCKWELL_COSIGN="$COSIGN"

step "Refuse an archive that doesn't match its checksum"
if run_install bad-archive; then die "a tampered archive was installed"; fi
said "doesn't match its signed checksum, so nothing was installed"
version_is "$OLD"
[ "$mode" = direct ] || running || die "a refused install stopped the service"
ok "refused, and v$OLD is still installed"

step "Refuse checksums that don't match their signature"
if run_install bad-signature; then die "checksums with a bad signature were installed"; fi
said "doesn't verify, so nothing was installed"
version_is "$OLD"
ok "refused, and v$OLD is still installed"

step "Upgrade in place to v$NEW"
stop_direct
run_install good || die "upgrading to v$NEW failed"
said "Upgraded $BIN from v$OLD to v$NEW."
version_is "$NEW"
start_direct
check_service
wait_panel
[ "$(server_id)" = "$id" ] || die "the upgrade changed this server's identity"
ok "the upgrade kept the data in $DATA"

step "Run it again on the same version"
before=$(pid)
run_install good || die "running it again failed"
said "Dockwell v$NEW is already installed"
[ "$(pid)" = "$before" ] || die "running it again on the same version restarted the service"
ok "nothing to do, and the service kept running"

step "Uninstall"
stop_direct
run_install good --uninstall || die "uninstalling failed"
said "Removed Dockwell"
[ ! -e "$BIN" ] || die "$BIN is still there"
if [ -e "$UNIT" ] || [ -e "$INITD" ]; then die "the service is still there"; fi
[ ! -e /etc/systemd/system/multi-user.target.wants/dockwell.service ] || die "dockwell.service is still enabled"
[ ! -e /etc/runlevels/default/dockwell ] || die "dockwell is still in the default runlevel"
[ -e "$DATA/control/state.db" ] || die "uninstalling removed the data in $DATA"
if get "$PANEL/healthz" >/dev/null 2>&1; then die "the panel still answers after uninstalling"; fi
run_install good --uninstall || die "uninstalling twice failed"
said "isn't installed on this server"
ok "removed, and the data is still in $DATA"

step "Join a control plane and run only the agent"
rm -rf "$DATA"
fixture=$(mktemp -d)
get "$RELEASES/good/download/v$NEW/dockwell_${NEW}_linux_$arch.tar.gz" | tar -xzf - -C "$fixture" dockwell ||
	die "can't unpack v$NEW to run a control plane to join"
"$fixture/dockwell" run --data-dir "$fixture/data" --listen 127.0.0.1:7601 --agent-listen 127.0.0.1:7444 \
	--log-format json >"$log/fixture.log" 2>&1 &
fixture_pid=$!
i=0
until get "$FIXTURE/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 100 ] || die "the control plane to join didn't start"
	sleep 0.1
done
ticket=$("$fixture/dockwell" token create --data-dir "$fixture/data" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[ -n "$ticket" ] || die "dockwell token create printed no ticket"
run_install good --join "$ticket" || die "installing with a ticket failed"
said "Joined as"
start_direct
[ -e "$DATA/agent/agent.json" ] || die "joining left no identity in $DATA/agent"
[ ! -e "$DATA/control" ] || die "a server that joined has a control plane"
i=0
until [ "$(connected_now "$log/fixture.log")" -ge 2 ]; do
	i=$((i + 1))
	[ "$i" -lt 150 ] || die "the agent didn't connect to the control plane within 15 s"
	sleep 0.1
done
ok "the agent connected to the control plane"
if get "$PANEL/healthz" >/dev/null 2>&1; then die "a server that joined serves a panel"; fi
check_service
ok "it runs only the agent"
export DOCKWELL_JOIN="$ticket"
run_install good || die "running it again with the spent ticket failed"
unset DOCKWELL_JOIN
said "already belongs to the control plane at"
stop_direct
run_install good --uninstall || die "uninstalling the agent failed"
kill "$fixture_pid" 2>/dev/null || true
wait "$fixture_pid" 2>/dev/null || true
rm -rf "$fixture" "$DATA" "$log"

# shellcheck source=/dev/null
printf '\nAll install tests passed on %s (%s).\n' "$(. /etc/os-release && echo "$PRETTY_NAME")" "$mode"
