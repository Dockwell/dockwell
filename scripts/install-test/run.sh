#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# The install script's test scenario. It runs as root on the system under
# test, which is either a container (prepare.sh sets it up) or a VM:
#
#   1. A first server: install an older release, refuse a tampered archive and
#      a bad signature, upgrade in place to the full build, re-run, uninstall.
#   2. With MIRROR set, the same server through a mirror, with GitHub and
#      Sigstore cut off: install, refuse a changed cosign, archive and
#      signature on the mirror, upgrade from a mirror without a trust root,
#      uninstall. Then the script's own steps that find the pinned trust root
#      on a mirror, which the key the test releases are signed with never
#      needs: take the release's copy, fall back to the latest release's
#      however the mirror says a file is missing, refuse a changed one. Then
#      verify a keyless signature from Sigstore's public instance offline,
#      against the pinned trust root.
#   3. A server that joins: install with a ticket from a control plane that
#      runs beside it, check it gets the agent build, runs only the agent and
#      connects, uninstall.
#   4. A server that joined with the older release, which has only the full
#      build: it gets the full build, connects, keeps the full build when it
#      upgrades, and connects again.
#
# When systemd or OpenRC is running, the install script starts the service and
# this checks it. In a container where systemd is only installed, not running,
# this starts the binary the way the unit says, and checks the unit with
# systemd-analyze verify. The script is piped to sh, as people run it.
#
#   INSTALL=<install.sh> RELEASES=<url> KEY=<cosign.pub> OLD=<v> NEW=<v> run.sh
#
# RELEASES holds the trees releases.sh makes, and MIRROR, when set, the trees
# mirror.sh makes. BUCKET and MIRROR_FILE, when set, serve the same mirror the
# way a storage bucket does, answering 403 for a missing file, and as a
# file:// URL. COSIGN, when set, is a copy of the pinned cosign to use
# after the first install, which downloads its own. Cutting off GitHub and
# Sigstore means adding them to /etc/hosts for a while, so this changes it.
set -eu

: "${INSTALL:?}" "${RELEASES:?}" "${KEY:?}" "${OLD:?}" "${NEW:?}"
BIN=/usr/local/bin/dockwell
DATA=/var/lib/dockwell
UNIT=/etc/systemd/system/dockwell.service
INITD=/etc/init.d/dockwell
PANEL=http://127.0.0.1:7600
FIXTURE=http://127.0.0.1:7601
CONNECTED=$(dirname "$0")/../connected.awk
[ -r "$CONNECTED" ] || { echo "can't read $CONNECTED, which counts connected servers" >&2; exit 1; }
MIRROR=${MIRROR:-}
BUCKET=${BUCKET:-}
MIRROR_FILE=${MIRROR_FILE:-}
MEMORY_LIMIT=$(sed -n 's/^MEMORY_LIMIT=//p' "$INSTALL")
[ -n "$MEMORY_LIMIT" ] || { echo "can't read MEMORY_LIMIT from $INSTALL" >&2; exit 1; }
COSIGN_VERSION=$(sed -n 's/^COSIGN_VERSION=//p' "$INSTALL")
SIGSTORE_ROOT_SHA256=$(sed -n 's/^SIGSTORE_ROOT_SHA256=//p' "$INSTALL")
# What an install through a mirror must not need: GitHub, and Sigstore's
# public instance.
CUT_OFF="github.com objects.githubusercontent.com release-assets.githubusercontent.com raw.githubusercontent.com
tuf-repo-cdn.sigstore.dev sigstore-tuf-root.storage.googleapis.com rekor.sigstore.dev log2025-1.rekor.sigstore.dev
fulcio.sigstore.dev timestamp.sigstore.dev"
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
	reconnect
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

# connected_now counts the servers connected now, by a control plane's log.
connected_now() {
	awk -f "$CONNECTED" "$1"
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

# run_mirror runs the install script through the mirror, piped to sh, with
# Dockwell's releases from one of its trees and cosign from another.
run_mirror() {
	tree=$1
	cosign_tree=$2
	shift 2
	# shellcheck disable=SC2002 # piped on purpose, as curl pipes it
	if cat "$INSTALL" | DOCKWELL_COSIGN_MIRROR="$MIRROR/cosign/$cosign_tree" DOCKWELL_VERIFY_KEY="$KEY" \
		sh -s -- --mirror "$MIRROR/$tree" "$@" >"$log/out" 2>&1; then
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

# trust_root runs only the install script's steps that resolve a release on
# the mirror at $1 and find the pinned trust root there, as a keyless check
# does, and keeps what they said in $log/out. $2, when set, is the version.
trust_root() {
	sed '/^main "\$@"$/d' "$INSTALL" >"$log/install-steps.sh"
	if (
		# shellcheck source=/dev/null
		. "$log/install-steps.sh"
		# The settings the install script's main and detect would make.
		# shellcheck disable=SC2034
		family=other mirror=$1 version=${2:-} build=full
		tmp="$log/trust-root"
		rm -rf "$tmp" && mkdir -p "$tmp"
		check_tools
		resolve
		get_trust_root
		[ "$(sha256 "$tmp/trusted_root.json")" = "$SIGSTORE_ROOT_SHA256" ] || fail "get_trust_root kept a root that isn't the pinned one"
	) >"$log/out" 2>&1; then
		status=0
	else
		status=$?
	fi
	cat "$log/out"
	return "$status"
}

# cut_off makes GitHub and Sigstore unreachable from here until reconnect.
cut_off() {
	cp /etc/hosts "$log/hosts"
	for host in $CUT_OFF; do
		printf '127.0.0.1 %s\n::1 %s\n' "$host" "$host" >>/etc/hosts
	done
}

reconnect() {
	[ -f "$log/hosts" ] || return 0
	# Written over, not moved: in a container, /etc/hosts is a mount.
	cat "$log/hosts" >/etc/hosts
	rm -f "$log/hosts"
}

# keyless checks the keyless fixture from the mirror with the pinned cosign,
# as install.sh checks a release, and keeps what cosign said in $log/out.
keyless() {
	TUF_ROOT="$log/tuf" HOME="$log" "$pinned_cosign" verify-blob --bundle "$log/keyless.sigstore.json" \
		--certificate-oidc-issuer https://accounts.google.com "$@" "$log/keyless.txt" >"$log/out" 2>&1 </dev/null
}

version_is() {
	got=$("$BIN" version | sed -n 's/^dockwell  *//p')
	[ "$got" = "v$1" ] || die "$BIN is $got, not v$1"
}

# build_is checks which build of dockwell is installed, as install.sh reads
# it: agent, full, or none, as a release from before there were two builds
# says.
build_is() {
	json=$("$BIN" version --json)
	case $1 in
	none) got=$(printf '%s' "$json" | grep -c '"edition"' || true) && [ "$got" = 0 ] ;;
	*) printf '%s' "$json" | grep -q "\"edition\": *\"BUILD_EDITION_$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')\"" ;;
	esac || die "$BIN isn't the $1 build: $json"
}

# connections counts the times server $2 connected, by a control plane's log $1.
connections() {
	grep '"msg":"server connected"' "$1" | grep -c "\"server\":\"$2\"" || true
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
said "Installing the full build of Dockwell v$OLD"
said "Installed $BIN."
version_is "$OLD"
build_is none
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
build_is full
ok "a control server from before the agent build keeps the full build"
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

if [ -n "$MIRROR" ]; then
	step "Install through a mirror, with GitHub and Sigstore cut off"
	# From here on cosign comes from the mirror.
	pinned_cosign=${DOCKWELL_COSIGN:-}
	unset DOCKWELL_COSIGN
	cut_off
	# Put /etc/hosts back however this ends, not only through die.
	trap reconnect EXIT
	trap 'exit 1' HUP INT TERM
	if run_install good; then die "an install that downloads cosign from GitHub worked with GitHub cut off"; fi
	said "Can't download cosign $COSIGN_VERSION from GitHub"
	ok "GitHub is cut off"
	run_mirror good good --version "$OLD" || die "installing v$OLD through the mirror failed"
	said "Downloading cosign $COSIGN_VERSION from the mirror at $MIRROR/cosign/good"
	said "The release signature checks out"
	said "Installed $BIN."
	version_is "$OLD"
	start_direct
	check_service
	wait_panel
	[ "$(server_id)" = "$id" ] || die "installing through the mirror changed this server's identity"
	ok "installed v$OLD through the mirror"

	step "Refuse what the mirror changed"
	if run_mirror good bad; then die "a cosign that doesn't match its pin was used"; fi
	said "doesn't match its pinned SHA-256, so nothing was installed"
	if run_mirror bad-archive good; then die "a tampered archive from the mirror was installed"; fi
	said "doesn't match its signed checksum, so nothing was installed"
	if run_mirror bad-signature good; then die "checksums with a bad signature from the mirror were installed"; fi
	said "doesn't verify, so nothing was installed"
	version_is "$OLD"
	if run_mirror good good --version 9.9.9; then die "a version the mirror doesn't have was installed"; fi
	said "Pick a version the mirror has with --version"
	ok "refused a changed cosign, archive and signature, and a version the mirror lacks, and v$OLD is still installed"

	step "Upgrade to v$NEW through a mirror that carries no trust root"
	# The releases here are signed with a key, which needs no trust root.
	stop_direct
	run_mirror no-root good || die "upgrading to v$NEW through a mirror without a trust root failed"
	said "Upgraded $BIN from v$OLD to v$NEW."
	version_is "$NEW"
	start_direct
	check_service
	wait_panel
	ok "upgraded through the mirror"

	step "Find the pinned trust root on the mirror"
	for base in "$MIRROR" "$BUCKET" "$MIRROR_FILE"; do
		[ -n "$base" ] || continue
		case $base in
		file:*)
			if ! command -v curl >/dev/null 2>&1 || ! curl -V | grep -q '^Protocols:.* file'; then
				ok "skipped $base, because this downloader can't read file:// URLs"
				continue
			fi
			;;
		esac
		trust_root "$base/good" || die "the trust root of the latest release wasn't found at $base/good"
		trust_root "$base/old-root" "$OLD" ||
			die "v$OLD has no trust root at $base/old-root, and the latest release's wasn't used"
		if trust_root "$base/no-root" "$NEW"; then die "a release without a trust root passed at $base/no-root"; fi
		said "Can't download sigstore_trusted_root.json from the mirror at $base/no-root"
		said "download/v$NEW/sigstore_trusted_root.json:"
		said "latest/download/sigstore_trusted_root.json:"
		ok "at $base, a release without its own trust root takes the latest release's, and a mirror with neither is refused"
	done
	if trust_root "$MIRROR/bad-root"; then die "a trust root that doesn't match its pin was used"; fi
	said "The Sigstore trust root on the mirror doesn't match its pinned SHA-256"
	trust_root "$MIRROR/latest-root" "$NEW" ||
		die "v$NEW's trust root doesn't match its pin, and the latest release's wasn't used"
	if trust_root "$MIRROR/latest-root"; then
		die "installing the latest release, a second copy of its trust root was fetched from latest/download"
	fi
	said "The Sigstore trust root on the mirror doesn't match its pinned SHA-256"
	ok "a changed trust root is refused, and one that predates the pin falls back to the latest release's, once"

	step "Verify a keyless signature with Sigstore cut off"
	if [ -z "$pinned_cosign" ]; then
		ok "skipped, because COSIGN doesn't name a copy of the pinned cosign"
	else
		get "$MIRROR/good/latest/download/sigstore_trusted_root.json" >"$log/root.json" ||
			die "can't download the trust root from the mirror"
		[ "$(sha256sum "$log/root.json" | cut -d ' ' -f 1)" = "$SIGSTORE_ROOT_SHA256" ] ||
			die "the mirror's trust root isn't the one install.sh pins"
		get "$MIRROR/keyless/cosign_checksums.txt" >"$log/keyless.txt" || die "can't download the keyless fixture"
		get "$MIRROR/keyless/cosign_checksums.txt.sigstore.json" >"$log/keyless.sigstore.json" ||
			die "can't download the keyless fixture's bundle"
		if keyless --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com; then
			die "cosign reached Sigstore's TUF repository, so Sigstore isn't cut off"
		fi
		said "tuf-repo-cdn.sigstore.dev"
		ok "without the pinned trust root, cosign can't reach Sigstore"
		keyless --certificate-identity keyless@projectsigstore.iam.gserviceaccount.com --trusted-root "$log/root.json" ||
			die "a keyless signature didn't verify offline against the pinned trust root: $(cat "$log/out")"
		if keyless --certificate-identity someone@example.com --trusted-root "$log/root.json"; then
			die "a keyless signature verified offline for the wrong identity"
		fi
		ok "verified a keyless signature offline against the pinned trust root, and refused the wrong identity"
	fi
	stop_direct
	run_install good --uninstall || die "uninstalling after the mirror install failed"
	reconnect
	trap - EXIT HUP INT TERM
	[ -z "$pinned_cosign" ] || export DOCKWELL_COSIGN="$pinned_cosign"
fi

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
said "Installing the agent build of Dockwell v$NEW"
said "Joined as"
build_is agent
ok "a server that joins gets the agent build"
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
grep '"msg":"server connected"' "$log/fixture.log" | grep "\"server\":\"$(server_id)\"" | grep -q '"build":"agent"' ||
	die "the control plane didn't see the agent build in the agent's hello"
if "$BIN" token create >"$log/out" 2>&1; then die "the agent build made a join ticket"; fi
said "isn't in the agent build of dockwell"
ok "it runs only the agent, with the agent build"
export DOCKWELL_JOIN="$ticket"
run_install good || die "running it again with the spent ticket failed"
unset DOCKWELL_JOIN
said "is already installed on this server, as the agent build"
said "already belongs to the control plane at"
stop_direct
run_install good --uninstall || die "uninstalling the agent failed"

step "A server that joined with v$OLD keeps the full build"
# v$OLD stands in for a release from before the agent build: it has only the
# full build, which runs the agent too.
rm -rf "$DATA"
ticket=$("$fixture/dockwell" token create --data-dir "$fixture/data" | awk '$1 == "dockwell" && $2 == "join" { print $3 }')
[ -n "$ticket" ] || die "dockwell token create printed no ticket"
run_install good --version "$OLD" --join "$ticket" || die "installing v$OLD with a ticket failed"
said "Dockwell v$OLD has no agent build, so this server gets the full build"
said "Joined as"
version_is "$OLD"
build_is none
start_direct
joined=$(server_id)
[ -n "$joined" ] || die "joining left no identity in $DATA/agent"
i=0
until [ "$(connections "$log/fixture.log" "$joined")" -ge 1 ]; do
	i=$((i + 1))
	[ "$i" -lt 150 ] || die "the agent of v$OLD didn't connect to the control plane within 15 s"
	sleep 0.1
done
ok "v$OLD's full build joined and connected"
stop_direct
run_install good || die "upgrading the server that joined with v$OLD failed"
said "Upgraded $BIN from v$OLD to v$NEW."
version_is "$NEW"
build_is full
[ "$(server_id)" = "$joined" ] || die "the upgrade changed this server's identity"
start_direct
i=0
until [ "$(connections "$log/fixture.log" "$joined")" -ge 2 ]; do
	i=$((i + 1))
	[ "$i" -lt 150 ] || die "the upgraded agent didn't connect to the control plane again within 15 s"
	sleep 0.1
done
if get "$PANEL/healthz" >/dev/null 2>&1; then die "a server that joined serves a panel after its upgrade"; fi
check_service
grep '"msg":"server connected"' "$log/fixture.log" | grep "\"server\":\"$joined\"" | tail -n 1 | grep -q '"build":"full"' ||
	die "the control plane didn't see the full build in the upgraded agent's hello"
ok "the upgrade kept the full build, and the agent connected again"
stop_direct
run_install good --uninstall || die "uninstalling the server that joined with v$OLD failed"
kill "$fixture_pid" 2>/dev/null || true
wait "$fixture_pid" 2>/dev/null || true
rm -rf "$fixture" "$DATA" "$log"

# shellcheck source=/dev/null
printf '\nAll install tests passed on %s (%s).\n' "$(. /etc/os-release && echo "$PRETTY_NAME")" "$mode"
