// SPDX-License-Identifier: AGPL-3.0-only

package files

import "testing"

// Saving these lets someone back in for good, so each takes a passkey. It goes by
// where a walk ended, so what leads here is one too.
func TestWhatsAWayBackIn(t *testing.T) {
	s := New("/var/lib/dockwell", nil, nil)
	for _, p := range []string{
		"/root/.ssh/authorized_keys", "/home/mira/.ssh/authorized_keys", "/home/mira/.ssh/authorized_keys2",
		"/etc/passwd", "/etc/shadow", "/etc/group",
		"/etc/sudoers", "/etc/sudoers.d/90-admins", "/etc/sudoers.dpkg-old",
		"/etc/ssh/sshd_config", "/etc/ssh/sshd_config.d/10-root.conf",
		"/etc/pam.d/sshd", "/etc/pam.d/common-auth",
		"/etc/crontab", "/etc/cron.d/backup", "/etc/cron.daily/logrotate", "/etc/cron.hourly/x", "/var/spool/cron/crontabs/root",
		"/etc/systemd/system/dockwell.service", "/etc/systemd/system/x.service.d/override.conf",
		"/lib/systemd/system/ssh.service", "/usr/lib/systemd/system/docker.service", "/usr/local/lib/systemd/system/x.service",
		"/run/systemd/system/x.service", "/home/mira/.config/systemd/user/x.service",
		"/etc/ld.so.preload",
		"/var/lib/dockwell/agent/config.yml", "/var/lib/dockwell",
	} {
		if !s.wayIn(p) {
			t.Errorf("%s isn't a way back in", p)
		}
	}
	for _, p := range []string{
		"/etc/nginx/nginx.conf", "/etc/hosts", "/etc/passwd.bak/x", "/etc/sshfoo", "/etc/cron.dx/y", "/etc/systemdish",
		"/var/www/.user.ini", "/srv/app/.env", "/home/mira/.ssh/config", "/home/mira/notes/authorized_keys.txt",
		"/var/lib/dockwell-backup/x", "/var/spool/cronos/x",
	} {
		if s.wayIn(p) {
			t.Errorf("%s reads as a way back in, and a prompt on every file stops meaning anything", p)
		}
	}
}
