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
	"strings"
	"time"
	"unicode/utf8"

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

// keyStarts is where a private key starts, with what follows its first line
// (base64 body or RFC 1421 encryption headers), so a program that only names
// one doesn't stop a download.
var keyStarts = lazyre.Regexp(`-{5}BEGIN [A-Z0-9 -]*PRIVATE KEY-{5}[ \t]*\r?\n(?:[ \t]*\r?\n)?[ \t]*(?:[A-Za-z0-9+/=]{16}|(?i:Proc-Type|DEK-Info):[ \t]*\S)`)

var beginKeyMark = []byte("-----BEGIN ")

// keyTail is how much of what went before a part is kept, to find a key that
// starts across two.
const keyTail = 256

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
// over send. Each top-level path is checked before any byte goes out.
func (s *Server) DownloadMany(ctx context.Context, paths []string, send func(*agentv1.SendFileRequest) error) (*agentv1.DownloadFileResult, error) {
	if err := CheckPaths(paths); err != nil {
		return nil, err
	}
	// Check every top-level path before sending the archive header, so a refused
	// or missing path answers cleanly with a refusal rather than a cut stream.
	parent := ""
	sameParent := true
	resolved := make([]string, 0, len(paths))
	var knownBytes uint64
	hasDir := false
	var preflightBuf []byte
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
			if preflightBuf == nil {
				preflightBuf = make([]byte, FrameBytes)
			}
			firstLen, why, seeked, _, rerr := scanFileHead(o.f, preflightBuf)
			if rerr != nil {
				where := o.where
				o.close()
				return &agentv1.DownloadFileResult{Refusal: denied(where, rerr)}, nil
			}
			if why != "" {
				where := o.where
				o.close()
				return &agentv1.DownloadFileResult{Refusal: refused(where, why)}, nil
			}
			if !seeked && firstLen > 0 {
				if r := (&keyScan{}).check(o.where, preflightBuf[:firstLen]); r != nil {
					o.close()
					return &agentv1.DownloadFileResult{Refusal: r}, nil
				}
			}
		}
		o.close()
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
	a := &archiver{ctx: ctx, z: z, g: g, root: parent, buf: make([]byte, FrameBytes)}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		refusal, err := s.archivePath(ctx, t, a, p)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal, LeftOut: a.leftOut}, nil
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent, LeftOut: a.leftOut}, nil
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
	line          [lineHead]byte
	lineLen       int
	lineOverflow  bool
	afterNul      nulLine
	dockerTail    [dockerTailLen]byte
	dockerTailLen int
	sawKey        bool
	sawHash       bool
	sawMountOpts  bool
	sawResolvConf bool
	sawEnv        bool
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
	h.checkLines(part)
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
	var joinBuf [dockerTailLen * 2]byte
	jn := copy(joinBuf[:], h.dockerTail[:h.dockerTailLen])
	jn += copy(joinBuf[jn:], part[:min(len(part), dockerTailLen)])
	join := joinBuf[:jn]
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

func (h *headScan) checkLines(part []byte) {
	if h.sawKey && h.sawHash {
		return
	}
	for rest := part; len(rest) > 0; {
		seg, more, ended := bytes.Cut(rest, []byte{'\n'})
		rest = more
		if h.afterNul.feed(seg, ended) {
			h.sawHash = true
		}
		if room := lineHead - h.lineLen; room > 0 {
			take := min(len(seg), room)
			copy(h.line[h.lineLen:], seg[:take])
			h.lineLen += take
			seg = seg[take:]
		}
		if len(seg) > 0 && !h.lineOverflow {
			if len(bytes.TrimRight(seg, " \t\r")) > 0 {
				h.lineOverflow = true
			}
		}
		if ended {
			h.finishLine()
			if h.sawKey && h.sawHash {
				return
			}
		}
	}
}

func (h *headScan) finishLine() {
	if h.lineLen == 0 {
		return
	}
	line := h.line[:h.lineLen]
	if !h.sawKey && !h.lineOverflow && bytes.Contains(line, beginKeyMark) && holdsPrivateKey(line) {
		h.sawKey = true
	}
	if !h.sawHash && bytes.IndexByte(line, '$') >= 0 && hashLine().Match(line) {
		h.sawHash = true
	}
	h.lineLen = 0
	h.lineOverflow = false
}

func (h *headScan) sawDocker() bool {
	return !h.binary && (h.sawMountOpts || h.sawResolvConf && h.sawEnv)
}

func (h *headScan) why(truncated bool) string {
	if h.binary || (!truncated && h.utf8CarryLen > 0) {
		return ""
	}
	h.finishLine()
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

// scanFileHead checks a file's first MaxFileBytes with heldBack's rules using
// only buf (FrameBytes), so no per-file MiB buffer is allocated (ADR 0109).
// When the file fits in buf or its first part already shows a binary file, it
// returns with buf[:firstLen] ready to stream and seeked == false. Otherwise it
// reads up to MaxFileBytes into buf and returns seeked == true so the caller
// can rewind f to 0 before streaming.
func scanFileHead(f io.Reader, buf []byte) (firstLen int, why string, seeked, isBin bool, err error) {
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, "", false, false, err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || len(buf) >= MaxFileBytes {
		if isBinary(buf[:n]) {
			return n, "", false, true, nil
		}
		return n, whatItHolds(buf[:n]), false, false, nil
	}
	if showsBinary(buf[:n]) {
		return n, "", false, true, nil
	}
	var hs headScan
	hs.feed(buf[:n])
	total := n
	for total < MaxFileBytes && !hs.binary {
		want := min(len(buf), MaxFileBytes-total)
		m, rerr := io.ReadFull(f, buf[:want])
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return 0, "", true, false, rerr
		}
		if m > 0 {
			seeked = true
			hs.feed(buf[:m])
			total += m
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			return n, hs.why(false), seeked, hs.binary || hs.utf8CarryLen > 0, nil
		}
	}
	if hs.binary {
		return n, "", seeked, true, nil
	}
	// At MaxFileBytes with a trailing incomplete UTF-8 rune, peek one byte to
	// tell whether the file continues (valid rune split by the cap) or ended.
	truncated := true
	if hs.utf8CarryLen > 0 {
		var one [1]byte
		m, rerr := f.Read(one[:])
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return 0, "", seeked, false, rerr
		}
		truncated = m > 0
	}
	return n, hs.why(truncated), seeked, !truncated && hs.utf8CarryLen > 0, nil
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

func (s *Server) sendFile(ctx context.Context, o *opened, out *frames) (*agentv1.DownloadFileResult, error) {
	buf := out.spare()
	firstLen, why, seeked, isBin, err := scanFileHead(o.f, buf)
	if err != nil {
		return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
	}
	if why != "" {
		return &agentv1.DownloadFileResult{Refusal: refused(o.where, why)}, nil
	}
	if seeked {
		if _, err := o.f.Seek(0, io.SeekStart); err != nil {
			return &agentv1.DownloadFileResult{Refusal: denied(o.where, err)}, nil
		}
		n, rerr := io.ReadFull(o.f, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return &agentv1.DownloadFileResult{Refusal: denied(o.where, rerr)}, nil
		}
		firstLen = n
	}
	scan := &keyScan{}
	var dockerTOCTOU headScan
	checkDocker := seeked && !isBin
	headSeen := 0
	if firstLen > 0 {
		if refusal := scan.check(o.where, buf[:firstLen]); refusal != nil {
			return &agentv1.DownloadFileResult{Refusal: refusal}, nil
		}
		if checkDocker {
			dockerTOCTOU.feedDocker(buf[:firstLen])
			headSeen += firstLen
			if dockerTOCTOU.sawDocker() {
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
	// The rest a part at a time, each looked at before it goes, read straight into
	// the part on its way.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := out.spare()
		n, err := o.f.Read(part)
		if n > 0 {
			if refusal := scan.check(o.where, part[:n]); refusal != nil {
				return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal}, nil
			}
			if checkDocker && headSeen < MaxFileBytes {
				take := min(n, MaxFileBytes-headSeen)
				dockerTOCTOU.feedDocker(part[:take])
				headSeen += take
				if dockerTOCTOU.sawDocker() {
					return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refused(o.where, whyDockerCopy)}, nil
				}
			}
			if err := out.took(n); err != nil {
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
// first line on a line of its own, encrypted or not, or anywhere in binary data
// followed by its body or encryption headers, and a line of a password file. It
// keeps the start of the line a part ends in, as the next part goes on with it.
type keyScan struct {
	tail     []byte
	line     []byte
	afterNul nulLine
}

// lineHead is how much of a line's start keyScan keeps, which is all the line
// rules look at.
const lineHead = 512

func (k *keyScan) check(where string, part []byte) *agentv1.FileRefusal {
	why := ""
	if len(k.tail) > 0 && len(part) > 0 {
		var lookBuf [keyTail + keyTail*2]byte
		n := copy(lookBuf[:], k.tail)
		n += copy(lookBuf[n:], part[:min(len(part), keyTail*2)])
		look := lookBuf[:n]
		if bytes.Contains(look, beginKeyMark) && keyStarts().Match(look) {
			why = whyPrivateKey
		}
	}
	if why == "" && bytes.Contains(part, beginKeyMark) && keyStarts().Match(part) {
		why = whyPrivateKey
	}
	if len(part) >= keyTail {
		k.tail = append(k.tail[:0], part[len(part)-keyTail:]...)
	} else if len(part) > 0 {
		keep := min(len(k.tail), keyTail-len(part))
		if keep < len(k.tail) {
			copy(k.tail, k.tail[len(k.tail)-keep:])
			k.tail = k.tail[:keep]
		}
		k.tail = append(k.tail, part...)
	}
	for rest := part; why == "" && len(rest) > 0; {
		seg, more, ended := bytes.Cut(rest, []byte{'\n'})
		rest = more
		if k.afterNul.feed(seg, ended) {
			why = whyHashes
		}
		if room := lineHead - len(k.line); room > 0 {
			k.line = append(k.line, seg[:min(len(seg), room)]...)
		}
		// Most lines have neither's marks, and a large file has many lines.
		switch {
		case bytes.Contains(k.line, beginKeyMark) && holdsPrivateKey(k.line):
			why = whyPrivateKey
		case bytes.IndexByte(k.line, '$') >= 0 && textLine(k.line) && hashLine().Match(k.line):
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
		return &agentv1.DownloadFileResult{SentBytes: out.sent, Refusal: refusal, LeftOut: a.leftOut}, nil
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	if err := out.flush(); err != nil {
		return nil, err
	}
	return &agentv1.DownloadFileResult{SentBytes: out.sent, LeftOut: a.leftOut}, nil
}

// archiver writes a folder or selection into a streaming zip archive, a file at a time.
type archiver struct {
	ctx        context.Context //nolint:containedctx // it lives as long as one archive
	z          *zipWriter
	g          *guard
	root       string
	buf        []byte
	count      int
	leftOut    uint64
	onProgress func(entries, leftOut uint64)
}

func (a *archiver) progress() {
	if a.onProgress != nil {
		a.onProgress(uint64(max(a.count, 0)), a.leftOut)
	}
}

// folder adds what's in the folder fd (at diskDir on disk, as rel inside the
// archive), at place in Docker's data, on the filesystem dev. Keeping diskDir
// separate from rel ensures credential rules match the true disk path even when
// rel starts with the selected folder's name.
func (a *archiver) folder(fd int, diskDir, rel string, at place, dev uint64, depth int) (*agentv1.FileRefusal, error) {
	if depth > maxDepth {
		a.leftOut++
		a.progress()
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
	// What a read refuses stays out, and is counted.
	into, why := a.g.enter(at, name, &st)
	if why != "" || checkCredentials(where) != "" {
		a.leftOut++
		a.progress()
		return nil, nil
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if st.Dev != dev {
			a.leftOut++
			a.progress()
			return nil, nil // another filesystem, mounted here: left out
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			a.leftOut++
			a.progress()
			return nil, nil
		}
		defer unix.Close(fd)
		if !sameAs(fd, &st, unix.S_IFDIR) {
			a.leftOut++
			a.progress()
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
			a.leftOut++
			a.progress()
			return nil, nil
		}
		f := os.NewFile(uintptr(fd), where)
		defer f.Close()
		// What was checked is what's read: a FIFO or a device put in its place
		// between the two is left out, as one there before is.
		if !sameAs(fd, &st, unix.S_IFREG) {
			a.leftOut++
			a.progress()
			return nil, nil
		}
		firstLen, why, seeked, isBin, err := scanFileHead(f, a.buf)
		if err != nil || why != "" {
			a.leftOut++
			a.progress()
			return nil, nil
		}
		if seeked {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				a.leftOut++
				a.progress()
				return nil, nil
			}
			n, rerr := io.ReadFull(f, a.buf)
			if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
				a.leftOut++
				a.progress()
				return nil, nil
			}
			firstLen = n
		}
		scan := &keyScan{}
		var dockerTOCTOU headScan
		checkDocker := seeked && !isBin
		headSeen := 0
		if firstLen > 0 {
			if refusal := scan.check(where, a.buf[:firstLen]); refusal != nil {
				return refusal, nil
			}
			if checkDocker {
				dockerTOCTOU.feedDocker(a.buf[:firstLen])
				headSeen += firstLen
				if dockerTOCTOU.sawDocker() {
					return refused(where, whyDockerCopy), nil
				}
			}
		}
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
			if _, err := w.Write(a.buf[:firstLen]); err != nil {
				return nil, err
			}
		}
		for {
			m, rerr := f.Read(a.buf)
			if m > 0 {
				if refusal := scan.check(where, a.buf[:m]); refusal != nil {
					return refusal, nil
				}
				if checkDocker && headSeen < MaxFileBytes {
					take := min(m, MaxFileBytes-headSeen)
					dockerTOCTOU.feedDocker(a.buf[:take])
					headSeen += take
					if dockerTOCTOU.sawDocker() {
						return refused(where, whyDockerCopy), nil
					}
				}
				if _, werr := w.Write(a.buf[:m]); werr != nil {
					return nil, werr
				}
			}
			if m == 0 && rerr != nil {
				break
			}
		}
		a.progress()
		return nil, nil
	}
	// A link, a socket, a device: left out, and counted.
	a.leftOut++
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

// textLine says whether a line's start reads as text: UTF-8 with no control
// bytes but tabs and a carriage return. A password file is text, and a run of
// random bytes in an image or an archive that happens to look like one of its
// lines isn't (CI on #921: random 2 MiB files tripped hashLine). The start is
// cut at lineHead, so a rune cut there still counts as text.
func textLine(line []byte) bool {
	for _, b := range line {
		if b < 0x20 && b != '\t' && b != '\r' || b == 0x7f {
			return false
		}
	}
	if utf8.Valid(line) {
		return true
	}
	if len(line) < lineHead {
		return false
	}
	for cut := 1; cut < utf8.UTFMax && cut < len(line); cut++ {
		if utf8.Valid(line[:len(line)-cut]) {
			return true
		}
	}
	return false
}

// nulLine keeps the text of a line after its last NUL: a password file's first
// line, inside a tar, starts right after its member header's NUL padding, which
// the line before it never ends with a newline (the key-scan review's M1). It's
// read as a password file's line only when it reads as text, so binary data
// that holds a NUL isn't mistaken for one.
type nulLine struct {
	buf   [lineHead]byte
	n     int
	after bool
}

// feed takes a newline-free segment of a line, and whether the line ends with
// it, and says whether the text after the line's last NUL is a hash line.
func (l *nulLine) feed(seg []byte, ended bool) bool {
	if i := bytes.LastIndexByte(seg, 0); i >= 0 {
		l.n, l.after, seg = 0, true, seg[i+1:]
	}
	if l.after {
		l.n += copy(l.buf[l.n:], seg)
	}
	if !ended {
		return false
	}
	found := l.after && holdsHashLine(l.buf[:l.n])
	l.n, l.after = 0, false
	return found
}

// holdsHashLine says whether a line reads as text and is a password file's line.
func holdsHashLine(line []byte) bool {
	return bytes.IndexByte(line, '$') >= 0 && textLine(line) && hashLine().Match(line)
}
