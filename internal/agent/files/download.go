// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// A download sends what a read would, and more of it: a file whatever it holds, up to
// any size, or a folder as a zip archive. So it goes by what a read refuses, by where
// it is and by its name, and by what a file's first MiB holds, and it stops where a
// private key starts anywhere in what it sends, before the key itself goes. A folder
// leaves out what a read would refuse in it, and doesn't follow a link or cross into
// another filesystem. It sends a part at a time, so the agent holds a part.

// keyStarts is where a private key starts, with what follows its first line, so a
// program that only names one doesn't stop a download.
var keyStarts = lazyre.Regexp(`-{5}BEGIN [A-Z0-9 ]*PRIVATE KEY-{5}\r?\n[A-Za-z0-9+/=]{16}`)

// keyTail is how much of what went before a part is kept, to find a key that
// starts across two.
const keyTail = 128

// Download sends what's at p: a file as it is, or a folder as a zip archive. send
// takes each part, the first saying what it is.
func (s *Server) Download(ctx context.Context, p string, send func(*agentv1.SendFileRequest) error) (*agentv1.DownloadFileResult, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	var o *opened
	refusal, err := attempt(ctx, s, p, func(ctx context.Context, t *trail) (*agentv1.FileRefusal, error) {
		var refusal *agentv1.FileRefusal
		o, refusal = s.openDownload(ctx, t, p)
		return refusal, nil
	}, func(r *agentv1.FileRefusal) *agentv1.FileRefusal { return r })
	switch {
	case err != nil:
		return nil, err
	case refusal != nil:
		return &agentv1.DownloadFileResult{Refusal: refusal}, nil
	}
	defer o.close()
	out := &frames{send: send, buf: make([]byte, 0, FrameBytes)}
	if o.dir {
		return s.sendFolder(ctx, o, out)
	}
	return s.sendFile(ctx, o, out)
}

// opened is what a download opened.
type opened struct {
	w     *walked
	where string
	dir   bool
	f     *os.File // a file's, open to read
	g     *guard
}

func (o *opened) close() {
	if o.f != nil {
		_ = o.f.Close()
	}
	o.w.close()
}

func (s *Server) openDownload(ctx context.Context, t *trail, p string) (*opened, *agentv1.FileRefusal) {
	g := s.guard(ctx)
	w, refusal := g.walk(t, p)
	if refusal != nil {
		return nil, refusal
	}
	o := &opened{w: w, where: w.path(), g: g}
	if why := credentials(o.where); why != "" {
		w.close()
		return nil, refused(o.where, why)
	}
	switch w.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		o.dir = true
		return o, nil
	case unix.S_IFREG:
	default:
		w.close()
		return nil, &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_NOT_A_FILE, Path: o.where,
			Message: fmt.Sprintf("%s is %s, not a file or a folder, so there's nothing to download", o.where, kindOf(&w.st)),
		}
	}
	if refusal := t.touch(o.where); refusal != nil {
		w.close()
		return nil, refusal
	}
	fd, err := unix.Openat(w.steps[len(w.steps)-2].fd, w.last().name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		w.close()
		return nil, failed(o.where, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || identify(&st) != identify(&w.st) || st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		w.close()
		return nil, &agentv1.FileRefusal{Kind: agentv1.FileRefusal_KIND_NOT_FOUND, Path: o.where, Message: o.where + " changed as the agent opened it. Download it again"}
	}
	o.f = os.NewFile(uintptr(fd), o.where)
	w.st = st
	return o, nil
}

// sameAs reports whether fd is what was is, by device and inode, and of the type
// kind, as a name can lead elsewhere between a stat and an open.
func sameAs(fd int, was *unix.Stat_t, kind uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == kind && identify(&st) == identify(was)
}

// heldBack says what a file's beginning holds that a read refuses, or nothing.
func heldBack(head []byte) string {
	if isBinary(head) {
		return ""
	}
	return whatItHolds(head)
}

func (s *Server) sendFile(ctx context.Context, o *opened, out *frames) (*agentv1.DownloadFileResult, error) {
	head := make([]byte, MaxFileBytes)
	n, err := io.ReadFull(o.f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
	}
	head = head[:n]
	if why := heldBack(head); why != "" {
		return &agentv1.DownloadFileResult{Refusal: refused(o.where, why)}, nil
	}
	if err := out.send(&agentv1.SendFileRequest{Name: o.w.last().name, SizeBytes: uint64(max(o.w.st.Size, 0)), Path: o.where}); err != nil {
		return nil, err
	}
	scan := &keyScan{}
	if refusal := scan.check(o.where, head); refusal != nil {
		return &agentv1.DownloadFileResult{Refusal: refusal}, nil
	}
	if _, err := out.Write(head); err != nil {
		return nil, err
	}
	// The rest a part at a time, each looked at before it goes.
	head = head[:min(cap(head), FrameBytes)]
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := o.f.Read(head)
		if n > 0 {
			if refusal := scan.check(o.where, head[:n]); refusal != nil {
				return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal}, nil
			}
			if _, err := out.Write(head[:n]); err != nil {
				return nil, err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: denied(o.where, err)}, nil
		}
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent}, nil
}

// keyScan finds where a private key or a password hash starts in what's sent,
// across parts, by the rules a read goes by in a file's first MiB: a PEM key's
// first line on a line of its own, encrypted or not, and a line of a password
// file. It keeps the start of the line a part ends in, as the next part goes on
// with it.
type keyScan struct {
	tail []byte
	line []byte
}

// lineHead is how much of a line's start keyScan keeps, which is all the line
// rules look at.
const lineHead = 512

func (k *keyScan) check(where string, part []byte) *agentv1.FileRefusal {
	look := append(k.tail, part[:min(len(part), keyTail*4)]...) //nolint:gocritic // a short window across the join
	why := ""
	if keyStarts().Match(look) || keyStarts().Match(part) {
		why = whyPrivateKey
	}
	k.tail = append(k.tail[:0], part[max(0, len(part)-keyTail):]...)
	for rest := part; why == "" && len(rest) > 0; {
		seg, more, ended := bytes.Cut(rest, []byte{'\n'})
		rest = more
		if room := lineHead - len(k.line); room > 0 {
			k.line = append(k.line, seg[:min(len(seg), room)]...)
		}
		// Most lines have neither's marks, and a large file has many lines.
		switch {
		case bytes.Contains(k.line, []byte("-----BEGIN ")) && holdsPrivateKey(k.line):
			why = whyPrivateKey
		case bytes.IndexByte(k.line, '$') >= 0 && hashLine().Match(k.line):
			why = whyHashes
		}
		if ended {
			k.line = k.line[:0]
		}
	}
	if why == "" {
		return nil
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_REFUSED, Path: where,
		Message: where + " holds " + why + ", so the download stopped before it, and is cut short. Connect to the server over SSH to work with it",
	}
}

func (s *Server) sendFolder(ctx context.Context, o *opened, out *frames) (*agentv1.DownloadFileResult, error) {
	name := o.w.last().name
	if len(o.w.steps) == 1 {
		name = "root"
	}
	if err := out.send(&agentv1.SendFileRequest{Name: name + ".zip", Path: o.where, Archive: true}); err != nil {
		return nil, err
	}
	z := zip.NewWriter(out)
	a := &archiver{ctx: ctx, z: z, g: o.g, where: o.where, buf: make([]byte, FrameBytes)}
	fd, err := unix.Openat(o.w.last().fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
	}
	refusal, err := a.folder(fd, "", o.w.last().place, o.w.st.Dev, 0)
	_ = unix.Close(fd)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal}, nil
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent}, nil
}

// archiver writes a folder into a zip archive, a file at a time.
type archiver struct {
	ctx   context.Context //nolint:containedctx // it lives as long as one download
	z     *zip.Writer
	g     *guard
	where string
	buf   []byte
	count int
}

// folder adds what's in the folder fd, as rel in the archive, at place in Docker's
// data, on the filesystem dev.
func (a *archiver) folder(fd int, rel string, at place, dev uint64, depth int) (*agentv1.FileRefusal, error) {
	if depth > maxDepth {
		return nil, nil // deeper than a removal goes: left out
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, nil
	}
	dir := os.NewFile(uintptr(dup), rel)
	defer dir.Close()
	for {
		names, err := dir.Readdirnames(256)
		for _, name := range names {
			if err := a.ctx.Err(); err != nil {
				return nil, err
			}
			if refusal, err := a.add(fd, rel, name, at, dev, depth); refusal != nil || err != nil {
				return refusal, err
			}
		}
		if err != nil {
			return nil, nil
		}
	}
}

func (a *archiver) add(parent int, rel, name string, at place, dev uint64, depth int) (*agentv1.FileRefusal, error) {
	if arriving(name) {
		return nil, nil // an upload's, as it arrives: not the folder's yet
	}
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return nil, nil // it went as we read
	}
	where := joined(a.where+"/"+rel, name)
	inside := name
	if rel != "" {
		inside = rel + "/" + name
	}
	// What a read refuses stays out.
	into, why := a.g.enter(at, name, &st)
	if why != "" || credentials(where) != "" {
		return nil, nil
	}
	a.count++
	if a.count > maxArchived {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: a.where,
			Message: fmt.Sprintf("%s holds more than %d files and folders, so the download stopped, and is cut short. Download a folder inside it, or copy it over SSH", a.where, maxArchived),
		}, nil
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if st.Dev != dev {
			return nil, nil // another filesystem, mounted here: left out
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil
		}
		defer unix.Close(fd)
		if !sameAs(fd, &st, unix.S_IFDIR) {
			return nil, nil // it changed as it was opened: left out
		}
		if _, err := a.z.CreateHeader(&zip.FileHeader{Name: inside + "/", Modified: time.Unix(st.Mtim.Unix()), Method: zip.Store}); err != nil {
			return nil, err
		}
		return a.folder(fd, inside, into, dev, depth+1)
	case unix.S_IFREG:
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil
		}
		f := os.NewFile(uintptr(fd), where)
		defer f.Close()
		// What was checked is what's read: a FIFO or a device put in its place
		// between the two is left out, as one there before is.
		if !sameAs(fd, &st, unix.S_IFREG) {
			return nil, nil
		}
		n, err := io.ReadFull(f, a.buf[:min(len(a.buf), MaxFileBytes)])
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil
		}
		if heldBack(a.buf[:n]) != "" {
			return nil, nil
		}
		h := &zip.FileHeader{Name: inside, Modified: time.Unix(st.Mtim.Unix()), Method: zip.Deflate}
		h.SetMode(os.FileMode(st.Mode & 0o777))
		w, err := a.z.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		scan := &keyScan{}
		for part := a.buf[:n]; ; {
			if refusal := scan.check(where, part); refusal != nil {
				return refusal, nil
			}
			if _, err := w.Write(part); err != nil {
				return nil, err
			}
			m, err := f.Read(a.buf)
			if m == 0 && err != nil {
				break
			}
			part = a.buf[:m]
		}
	}
	// A link, a socket, a device: left out.
	return nil, nil
}

// frames sends what's written a part at a time, each FrameBytes at most.
type frames struct {
	send func(*agentv1.SendFileRequest) error
	buf  []byte
	sent uint64
}

func (f *frames) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		take := min(len(p), cap(f.buf)-len(f.buf))
		f.buf = append(f.buf, p[:take]...)
		p, n = p[take:], n+take
		if len(f.buf) == cap(f.buf) {
			if err := f.flush(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (f *frames) flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	if err := f.send(&agentv1.SendFileRequest{Data: f.buf}); err != nil {
		return err
	}
	f.sent += uint64(len(f.buf))
	// A send has marshalled the part by the time it returns, so the buffer is free.
	f.buf = f.buf[:0]
	return nil
}
