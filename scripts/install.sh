#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Installs Dockwell on a Linux server. On the first server it runs the control
# plane and an agent, with the full build of dockwell:
#
#   curl -fsSL https://get.dockwell.dev | sh
#
# To add another server, make a ticket on the control server with
# `dockwell token create`, then run this on the new server. It joins and runs
# only the agent, with the agent build, which links none of the control plane
# (docs/adr/0023-a-full-build-and-an-agent-build.md):
#
#   curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>
#
# Running it again upgrades in place, and keeps the build the server has.
# `--uninstall` removes Dockwell and leaves your workloads and data where they
# are. Run it with --help for the options, and see docs/install.md.
#
# Nothing is installed until the release checks out: a pinned cosign verifies
# that Dockwell's release workflow signed the checksums, and the archive must
# match them (docs/adr/0010-install-script-verifies-with-pinned-cosign.md).
#
# Where GitHub or Sigstore is hard to reach, as from mainland China, --mirror
# downloads the release from a mirror instead, and checks it the same way
# against a copy of Sigstore's trust root pinned here, so the check doesn't
# reach Sigstore (docs/adr/0013-mirrors-for-releases-and-images.md).
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
# Where cosign comes from, unless --cosign-mirror names another.
COSIGN_RELEASES=https://github.com/sigstore/cosign/releases

# Sigstore's trust root, pinned by SHA-256, for checking a release from a mirror
# without reaching Sigstore. It is trusted_root.json from Sigstore's TUF
# repository, kept as scripts/sigstore_trusted_root.json and attached to every
# release. When Sigstore changes it, update both: see docs/releasing.md.
SIGSTORE_ROOT=sigstore_trusted_root.json
SIGSTORE_ROOT_SHA256=6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66

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

usage() {
	cat <<'EOF'
Installs Dockwell on this server, or upgrades it in place.

Usage:
  curl -fsSL https://get.dockwell.dev | sh -s -- [options]
  sh install.sh [options]

Options:
  --join <ticket>      add this server to a control plane, with a ticket from
                       dockwell token create; it runs only the agent, with the
                       agent build (env DOCKWELL_JOIN)
  --version <version>  install that release, such as 0.1.0, instead of the
                       latest (env DOCKWELL_VERSION)
  --mirror <url>       download Dockwell from a mirror of its GitHub releases, and
                       check it without reaching Sigstore (env DOCKWELL_MIRROR)
  --cosign-mirror <url>
                       download cosign from a mirror of its GitHub releases
                       (env DOCKWELL_COSIGN_MIRROR)
  --uninstall          remove Dockwell; your workloads and data stay
  -h, --help           show this help

Without --join, this server becomes a control server: it runs the control
plane and its own agent, with the full build. An upgrade keeps the build the
server has. Run as root. See
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

# fetch downloads a URL to a file, following redirects only to HTTPS where the
# downloader can enforce it (curl, and GNU wget). It leaves the HTTP status of
# the last answer in $status, empty when no server answered, and what the
# downloader said in $tmp/fetch.log.
fetch() {
	status=
	code=0
	if [ "$downloader" = curl ]; then
		status=$(curl -fsSL --retry 3 --connect-timeout 20 --proto-redir =https -w '%{http_code}' -o "$2" "$1" 2>"$tmp/fetch.log") || code=$?
		[ "$status" != 000 ] || status=
	else
		wget -q -S -T 20 ${wget_https:+"$wget_https"} -O "$2" "$1" 2>"$tmp/fetch.log" || code=$?
		status=$(sed -n 's/^ *HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$tmp/fetch.log" | tail -n 1)
	fi
	return "$code"
}

# not_found is true when the last fetch failed because the server answered that
# the file isn't there, rather than for any other reason.
not_found() {
	[ "$status" = 404 ]
}

# fetch_error prints why the last fetch failed, for a message that goes on to
# say what to do.
fetch_error() {
	if [ -n "$status" ]; then
		say "the server answered HTTP $status"
	else
		sed -n '$p' "$tmp/fetch.log" | sed 's/^curl: ([0-9]*) //; s/^wget: //'
	fi
}

# mirror_failed fails when the mirror didn't serve a file the install needs. $1
# is the file's path on the mirror, $2 what it holds, and $3 the fix. A mirror
# such as a storage bucket may answer 403 for a file it doesn't have, so any
# failure may mean the file is missing.
mirror_failed() {
	if not_found; then
		fail "The mirror at $mirror doesn't have $1, $2, so nothing was installed. $3"
	fi
	fail "Can't download $1, $2, from the mirror at $mirror: $(fetch_error). The mirror may not have it, or this server can't reach the mirror, so nothing was installed. $3"
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
	if [ "$(id -u)" -ne 0 ]; then
		doing=Installing
		[ "$action" != uninstall ] || doing=Removing
		opts=$(options)
		env_note=
		[ -z "$from_env" ] || env_note=" sudo doesn't pass the DOCKWELL_ settings on, so the options carry them instead."
		fail "$doing Dockwell needs root. Run it again as root: put sudo before sh, as in ... | sudo sh${opts:+ -s --$opts}, or run sudo sh install.sh$opts.$env_note"
	fi

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
	wget_https=
	if has curl; then
		downloader=curl
	else
		downloader=wget
		# busybox wget can't refuse a plain HTTP redirect; the pinned hash and
		# the signature still catch any tampering.
		if wget --help 2>&1 | grep -q -- --https-only; then wget_https=--https-only; fi
	fi
}

# installed_version prints the version of the installed binary, without its v.
installed_version() {
	[ -x "$BIN" ] || return 0
	"$BIN" version 2>/dev/null </dev/null | sed -n 's/^dockwell  *v\{0,1\}//p'
}

# installed_build prints which build of dockwell is installed, agent or full,
# or nothing when none runs here. Releases from before there were two builds,
# v0.1.0 and v0.2.0, name none in dockwell version --json: they are the full
# build.
installed_build() {
	[ -n "$(installed_version)" ] || return 0
	case $("$BIN" version --json 2>/dev/null </dev/null) in
	*'"BUILD_EDITION_AGENT"'*) say agent ;;
	*) say full ;;
	esac
}

# pick_build sets build to the build of dockwell this server runs. A control
# server runs the full build, which has the control plane, and a server that
# joins runs the agent build, which has only the agent. A server that already
# joined keeps the build it has: one that joined before there was an agent
# build runs the full build, which runs the agent too. The data directory
# says what the server is, as dockwell role reads it: a control plane keeps
# its state in $DATA/control, and an agent its identity in $DATA/agent.
pick_build() {
	installed_build=$(installed_build)
	if [ -d "$DATA/control" ]; then
		build=full
	elif [ -n "$installed_build" ] && [ -e "$DATA/agent/agent.json" ]; then
		build=$installed_build
	elif [ -n "$ticket" ] || [ -e "$DATA/agent/agent.json" ]; then
		build=agent
	else
		build=full
	fi
}

valid_version() {
	case $1 in
	'' | *[!0-9A-Za-z.+-]*) return 1 ;;
	esac
}

# resolve sets version to the release to install, and downloads its checksums.
resolve() {
	releases=${mirror:-${DOCKWELL_RELEASES_URL:-$REPO_URL/releases}}
	releases=${releases%/}
	version=${version#v}
	latest=
	code=0
	if [ -z "$version" ] || [ "$version" = latest ]; then
		latest=1
		fetch "$releases/latest/download/checksums.txt" "$tmp/checksums.txt" || code=$?
		if [ "$code" -ne 0 ] && [ -n "$mirror" ]; then
			mirror_failed latest/download/checksums.txt "the latest release's checksums" \
				"Check the mirror has the latest release, or pick a version it has with --version, then run this again."
		elif [ "$code" -ne 0 ] && not_found; then
			fail "There's no Dockwell release at $releases yet, so there's nothing to install. Run this again once one is published, or pick a version with --version."
		elif [ "$code" -ne 0 ]; then
			fail "Can't download the latest release's checksums from $releases: $(fetch_error). Check this server can reach it, then run this again."
		fi
		version=$(sed -n "s/^[0-9a-f]\{64\}  dockwell_\(.*\)_linux_${arch}\.tar\.gz\$/\1/p" "$tmp/checksums.txt" | head -n 1)
		valid_version "$version" ||
			fail "The latest release at $releases has no linux/$arch archive. Install a version that has one with --version."
	else
		valid_version "$version" || fail "\"$version\" isn't a version. Pass one such as 0.1.0."
		fetch "$releases/download/v$version/checksums.txt" "$tmp/checksums.txt" || code=$?
		if [ "$code" -ne 0 ] && [ -n "$mirror" ]; then
			mirror_failed "download/v$version/checksums.txt" "the checksums of Dockwell v$version" \
				"Pick a version the mirror has with --version, or check the mirror has v$version, then run this again."
		elif [ "$code" -ne 0 ] && not_found; then
			fail "There's no Dockwell v$version at $releases. Pick a version from $REPO_URL/releases."
		elif [ "$code" -ne 0 ]; then
			fail "Can't download the checksums of Dockwell v$version from $releases: $(fetch_error). Check this server can reach it, then run this again."
		fi
	fi
	download="$releases/download/v$version"
	archive="dockwell_${version}_linux_${arch}.tar.gz"
	# The checksums aren't checked yet, but they only pick the file to ask for:
	# verify checks their signature, then the archive against them.
	if [ "$build" = agent ]; then
		if awk -v f="dockwell-agent_${version}_linux_${arch}.tar.gz" '$2 == f { found = 1 } END { exit !found }' "$tmp/checksums.txt"; then
			archive="dockwell-agent_${version}_linux_${arch}.tar.gz"
		else
			build=full
			build_note="Dockwell v$version has no agent build, so this server gets the full build, which runs the agent too."
		fi
	fi
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
		runs "$cosign" ||
			fail "DOCKWELL_COSIGN is $cosign, which this server can't run. Make it executable, or unset DOCKWELL_COSIGN to download cosign."
		return 0
	fi
	cosign="$tmp/cosign"
	if [ -n "$cosign_mirror" ]; then
		base=$cosign_mirror
		from="from the mirror at $cosign_mirror"
		unreachable="Check the mirror has download/$COSIGN_VERSION/cosign-linux-$arch, then run this again."
		changed="The mirror must serve cosign $COSIGN_VERSION unchanged; tell whoever runs it, or install without --cosign-mirror."
	else
		base=$COSIGN_RELEASES
		from="from GitHub"
		unreachable="Check this server can reach github.com${mirror:+, or download cosign from a mirror too with --cosign-mirror}, then run this again."
		changed="Run this again; if it keeps failing, report it at $REPO_URL/security."
	fi
	say "Downloading cosign $COSIGN_VERSION $from to check the release signature."
	fetch "$base/download/$COSIGN_VERSION/cosign-linux-$arch" "$cosign" ||
		fail "Can't download cosign $COSIGN_VERSION $from: $(fetch_error). The release can't be checked without it. $unreachable"
	[ "$(sha256 "$cosign")" = "$want" ] ||
		fail "The cosign $from doesn't match its pinned SHA-256, so nothing was installed. $changed"
	chmod 0755 "$cosign"
	runs "$cosign" ||
		fail "This server can't run programs from $(dirname "$tmp"), perhaps because it is mounted noexec, so the release can't be checked. Set TMPDIR to a directory that allows it, such as /root, then run this again."
}

# get_trust_root puts the Sigstore trust root this script pins at
# $tmp/trusted_root.json, from the mirror. It takes the copy in the release it
# installs. When that copy doesn't match the pin, as in a release made before
# Sigstore last changed its root, or can't be downloaded, as from a release
# made before mirrors were supported, it takes the latest release's copy,
# whose script pins the root that release carries. Any failed download counts
# as a miss, since a mirror such as a file:// path or a storage bucket may not
# answer 404 for a file it lacks. Whatever it takes must match the pin.
get_trust_root() {
	root_mismatch=
	root_errors=
	for dir in "$download" "$releases/latest/download"; do
		if fetch "$dir/$SIGSTORE_ROOT" "$tmp/trusted_root.json"; then
			[ "$(sha256 "$tmp/trusted_root.json")" != "$SIGSTORE_ROOT_SHA256" ] || return 0
			root_mismatch=1
			# Installing the latest release, the latest release's copy is
			# this same file.
			[ -z "$latest" ] || break
		else
			root_errors="${root_errors:+$root_errors; }${dir#"$releases"/}/$SIGSTORE_ROOT: $(fetch_error)"
		fi
	done
	rm -f "$tmp/trusted_root.json"
	[ -z "$root_mismatch" ] ||
		fail "The Sigstore trust root on the mirror doesn't match its pinned SHA-256, so the release can't be checked and nothing was installed. Run the install script from the mirror's latest release, which pins the root that release carries: $releases/latest/download/install.sh. If that fails too, the mirror changed the file; tell whoever runs it."
	fail "Can't download $SIGSTORE_ROOT from the mirror at $releases ($root_errors), so the release can't be checked without reaching Sigstore, and nothing was installed. Check the mirror has it in download/v$version/ or latest/download/, as the same release on GitHub has it, then run this again."
}

# runs is true when a program can run here at all.
runs() {
	TUF_ROOT="$tmp/tuf" HOME="$tmp" "$1" version >/dev/null 2>&1 </dev/null
}

# verify checks the checksums' signature, then the archive against them, and
# leaves the binary at $tmp/dockwell. Nothing outside $tmp changes before it.
verify() {
	[ -z "$mirror" ] ||
		say "Downloading from the mirror at $mirror. Sigstore's trust root is pinned in this script, so the check doesn't need to reach Sigstore."
	get_cosign
	if ! fetch "$download/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json"; then
		[ -z "$mirror" ] ||
			mirror_failed "download/v$version/checksums.txt.sigstore.json" "the signature of Dockwell v$version" \
				"Check the mirror holds every file of the release, or pick a version it has with --version, then run this again."
		fail "Can't download the signature of Dockwell v$version from $download, so nothing was installed. Check this server can reach it, then run this again."
	fi
	if [ -n "${DOCKWELL_VERIFY_KEY:-}" ]; then
		say "Checking the release against the key in DOCKWELL_VERIFY_KEY, instead of Dockwell's release signature."
		set -- --key "$DOCKWELL_VERIFY_KEY" --insecure-ignore-tlog=true
		signer="the key in $DOCKWELL_VERIFY_KEY"
	else
		set -- --certificate-identity "$SIGNER_IDENTITY$version" --certificate-oidc-issuer "$SIGNER_ISSUER"
		signer="Dockwell's release workflow at tag v$version"
		# Through a mirror, the signing certificate is checked against the
		# pinned trust root instead of one fetched from Sigstore. A key needs
		# no trust root, so a mirror that re-signs needn't carry one.
		if [ -n "$mirror" ]; then
			get_trust_root
			set -- "$@" --trusted-root "$tmp/trusted_root.json"
		fi
	fi
	# cosign keeps Sigstore's trust root in TUF_ROOT; keep it in $tmp too.
	if ! TUF_ROOT="$tmp/tuf" HOME="$tmp" "$cosign" verify-blob "$@" \
		--bundle "$tmp/checksums.txt.sigstore.json" "$tmp/checksums.txt" >"$tmp/cosign.log" 2>&1 </dev/null; then
		cat "$tmp/cosign.log" >&2
		[ -z "$mirror" ] ||
			fail "The signature on the checksums of Dockwell v$version doesn't verify, so nothing was installed. If the same release from GitHub verifies, the mirror changed it; tell whoever runs the mirror. Otherwise report it at $REPO_URL/security."
		fail "The signature on the checksums of Dockwell v$version doesn't verify, so nothing was installed. If it says it can't reach Sigstore, check this server can reach tuf-repo-cdn.sigstore.dev and run this again; otherwise report it at $REPO_URL/security."
	fi
	say "The release signature checks out: $signer signed it."

	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "The signed checksums of Dockwell v$version don't list $archive. That release has no linux/$arch build; pick another with --version."
	if ! fetch "$download/$archive" "$tmp/$archive"; then
		[ -z "$mirror" ] ||
			mirror_failed "download/v$version/$archive" "the $build build of Dockwell v$version for linux/$arch" \
				"Check the mirror holds every file of the release, or pick a version it has with --version, then run this again."
		fail "Can't download $archive from $download. Check this server can reach it, then run this again."
	fi
	if [ "$(sha256 "$tmp/$archive")" != "$want" ]; then
		[ -z "$mirror" ] ||
			fail "$archive from the mirror doesn't match its signed checksum, so nothing was installed. The mirror changed it or holds a broken copy; tell whoever runs it."
		fail "$archive doesn't match its signed checksum, so nothing was installed. Run this again; if it keeps failing, report it at $REPO_URL/security."
	fi
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
# dockwell join itself refuses on a control server.
join() {
	[ -n "$ticket" ] || return 0
	current_role=$(role)
	case $current_role in
	agent\ *)
		say "This server already belongs to the control plane at ${current_role#agent }, so the ticket isn't used. To move it to another control plane, remove $DATA/agent and run this again with a new ticket."
		return 0
		;;
	esac
	if ! "$BIN" join "$ticket" >"$tmp/join.log" 2>&1 </dev/null; then
		cat "$tmp/join.log" >&2
		fail "This server didn't join, so Dockwell isn't started. Fix what dockwell join says above, then run this again; the ticket still works unless it expired."
	fi
	sed -n 1p "$tmp/join.log"
}

# role prints what dockwell run does on this server, as the binary decides:
# "control", or "agent <control plane URL>".
role() {
	"$BIN" role --data-dir "$DATA" </dev/null 2>"$tmp/role.log" || {
		cat "$tmp/role.log" >&2
		fail "Can't tell whether this server is a control server or an agent. Fix what dockwell role says above, then run this again."
	}
}

# service_listen prints the panel's listen address as the service sets it, in
# a systemd drop-in or /etc/conf.d/dockwell, or nothing when it doesn't.
service_listen() {
	case $init in
	systemd)
		[ -z "$booted" ] ||
			systemctl show -p Environment --value dockwell 2>/dev/null | tr ' ' '\n' | sed -n 's/^DOCKWELL_LISTEN=//p' | tail -n 1
		;;
	openrc)
		[ ! -r /etc/conf.d/dockwell ] ||
			sed -n 's/^[[:space:]]*\(export[[:space:]]*\)\{0,1\}DOCKWELL_LISTEN=["'"'"']\{0,1\}\([^"'"'"'[:space:]]*\).*/\2/p' /etc/conf.d/dockwell | tail -n 1
		;;
	esac
}

# panel_url prints where the panel answers here: the listen address the service
# sets, which check_listen keeps on loopback, or the default.
panel_url() {
	listen=$(service_listen)
	listen=${listen:-127.0.0.1:7600}
	printf 'http://%s:%s\n' "${listen%:*}" "${listen##*:}"
}

# serves_panel is true when this server runs the control plane, which serves
# the panel, or will once installed: it has no ticket and hasn't joined one.
serves_panel() {
	[ -z "$ticket" ] || return 1
	[ -x "$BIN" ] || return 0
	case $("$BIN" role --data-dir "$DATA" 2>/dev/null </dev/null) in
	agent\ *) return 1 ;;
	esac
}

# check_listen refuses, before anything changes, a listen address that
# dockwell run refuses: the panel stays on loopback, and only a domain serves
# it beyond this server, over HTTPS. An address without a port is left for
# dockwell run to explain.
check_listen() {
	listen=$(service_listen)
	case $listen in
	*:*) ;;
	*) return 0 ;;
	esac
	case ${listen%:*} in
	127.* | localhost | '[::1]') return 0 ;;
	esac
	case $init in
	systemd) how="with systemctl edit dockwell" ;;
	openrc) how="from /etc/conf.d/dockwell" ;;
	esac
	fail "The service sets DOCKWELL_LISTEN=$listen, which would serve the panel over plain HTTP beyond this server, so Dockwell refuses to start with it. Nothing was installed. Remove it $how, or set it to a loopback address such as 127.0.0.1:7700, then run this again. To reach the panel from elsewhere, set DOCKWELL_DOMAIN instead, which serves it over HTTPS."
}

# steady is true when the service stays up past its restart delay: a crash
# loop restarts it, which a status check on its own can miss.
steady() {
	case $init in
	systemd)
		before=$(systemctl show -p NRestarts --value dockwell 2>/dev/null)
		sleep 7
		systemctl is-active --quiet dockwell &&
			[ "$(systemctl show -p NRestarts --value dockwell 2>/dev/null)" = "$before" ]
		;;
	openrc)
		if ! has pgrep; then
			sleep 7
			rc-service dockwell status >/dev/null 2>&1
			return
		fi
		sleep 1
		before=$(pgrep -f "^$BIN run" | head -n 1)
		sleep 7
		[ -n "$before" ] && [ "$(pgrep -f "^$BIN run" | head -n 1)" = "$before" ]
		;;
	esac
}

logs_hint() {
	case $init in
	systemd) say "journalctl -u dockwell -n 50" ;;
	openrc) say "tail -n 50 $OPENRC_LOG" ;;
	esac
}

# show_log prints the service's last log lines, which say why it stopped,
# such as a setting dockwell run refuses.
show_log() {
	case $init in
	systemd) journalctl -u dockwell -n 10 --no-pager -o cat 2>/dev/null >&2 || true ;;
	openrc) tail -n 10 "$OPENRC_LOG" 2>/dev/null >&2 || true ;;
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
		panel=$(panel_url)
		deadline=$(($(date +%s) + 30))
		until answers "$panel/healthz"; do
			if [ "$(date +%s)" -ge "$deadline" ]; then
				show_log
				fail "Dockwell started, but its panel didn't answer on $panel within 30 s. Its last log lines, above, say why; see more with: $(logs_hint)"
			fi
			nap
		done
	else
		steady || fail "Dockwell started, but its agent keeps stopping. See what it logged with: $(logs_hint)"
	fi
}

install_dockwell() {
	check_tools
	tmp=$(mktemp -d "${TMPDIR:-/var/tmp}/dockwell-install.XXXXXX" 2>/dev/null || mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	if serves_panel; then
		check_listen
	fi
	pick_build
	build_note=
	resolve
	current=$(installed_version)
	changed=
	if [ "$current" = "$version" ] && [ "$installed_build" = "$build" ]; then
		say "Dockwell v$version is already installed on this server, as the $build build."
	else
		say "Installing the $build build of Dockwell v$version on $os_name, linux/$arch."
		[ -z "$build_note" ] || say "$build_note"
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
		if [ -z "$current" ]; then
			say "Installed $BIN."
		elif [ "$installed_build" != "$build" ]; then
			say "Replaced the $installed_build build of Dockwell v$current in $BIN with the $build build of v$version."
		else
			say "Upgraded $BIN from v$current to v$version."
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
	current_role=$(role)
	if [ "$current_role" = control ]; then
		panel=$(panel_url)
		port=${panel##*:}
		bound=${panel#http://}
		bound=${bound%:*}
		# A new install has no owner yet: print a fresh one-time claim link
		# and how to open it. Once someone has claimed it, dockwell claim
		# exits with status 3, and the summary says where the panel is
		# instead. Any other failure is shown as it is.
		code=0
		claim=$("$BIN" claim --data-dir "$DATA" 2>"$tmp/claim.log" </dev/null) || code=$?
		case $code in
		0)
			say "Dockwell v$version is running, and this is your control server."
			printf '%s\n' "$claim"
			;;
		3)
			cat <<EOF
Dockwell v$version is running, and this is your control server. Its panel
listens on $panel. To open it from your computer, forward the port over
SSH, then open http://localhost:$port there:

  ssh -L $port:$bound:$port <user>@<this server>
EOF
			;;
		*)
			cat "$tmp/claim.log" >&2
			fail "Dockwell v$version is running, but dockwell claim failed, so there's no link to claim it with. Fix what it says above, then run dockwell claim on this server for a link."
			;;
		esac
		cat <<EOF

To add a server, run dockwell token create here and follow what it prints.
Other servers reach this one on TCP port 7443, so open it to them.
EOF
	else
		say "Dockwell v$version is running on this server as an agent of ${current_role#agent }. It appears on the servers page of your panel."
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

# options prints the options this run was given, for a command to run again,
# with the ticket left for the reader to paste.
options() {
	[ "$action" != uninstall ] || printf ' --uninstall'
	[ -z "$ticket" ] || printf ' --join <ticket>'
	[ -z "$version" ] || printf ' --version %s' "$(quote "$version")"
	[ -z "$mirror" ] || printf ' --mirror %s' "$(quote "$mirror")"
	[ -z "$cosign_mirror" ] || printf ' --cosign-mirror %s' "$(quote "$cosign_mirror")"
}

# quote prints a value as sh reads it back: as it is when it holds only
# characters sh leaves alone, otherwise in single quotes, with each ' as '\''.
quote() {
	case $1 in
	'' | *[!A-Za-z0-9_./:@%+=,-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
	*) printf '%s' "$1" ;;
	esac
}

main() {
	ticket=${DOCKWELL_JOIN:-}
	version=${DOCKWELL_VERSION:-}
	mirror=${DOCKWELL_MIRROR:-}
	cosign_mirror=${DOCKWELL_COSIGN_MIRROR:-}
	from_env=${DOCKWELL_JOIN:-${DOCKWELL_VERSION:-${DOCKWELL_MIRROR:-${DOCKWELL_COSIGN_MIRROR:-}}}}
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
		--mirror)
			[ $# -ge 2 ] || fail "--mirror needs the URL of a mirror of Dockwell's releases, such as https://mirror.example.com/dockwell/releases."
			mirror=$2
			shift 2
			;;
		--mirror=*)
			mirror=${1#--mirror=}
			shift
			;;
		--cosign-mirror)
			[ $# -ge 2 ] || fail "--cosign-mirror needs the URL of a mirror of cosign's releases, such as https://mirror.example.com/cosign/releases."
			cosign_mirror=$2
			shift 2
			;;
		--cosign-mirror=*)
			cosign_mirror=${1#--cosign-mirror=}
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

	mirror=${mirror%/}
	cosign_mirror=${cosign_mirror%/}

	detect
	case $action in
	install) install_dockwell ;;
	uninstall) uninstall ;;
	esac
}

main "$@"
