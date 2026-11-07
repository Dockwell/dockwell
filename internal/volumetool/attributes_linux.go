// SPDX-License-Identifier: AGPL-3.0-only

package volumetool

import (
	"archive/tar"
	"errors"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// keepXattr says whether an extended attribute goes with the data. An SELinux
// label belongs to the server it's on: Docker gives a volume its own, and one
// from elsewhere could lock its app out.
func keepXattr(name string) bool { return name != "security.selinux" }

// readXattrs reads a file's extended attributes, ACLs and capabilities among
// them. A filesystem without them has none.
func readXattrs(path string) (map[string]string, error) {
	size, err := unix.Llistxattr(path, nil)
	switch {
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.ENODATA), errors.Is(err, unix.EPERM):
		return nil, nil
	case err != nil:
		return nil, err
	case size == 0:
		return nil, nil
	}
	buf := make([]byte, size)
	if size, err = unix.Llistxattr(path, buf); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name := range strings.SplitSeq(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if !keepXattr(name) {
			continue
		}
		value, err := readXattr(path, name)
		switch {
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.EPERM), errors.Is(err, unix.ENOTSUP):
			continue // it went, or it's one only the kernel reads, such as trusted.
		case err != nil:
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

func readXattr(path, name string) (string, error) {
	size, err := unix.Lgetxattr(path, name, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, size)
	size, err = unix.Lgetxattr(path, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:size]), nil
}

// writeXattrs sets a file's extended attributes. A filesystem that takes none
// fails the restore, as it would quietly lose an ACL otherwise.
func writeXattrs(path string, xattrs map[string]string) error {
	for name, value := range xattrs {
		if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil {
			return err
		}
	}
	return nil
}

// makeNode makes a device or a named pipe.
func makeNode(path string, hdr *tar.Header) error {
	mode := uint32(hdr.Mode & 0o7777)
	switch hdr.Typeflag {
	case tar.TypeChar:
		mode |= syscall.S_IFCHR
	case tar.TypeBlock:
		mode |= syscall.S_IFBLK
	case tar.TypeFifo:
		mode |= syscall.S_IFIFO
	}
	return unix.Mknod(path, mode, int(unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))) //nolint:gosec // device numbers fit, as Linux gives them
}

// setTime sets a file's time, and a link's own rather than what it leads to.
func setTime(path string, at time.Time) error {
	ts, err := unix.TimeToTimespec(at)
	if err != nil {
		return err
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}
