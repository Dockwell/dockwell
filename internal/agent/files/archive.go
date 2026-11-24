// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Compressing a selection into a .zip archive on disk and extracting an
// existing .zip, .tar.gz or .tgz archive on disk (ADR 0045). Both follow the
// same rules as making, uploading and downloading: paths read where they lead,
// symlinks aren't followed for the target or inside archives, credentials and
// private keys stay out, writes into a way back in take a passkey, disk space
// and entry counts stay bounded, and failures leave nothing partial behind.

// MaxCompressPaths is the most top-level paths one Compress or DownloadMany
// call accepts.
const MaxCompressPaths = 1000

// ArchiveKind reports the unpack format of an archive by its file name, or
// UNPACK_UNSPECIFIED when its name doesn't end in .zip, .tar.gz or .tgz.
func ArchiveKind(name string) dockwellv1.Unpack {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip") && len(lower) > len(".zip"):
		return dockwellv1.Unpack_UNPACK_ZIP
	case strings.HasSuffix(lower, ".tar.gz") && len(lower) > len(".tar.gz"):
		return dockwellv1.Unpack_UNPACK_TAR_GZ
	case strings.HasSuffix(lower, ".tgz") && len(lower) > len(".tgz"):
		return dockwellv1.Unpack_UNPACK_TAR_GZ
	default:
		return dockwellv1.Unpack_UNPACK_UNSPECIFIED
	}
}

// CheckPaths validates a list of top-level paths for Compress or DownloadMany:
// at least one, at most MaxCompressPaths, each a valid path and name, and with
// distinct base names so archive entries never collide.
func CheckPaths(paths []string) error {
	if len(paths) == 0 {
		return errors.New("choose at least one file or folder")
	}
	if len(paths) > MaxCompressPaths {
		return fmt.Errorf("that's %d items, and one archive takes %d at most. Compress a folder instead", len(paths), MaxCompressPaths)
	}
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if err := CheckPath(p); err != nil {
			return err
		}
		if err := CheckName(p); err != nil {
			return err
		}
		base := path.Base(strings.TrimRight(p, "/"))
		if _, dup := seen[base]; dup {
			return fmt.Errorf("two items are both called %s, so they can't sit at the top of one archive", base)
		}
		seen[base] = struct{}{}
	}
	return nil
}

// CheckZipArchivePath checks that archivePath is a valid path ending in .zip.
func CheckZipArchivePath(archivePath string) error {
	if err := CheckPath(archivePath); err != nil {
		return err
	}
	if err := CheckName(archivePath); err != nil {
		return err
	}
	name := path.Base(strings.TrimRight(archivePath, "/"))
	if ArchiveKind(name) != dockwellv1.Unpack_UNPACK_ZIP {
		return errors.New("an archive's name has to end in .zip, such as site.zip")
	}
	return nil
}

// Extract unpacks an existing .zip, .tar.gz or .tgz archive at p into a new
// folder beside it, never over what's there. confirmed is the destination
// folder path a passkey confirmed, where extracting there is a way back in.
func (s *Server) Extract(ctx context.Context, p, confirmed string) (*agentv1.ExtractArchiveResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if err := CheckName(p); err != nil {
		return nil, err
	}
	as := ArchiveKind(path.Base(strings.TrimRight(p, "/")))
	if as == dockwellv1.Unpack_UNPACK_UNSPECIFIED {
		return nil, errors.New("only .zip, .tar.gz and .tgz archives can be extracted")
	}
	res := s.extract(ctx, p, as, confirmed)
	s.settleSoon()
	return res, nil
}

func extractRefused(archive, why string) *agentv1.FileRefusal {
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_UNPACK_REFUSED, Path: archive,
		Message: archive + " wasn't extracted: " + why + ". Extract it over SSH",
	}
}

func (s *Server) extract(ctx context.Context, p string, as dockwellv1.Unpack, confirmed string) *agentv1.ExtractArchiveResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.ExtractArchiveResult {
		return &agentv1.ExtractArchiveResult{Refusal: r}
	}
	t := &trail{s: s}
	g := s.guard(ctx)
	src, refusal := s.targetOf(t, g, p)
	if refusal != nil {
		return refuse(refusal)
	}
	defer src.w.close()
	if refusal := t.touch(src.where); refusal != nil {
		return refuse(refusal)
	}
	st, err := src.stat()
	if err != nil {
		return refuse(failed(src.where, err))
	}
	if _, why := g.enter(src.w.last().place, src.name, &st); why != "" {
		return refuse(refused(src.where, why))
	}
	if why := credentials(src.where); why != "" {
		return refuse(refused(src.where, why))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: src.where,
			Message: fmt.Sprintf("%s is %s, not an archive file, so there's nothing to extract", src.where, kindOf(&st)),
		})
	}
	if st.Nlink != 1 {
		return refuse(refused(src.where, hardLinkedFile))
	}
	dstName := UnpackedName(src.name)
	dst := &target{w: src.w, name: dstName, where: joined(src.w.path(), dstName)}
	if refusal := t.touch(dst.where); refusal != nil {
		return refuse(refusal)
	}
	switch _, err := dst.stat(); {
	case err == nil:
		return refuse(exists(dst.where, "nothing was extracted"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(dst.where, err))
	}
	if why := credentials(dst.where); why != "" {
		return refuse(refused(dst.where, why))
	}
	wayIn := s.wayInAt(dst, nil, true)
	if wayIn && confirmed != dst.where {
		return refuse(needsPasskey(dst.where, "extracting into it"))
	}
	afd, err := unix.Openat(src.dir(), src.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse(failed(src.where, err))
	}
	archive := os.NewFile(uintptr(afd), src.where)
	defer archive.Close()
	if !sameAs(afd, &st, unix.S_IFREG) {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_CHANGED, Path: src.where,
			Message: src.where + " changed as the agent opened it. Try again",
		})
	}
	if err := unix.Mkdirat(src.dir(), dst.name, madeDirMode); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return refuse(exists(dst.where, "nothing was extracted"))
		}
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: dst.where,
			Message: fmt.Sprintf("the agent can't make %s: %v. %s", dst.where, err, fixFor(err)),
		})
	}
	dfd, err := unix.Openat(src.dir(), dst.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse(extractRefused(src.where, fmt.Sprintf("the agent can't open %s: %v", dst.where, err)))
	}
	defer unix.Close(dfd)
	var dst0 unix.Stat_t
	if err := unix.Fstat(dfd, &dst0); err != nil || dst0.Uid != uint32(unix.Geteuid()) || !emptyDir(dfd) { //nolint:gosec // a user ID fits
		return refuse(extractRefused(src.where, dst.where+" changed as the agent made it"))
	}
	ownLike(dfd, &src.w.st, dockwellv1.FileType_FILE_TYPE_DIRECTORY)
	u := &unpacker{
		s: s, ctx: ctx, root: dfd, where: dst.where, folder: &src.w.st, dev: dst0.Dev,
		ways: s.waysIn(), allowWayIn: wayIn && confirmed == dst.where,
		buf: make([]byte, FrameBytes), limit: cmp.Or(s.unpackLimit, maxUnpacked), statfs: s.statfs,
	}
	if u.statfs == nil {
		u.statfs = unix.Fstatfs
	}
	switch as {
	case dockwellv1.Unpack_UNPACK_UNSPECIFIED:
		err = errors.New("only .zip, .tar.gz and .tgz archives can be extracted")
	case dockwellv1.Unpack_UNPACK_ZIP:
		err = u.zip(archive, st.Size)
	case dockwellv1.Unpack_UNPACK_TAR_GZ:
		err = u.tarGz(archive)
	}
	if err != nil {
		// All or nothing: remove what it made by device and inode.
		if rerr := removeAll(src.dir(), dst.name, dst0.Dev, dst0.Ino, 0); rerr != nil {
			return refuse(extractRefused(src.where, err.Error()+", and what it made is left in "+dst.where))
		}
		return refuse(extractRefused(src.where, err.Error()))
	}
	return &agentv1.ExtractArchiveResult{
		Extracted: &dockwellv1.ExtractArchiveResponse{
			Path: src.where, UnpackedTo: dst.where, UnpackedEntries: u.count, WayBackIn: wayIn,
		},
	}
}

// Compress packs paths into a new .zip archive at archivePath, never over
// what's there. confirmed is the archivePath a passkey confirmed, where making
// it is a way back in.
func (s *Server) Compress(ctx context.Context, paths []string, archivePath, confirmed string) (*agentv1.CompressFilesResult, error) {
	if err := CheckPaths(paths); err != nil {
		return nil, err
	}
	if err := CheckZipArchivePath(archivePath); err != nil {
		return nil, err
	}
	res := s.compress(ctx, paths, archivePath, confirmed)
	s.settleSoon()
	return res, nil
}

// boundedZipWriter writes a zip archive to disk while enforcing MaxUploadBytes
// and a free-space margin.
type boundedZipWriter struct {
	w       io.Writer
	fd      int
	where   string
	written uint64
	checked uint64
	statfs  func(fd int, st *unix.Statfs_t) error
	full    error
	tooBig  bool
}

func (b *boundedZipWriter) checkRoom() error {
	b.checked = b.written
	var st unix.Statfs_t
	if err := b.statfs(b.fd, &st); err != nil {
		return nil //nolint:nilerr // a disk that can't say goes on, as a write that fills it stops it anyway
	}
	bsize := uint64(st.Bsize) //nolint:gosec // a block size is positive
	margin := min(uint64(1)<<30, st.Blocks*bsize/20)
	if free := st.Bavail * bsize; free < margin {
		b.full = fmt.Errorf("the disk %s is on has %s free now, so it stopped short of filling it", path.Dir(b.where), sizeOf(free))
		return b.full
	}
	return nil
}

func (b *boundedZipWriter) Write(p []byte) (int, error) {
	if b.written+uint64(len(p)) > MaxUploadBytes {
		b.tooBig = true
		return 0, fmt.Errorf("that archive grew past the %s limit", sizeOf(MaxUploadBytes))
	}
	n, err := b.w.Write(p)
	b.written += uint64(max(n, 0))
	if err == nil && b.written-b.checked >= roomEvery {
		err = b.checkRoom()
	}
	return n, err
}

func (s *Server) compress(ctx context.Context, paths []string, archivePath, confirmed string) *agentv1.CompressFilesResult {
	refuse := func(r *agentv1.FileRefusal) *agentv1.CompressFilesResult {
		return &agentv1.CompressFilesResult{Refusal: r}
	}
	t := &trail{s: s}
	g := s.guard(ctx)
	dst, refusal := s.targetOf(t, g, archivePath)
	if refusal != nil {
		return refuse(refusal)
	}
	defer dst.w.close()
	if refusal := t.touch(dst.where); refusal != nil {
		return refuse(refusal)
	}
	switch _, err := dst.stat(); {
	case err == nil:
		return refuse(exists(dst.where, "nothing was compressed"))
	case !errors.Is(err, unix.ENOENT):
		return refuse(failed(dst.where, err))
	}
	if why := credentials(dst.where); why != "" {
		return refuse(refused(dst.where, why))
	}
	wayIn := s.wayInAt(dst, nil, false)
	if wayIn && confirmed != dst.where {
		return refuse(needsPasskey(dst.where, "making it"))
	}
	// Stage under uploadPrefix so arriving() keeps it out of listings and archives while writing.
	tmpName := uploadPrefix + rand.Text()
	tmpFd, err := unix.Openat(dst.dir(), tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: dst.where,
			Message: fmt.Sprintf("the agent can't make %s: %v. %s", dst.where, err, fixFor(err)),
		})
	}
	var tmpSt unix.Stat_t
	if err := unix.Fstat(tmpFd, &tmpSt); err != nil {
		_ = unix.Close(tmpFd)
		_ = unix.Unlinkat(dst.dir(), tmpName, 0)
		return refuse(denied(dst.where, err))
	}
	tmpFile := os.NewFile(uintptr(tmpFd), dst.where)
	placed := false
	defer func() {
		_ = tmpFile.Close()
		if !placed {
			_ = removeAll(dst.dir(), tmpName, tmpSt.Dev, tmpSt.Ino, 0)
		}
	}()
	statfs := s.statfs
	if statfs == nil {
		statfs = unix.Fstatfs
	}
	bw := &boundedZipWriter{w: tmpFile, fd: tmpFd, where: dst.where, statfs: statfs}
	if err := bw.checkRoom(); err != nil {
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NO_SPACE, Path: dst.where,
			Message: err.Error() + ". Free some space first",
		})
	}
	zw := zip.NewWriter(bw)
	a := &archiver{ctx: ctx, z: zw, g: g, where: path.Dir(dst.where), buf: make([]byte, FrameBytes)}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return refuse(notAnswering(p))
		}
		refusal, err := s.archivePath(ctx, t, a, p)
		if refusal != nil {
			return refuse(refusal)
		}
		if err != nil {
			return refuse(compressError(dst.where, bw, err))
		}
	}
	if err := zw.Close(); err != nil {
		return refuse(compressError(dst.where, bw, err))
	}
	if err := unix.Fsync(tmpFd); err != nil {
		return refuse(compressError(dst.where, bw, err))
	}
	ownLike(tmpFd, &dst.w.st, dockwellv1.FileType_FILE_TYPE_FILE)
	if err := unix.Renameat2(dst.dir(), tmpName, dst.dir(), dst.name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return refuse(exists(dst.where, "nothing was compressed"))
		}
		return refuse(&agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_DENIED, Path: dst.where,
			Message: fmt.Sprintf("the agent can't put %s in place: %v. %s", dst.where, err, fixFor(err)),
		})
	}
	var finalSt unix.Stat_t
	if err := unix.Fstatat(dst.dir(), dst.name, &finalSt, unix.AT_SYMLINK_NOFOLLOW); err != nil || finalSt.Dev != tmpSt.Dev || finalSt.Ino != tmpSt.Ino {
		_ = unix.Renameat2(dst.dir(), dst.name, dst.dir(), tmpName, unix.RENAME_NOREPLACE)
		return refuse(gone(dst.where))
	}
	placed = true
	return &agentv1.CompressFilesResult{
		Compressed: &dockwellv1.CompressFilesResponse{
			Path: dst.where, File: entry(dst.name, &finalSt, owners{}), Entries: uint64(max(a.count, 0)), WayBackIn: wayIn,
		},
	}
}

func compressError(where string, bw *boundedZipWriter, err error) *agentv1.FileRefusal {
	switch {
	case bw.tooBig:
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_TOO_LARGE, Path: where,
			Message: err.Error() + ". Compress fewer files, or compress them over SSH",
		}
	case bw.full != nil, errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		msg := err.Error()
		if bw.full == nil {
			msg = fmt.Sprintf("the disk %s is on filled while writing %s", path.Dir(where), where)
		}
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NO_SPACE, Path: where,
			Message: msg + ". Free some space first",
		}
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_DENIED, Path: where,
		Message: fmt.Sprintf("the agent can't write %s: %v. %s", where, err, fixFor(err)),
	}
}

// archivePath opens one top-level path p and packs it (file or directory) into a.
func (s *Server) archivePath(ctx context.Context, t *trail, a *archiver, p string) (*agentv1.FileRefusal, error) {
	o, refusal := s.openDownload(ctx, t, p)
	if refusal != nil {
		return refusal, nil
	}
	defer o.close()
	name := path.Base(strings.TrimRight(p, "/"))
	if name == "/" || name == "." || name == "" {
		name = o.w.last().name
	}
	a.where = o.where
	if o.dir {
		a.count++
		if a.count > maxArchived {
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_REFUSED, Path: o.where,
				Message: fmt.Sprintf("this archive holds more than %d files and folders, so it stopped. Archive a smaller selection, or use SSH", maxArchived),
			}, nil
		}
		if _, err := a.z.CreateHeader(&zip.FileHeader{Name: name + "/", Modified: time.Unix(o.w.st.Mtim.Unix()), Method: zip.Store}); err != nil {
			return nil, err
		}
		fd, err := unix.Openat(o.w.last().fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return denied(o.where, err), nil
		}
		defer unix.Close(fd)
		return a.folder(fd, name, o.w.last().place, o.w.st.Dev, 0)
	}
	a.count++
	if a.count > maxArchived {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: o.where,
			Message: fmt.Sprintf("this archive holds more than %d files and folders, so it stopped. Archive a smaller selection, or use SSH", maxArchived),
		}, nil
	}
	head := make([]byte, MaxFileBytes)
	n, err := io.ReadFull(o.f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return denied(o.where, err), nil
	}
	head = head[:n]
	if why := heldBack(head); why != "" {
		return refused(o.where, why), nil
	}
	h := &zip.FileHeader{Name: name, Modified: time.Unix(o.w.st.Mtim.Unix()), Method: zip.Deflate}
	h.SetMode(os.FileMode(o.w.st.Mode & 0o777))
	w, err := a.z.CreateHeader(h)
	if err != nil {
		return nil, err
	}
	scan := &keyScan{}
	if refusal := scan.check(o.where, head); refusal != nil {
		return refusal, nil
	}
	if _, err := w.Write(head); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := o.f.Read(a.buf)
		if m > 0 {
			if refusal := scan.check(o.where, a.buf[:m]); refusal != nil {
				return refusal, nil
			}
			if _, werr := w.Write(a.buf[:m]); werr != nil {
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return denied(o.where, err), nil
		}
	}
}
