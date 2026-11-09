// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// An archive unpacks into a new folder beside it, named as it is without .zip,
// .tar.gz or .tgz, so nothing that's there is replaced. Each entry is made through
// the descriptors of the folders above it, which are made or opened without
// following a link, so no entry can lead out of that folder. Links, devices and
// the like aren't made. An entry that would be a way back in, such as
// .ssh/authorized_keys, stops it, as the upload's passkey was for the archive, and
// so does more than an upload may be, or more entries than maxUnpacked. What it
// made by then goes again, so it's all or nothing.

// unpackedName is the folder an archive called name unpacks into.
func unpackedName(name string) string {
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if base, ok := strings.CutSuffix(strings.ToLower(name), ext); ok && base != "" {
			return name[:len(base)]
		}
	}
	return name + "-unpacked"
}

// unpackRefused is an archive that wasn't unpacked, and why.
func unpackRefused(archive, why string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_UNPACK_REFUSED, Path: archive,
		Message: archive + " was uploaded, and wasn't unpacked: " + why + ". Unpack it over SSH",
	}
}

// unpack unpacks the archive at p into a new folder beside it, and says where, and
// how many entries it made.
func (s *Server) unpack(ctx context.Context, p string, as dockwellv1.Unpack) (string, uint64, *agentv1.FileRefusal) {
	t := &trail{s: s}
	g := s.guard(ctx)
	src, refusal := s.targetOf(t, g, p)
	if refusal != nil {
		return "", 0, refusal
	}
	defer src.w.close()
	afd, err := unix.Openat(src.dir(), src.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", 0, unpackRefused(src.where, fmt.Sprintf("the agent can't open it: %v", err))
	}
	archive := os.NewFile(uintptr(afd), src.where)
	defer archive.Close()
	var ast unix.Stat_t
	if err := unix.Fstat(afd, &ast); err != nil || ast.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", 0, unpackRefused(src.where, "it isn't a file")
	}
	dst := &target{w: src.w, name: unpackedName(src.name), where: joined(src.w.path(), unpackedName(src.name))}
	if _, err := dst.stat(); err == nil {
		return "", 0, unpackRefused(src.where, dst.where+" is there already")
	}
	if s.wayInAt(dst, nil, true) {
		return "", 0, unpackRefused(src.where, "unpacking it into "+dst.where+" can let someone back onto this server")
	}
	if err := unix.Mkdirat(src.dir(), dst.name, madeDirMode); err != nil {
		return "", 0, unpackRefused(src.where, fmt.Sprintf("the agent can't make %s: %v", dst.where, err))
	}
	dfd, err := unix.Openat(src.dir(), dst.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", 0, unpackRefused(src.where, fmt.Sprintf("the agent can't open %s: %v", dst.where, err))
	}
	defer unix.Close(dfd)
	var dst0 unix.Stat_t
	if err := unix.Fstat(dfd, &dst0); err != nil || dst0.Uid != uint32(unix.Geteuid()) || !emptyDir(dfd) { //nolint:gosec // a user ID fits
		return "", 0, unpackRefused(src.where, dst.where+" changed as the agent made it")
	}
	ownLike(dfd, &src.w.st, dockwellv1.FileType_FILE_TYPE_DIRECTORY)
	u := &unpacker{s: s, ctx: ctx, root: dfd, where: dst.where, folder: &src.w.st, dev: dst0.Dev, ways: s.waysIn(), buf: make([]byte, FrameBytes), limit: cmp.Or(s.unpackLimit, maxUnpacked), statfs: s.statfs}
	if u.statfs == nil {
		u.statfs = unix.Fstatfs
	}
	switch as {
	case dockwellv1.Unpack_UNPACK_ZIP:
		err = u.zip(archive, ast.Size)
	case dockwellv1.Unpack_UNPACK_TAR_GZ:
		err = u.tarGz(archive)
	default:
		err = fmt.Errorf("there's no way to unpack it as %v", as)
	}
	if err != nil {
		// All or nothing: what it made goes, by what it is.
		if rerr := removeAll(src.dir(), dst.name, dst0.Dev, dst0.Ino, 0); rerr != nil {
			return "", 0, unpackRefused(src.where, err.Error()+", and what it made is left in "+dst.where)
		}
		return "", 0, unpackRefused(src.where, err.Error())
	}
	return dst.where, u.count, nil
}

// unpacker makes an archive's entries in a folder.
type unpacker struct {
	s       *Server
	ctx     context.Context //nolint:containedctx // it lives as long as one unpack
	root    int
	where   string
	folder  *unix.Stat_t
	dev     uint64
	ways    *waysIn
	buf     []byte
	count   uint64
	limit   uint64
	written uint64
	// checked is how much was written when the free space was last looked at,
	// and full why it stopped, if it did for that.
	checked uint64
	full    error
	// statfs reads the free space under root.
	statfs func(fd int, st *unix.Statfs_t) error
}

// roomEvery is how much an unpack writes, and roomEntries how many entries it
// makes, between looks at the disk's free space.
const (
	roomEvery   = 64 << 20
	roomEntries = 256
)

// room stops an unpack that would leave the disk with less than its margin free:
// 1 GB, or a twentieth of the disk where that's less. An archive that unpacks to
// less than an upload may be can still fill a disk, and what fills it fills it
// for everything else on the server too.
func (u *unpacker) room() error {
	u.checked = u.written
	var st unix.Statfs_t
	if err := u.statfs(u.root, &st); err != nil {
		return nil //nolint:nilerr // a disk that can't say goes on, as a write that fills it stops it anyway
	}
	bsize := uint64(st.Bsize) //nolint:gosec // a block size is positive
	margin := min(uint64(1)<<30, st.Blocks*bsize/20)
	if free := st.Bavail * bsize; free < margin {
		u.full = fmt.Errorf("the disk %s is on has %s free now, so it stopped short of filling it", u.where, sizeOf(free))
		return u.full
	}
	return nil
}

// roomWriter writes to w, and looks at the free space every roomEvery.
type roomWriter struct {
	w io.Writer
	u *unpacker
}

func (r roomWriter) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	r.u.written += uint64(max(n, 0))
	if err == nil && r.u.written-r.u.checked >= roomEvery {
		err = r.u.room()
	}
	return n, err
}

func (u *unpacker) zip(f *os.File, size int64) error {
	d, err := readZipDir(f, size)
	if err != nil {
		return err
	}
	// What it says it holds is refused before anything's made, or read.
	if d.count > u.limit {
		return fmt.Errorf("it holds %d entries, more than the %d an archive unpacks to here", d.count, u.limit)
	}
	return d.each(func(e *zipEntry) error {
		switch {
		case e.mode.IsDir():
			return u.entry(e.name, true, 0, nil)
		case e.mode.IsRegular():
			r, err := d.open(e)
			if err != nil {
				return err
			}
			return u.entry(e.name, false, uint32(e.mode.Perm()), r)
		}
		return nil
	})
}

func (u *unpacker) tarGz(f *os.File) error {
	z, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("it isn't a .tar.gz archive Dockwell can read: %w", err)
	}
	defer z.Close()
	r := tar.NewReader(z)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("it isn't a .tar.gz archive Dockwell can read: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = u.entry(h.Name, true, 0, nil)
		case tar.TypeReg:
			err = u.entry(h.Name, false, uint32(h.Mode)&0o777, r) //nolint:gosec // a mode's low bits
		}
		if err != nil {
			return err
		}
	}
}

// parts checks an entry's name, and splits it.
func (u *unpacker) parts(name string) ([]string, error) {
	clean := strings.Trim(strings.ReplaceAll(name, "\\", "/"), "/")
	if clean == "" || clean == "." {
		return nil, nil
	}
	if strings.IndexFunc(clean, unicode.IsControl) >= 0 {
		return nil, errors.New("it holds an entry with a control character in its name")
	}
	parts := strings.Split(clean, "/")
	if len(parts) > maxUnpackDepth {
		return nil, fmt.Errorf("it holds %s, more than %d folders deep", Printable(name), maxUnpackDepth)
	}
	for _, part := range parts {
		switch {
		case part == "..":
			return nil, fmt.Errorf("it holds %s, which leads out of its folder", Printable(name))
		case len(part) > MaxName:
			return nil, fmt.Errorf("it holds %s, whose name is longer than a server takes", Printable(name))
		}
	}
	return slices.DeleteFunc(parts, func(p string) bool { return p == "" || p == "." }), nil
}

// entry makes one entry of an archive, a folder or a file holding what r gives.
func (u *unpacker) entry(name string, dir bool, perm uint32, r io.Reader) error {
	if err := u.ctx.Err(); err != nil {
		return err
	}
	parts, err := u.parts(name)
	if err != nil || len(parts) == 0 {
		return err
	}
	u.count++
	if u.count > u.limit {
		return fmt.Errorf("it holds more than %d entries", u.limit)
	}
	where := u.where + "/" + strings.Join(parts, "/")
	if u.ways.listed(where, u.s.dataDir, false) {
		return fmt.Errorf("it holds %s, which can let someone back onto this server", Printable(strings.Join(parts, "/")))
	}
	if why := credentials(where); why != "" {
		return fmt.Errorf("it holds %s, which is %s", Printable(strings.Join(parts, "/")), why)
	}
	if dir {
		fd, err := u.folderAt(parts)
		if err != nil {
			return err
		}
		return unix.Close(fd)
	}
	if u.count%roomEntries == 1 {
		if err := u.room(); err != nil {
			return err
		}
	}
	parent, err := u.folderAt(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	base := parts[len(parts)-1]
	fd, err := unix.Openat(parent, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("the agent can't make %s: %w", Printable(strings.Join(parts, "/")), err)
	}
	f := os.NewFile(uintptr(fd), base)
	defer f.Close()
	left := int64(MaxUploadBytes - u.written)
	n, err := io.CopyBuffer(roomWriter{f, u}, io.LimitReader(r, left+1), u.buf)
	switch {
	case u.full != nil:
		return u.full
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return fmt.Errorf("the disk %s is on filled as it unpacked %s", u.where, Printable(strings.Join(parts, "/")))
	case err != nil:
		return fmt.Errorf("the agent can't unpack %s: %w", Printable(strings.Join(parts, "/")), err)
	case n > left:
		return fmt.Errorf("it holds more than the %s an upload can be", sizeOf(MaxUploadBytes))
	}
	ownLike(fd, u.folder, dockwellv1.FileType_FILE_TYPE_FILE)
	if perm&0o111 != 0 {
		_ = unix.Fchmod(fd, madeFileMode|0o111)
	}
	return nil
}

// folderAt opens the folder parts names under the root, making what isn't there,
// one part at a time, never through a link, and never onto another filesystem.
func (u *unpacker) folderAt(parts []string) (int, error) {
	fd, err := unix.Dup(u.root)
	if err != nil {
		return -1, err
	}
	for i, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, part, madeDirMode); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err == nil {
					ownLike(next, u.folder, dockwellv1.FileType_FILE_TYPE_DIRECTORY)
				}
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("it holds %s, which can't be a folder here: %w", Printable(strings.Join(parts[:i+1], "/")), err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil || st.Dev != u.dev {
			_ = unix.Close(next)
			return -1, fmt.Errorf("it holds %s, which is on another filesystem", Printable(strings.Join(parts[:i+1], "/")))
		}
		fd = next
	}
	return fd, nil
}
