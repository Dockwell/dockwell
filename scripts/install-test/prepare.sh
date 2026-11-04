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
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends curl ca-certificates systemd >/dev/null
	;;
*" rhel "* | *" fedora "*)
	# curl-minimal is already there, and conflicts with the curl package.
	dnf install -y -q tar gzip systemd >/dev/null
	;;
*)
	echo "prepare.sh: $PRETTY_NAME isn't one of the systems the install tests cover." >&2
	exit 1
	;;
esac
echo "prepare.sh: $PRETTY_NAME is ready"
