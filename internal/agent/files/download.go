// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

// A download sends what a read would, and more of it: a file whatever it holds, up to
// any size, or a folder as a zip archive. So it goes by what a read refuses, by where
// it is and by its name, and by what a file's first MiB holds, and it stops where a
// private key starts anywhere in what it sends, before the key itself goes. A folder
// or selection zip leaves out what a read or key check refuses in it and lists those
// paths, and doesn't follow a link or cross into another filesystem. It sends a part
// at a time, so the agent holds a part.

// maxLeftOutPaths is how many left-out archive paths are listed in a result,
// job, or zip comment so memory stays flat when a folder skips many entries.
const (
	maxLeftOutPaths     = 10
	maxLeftOutPathBytes = 200
)

// seekFile rewinds an open file to its start after scanning. Tests replace it
// to stand in for a FUSE mount that refuses lseek.
var seekFile = func(f *os.File) error {
	_, err := f.Seek(0, io.SeekStart)
	return err
}

func seekRefused(where string, err error) *agentv1.FileRefusal {
	cause := err
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		cause = unwrapped
	}
	return &agentv1.FileRefusal{
		Kind: agentv1.FileRefusal_KIND_REFUSED, Path: where,
		Message: fmt.Sprintf("%s is on a mount that can't seek (%v), so Dockwell can't rewind it after checking its start. Copy it over SSH", where, cause),
	}
}

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

// DownloadMany streams several files and folders together as one zip archive
// over send. Each top-level path is checked before any byte goes out; a
// single-file selection that trips the key check is refused cleanly, while a
// multi-item selection or folder leaves offending files out and lists them.
func (s *Server) DownloadMany(ctx context.Context, paths []string, send func(*agentv1.SendFileRequest) error) (*agentv1.DownloadFileResult, error) {
	if err := CheckPaths(paths); err != nil {
		return nil, err
	}
	parent := ""
	sameParent := true
	resolved := make([]string, 0, len(paths))
	var knownBytes uint64
	hasDir := false
	var single *preflightedFile
	defer func() { single.close() }()
	for i, p := range paths {
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
		resolved = append(resolved, o.where)
		if o.dir {
			hasDir = true
		} else {
			knownBytes += uint64(max(o.w.st.Size, 0))
			if len(paths) == 1 {
				pf, ref, perr := preflightSingleFile(ctx, o, archiveEntryName(p, o.w.last().name), make([]byte, FrameBytes))
				if perr != nil {
					o.close()
					return nil, perr
				}
				if ref != nil {
					o.close()
					return &agentv1.DownloadFileResult{Refusal: ref}, nil
				}
				single = pf
				o = nil
			}
		}
		if o != nil {
			o.close()
		}
		dir := path.Dir(strings.TrimRight(p, "/"))
		if i == 0 {
			parent = dir
		} else if dir != parent {
			sameParent = false
		}
	}
	name := "files.zip"
	if sameParent && parent != "" && parent != "/" {
		name = path.Base(parent) + ".zip"
	}
	if hasDir {
		knownBytes = 0
	}
	out := &frames{send: send, buf: make([]byte, 0, FrameBytes)}
	if err := out.send(&agentv1.SendFileRequest{Name: name, Path: resolved[0], SizeBytes: knownBytes, Archive: true, ResolvedPaths: resolved}); err != nil {
		return nil, err
	}
	z, err := newZipWriter(out, s.tmpDir())
	if err != nil {
		return nil, err
	}
	defer z.Abort()
	g := s.guard(ctx)
	t := &trail{s: s}
	var buf []byte
	if single != nil {
		buf = single.buf
	} else {
		buf = make([]byte, FrameBytes)
	}
	a := &archiver{ctx: ctx, z: z, g: g, root: parent, buf: buf}
	if single != nil {
		refusal, err := a.packPreflightedFile(single)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal, LeftOut: a.leftOut, LeftOutPaths: slices.Clone(a.leftOutPaths)}, nil
		}
	} else {
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			refusal, err := s.archivePath(ctx, t, a, p)
			if err != nil {
				return nil, err
			}
			if refusal != nil {
				return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal, LeftOut: a.leftOut, LeftOutPaths: slices.Clone(a.leftOutPaths)}, nil
			}
		}
	}
	if a.leftOut > 0 {
		z.SetComment(leftOutComment(a.leftOut, a.leftOutPaths))
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent, LeftOut: a.leftOut, LeftOutPaths: slices.Clone(a.leftOutPaths)}, nil
}

// preflightedFile holds a single selected file already opened and scanned by
// preflightSingleFile so DownloadMany and Compress pack it without re-opening
// or scanning its start again.
type preflightedFile struct {
	o           *opened
	name        string
	buf         []byte
	firstLen    int
	eof         bool
	checkDocker bool
	headSeen    int
	scan        keyScan
	docker      headScan
}

func (p *preflightedFile) close() {
	if p != nil && p.o != nil {
		p.o.close()
		p.o = nil
	}
}

func archiveEntryName(p, fallback string) string {
	name := path.Base(strings.TrimRight(p, "/"))
	if name == "/" || name == "." || name == "" {
		return fallback
	}
	return name
}

// preflightSingleFile scans a single selected file's start (first part for
// binaries, first MiB for text) before any archive header or staging file is
// created, so a refused single-file selection fails cleanly while a passing
// file is handed straight to archiver without extra passes.
func preflightSingleFile(ctx context.Context, o *opened, name string, buf []byte) (*preflightedFile, *agentv1.FileRefusal, error) {
	pf := &preflightedFile{o: o, name: name, buf: buf}
	sc, err := scanFileHead(ctx, o.f, buf, &pf.docker)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, denied(o.where, err), nil
	}
	if sc.why != "" {
		return nil, refused(o.where, sc.why), nil
	}
	firstLen, eof := sc.firstLen, sc.eof
	if sc.seeked {
		if err := seekFile(o.f); err != nil {
			return nil, seekRefused(o.where, err), nil
		}
		n, rerr := io.ReadFull(o.f, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return nil, denied(o.where, rerr), nil
		}
		firstLen = n
		eof = errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
	}
	checkDocker := sc.seeked && !sc.isBin
	pf.docker.reset()
	headSeen := 0
	if firstLen > 0 {
		if why := pf.scan.scan(buf[:firstLen], true); why != "" {
			return nil, refused(o.where, why), nil
		}
		if eof {
			if why := pf.scan.finishWhy(true); why != "" {
				return nil, refused(o.where, why), nil
			}
		}
		if checkDocker {
			pf.docker.feedDocker(buf[:firstLen])
			headSeen += firstLen
			if pf.docker.sawDocker() {
				return nil, refused(o.where, whyDockerCopy), nil
			}
		}
	}
	pf.firstLen = firstLen
	pf.eof = eof
	pf.checkDocker = checkDocker
	pf.headSeen = headSeen
	return pf, nil, nil
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
	if why := checkCredentials(o.where); why != "" {
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
	if st.Nlink != 1 {
		_ = unix.Close(fd)
		w.close()
		return nil, refused(o.where, hardLinkedFile)
	}
	o.f = os.NewFile(uintptr(fd), o.where)
	w.st = st
	return o, nil
}

// sameAs checks the opened identity and type and leaves hard-linked files out.
func sameAs(fd int, was *unix.Stat_t, kind uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == kind && identify(&st) == identify(was) &&
		(kind != unix.S_IFREG || st.Nlink == 1)
}

// dockerTailLen is the overlap kept between headScan chunks so Docker state
// tokens split across a chunk boundary still match.
const dockerTailLen = 15 // len(`"ResolvConfPath"`) - 1

var (
	dockerMountOpts  = []byte(`"MountOpts"`)
	dockerResolvConf = []byte(`"ResolvConfPath"`)
	dockerEnv        = []byte(`"Env"`)
)

// headScan checks a file's first MaxFileBytes a part at a time with heldBack's
// rules (binary files pass; text files are refused for a private key, a password
// hash line, or Docker state), so downloads and folder zips agree on the first
// MiB without allocating per-file MiB buffers (ADR 0109).
type headScan struct {
	binary        bool
	utf8Carry     [utf8.UTFMax]byte
	utf8CarryLen  int
	keys          keyScan
	dockerTail    [dockerTailLen]byte
	dockerTailLen int
	dockerJoin    [dockerTailLen * 2]byte
	sawKey        bool
	sawHash       bool
	sawMountOpts  bool
	sawResolvConf bool
	sawEnv        bool
}

func (h *headScan) reset() {
	h.binary = false
	h.utf8CarryLen = 0
	h.keys.reset()
	h.dockerTailLen = 0
	h.sawKey = false
	h.sawHash = false
	h.sawMountOpts = false
	h.sawResolvConf = false
	h.sawEnv = false
}

func (h *headScan) feedDocker(part []byte) {
	if h.binary || len(part) == 0 {
		return
	}
	if !h.checkUTF8(part) {
		h.binary = true
		return
	}
	h.checkDocker(part)
}

func (h *headScan) feed(part []byte) {
	if h.binary || len(part) == 0 {
		return
	}
	if !h.checkUTF8(part) {
		h.binary = true
		return
	}
	h.checkDocker(part)
	if !h.sawKey {
		switch h.keys.scan(part, !h.sawHash) {
		case whyPrivateKey:
			h.sawKey = true
		case whyHashes:
			h.sawHash = true
		}
	}
}

func (h *headScan) checkUTF8(part []byte) bool {
	if bytes.IndexByte(part, 0) >= 0 {
		return false
	}
	p := part
	if h.utf8CarryLen > 0 {
		take := min(len(p), utf8.UTFMax-h.utf8CarryLen)
		copy(h.utf8Carry[h.utf8CarryLen:], p[:take])
		combined := h.utf8Carry[:h.utf8CarryLen+take]
		if !utf8.FullRune(combined) {
			h.utf8CarryLen += take
			return true
		}
		r, size := utf8.DecodeRune(combined)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		p = p[size-h.utf8CarryLen:]
		h.utf8CarryLen = 0
	}
	for cut := range min(utf8.UTFMax, len(p)+1) {
		whole, split := p[:len(p)-cut], p[len(p)-cut:]
		if utf8.Valid(whole) && (cut == 0 || !utf8.FullRune(split)) {
			h.utf8CarryLen = copy(h.utf8Carry[:], split)
			return true
		}
	}
	return false
}

func (h *headScan) checkDocker(part []byte) {
	if h.sawMountOpts || h.sawResolvConf && h.sawEnv {
		return
	}
	jn := copy(h.dockerJoin[:], h.dockerTail[:h.dockerTailLen])
	jn += copy(h.dockerJoin[jn:], part[:min(len(part), dockerTailLen)])
	join := h.dockerJoin[:jn]
	if !h.sawMountOpts && (bytes.Contains(join, dockerMountOpts) || bytes.Contains(part, dockerMountOpts)) {
		h.sawMountOpts = true
	}
	if !h.sawResolvConf && (bytes.Contains(join, dockerResolvConf) || bytes.Contains(part, dockerResolvConf)) {
		h.sawResolvConf = true
	}
	if !h.sawEnv && (bytes.Contains(join, dockerEnv) || bytes.Contains(part, dockerEnv)) {
		h.sawEnv = true
	}
	if len(part) >= dockerTailLen {
		h.dockerTailLen = copy(h.dockerTail[:], part[len(part)-dockerTailLen:])
	} else {
		keep := min(h.dockerTailLen, dockerTailLen-len(part))
		if keep < h.dockerTailLen {
			copy(h.dockerTail[:], h.dockerTail[h.dockerTailLen-keep:h.dockerTailLen])
		}
		copy(h.dockerTail[keep:], part)
		h.dockerTailLen = keep + len(part)
	}
}

func (h *headScan) sawDocker() bool {
	return !h.binary && (h.sawMountOpts || h.sawResolvConf && h.sawEnv)
}

func (h *headScan) why(truncated bool) string {
	if h.binary || (!truncated && h.utf8CarryLen > 0) {
		return ""
	}
	if !h.sawKey {
		switch h.keys.finishWhy(!h.sawHash) {
		case whyPrivateKey:
			h.sawKey = true
		case whyHashes:
			h.sawHash = true
		}
	}
	switch {
	case h.sawKey:
		return whyPrivateKey
	case h.sawHash:
		return whyHashes
	case h.sawMountOpts || h.sawResolvConf && h.sawEnv:
		return whyDockerCopy
	}
	return ""
}

// fileScan summarizes what scanFileHead learned of a file.
type fileScan struct {
	firstLen int
	why      string
	seeked   bool
	isBin    bool
	eof      bool
}

// scanFileHead checks a file's first MaxFileBytes with heldBack's rules using
// only buf (FrameBytes) and caller-owned hs, so no per-file MiB buffer or
// per-file scanner is allocated (ADR 0109). When the file fits in buf or its
// first part already shows a binary file, it returns with buf[:firstLen] ready
// to stream and seeked == false (with eof true when the whole file fit in buf).
// Otherwise it reads up to MaxFileBytes into buf and returns seeked == true so
// the caller can rewind f to 0 before streaming.
func scanFileHead(ctx context.Context, f io.Reader, buf []byte, hs *headScan) (fileScan, error) {
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fileScan{}, err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || len(buf) >= MaxFileBytes {
		eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if isBinary(buf[:n]) {
			return fileScan{firstLen: n, isBin: true, eof: eof}, nil
		}
		return fileScan{firstLen: n, why: whatItHolds(buf[:n]), eof: eof}, nil
	}
	if showsBinary(buf[:n]) {
		return fileScan{firstLen: n, isBin: true}, nil
	}
	hs.reset()
	hs.feed(buf[:n])
	total := n
	seeked := false
	for total < MaxFileBytes && !hs.binary {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fileScan{seeked: seeked}, ctxErr
		}
		want := min(len(buf), MaxFileBytes-total)
		m, rerr := io.ReadFull(f, buf[:want])
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return fileScan{seeked: true}, rerr
		}
		if m > 0 {
			seeked = true
			hs.feed(buf[:m])
			total += m
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			return fileScan{firstLen: n, why: hs.why(false), seeked: seeked, isBin: hs.binary || hs.utf8CarryLen > 0, eof: !seeked}, nil
		}
	}
	if hs.binary {
		return fileScan{firstLen: n, seeked: seeked, isBin: true}, nil
	}
	// At MaxFileBytes with a trailing incomplete UTF-8 rune, peek one byte to
	// tell whether the file continues (valid rune split by the cap) or ended.
	truncated := true
	if hs.utf8CarryLen > 0 {
		var one [1]byte
		m, rerr := f.Read(one[:])
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return fileScan{seeked: seeked}, rerr
		}
		truncated = m > 0
	}
	return fileScan{firstLen: n, why: hs.why(truncated), seeked: seeked, isBin: !truncated && hs.utf8CarryLen > 0}, nil
}

// showsBinary says whether the start of a file is enough to tell that isBinary
// holds for more of it: a NUL, or bytes that aren't UTF-8 before where the start
// was cut, as a character the cut split reads as UTF-8 once it's whole.
func showsBinary(start []byte) bool {
	if bytes.IndexByte(start, 0) >= 0 {
		return true
	}
	for cut := range min(utf8.UTFMax, len(start)+1) {
		whole, split := start[:len(start)-cut], start[len(start)-cut:]
		if utf8.Valid(whole) && (cut == 0 || !utf8.FullRune(split)) {
			return false
		}
	}
	return true
}

// heldBack says what a file's beginning holds that a read refuses, or nothing.
func heldBack(head []byte) string {
	if isBinary(head) {
		return ""
	}
	return whatItHolds(head)
}

// streamFileBody streams the remainder of f after its first chunk, checking
// every part with scan (and dockerTOCTOU within MaxFileBytes when checkDocker)
// before passing it to write.
func streamFileBody(
	ctx context.Context,
	f io.Reader,
	where string,
	nextBuf func() []byte,
	write func([]byte) error,
	scan *keyScan,
	dockerTOCTOU *headScan,
	checkDocker bool,
	headSeen int,
) (*agentv1.FileRefusal, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := nextBuf()
		n, err := f.Read(part)
		if n > 0 {
			if refusal := scan.check(where, part[:n]); refusal != nil {
				return refusal, nil
			}
			if errors.Is(err, io.EOF) {
				if refusal := scan.finish(where); refusal != nil {
					return refusal, nil
				}
			}
			if checkDocker && headSeen < MaxFileBytes {
				take := min(n, MaxFileBytes-headSeen)
				dockerTOCTOU.feedDocker(part[:take])
				headSeen += take
				if dockerTOCTOU.sawDocker() {
					return refused(where, whyDockerCopy), nil
				}
			}
			if werr := write(part[:n]); werr != nil {
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			if n == 0 {
				if refusal := scan.finish(where); refusal != nil {
					return refusal, nil
				}
			}
			return nil, nil
		}
		if err != nil {
			return denied(where, err), nil
		}
	}
}

func (s *Server) sendFile(ctx context.Context, o *opened, out *frames) (*agentv1.DownloadFileResult, error) {
	buf := out.spare()
	var hs headScan
	sc, err := scanFileHead(ctx, o.f, buf, &hs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
	}
	if sc.why != "" {
		return &agentv1.DownloadFileResult{Refusal: refused(o.where, sc.why)}, nil
	}
	firstLen, eof := sc.firstLen, sc.eof
	if sc.seeked {
		if err := seekFile(o.f); err != nil {
			return &agentv1.DownloadFileResult{Refusal: seekRefused(o.where, err)}, nil
		}
		n, rerr := io.ReadFull(o.f, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return &agentv1.DownloadFileResult{Refusal: denied(o.where, rerr)}, nil
		}
		firstLen = n
		eof = errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
	}
	var scan keyScan
	checkDocker := sc.seeked && !sc.isBin
	if checkDocker {
		hs.reset()
	}
	headSeen := 0
	if firstLen > 0 {
		if refusal := scan.check(o.where, buf[:firstLen]); refusal != nil {
			return &agentv1.DownloadFileResult{Refusal: refusal}, nil
		}
		if eof {
			if refusal := scan.finish(o.where); refusal != nil {
				return &agentv1.DownloadFileResult{Refusal: refusal}, nil
			}
		}
		if checkDocker {
			hs.feedDocker(buf[:firstLen])
			headSeen += firstLen
			if hs.sawDocker() {
				return &agentv1.DownloadFileResult{Refusal: refused(o.where, whyDockerCopy)}, nil
			}
		}
	}
	if err := out.send(&agentv1.SendFileRequest{Name: o.w.last().name, SizeBytes: uint64(max(o.w.st.Size, 0)), Path: o.where}); err != nil {
		return nil, err
	}
	if firstLen > 0 {
		if err := out.took(firstLen); err != nil {
			return nil, err
		}
	}
	if !eof {
		refusal, err := streamFileBody(
			ctx, o.f, o.where,
			out.spare,
			func(p []byte) error { return out.took(len(p)) },
			&scan, &hs, checkDocker, headSeen,
		)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal}, nil
		}
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent}, nil
}

func (s *Server) sendFolder(ctx context.Context, o *opened, out *frames) (*agentv1.DownloadFileResult, error) {
	name := o.w.last().name
	if len(o.w.steps) == 1 {
		name = "root"
	}
	if err := out.send(&agentv1.SendFileRequest{Name: name + ".zip", Path: o.where, Archive: true}); err != nil {
		return nil, err
	}
	z, err := newZipWriter(out, s.tmpDir())
	if err != nil {
		return nil, err
	}
	defer z.Abort()
	a := &archiver{ctx: ctx, z: z, g: o.g, root: o.where, buf: make([]byte, FrameBytes)}
	fd, err := unix.Openat(o.w.last().fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
	}
	refusal, err := a.folder(fd, o.where, "", o.w.last().place, o.w.st.Dev, 0)
	_ = unix.Close(fd)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal, LeftOut: a.leftOut, LeftOutPaths: slices.Clone(a.leftOutPaths)}, nil
	}
	if a.leftOut > 0 {
		z.SetComment(leftOutComment(a.leftOut, a.leftOutPaths))
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent, LeftOut: a.leftOut, LeftOutPaths: slices.Clone(a.leftOutPaths)}, nil
}

// archiver writes a folder or selection into a streaming zip archive, a file at a time.
type archiver struct {
	ctx          context.Context //nolint:containedctx // it lives as long as one archive
	z            *zipWriter
	g            *guard
	root         string
	buf          []byte
	scan         keyScan
	hs           headScan
	count        int
	leftOut      uint64
	leftOutPaths []string
	onProgress   func(entries, leftOut uint64)
}

func (a *archiver) progress() {
	if a.onProgress != nil {
		a.onProgress(uint64(max(a.count, 0)), a.leftOut)
	}
}

func (a *archiver) noteLeftOut(inside string) {
	a.leftOut++
	if len(a.leftOutPaths) < maxLeftOutPaths && inside != "" {
		a.leftOutPaths = append(a.leftOutPaths, shortLeftOutPath(inside))
	}
	a.progress()
}

// shortLeftOutPath cleans inside and caps it to maxLeftOutPathBytes with a
// middle ellipsis on UTF-8 rune boundaries so zip comments and job records stay
// compact and valid UTF-8 even for deep paths.
func shortLeftOutPath(inside string) string {
	s := Printable(inside)
	if len(s) <= maxLeftOutPathBytes {
		return s
	}
	const ellipsis = "…"
	budget := maxLeftOutPathBytes - len(ellipsis)
	headMax := budget / 2
	tailMax := budget - headMax
	headEnd := headMax
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - tailMax
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	if headEnd >= tailStart {
		return s
	}
	return s[:headEnd] + ellipsis + s[tailStart:]
}

func leftOutComment(total uint64, paths []string) string {
	if total == 0 {
		return ""
	}
	if len(paths) == 0 {
		return fmt.Sprintf("Left out: %d", total)
	}
	listed := strings.Join(paths, ", ")
	if uint64(len(paths)) < total {
		return fmt.Sprintf("Left out (%d), including: %s", total, listed)
	}
	return fmt.Sprintf("Left out (%d): %s", total, listed)
}

// folder adds what's in the folder fd (at diskDir on disk, as rel inside the
// archive), at place in Docker's data, on the filesystem dev. Keeping diskDir
// separate from rel ensures credential rules match the true disk path even when
// rel starts with the selected folder's name.
func (a *archiver) folder(fd int, diskDir, rel string, at place, dev uint64, depth int) (*agentv1.FileRefusal, error) {
	if depth > maxDepth {
		a.noteLeftOut(rel)
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
			if refusal, err := a.add(fd, diskDir, rel, name, at, dev, depth); refusal != nil || err != nil {
				return refusal, err
			}
		}
		if err != nil {
			return nil, nil
		}
	}
}

func (a *archiver) add(parent int, diskDir, rel, name string, at place, dev uint64, depth int) (*agentv1.FileRefusal, error) {
	if arriving(name) {
		return nil, nil // an upload's, as it arrives: not the folder's yet
	}
	var st unix.Stat_t
	if unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return nil, nil // it went as we read
	}
	where := joined(diskDir, name)
	inside := name
	if rel != "" {
		inside = rel + "/" + name
	}
	// What a read refuses stays out, and is counted and listed.
	into, why := a.g.enter(at, name, &st)
	if why != "" || checkCredentials(where) != "" {
		a.noteLeftOut(inside)
		return nil, nil
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if st.Dev != dev {
			a.noteLeftOut(inside)
			return nil, nil // another filesystem, mounted here: left out
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			a.noteLeftOut(inside)
			return nil, nil
		}
		defer unix.Close(fd)
		if !sameAs(fd, &st, unix.S_IFDIR) {
			a.noteLeftOut(inside)
			return nil, nil // it changed as it was opened: left out
		}
		a.count++
		if a.count > maxArchived {
			return &agentv1.FileRefusal{
				Kind: agentv1.FileRefusal_KIND_REFUSED, Path: a.root,
				Message: fmt.Sprintf("%s holds more than %d files and folders, so the archive stopped. Choose a smaller folder, or use SSH", a.root, maxArchived),
			}, nil
		}
		if _, err := a.z.Create(inside+"/", time.Unix(st.Mtim.Unix()), fs.ModeDir|fs.FileMode(st.Mode&0o777), zipStored); err != nil {
			return nil, err
		}
		a.progress()
		return a.folder(fd, where, inside, into, dev, depth+1)
	case unix.S_IFREG:
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			a.noteLeftOut(inside)
			return nil, nil
		}
		f := os.NewFile(uintptr(fd), where)
		defer f.Close()
		// What was checked is what's read: a FIFO or a device put in its place
		// between the two is left out, as one there before is.
		if !sameAs(fd, &st, unix.S_IFREG) {
			a.noteLeftOut(inside)
			return nil, nil
		}
		return a.addFile(f, where, inside, &st)
	}
	// A link, a socket, a device: left out, counted and listed.
	a.noteLeftOut(inside)
	return nil, nil
}

// addFile scans a file's start (first part for binaries, first MiB for text)
// before creating its zip entry, leaving out files that trip the check without
// writing a byte of them. Binaries and files <= FrameBytes are read once with
// no rewind; text files over FrameBytes scan their first MiB, rewind, and
// stream with a TOCTOU check. If a key or hash appears past the initial scan,
// the archive stops before it goes in.
func (a *archiver) addFile(f *os.File, where, inside string, st *unix.Stat_t) (*agentv1.FileRefusal, error) {
	sc, err := scanFileHead(a.ctx, f, a.buf, &a.hs)
	if err != nil {
		if a.ctx.Err() != nil {
			return nil, a.ctx.Err()
		}
		a.noteLeftOut(inside)
		return nil, nil
	}
	if sc.why != "" {
		a.noteLeftOut(inside)
		return nil, nil
	}
	firstLen, eof := sc.firstLen, sc.eof
	if sc.seeked {
		if err := seekFile(f); err != nil {
			a.noteLeftOut(inside)
			return nil, nil //nolint:nilerr // non-seekable text file over FrameBytes is left out of the archive
		}
		n, rerr := io.ReadFull(f, a.buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			a.noteLeftOut(inside)
			return nil, nil
		}
		firstLen = n
		eof = errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
	}
	a.scan.reset()
	checkDocker := sc.seeked && !sc.isBin
	if checkDocker {
		a.hs.reset()
	}
	headSeen := 0
	if firstLen > 0 {
		if a.scan.scan(a.buf[:firstLen], true) != "" || (eof && a.scan.finishWhy(true) != "") {
			a.noteLeftOut(inside)
			return nil, nil
		}
		if checkDocker {
			a.hs.feedDocker(a.buf[:firstLen])
			headSeen += firstLen
			if a.hs.sawDocker() {
				a.noteLeftOut(inside)
				return nil, nil
			}
		}
	}
	return a.writeFileEntry(f, where, inside, st, a.buf, firstLen, eof, &a.scan, &a.hs, checkDocker, headSeen)
}

func (a *archiver) packPreflightedFile(pf *preflightedFile) (*agentv1.FileRefusal, error) {
	a.root = pf.o.where
	return a.writeFileEntry(pf.o.f, pf.o.where, pf.name, &pf.o.w.st, pf.buf, pf.firstLen, pf.eof, &pf.scan, &pf.docker, pf.checkDocker, pf.headSeen)
}

func (a *archiver) writeFileEntry(
	f *os.File,
	where, inside string,
	st *unix.Stat_t,
	buf []byte,
	firstLen int,
	eof bool,
	scan *keyScan,
	dockerTOCTOU *headScan,
	checkDocker bool,
	headSeen int,
) (*agentv1.FileRefusal, error) {
	a.count++
	if a.count > maxArchived {
		return &agentv1.FileRefusal{
			Kind: agentv1.FileRefusal_KIND_REFUSED, Path: a.root,
			Message: fmt.Sprintf("%s holds more than %d files and folders, so the archive stopped. Choose a smaller folder, or use SSH", a.root, maxArchived),
		}, nil
	}
	w, err := a.z.Create(inside, time.Unix(st.Mtim.Unix()), fs.FileMode(st.Mode&0o777), zipDeflated)
	if err != nil {
		return nil, err
	}
	if firstLen > 0 {
		if _, err := w.Write(buf[:firstLen]); err != nil {
			return nil, err
		}
	}
	if !eof {
		refusal, err := streamFileBody(
			a.ctx, f, where,
			func() []byte { return buf },
			func(p []byte) error {
				_, werr := w.Write(p)
				return werr
			},
			scan, dockerTOCTOU, checkDocker, headSeen,
		)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return refusal, nil
		}
	}
	a.progress()
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

// spare is the room left in the part on its way, for a read to fill before took
// says how much it did.
func (f *frames) spare() []byte {
	return f.buf[len(f.buf):cap(f.buf)]
}

// took adds the n bytes a read put in spare to the part, and sends it once full.
func (f *frames) took(n int) error {
	f.buf = f.buf[:len(f.buf)+n]
	if len(f.buf) == cap(f.buf) {
		return f.flush()
	}
	return nil
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
