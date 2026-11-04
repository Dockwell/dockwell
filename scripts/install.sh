#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Installs Dockwell on a Linux server. On the first server it runs the control
# plane and an agent:
#
#   curl -fsSL https://get.dockwell.dev | sh
#
# To add another server, make a ticket on the control server with
# `dockwell token create`, then run this on the new server. It joins and runs
# only the agent:
#
#   curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>
#
# Running it again upgrades in place. `--uninstall` removes Dockwell and leaves
# your workloads and data where they are. Run it with --help for the options,
# and see docs/install.md.
#
# Nothing is installed until the release checks out: a pinned cosign verifies
# that Dockwell's release workflow signed the checksums, and the archive must
# match them (docs/adr/0010-install-script-verifies-with-pinned-cosign.md).
#
# Everything runs from main, on the last line, so a download cut short runs
# nothing.

set -eu

# Where releases come from, and who must have signed them.
REPO_URL=https://github.com/Dockwell/dockwell
SIGNER_IDENTITY="$REPO_URL/.github/workflows/release.yml@refs/tags/v"
SIGNER_ISSUER=https://token.actions.githubusercontent.com

# The cosign that verifies releases: the same version that signs them in
# .github/workflows/release.yml, pinned by the SHA-256 of each binary. Update
# all three together, from cosign's verified cosign_checksums.txt.
COSIGN_VERSION=v3.1.3
COSIGN_SHA256_AMD64=4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71
COSIGN_SHA256_ARM64=c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a

# The Go runtime's soft memory limit for the service. The control plane and its
# agent have a 250 MB RSS budget (docs/product/prd.md); 200 MiB (210 MB) leaves
# the rest for what the Go runtime doesn't manage, such as the binary's own
# pages and SQLite's cache.
MEMORY_LIMIT=200MiB

BIN=/usr/local/bin/dockwell
DATA=/var/lib/dockwell
UNIT=/etc/systemd/system/dockwell.service
INITD=/etc/init.d/dockwell
OPENRC_LOG=/var/log/dockwell.log
PANEL=http://127.0.0.1:7600

usage() {
	cat <<'EOF'
Installs Dockwell on this server, or upgrades it in place.

Usage:
  curl -fsSL https://get.dockwell.dev | sh -s -- [options]
  sh install.sh [options]

Options:
  --join <ticket>      add this server to a control plane, with a ticket from
                       dockwell token create; it runs only the agent (env DOCKWELL_JOIN)
  --version <version>  install that release, such as 0.1.0, instead of the
                       latest (env DOCKWELL_VERSION)
  --uninstall          remove Dockwell; your workloads and data stay
  -h, --help           show this help

Without --join, this server becomes a control server: it runs the control
plane and its own agent. Run as root. See
https://github.com/Dockwell/dockwell/blob/main/docs/install.md
EOF
}

say() {
	printf '%s\n' "$*"
}

fail() {
	printf 'Error: %s\n' "$*" >&2
	exit 1
}

has() {
	command -v "$1" >/dev/null 2>&1
}

# sha256 prints the SHA-256 of a file.
sha256() {
	sha256sum "$1" | cut -d ' ' -f 1
}

# fetch downloads a URL to a file. Redirects must stay on HTTPS.
fetch() {
	if [ "$downloader" = curl ]; then
		curl -fsSL --retry 3 --connect-timeout 20 --proto-redir =https -o "$2" "$1"
	else
		wget -q -T 20 -O "$2" "$1"
	fi
}

# not_found is true when fetch's exit status means the server answered that the
# file isn't there, rather than that it couldn't be reached.
not_found() {
	case $downloader:$1 in
	curl:22 | wget:8) return 0 ;;
	esac
	return 1
}

# answers is true when a URL answers with success.
answers() {
	if [ "$downloader" = curl ]; then
		curl -fsS -o /dev/null --max-time 2 "$1" 2>/dev/null
	else
		wget -q -T 2 -O /dev/null "$1" 2>/dev/null
	fi
}

nap() {
	sleep 0.2 2>/dev/null || sleep 1
}

# install_hint names the command that installs packages on this OS family.
install_hint() {
	case $family in
	debian) say "apt-get install -y $*" ;;
	rhel) say "dnf install -y $*" ;;
	alpine) say "apk add $*" ;;
	*) say "your package manager" ;;
	esac
}

detect() {
	[ "$(uname -s)" = Linux ] || fail "This system is $(uname -s). Dockwell runs on Linux servers."
	[ "$(id -u)" -eq 0 ] || fail "Installing Dockwell needs root. Run it again as root, or with sudo: curl -fsSL https://get.dockwell.dev | sudo sh"

	machine=$(uname -m)
	case $machine in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "This server's CPU is $machine. Dockwell runs on amd64 and arm64." ;;
	esac

	os_id=
	os_like=
	os_name="Linux"
	# os-release is shell syntax by design; read it in a subshell.
	# shellcheck source=/dev/null
	if [ -r /etc/os-release ]; then
		os_id=$(. /etc/os-release && printf '%s' "${ID:-}")
		os_like=$(. /etc/os-release && printf '%s' "${ID_LIKE:-}")
		os_name=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-${NAME:-Linux}}")
	fi
	case " $os_id $os_like " in
	*" alpine "*) family=alpine ;;
	*" debian "* | *" ubuntu "*) family=debian ;;
	*" rhel "* | *" centos "* | *" fedora "*) family=rhel ;;
	*) family=other ;;
	esac

	if [ "$family" = alpine ] || { [ -x /sbin/openrc-run ] && ! has systemctl; }; then
		init=openrc
		[ -x /sbin/openrc-run ] || fail "OpenRC isn't installed, so Dockwell can't run as a service. Install it with $(install_hint openrc), then run this again."
	elif has systemctl; then
		init=systemd
	else
		fail "This server has neither systemd nor OpenRC, so Dockwell can't run as a service. Dockwell supports Debian, Ubuntu, the RHEL family and Alpine."
	fi
	# Whether the init system is running, as opposed to installed in a
	# container or chroot that it didn't boot.
	booted=
	if [ "$init" = systemd ] && [ -d /run/systemd/system ]; then
		booted=1
	elif [ "$init" = openrc ] && [ -e /run/openrc/softlevel ]; then
		booted=1
	fi
}

check_tools() {
	missing=
	for tool in tar gzip sha256sum cut awk mktemp; do
		has "$tool" || missing="$missing $tool"
	done
	has curl || has wget || missing="$missing curl"
	[ -z "$missing" ] || fail "This server lacks${missing}, which the install needs. Install it with $(install_hint "${missing# }"), then run this again."
	if has curl; then downloader=curl; else downloader=wget; fi
}

# installed_version prints the version of the installed binary, without its v.
installed_version() {
	[ -x "$BIN" ] || return 0
	"$BIN" version 2>/dev/null </dev/null | sed -n 's/^dockwell  *v\{0,1\}//p'
}

valid_version() {
	case $1 in
	'' | *[!0-9A-Za-z.+-]*) return 1 ;;
	esac
}

# resolve sets version to the release to install, and downloads its checksums.
resolve() {
	releases=${DOCKWELL_RELEASES_URL:-$REPO_URL/releases}
	releases=${releases%/}
	version=${version#v}
	code=0
	if [ -z "$version" ] || [ "$version" = latest ]; then
		fetch "$releases/latest/download/checksums.txt" "$tmp/checksums.txt" || code=$?
		if not_found "$code"; then
			fail "There's no Dockwell release at $releases yet, so there's nothing to install. Run this again once one is published, or pick a version with --version."
		elif [ "$code" -ne 0 ]; then
			fail "Can't download the latest release's checksums from $releases. Check this server can reach it, then run this again."
		fi
		version=$(sed -n "s/^[0-9a-f]\{64\}  dockwell_\(.*\)_linux_${arch}\.tar\.gz\$/\1/p" "$tmp/checksums.txt" | head -n 1)
		valid_version "$version" ||
			fail "The latest release at $releases has no linux/$arch archive. Install a version that has one with --version."
	else
		valid_version "$version" || fail "\"$version\" isn't a version. Pass one such as 0.1.0."
		fetch "$releases/download/v$version/checksums.txt" "$tmp/checksums.txt" || code=$?
		if not_found "$code"; then
			fail "There's no Dockwell v$version at $releases. Pick a version from $REPO_URL/releases."
		elif [ "$code" -ne 0 ]; then
			fail "Can't download the checksums of Dockwell v$version from $releases. Check this server can reach it, then run this again."
		fi
	fi
	download="$releases/download/v$version"
	archive="dockwell_${version}_linux_${arch}.tar.gz"
}

# get_cosign puts a cosign that matches the pinned SHA-256 at $cosign.
get_cosign() {
	case $arch in
	amd64) want=$COSIGN_SHA256_AMD64 ;;
	arm64) want=$COSIGN_SHA256_ARM64 ;;
	esac
	if [ -n "${DOCKWELL_COSIGN:-}" ]; then
		cosign=$DOCKWELL_COSIGN
		[ -f "$cosign" ] || fail "DOCKWELL_COSIGN is $cosign, which doesn't exist. Point it at cosign $COSIGN_VERSION for linux/$arch, or unset it to download that."
		[ "$(sha256 "$cosign")" = "$want" ] ||
			fail "DOCKWELL_COSIGN is $cosign, which isn't cosign $COSIGN_VERSION for linux/$arch, so it can't be trusted to check the release. Point it at that version, or unset it to download it."
		return 0
	fi
	say "Downloading cosign $COSIGN_VERSION to check the release signature."
	cosign="$tmp/cosign"
	fetch "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-linux-$arch" "$cosign" ||
		fail "Can't download cosign from GitHub, so the release can't be checked. Check this server can reach github.com, then run this again."
	[ "$(sha256 "$cosign")" = "$want" ] ||
		fail "The cosign download doesn't match its pinned SHA-256, so nothing was installed. Run this again; if it keeps failing, report it at $REPO_URL/security."
	chmod 0755 "$cosign"
}

# verify checks the checksums' signature, then the archive against them, and
# leaves the binary at $tmp/dockwell. Nothing outside $tmp changes before it.
verify() {
	get_cosign
	fetch "$download/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
		fail "Can't download the signature of Dockwell v$version from $download, so nothing was installed. Check this server can reach it, then run this again."
	if [ -n "${DOCKWELL_VERIFY_KEY:-}" ]; then
		say "Checking the release against the key in DOCKWELL_VERIFY_KEY, instead of Dockwell's release signature."
		set -- --key "$DOCKWELL_VERIFY_KEY" --insecure-ignore-tlog=true
		signer="the key in $DOCKWELL_VERIFY_KEY"
	else
		set -- --certificate-identity "$SIGNER_IDENTITY$version" --certificate-oidc-issuer "$SIGNER_ISSUER"
		signer="Dockwell's release workflow at tag v$version"
	fi
	# cosign keeps Sigstore's trust root in TUF_ROOT; keep it in $tmp too.
	if ! TUF_ROOT="$tmp/tuf" HOME="$tmp" "$cosign" verify-blob "$@" \
		--bundle "$tmp/checksums.txt.sigstore.json" "$tmp/checksums.txt" >"$tmp/cosign.log" 2>&1 </dev/null; then
		cat "$tmp/cosign.log" >&2
		fail "The signature on the checksums of Dockwell v$version doesn't verify, so nothing was installed. If it says it can't reach Sigstore, check this server can reach tuf-repo-cdn.sigstore.dev and run this again; otherwise report it at $REPO_URL/security."
	fi
	say "The release signature checks out: $signer signed it."

	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "The signed checksums of Dockwell v$version don't list $archive. That release has no linux/$arch build; pick another with --version."
	fetch "$download/$archive" "$tmp/$archive" ||
		fail "Can't download $archive from $download. Check this server can reach it, then run this again."
	[ "$(sha256 "$tmp/$archive")" = "$want" ] ||
		fail "$archive doesn't match its signed checksum, so nothing was installed. Run this again; if it keeps failing, report it at $REPO_URL/security."
	tar -xzf "$tmp/$archive" -C "$tmp" dockwell ||
		fail "Can't unpack dockwell from $archive. Check there is free space in ${TMPDIR:-/var/tmp}, then run this again."
}

# put writes stdin to a file, and records whether that changed it.
put() {
	cat >"$tmp/put"
	if [ -f "$1" ] && [ "$(sha256 "$tmp/put")" = "$(sha256 "$1")" ]; then
		rm -f "$tmp/put"
		return 0
	fi
	mkdir -p "$(dirname "$1")"
	# Copied, not moved, so the file takes its directory's SELinux label.
	cp "$tmp/put" "$1.new"
	chmod "$2" "$1.new"
	mv -f "$1.new" "$1"
	rm -f "$tmp/put"
	changed=1
}

write_service() {
	case $init in
	systemd)
		put "$UNIT" 0644 <<EOF
# Installed by the Dockwell install script, which replaces it on upgrade.
# Change settings in a drop-in instead: systemctl edit dockwell
[Unit]
Description=Dockwell
Documentation=$REPO_URL/blob/main/docs/install.md
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=$BIN run
# A soft limit sized from the 250 MB budget for the control plane and its agent.
Environment=GOMEMLIMIT=$MEMORY_LIMIT
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
		;;
	openrc)
		put "$INITD" 0755 <<EOF
#!/sbin/openrc-run
# Installed by the Dockwell install script, which replaces it on upgrade.
# Change settings in /etc/conf.d/dockwell instead.

description="Dockwell"
command="$BIN"
command_args="run"
supervisor=supervise-daemon
respawn_delay=5
respawn_max=0
output_log="$OPENRC_LOG"
error_log="$OPENRC_LOG"

# A soft limit sized from the 250 MB budget for the control plane and its agent.
: "\${GOMEMLIMIT:=$MEMORY_LIMIT}"
export GOMEMLIMIT

depend() {
	use net
	after firewall
}
EOF
		;;
	esac
	if has restorecon; then
		restorecon "$BIN" "$UNIT" "$INITD" >/dev/null 2>&1 || true
	fi
}

# join spends the ticket, before the service ever starts: started without an
# identity, dockwell run would make this server a control server instead.
join() {
	[ -n "$ticket" ] || return 0
	if [ -e "$DATA/control/state.db" ]; then
		fail "This server runs a control plane, so it can't join another one. Run this without --join to upgrade it, or join a different server."
	fi
	if [ -e "$DATA/agent/agent.json" ]; then
		say "This server already joined a control plane, so the ticket isn't used."
		return 0
	fi
	if ! "$BIN" join "$ticket" >"$tmp/join.log" 2>&1 </dev/null; then
		cat "$tmp/join.log" >&2
		fail "This server didn't join, so Dockwell isn't started. Fix what dockwell join says above, then run this again; the ticket still works unless it expired."
	fi
	sed -n 1p "$tmp/join.log"
}

# role is control when dockwell run will start the control plane here, as it
# decides: when there's a control plane already, or no identity from a join.
role() {
	if [ -e "$DATA/control/state.db" ] || [ ! -e "$DATA/agent/agent.json" ]; then
		echo control
	else
		echo agent
	fi
}

logs_hint() {
	case $init in
	systemd) say "journalctl -u dockwell -n 50" ;;
	openrc) say "tail -n 50 $OPENRC_LOG" ;;
	esac
}

# start enables the service so it starts at boot. When the init system is
# running, it also starts the service, or restarts it when anything changed,
# and waits until it is up.
start() {
	case $init in
	systemd)
		if [ -z "$booted" ]; then
			# Offline, systemctl only makes the links that start it at boot.
			systemctl enable --quiet dockwell 2>/dev/null || true
			return 0
		fi
		systemctl daemon-reload || fail "systemctl daemon-reload failed, so the new service isn't loaded. Fix what it says above, then run this again."
		systemctl enable --quiet dockwell || fail "Can't enable dockwell.service. Fix what systemctl says above, then run this again."
		if [ -n "$changed" ]; then
			systemctl restart dockwell
		else
			systemctl start dockwell
		fi || fail "dockwell.service didn't start. See what it logged with: $(logs_hint)"
		;;
	openrc)
		rc-update add dockwell default >/dev/null 2>&1 || fail "Can't add dockwell to the default runlevel. Fix what rc-update says, then run this again."
		[ -n "$booted" ] || return 0
		if ! rc-service dockwell status >/dev/null 2>&1; then
			rc-service dockwell start >/dev/null
		elif [ -n "$changed" ]; then
			rc-service dockwell restart >/dev/null
		fi || fail "The dockwell service didn't start. See what it logged with: $(logs_hint)"
		;;
	esac
	started=1

	if [ "$(role)" = control ]; then
		i=0
		until answers "$PANEL/healthz"; do
			i=$((i + 1))
			[ "$i" -lt 150 ] || fail "Dockwell started, but its panel didn't answer on $PANEL within 30 s. See what it logged with: $(logs_hint)"
			nap
		done
	else
		sleep 2
		case $init in
		systemd) systemctl is-active --quiet dockwell ;;
		openrc) rc-service dockwell status >/dev/null 2>&1 ;;
		esac || fail "Dockwell started, but its agent stopped again. See what it logged with: $(logs_hint)"
	fi
}

install_dockwell() {
	check_tools
	tmp=$(mktemp -d "${TMPDIR:-/var/tmp}/dockwell-install.XXXXXX" 2>/dev/null || mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	resolve
	current=$(installed_version)
	changed=
	if [ "$current" = "$version" ]; then
		say "Dockwell v$version is already installed on this server."
	else
		say "Installing Dockwell v$version on $os_name, linux/$arch."
		[ "$family" != other ] ||
			say "Dockwell supports Debian, Ubuntu, the RHEL family and Alpine; this system isn't one of them, so it may not work."
		verify
		mkdir -p "$(dirname "$BIN")"
		# Copied next to the old binary and renamed over it, so a running
		# service keeps its file until it restarts, and SELinux labels it.
		cp "$tmp/dockwell" "$BIN.new"
		chmod 0755 "$BIN.new"
		mv -f "$BIN.new" "$BIN"
		changed=1
		if [ -n "$current" ]; then
			say "Upgraded $BIN from v$current to v$version."
		else
			say "Installed $BIN."
		fi
	fi

	join
	write_service
	case $init in
	systemd) service="The systemd service dockwell.service" ;;
	openrc) service="The OpenRC service dockwell" ;;
	esac

	started=
	start
	if [ -z "$started" ]; then
		say "$service is installed, with GOMEMLIMIT=$MEMORY_LIMIT."
		say "$init isn't running here, as in a container, so Dockwell isn't started. It starts at the next boot; to start it now without $init, run: $BIN run"
		return 0
	fi
	say "$service is running, with GOMEMLIMIT=$MEMORY_LIMIT."
	say ""
	if [ "$(role)" = control ]; then
		cat <<EOF
Dockwell v$version is running, and this is your control server. Its panel
listens on $PANEL, so only this server can reach it. To open it from
your computer, forward the port over SSH, then open that address there:

  ssh -L 7600:127.0.0.1:7600 <user>@<this server>

To add a server, run dockwell token create here and follow what it prints.
Other servers reach this one on TCP port 7443, so open it to them.
EOF
	else
		control_url=$(sed -n 's/.*"control_url": *"\([^"]*\)".*/\1/p' "$DATA/agent/agent.json" | head -n 1)
		say "Dockwell v$version is running on this server as an agent of ${control_url:-its control server}. It appears on the servers page of your panel."
	fi
}

uninstall() {
	found=
	if [ -f "$UNIT" ]; then
		found=1
		if [ -n "$booted" ] && [ "$init" = systemd ]; then
			systemctl disable --now --quiet dockwell 2>/dev/null || true
		else
			systemctl disable --quiet dockwell 2>/dev/null || true
		fi
		rm -f "$UNIT"
		[ -z "$booted" ] || [ "$init" != systemd ] || systemctl daemon-reload
	fi
	if [ -f "$INITD" ]; then
		found=1
		if [ -n "$booted" ] && [ "$init" = openrc ]; then
			rc-service dockwell stop >/dev/null 2>&1 || true
		fi
		rc-update del dockwell default >/dev/null 2>&1 || true
		rm -f "$INITD"
	fi
	if [ -e "$BIN" ]; then
		found=1
		rm -f "$BIN"
	fi
	if [ -z "$found" ]; then
		say "Dockwell isn't installed on this server, so there's nothing to remove."
		return 0
	fi
	say "Removed Dockwell: $BIN and its service. Your workloads keep running."
	if [ -d "$DATA" ]; then
		cat <<EOF
Its data is still in $DATA, so installing again picks up where it left off.
To delete that too, run: rm -rf $DATA
That removes this server's identity. On a control server it also removes the
record of every server that joined, so each of them would have to join again.
EOF
	fi
}

main() {
	ticket=${DOCKWELL_JOIN:-}
	version=${DOCKWELL_VERSION:-}
	action=install
	while [ $# -gt 0 ]; do
		case $1 in
		--join)
			[ $# -ge 2 ] || fail "--join needs a ticket. Make one on the control server with dockwell token create."
			ticket=$2
			shift 2
			;;
		--join=*)
			ticket=${1#--join=}
			shift
			;;
		--version)
			[ $# -ge 2 ] || fail "--version needs a version, such as 0.1.0."
			version=$2
			shift 2
			;;
		--version=*)
			version=${1#--version=}
			shift
			;;
		--uninstall)
			action=uninstall
			shift
			;;
		-h | --help)
			usage
			return 0
			;;
		*) fail "There's no option $1. Run this with --help to see the options." ;;
		esac
	done

	detect
	case $action in
	install) install_dockwell ;;
	uninstall) uninstall ;;
	esac
}

main "$@"
