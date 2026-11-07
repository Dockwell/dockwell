// SPDX-License-Identifier: AGPL-3.0-only

// Package mounts reads what's mounted on this server, for what has to keep away from
// a mount that stopped answering. It asks the kernel, and never a mount.
package mounts

import (
	"os"
	"strconv"
	"strings"
)

// Read reads what's mounted here, as /proc/self/mountinfo has it. It's the kernel's
// own list, which no mount can hold up. It's empty where there's no such list.
func Read() string {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	return string(info)
}

// Remote reports a filesystem that can stop answering because something else did: a
// network file system, or FUSE, which is a process on this server that can hang the
// same way.
func Remote(fstype string) bool {
	switch fstype {
	case "nfs", "nfs4", "cifs", "smb3", "smbfs", "9p", "ceph", "glusterfs", "lustre", "afs", "ncpfs", "fuse", "fuseblk":
		return true
	}
	return strings.HasPrefix(fstype, "fuse.")
}

// PassOver says what to keep away from, given a path that stopped answering: the
// whole mount, for one that can stop answering, as everything on it is down together.
// Elsewhere it's that path alone, as a local disk that's slow in one place isn't down.
func PassOver(mountinfo, at string) string {
	best, remote := "", false
	for line := range strings.Lines(mountinfo) {
		before, after, ok := strings.Cut(line, " - ")
		fields, fstype := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(fstype) == 0 {
			continue
		}
		point := Unescape(fields[4])
		if Holds(point, at) && len(point) >= len(best) {
			best, remote = point, Remote(fstype[0])
		}
	}
	if remote {
		return best
	}
	return at
}

// Holds reports whether path is place, or under it.
func Holds(place, path string) bool {
	return path == place || strings.HasPrefix(path, strings.TrimSuffix(place, "/")+"/")
}

// Unescape undoes the octal escapes the kernel's lists of mounts use for spaces and
// other odd characters in paths.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
