// SPDX-License-Identifier: AGPL-3.0-only

package mounts_test

import (
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
