// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/artifact"
)

var (
	errContextChanged = errors.New("the folder changed as Dockwell read it. Review the build again")
	errContextUnsafe  = errors.New("the context contains a protected path, link, special file, or mount")
	// ErrContextHardLink avoids staging bytes that might have a protected name elsewhere.
	ErrContextHardLink = errors.New("a file in the build folder has another name. Copy it into the folder as a regular file")
	// ErrContextEntry marks a refusal safe to show to the authorized folder picker.
	ErrContextEntry = errors.New("the build context can't store this entry")
)

// SnapshotContext applies FileService's existing path policy before any bytes
// go into the private build spool. It holds no descriptor when BuildKit runs.
func (s *Server) SnapshotContext(ctx context.Context, spool *artifact.Spool, owner, folder, dockerfile string) (*v1.DockerArtifactInfo, error) {
	if err := CheckPath(folder); err != nil {
		return nil, err
	}
	release, err := s.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return attempt(ctx, s, folder, func(ctx context.Context, t *trail) (*v1.DockerArtifactInfo, error) {
		o, refusal := s.openDownload(ctx, t, folder)
		if refusal != nil {
			return nil, errContextUnsafe
		}
		defer o.close()
		if !o.dir {
			return nil, errors.New("choose a folder for the build context")
		}
		w := &contextWalk{ctx: ctx, t: t, g: o.g, root: o.where, buf: make([]byte, FrameBytes), fs: o.w.st.Dev}
		root := o.w.last()
		for _, name := range []string{dockerfile + ".dockerignore", ".dockerignore"} {
			if name == ".dockerignore" && w.ignore != nil {
				break
			}
			if err := w.readIgnore(root, name); err != nil {
				return nil, err
			}
		}
		w.dockerfile = dockerfile
		return spool.SnapshotBuild(ctx, owner, func(out io.Writer) error {
			w.tar = tar.NewWriter(out)
			fd, err := unix.Openat(root.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			return errors.Join(w.folder(fd, "", root.place, &o.w.st, 0), w.tar.Close())
		})
	}, func(*agentv1.FileRefusal) *v1.DockerArtifactInfo { return nil })
}

type contextWalk struct {
	ctx              context.Context //nolint:containedctx // one file operation
	t                *trail
	g                *guard
	root, dockerfile string
	tar              *tar.Writer
	ignore           *patternmatcher.PatternMatcher
	buf              []byte
	fs               uint64
	entries          int
}

// readIgnore looks up the selected Dockerfile's ignore file without following
// links. Dockerfile-specific ignore rules take precedence over the root rules.
func (w *contextWalk) readIgnore(root step, name string) error {
	at := root
	var open []int
	defer func() {
		for _, fd := range open {
			_ = unix.Close(fd)
		}
	}()
	names := strings.Split(name, "/")
	for i, part := range names {
		if w.t.touch(path.Join(w.root, strings.Join(names[:i+1], "/"))) != nil {
			return errContextUnsafe
		}
		fd, st, next, err := w.entry(at.fd, at.place, part, path.Join(w.root, strings.Join(names[:i+1], "/")))
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		open = append(open, fd)
		at = step{fd: fd, place: next}
		if i < len(names)-1 {
			if st.Mode&unix.S_IFMT != unix.S_IFDIR {
				return errContextUnsafe
			}
			continue
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size > artifact.DockerfileSize {
			return errContextUnsafe
		}
		buf := make([]byte, int(st.Size))
		n, err := unix.Pread(fd, buf, 0)
		if err != nil || n != len(buf) || fileChanged(fd, &st) {
			return errContextChanged
		}
		patterns, err := ignorefile.ReadAll(bytes.NewReader(buf))
		if err != nil {
			return artifact.ErrArchive
		}
		w.ignore, err = patternmatcher.New(patterns)
		return err
	}
	return nil
}

func (w *contextWalk) entry(parent int, at place, name, where string) (int, unix.Stat_t, place, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, st, free, err
	}
	into, why := w.g.enter(at, name, &st)
	kind := st.Mode & unix.S_IFMT
	if why != "" || credentials(where) != "" || st.Dev != w.fs || kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return -1, st, free, errContextUnsafe
	}
	if kind == unix.S_IFREG && st.Nlink != 1 {
		return -1, st, free, ErrContextHardLink
	}
	mode := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if kind == unix.S_IFDIR {
		mode |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(parent, name, mode, 0)
	if err != nil {
		return -1, st, free, errContextChanged
	}
	if fileChanged(fd, &st) {
		_ = unix.Close(fd)
		return -1, st, free, errContextChanged
	}
	if kind == unix.S_IFDIR && dockerShaped(fd) {
		_ = unix.Close(fd)
		return -1, st, free, errContextUnsafe
	}
	return fd, st, into, nil
}

func fileChanged(fd int, before *unix.Stat_t) bool {
	var current unix.Stat_t
	return unix.Fstat(fd, &current) != nil || identify(&current) != identify(before) ||
		current.Mode != before.Mode || current.Size != before.Size || current.Nlink != before.Nlink ||
		current.Mtim != before.Mtim || current.Ctim != before.Ctim
}

func (w *contextWalk) folder(fd int, rel string, at place, before *unix.Stat_t, depth int) error {
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	if depth > 48 || fileChanged(fd, before) {
		return errContextChanged
	}
	for {
		names, err := f.Readdirnames(64)
		for _, name := range names {
			if err := w.add(fd, at, path.Join(rel, name), name, depth); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if fileChanged(fd, before) {
		return errContextChanged
	}
	return nil
}

func (w *contextWalk) add(parent int, at place, rel, name string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if name == ".git" {
		return nil
	}
	ignored := false
	if w.ignore != nil && rel != w.dockerfile && rel != ".dockerignore" && rel != w.dockerfile+".dockerignore" {
		var err error
		ignored, err = w.ignore.MatchesOrParentMatches(rel)
		if err != nil {
			return artifact.ErrArchive
		}
	}
	if ignored {
		if !w.ignore.Exclusions() {
			return nil
		}
		// An exception can select a descendant. Excluded non-directories
		// are never opened or validated, even when an exception exists.
		var st unix.Stat_t
		if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errContextChanged
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return nil
		}
	} else {
		w.entries++
		if w.entries > 50000 {
			return fmt.Errorf("the build folder reached its 50,000-entry limit at %q. Exclude generated files in .dockerignore: %w", rel, ErrContextEntry)
		}
		if !artifact.ContextPath(rel) {
			return fmt.Errorf("rename %q or exclude it in .dockerignore: %w", rel, ErrContextEntry)
		}
	}
	where := path.Join(w.root, rel)
	if w.t.touch(where) != nil {
		return errContextUnsafe
	}
	fd, st, into, err := w.entry(parent, at, name, where)
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		if !ignored {
			if err := w.tar.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatUSTAR}); err != nil {
				_ = unix.Close(fd)
				return err
			}
		}
		return w.folder(fd, rel, into, &st, depth+1)
	}
	f := os.NewFile(uintptr(fd), where)
	defer f.Close()
	if ignored {
		return nil
	}
	if err := w.tar.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Size: st.Size, Mode: int64(st.Mode & 0o777), Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	scan := &keyScan{}
	first := true
	for rest := st.Size; rest > 0; {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		p := w.buf[:min(int64(len(w.buf)), rest)]
		if _, err := io.ReadFull(f, p); err != nil {
			return errContextChanged
		}
		if first && heldBack(p) != "" || scan.check(where, p) != nil {
			return errContextUnsafe
		}
		first = false
		if _, err := w.tar.Write(p); err != nil {
			return err
		}
		rest -= int64(len(p))
	}
	if scan.finish(where) != nil {
		return errContextUnsafe
	}
	if fileChanged(fd, &st) {
		return errContextChanged
	}
	return nil
}
