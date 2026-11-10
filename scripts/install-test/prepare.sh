#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Readies a stock distribution container for run.sh with what a server would
# have: a downloader and CA certificates. Debian, Ubuntu and AlmaLinux also
# get systemd, so the unit can be enabled and checked with systemd-analyze,
# though it isn't running. Alpine gets OpenRC, started, so the service really
# runs, and keeps only busybox wget, so the install script's wget path runs.
set -eu

# shellcheck source=/dev/null
. /etc/os-release
case " $ID ${ID_LIKE:-} " in
*" alpine "*)
	apk add --no-cache --quiet openrc
	# Enough of OpenRC for rc-service to supervise services in a container.
	sed -i 's/^#\{0,1\}rc_sys=.*/rc_sys="docker"/' /etc/rc.conf
	mkdir -p /run/openrc
	touch /run/openrc/softlevel
	openrc default >/dev/null 2>&1 || true
	;;
*" debian "* | *" ubuntu "*)
	export DEBIAN_FRONTEND=noninteractive
	# A mirror that can't be reached leaves apt-get update with old or partial
	# package lists and a zero exit, and the install then fails on versions
	# that don't fit together. So a list that didn't download counts as a
	# failure, each download is tried again, and the whole step gets three
	# tries. CI's containers have no IPv6 route, so apt asks over IPv4 only.
	apt="-o Acquire::ForceIPv4=true -o Acquire::Retries=3 -o Acquire::http::Timeout=30"
	for try in 1 2 3; do
		# shellcheck disable=SC2086 # the options are meant to split
		apt-get update -qq $apt -o APT::Update::Error-Mode=any &&
			apt-get install -y -qq $apt --no-install-recommends curl ca-certificates systemd >/dev/null &&
			break
		[ "$try" -lt 3 ] || {
			echo "prepare.sh: apt-get couldn't install the packages after 3 tries." >&2
			exit 1
		}
		echo "prepare.sh: apt-get didn't finish; trying again." >&2
		sleep 5
	done
	;;
*" rhel "* | *" fedora "*)
	# curl-minimal is already there, and conflicts with the curl package. A
	# slow mirror can stall dnf for good, so each try has a deadline. The
	# first picks the fastest mirror from the list; if the list keeps handing
	# out slow ones, later tries go to the distribution's own repository
	# server instead, where its repo files name one.
	for try in 1 2 3; do
		timeout 240 dnf install -y -q --setopt=timeout=30 --setopt=retries=3 --setopt=install_weak_deps=False \
			--setopt=fastestmirror=True --setopt=max_parallel_downloads=10 \
			tar gzip systemd >/dev/null && break
		[ "$try" -lt 3 ] || {
			echo "prepare.sh: dnf couldn't install the packages after 3 tries." >&2
			exit 1
		}
		if grep -qs '^# *baseurl=' /etc/yum.repos.d/*.repo; then
			echo "prepare.sh: dnf didn't finish; trying again from the distribution's own repository server." >&2
			sed -i -e 's/^mirrorlist=/#mirrorlist=/' -e 's/^# *baseurl=/baseurl=/' /etc/yum.repos.d/*.repo
		else
			echo "prepare.sh: dnf didn't finish; trying again." >&2
		fi
		dnf clean metadata >/dev/null 2>&1 || true
	done
	;;
*)
	echo "prepare.sh: $PRETTY_NAME isn't one of the systems the install tests cover." >&2
	exit 1
	;;
esac
echo "prepare.sh: $PRETTY_NAME is ready"
