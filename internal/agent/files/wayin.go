// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"path"
	"slices"
	"strings"

	"github.com/dockwell/dockwell/internal/mounts"
)

// wayInFiles and wayInPlaces are where a save lets whoever wrote it back onto the
// server, or runs as root, however the session that wrote it ends: a key, an account,
// a rule for who can be root, or something that runs. A place holds everything under
// it. These are paths a walk ended at, with every link followed, so a link to one is
// one too.
var (
	wayInFiles = []string{
		"/etc/passwd", "/etc/passwd-", "/etc/shadow", "/etc/shadow-", "/etc/group", "/etc/group-", "/etc/gshadow", "/etc/gshadow-",
		"/etc/crontab", "/etc/anacrontab", "/etc/ld.so.preload",
	}
	wayInPlaces = []string{
		"/etc/ssh", "/etc/pam.d", "/etc/sudoers.d",
		"/etc/cron.d", "/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly", "/var/spool/cron",
		"/etc/systemd", "/run/systemd", "/usr/lib/systemd", "/lib/systemd", "/usr/local/lib/systemd",
	}
)

// wayIn reports a path that lets whoever writes it back in, wherever a walk ended.
// Saving one takes a person's passkey, just now (ADR 0032). The agent decides, as
// only it knows where a path leads.
func (s *Server) wayIn(where string) bool {
	switch base := path.Base(where); {
	case base == "authorized_keys", base == "authorized_keys2":
		return true
	case strings.HasPrefix(base, "sudoers"):
		return true
	case strings.Contains(where, "/.config/systemd/"):
		return true // a person's own units, which run as them
	}
	if slices.Contains(wayInFiles, where) || mounts.Holds(s.dataDir, where) {
		return true
	}
	return slices.ContainsFunc(wayInPlaces, func(place string) bool { return mounts.Holds(place, where) })
}
