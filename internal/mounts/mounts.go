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

// Mount is one line of a mountinfo list: the filesystem's device, the path within
// it that's mounted, where, and what kind it is.
type Mount struct {
	// Dev is the filesystem's device, as major:minor.
	Dev string
	// Root is the path within that filesystem that's mounted: / for all of it, or
	// the folder or file a bind mounted.
	Root string
	// Point is where it's mounted, in the namespace the list is of.
	Point  string
	FSType string
}

// Parse reads a mountinfo list, in order, as the kernel stacks them.
func Parse(mountinfo string) []Mount {
	var out []Mount
	for line := range strings.Lines(mountinfo) {
		before, after, ok := strings.Cut(line, " - ")
		fields, rest := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(rest) == 0 {
			continue
		}
		out = append(out, Mount{Dev: fields[2], Root: Unescape(fields[3]), Point: Unescape(fields[4]), FSType: rest[0]})
	}
	return out
}

// At is what's mounted at point, the last mounted over any before it, and false
// when nothing is.
func At(list []Mount, point string) (Mount, bool) {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Point == point {
			return list[i], true
		}
	}
	return Mount{}, false
}

// ServerPaths says where on the server what m mounts is: each place the server's
// own list, host, has m's filesystem mounted from a folder that holds m's root.
// None means the server has that filesystem nowhere, as for a tmpfs or a disk of
// its own.
func ServerPaths(m Mount, host []Mount) []string {
	var out []string
	for _, h := range host {
		if h.Dev != m.Dev || !Holds(h.Root, m.Root) {
			continue
		}
		rest := strings.TrimPrefix(m.Root, strings.TrimSuffix(h.Root, "/"))
		p := strings.TrimSuffix(h.Point, "/") + rest
		if p == "" {
			p = "/"
		}
		out = append(out, p)
	}
	return out
}

// Pseudo reports a filesystem that's the kernel's own, such as proc or sysfs,
// which shows or changes the whole server wherever it's mounted.
func Pseudo(fstype string) bool {
	switch fstype {
	case "proc", "sysfs", "devtmpfs", "devpts", "debugfs", "tracefs", "securityfs", "cgroup", "cgroup2", "bpf",
		"configfs", "efivarfs", "fusectl", "pstore", "binfmt_misc", "hugetlbfs", "mqueue", "nsfs", "autofs", "rpc_pipefs":
		return true
	}
	return false
}
