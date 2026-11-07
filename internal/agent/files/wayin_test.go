// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// The list a review drew up of what lets someone back in, each of which falls under
// the rule a plan's mount goes by, or a name of its own.
func TestWhatsAWayBackIn(t *testing.T) {
	for _, p := range []string{
		"/root/.ssh/authorized_keys", "/home/mira/.ssh/authorized_keys", "/home/mira/.ssh/authorized_keys2", "/srv/deploy/.ssh/authorized_keys",
		"/etc/passwd", "/etc/shadow", "/etc/group", "/etc/hosts.equiv",
		"/etc/sudoers", "/etc/sudoers.d/90-admins", "/etc/sudo.conf", "/etc/doas.conf",
		"/etc/ssh/sshd_config", "/etc/ssh/sshd_config.d/10-root.conf", "/etc/pam.d/sshd", "/etc/security/limits.conf",
		"/etc/crontab", "/etc/cron.d/backup", "/etc/cron.daily/logrotate", "/etc/cron.hourly/x", "/var/spool/cron/crontabs/root",
		"/etc/systemd/system/dockwell.service", "/etc/systemd/system/x.service.d/override.conf",
		"/lib/systemd/system/ssh.service", "/usr/lib/systemd/system/docker.service", "/usr/local/lib/systemd/system/x.service", "/run/systemd/system/x.service",
		"/etc/init.d/dockwell", "/etc/conf.d/dockwell", "/etc/rc.local", "/etc/default/grub",
		"/etc/logrotate.d/nginx", "/etc/apt/apt.conf.d/99local", "/etc/apt/sources.list", "/etc/apt/sources.list.d/x.list",
		"/etc/profile", "/etc/profile.d/x.sh", "/etc/bash.bashrc", "/etc/environment", "/etc/update-motd.d/10-x",
		"/etc/polkit-1/rules.d/10-x.rules", "/usr/share/polkit-1/rules.d/10-x.rules",
		"/etc/udev/rules.d/10-x.rules", "/lib/udev/rules.d/10-x.rules", "/etc/modprobe.d/x.conf", "/etc/sysctl.d/10-x.conf",
		"/etc/tmpfiles.d/x.conf", "/usr/lib/tmpfiles.d/x.conf", "/etc/ld.so.conf", "/etc/ld.so.conf.d/x.conf", "/etc/ld.so.preload",
		"/etc/network/if-up.d/x", "/etc/NetworkManager/dispatcher.d/x", "/etc/docker/daemon.json",
		"/root/.bashrc", "/root/.profile", "/home/mira/.bashrc", "/home/mira/.profile",
		"/home/mira/.config/systemd/user/x.service", "/home/mira/.local/share/systemd/user/x.service",
		"/srv/deploy/.config/systemd/user/x.service", "/srv/deploy/.local/share/systemd/user/x.service",
		"/home/mira/.k5login", "/srv/deploy/.k5login", "/srv/deploy/.rhosts",
		"/var/lib/dockwell/agent/config.yml",
	} {
		if !listedPath(p, "/var/lib/dockwell", nil, false) {
			t.Errorf("%s isn't a way back in", p)
		}
	}
	// What people edit most asks nothing, or the prompt stops meaning anything.
	for _, p := range []string{
		"/srv/app/.env", "/srv/app/config.yml", "/var/www/html/index.php", "/var/www/.user.ini", "/opt/shop/config.yml", "/opt/shop/.env",
		"/home/mira/site/index.html", "/home/mira/notes/authorized_keys.txt", "/var/log/app.log", "/mnt/data/settings.json",
		"/var/lib/dockwell-backup/x", "/srv/deploy/.bashrc",
	} {
		if listedPath(p, "/var/lib/dockwell", nil, false) {
			t.Errorf("%s reads as a way back in", p)
		}
	}
	// A volume's files are an app's, though Docker's data is on the rule's list.
	if listedPath("/var/lib/docker/volumes/app/_data/app.env", "", nil, true) {
		t.Error("a file in a volume reads as a way back in")
	}
	if !listedPath("/var/lib/docker/volumes/app/_data/.ssh/authorized_keys", "", nil, true) {
		t.Error("a key in a volume doesn't")
	}
	// Who can become root runs their shell's files wherever their home is.
	if !listedPath("/srv/deploy/.bashrc", "", []string{"/srv/deploy"}, false) {
		t.Error("the shell file of someone who can become root isn't a way back in")
	}
}

// Those homes come from who the server says there is.
func TestWhoseHomesCount(t *testing.T) {
	s := New(t.TempDir(), nil, nil)
	s.accounts = func() (string, string) {
		return "root:x:0:0:root:/root:/bin/bash\n" +
				"mira:x:1000:1000::/home/mira:/bin/bash\n" +
				"deploy:x:1001:1001::/srv/deploy:/bin/bash\n" +
				"app:x:1002:1002::/srv/app:/bin/false\n" +
				"ops:x:1003:999::/opt/ops:/bin/bash\n" +
				"daemon:x:1:1::/:/usr/sbin/nologin\n",
			"sudo:x:27:mira\ndocker:x:999:deploy\nusers:x:100:app\n"
	}
	got := strings.Join(s.adminHomes(), " ")
	if want := "/root /home/mira /srv/deploy /opt/ops"; got != want {
		t.Errorf("homes = %q, want %q", got, want)
	}
}

// A listed place under another name is still it, by what it is. A bind mount needs
// root to make, so the directory here is listed where it is, under a name the rule
// doesn't know: only what it is can tell.
func TestAWayInGoesByWhatAPlaceIs(t *testing.T) {
	d := newDisk(t)
	listed := filepath.Join(d.root, "elsewhere")
	inside := filepath.Join(listed, "conf")
	write(t, inside, "x\n")
	d.server.systemPlaces = func() []string { return []string{listed} }
	if got := d.read(t, inside).GetFile(); !got.GetWayBackIn() {
		t.Errorf("a file in a listed place reads %v", got)
	}
	plain := filepath.Join(d.root, "plain")
	write(t, plain, "x\n")
	if got := d.read(t, plain).GetFile(); got.GetWayBackIn() {
		t.Errorf("a file elsewhere reads %v", got)
	}
}

// A hard link is the file it links to, whatever it's called.
func TestAHardLinkToAListedFileIsOne(t *testing.T) {
	d := newDisk(t)
	listed := filepath.Join(d.root, "protected")
	write(t, listed, "x\n")
	d.server.listedFiles = []string{listed}
	other := filepath.Join(d.root, "vol", "app.conf")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(listed, other); err != nil {
		t.Fatal(err)
	}
	version := savable(t, d, other)
	if got := d.save(t, other, "typed\n", version).GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("got %v", got)
	}
	if content, _ := os.ReadFile(listed); string(content) != "x\n" {
		t.Fatalf("it wrote %q", content)
	}
}

// A home on a network mount isn't stat'ed, as that holds every call while it's down,
// so what's in it is only known by its path. One on a local disk is read as it is.
func TestAHomeOnANetworkMountIsntAsked(t *testing.T) {
	d := newDisk(t)
	share := filepath.Join(d.root, "nfs")
	home := filepath.Join(share, "root")
	write(t, filepath.Join(home, ".bashrc"), "x\n")
	local := filepath.Join(d.root, "local")
	write(t, filepath.Join(local, ".bashrc"), "x\n")
	d.server.accounts = func() (string, string) {
		return "root:x:0:0::" + home + ":/bin/sh\nops:x:0:0::" + local + ":/bin/sh\n", ""
	}
	d.server.mounts = func() string {
		return "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n40 22 0:50 / " + share + " rw - nfs4 filer:/export rw\n"
	}
	ways := d.server.waysIn()
	if len(ways.homes) != 2 {
		t.Fatalf("homes = %v", ways.homes)
	}
	at := func(p string) identity {
		t.Helper()
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			t.Fatal(err)
		}
		return identify(&st)
	}
	if ways.files[at(filepath.Join(home, ".bashrc"))] {
		t.Error("it asked a network mount what's in a home there")
	}
	if !ways.files[at(filepath.Join(local, ".bashrc"))] {
		t.Error("it didn't read a home on a local disk")
	}
	// A listed name there still counts, by its path, which asks nothing of the mount.
	if !listedPath(filepath.Join(home, ".bashrc"), "", ways.homes, false) {
		t.Error("a shell file in that home isn't a way back in")
	}
}

// A volume's files are an app's, though Docker's own data is listed, so what's above
// a volume doesn't count. What's in it does, as its data can really be somewhere else.
func TestAVolumeAsksNothingUnlessItsDataIsListed(t *testing.T) {
	d := newDisk(t)
	root := filepath.Join(d.root, "docker")
	if err := os.MkdirAll(filepath.Join(root, "containers"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "volumes", "app", "_data")
	file := filepath.Join(data, "app.env")
	write(t, file, "x\n")
	d.server = New(d.data, nil, func(context.Context) (string, error) { return root, nil })
	d.server.systemPlaces = func() []string { return []string{root, filepath.Join(root, "volumes")} }
	if got := d.read(t, file).GetFile(); got.GetContent() == "" || got.GetWayBackIn() {
		t.Fatalf("a file in a volume reads %v", got)
	}
	d.server.systemPlaces = func() []string { return []string{data} }
	if got := d.read(t, file).GetFile(); !got.GetWayBackIn() {
		t.Fatalf("a volume whose data is somewhere listed reads %v", got)
	}
}
