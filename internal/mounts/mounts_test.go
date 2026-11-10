// SPDX-License-Identifier: AGPL-3.0-only

package mounts_test

import (
	"fmt"
	"testing"

	"github.com/dockwell/dockwell/internal/mounts"
)

func TestAMountThatCanStopAnsweringIsPassedOverWhole(t *testing.T) {
	info := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 22 0:50 / /mnt/nfs rw,relatime shared:2 - nfs4 filer:/export rw,vers=4.2
41 22 0:51 / /mnt/my\040share rw shared:3 - cifs //filer/share rw
42 22 0:52 / /mnt/rclone rw shared:4 - fuse.rclone remote: rw
43 22 0:53 / /mnt/tmp rw shared:5 - tmpfs tmpfs rw
`
	for path, want := range map[string]string{
		"/mnt/nfs/a/b":         "/mnt/nfs",
		"/mnt/nfs":             "/mnt/nfs",
		"/mnt/my share/a":      "/mnt/my share",
		"/mnt/rclone/x":        "/mnt/rclone",
		"/mnt/tmp/x":           "/mnt/tmp/x",
		"/var/www/index.html":  "/var/www/index.html",
		"/mnt/nfsish/not-that": "/mnt/nfsish/not-that",
	} {
		if got := mounts.PassOver(info, path); got != want {
			t.Errorf("PassOver(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestUnescape(t *testing.T) {
	if got := mounts.Unescape(`/mnt/a\040b\011c`); got != "/mnt/a b\tc" {
		t.Errorf("Unescape = %q", got)
	}
}

func TestServerPaths(t *testing.T) {
	host := mounts.Parse(`22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 22 0:25 / /run rw,nosuid shared:5 - tmpfs tmpfs rw
41 22 8:17 / /var/lib/docker rw shared:20 - ext4 /dev/sdb1 rw
50 22 8:1 /srv/data /mnt/data rw shared:1 - ext4 /dev/sda1 rw
`)
	inside := mounts.Parse(`600 500 0:90 / / rw - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/A
601 600 0:91 / /proc rw - proc proc rw
602 600 8:1 /etc /host-etc rw - ext4 /dev/sda1 rw
603 600 8:17 /volumes/pg/_data /var/lib/postgresql/data rw - ext4 /dev/sdb1 rw
604 600 0:25 /docker.sock /var/run/docker.sock rw - tmpfs tmpfs rw
605 600 0:92 / /scratch rw - tmpfs tmpfs rw
606 600 8:1 /etc/my\040files /spaced rw - ext4 /dev/sda1 rw
607 600 8:1 / /host-etc rw - ext4 /dev/sda1 rw
`)
	for point, want := range map[string]string{
		"/host-etc":                "[/]",
		"/var/lib/postgresql/data": "[/var/lib/docker/volumes/pg/_data]",
		"/var/run/docker.sock":     "[/run/docker.sock]",
		"/scratch":                 "[]",
		"/spaced":                  "[/etc/my files]",
		"/proc":                    "[]",
	} {
		m, ok := mounts.At(inside, point)
		if !ok {
			t.Fatalf("nothing at %s", point)
		}
		if got := fmt.Sprint(mounts.ServerPaths(m, host)); got != want {
			t.Errorf("%s is %s on the server, want %s", point, got, want)
		}
	}
	// What a bind of a folder within it mounts is found through any place the server
	// mounts that folder too.
	m, _ := mounts.At(inside, "/var/lib/postgresql/data")
	m.Dev, m.Root = "8:1", "/srv/data/x"
	if got := fmt.Sprint(mounts.ServerPaths(m, host)); got != "[/srv/data/x /mnt/data/x]" {
		t.Errorf("a folder mounted twice = %s", got)
	}
	if !mounts.Pseudo("proc") || !mounts.Pseudo("sysfs") || mounts.Pseudo("ext4") || mounts.Pseudo("tmpfs") {
		t.Error("Pseudo is wrong")
	}
}
