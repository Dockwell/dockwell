// SPDX-License-Identifier: AGPL-3.0-only

package gitssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/netguard"
)

const (
	// maxListBytes caps kept ref listing bytes (protocol v2 ls-refs and kept v1
	// HEAD/branches/tags) well under 32 MiB so concurrent SSH listings cannot
	// spike control-plane memory (ADR 0113).
	maxListBytes = 2 << 20
	// maxListWireBytes caps total wire bytes streamed during a protocol v0/v1
	// ref advertisement before filtering out non-branch/tag refs (such as
	// refs/pull/* or refs/changes/*). Dropped lines are discarded immediately
	// after readPktBounded (<= 4 KiB each), so streaming them uses O(1) memory.
	maxListWireBytes = 32 << 20
	// maxListPktBytes caps a single pkt-line during ref listing so a hostile
	// server cannot force a large per-line allocation.
	maxListPktBytes = 4 << 10
	// maxRefs caps the number of branches/tags kept (matches gitfetch.List).
	maxRefs = 10_000
	// DefaultMaxPack is the default pack size cap (matches gitfetch.Default.Pack).
	DefaultMaxPack = 64 << 20
	// DefaultMaxDepth is the default shallow depth bound when verifying an older
	// commit on a branch.
	DefaultMaxDepth = 200
)

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Refs holds the repository's refs and default branch, matching gitref.Refs.
type Refs struct {
	Head    string
	Commits map[string]string
}

// Branches returns branch names sorted with the default branch first.
func (r Refs) Branches() []string {
	var out []string
	for full := range r.Commits {
		if b, ok := strings.CutPrefix(full, "refs/heads/"); ok && b != r.Head {
			out = append(out, b)
		}
	}
	slices.Sort(out)
	if r.Head != "" {
		out = append([]string{r.Head}, out...)
	}
	return out
}

// PackLimits bounds a pack fetch over SSH.
type PackLimits struct {
	Pack     int64
	MaxDepth int
}

// DefaultPackLimits returns the default size and depth limits.
func DefaultPackLimits() PackLimits {
	return PackLimits{Pack: DefaultMaxPack, MaxDepth: DefaultMaxDepth}
}

// List connects over SSH, verifies the pinned host key, authenticates, runs
// git-upload-pack (v2 with v1 fallback), and returns the repository's refs.
func List(ctx context.Context, cfg Config, dial netguard.DialFunc) (Refs, Handshake, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.listTimeout())
	defer cancel()

	conn, hs, err := dialPinned(ctx, cfg, dial)
	if err != nil {
		return Refs{}, hs, err
	}
	defer conn.Close()

	refs, err := listOnConn(ctx, conn, cfg)
	if err != nil {
		return Refs{}, hs, err
	}
	return refs, hs, nil
}

// Branches lists the repository's branches (default first) over SSH.
func Branches(ctx context.Context, cfg Config, dial netguard.DialFunc) (head string, branches []string, hs Handshake, err error) {
	refs, hs, err := List(ctx, cfg, dial)
	if err != nil {
		return "", nil, hs, err
	}
	return refs.Head, refs.Branches(), hs, nil
}

// Resolve resolves ref (branch, tag, empty for default branch, or full 40-hex
// commit ID) to a commit SHA and full ref name over SSH.
func Resolve(ctx context.Context, cfg Config, dial netguard.DialFunc, ref string) (commit, full string, err error) {
	if commitID.MatchString(strings.ToLower(ref)) {
		return strings.ToLower(ref), "", nil
	}
	refs, _, err := List(ctx, cfg, dial)
	if err != nil {
		return "", "", err
	}
	return resolveInRefs(cfg.Endpoint.Shown(), refs, ref)
}

func resolveInRefs(where string, refs Refs, ref string) (commit, full string, err error) {
	if ref == "" {
		if refs.Head == "" {
			return "", "", fmt.Errorf("%s doesn't say what its default branch is, as an empty repository doesn't. Name a branch, or push one", where)
		}
		return refs.Commits["refs/heads/"+refs.Head], "refs/heads/" + refs.Head, nil
	}
	names := []string{"refs/heads/" + ref, "refs/tags/" + ref}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
		names = []string{ref}
	}
	for _, n := range names {
		if c, ok := refs.Commits[n]; ok {
			return c, n, nil
		}
	}
	branches := refs.Branches()
	switch {
	case len(branches) == 0:
		return "", "", fmt.Errorf("%s has no branch or tag %s, and no branches at all yet. Push one, then try again", where, ref)
	case len(branches) > 5:
		return "", "", fmt.Errorf("%s has no branch or tag %s. Its branches include %s", where, ref, listNames(branches[:5]))
	}
	return "", "", fmt.Errorf("%s has no branch or tag %s. Its branches are %s", where, ref, listNames(branches))
}

func listNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// FetchPack fetches a shallow Git pack for commit (verifying commit is on ref
// when ref is a branch or tag) over SSH, validates every object with
// gitfetch.VerifyPack (SHA-1DC, object count, delta depth, inflation budget),
// and returns the open temporary pack file (caller must close and remove it).
func FetchPack(ctx context.Context, cfg Config, dial netguard.DialFunc, ref, commit string, lim PackLimits) (*os.File, int64, error) {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if !commitID.MatchString(commit) {
		return nil, 0, fmt.Errorf("%q isn't a commit. Name one in full, as 40 hex digits", commit)
	}
	if lim.Pack <= 0 {
		lim.Pack = DefaultMaxPack
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = DefaultMaxDepth
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.fetchTimeout())
	defer cancel()

	conn, _, err := dialPinned(ctx, cfg, dial)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()

	// Start git-upload-pack session (v2 preferred, v1 fallback).
	sess, stdin, stdout, stderr, err := startUploadPack(conn, cfg.Endpoint.RepoPath)
	if err != nil {
		return nil, 0, err
	}
	defer sess.Close()

	br := bufio.NewReader(deadlineReader{r: stdout, conn: conn.raw, idle: cfg.idleTimeout()})
	isV2, v2Caps, v1Refs, v1Caps, err := readGreeting(br, stderr)
	if err != nil {
		return nil, 0, mapSessionErr(ctx, err)
	}

	var wantCommit, refTip string
	depth := 1
	if ref == "" || ref == commit {
		wantCommit = commit
	} else {
		var refs Refs
		if isV2 {
			refs, err = sendLsRefsV2(stdin, br)
			if err != nil {
				return nil, 0, mapSessionErr(ctx, err)
			}
		} else {
			refs = v1Refs
		}
		tip, _, err := resolveInRefs(cfg.Endpoint.Shown(), refs, ref)
		if err != nil {
			_, _ = io.WriteString(stdin, "0000")
			_ = stdin.Close()
			return nil, 0, err
		}
		refTip = tip
		wantCommit = tip
		if tip != commit {
			depth = lim.MaxDepth
		}
	}

	f, err := os.CreateTemp(cfg.PackTempDir, "dockwell-ssh-pack-*")
	if err != nil {
		return nil, 0, fmt.Errorf("keeping what %s sends: %w", cfg.Endpoint.Host, err)
	}
	cleanupFile := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}

	var size int64
	if isV2 {
		size, err = fetchV2(stdin, br, f, wantCommit, depth, lim.Pack, v2Caps)
	} else {
		size, err = fetchV1(stdin, br, f, wantCommit, depth, lim.Pack, v1Caps)
	}
	_ = stdin.Close()
	if err != nil {
		cleanupFile()
		return nil, 0, mapSessionErr(ctx, err)
	}
	if err := gitfetch.VerifyPack(ctx, f, size, 4*lim.Pack, commit, refTip); err != nil {
		cleanupFile()
		if errors.Is(err, gitfetch.ErrCommitNotOnRef) {
			return nil, 0, fmt.Errorf("%s isn't on %s of %s, so Dockwell won't build it: a build's commit has to be on the branch or tag it names", commit[:7], ref, cfg.Endpoint.Shown())
		}
		return nil, 0, fmt.Errorf("reading what %s sent: %w", cfg.Endpoint.Host, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanupFile()
		return nil, 0, err
	}
	return f, size, nil
}

func startUploadPack(conn *sshConn, repoPath string) (*ssh.Session, io.WriteCloser, io.Reader, *bytes.Buffer, error) {
	sess, err := conn.client.NewSession()
	if err != nil {
		return nil, nil, nil, nil, ErrUnreachable
	}
	// Request Git wire protocol v2; ignore Setenv rejection because many sshd
	// configs don't list AcceptEnv GIT_PROTOCOL and fall back cleanly to v1.
	_ = sess.Setenv("GIT_PROTOCOL", "version=2")
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, nil, ErrUnreachable
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, nil, ErrUnreachable
	}
	var stderr bytes.Buffer
	sess.Stderr = &limitedWriter{w: &stderr, left: 8 << 10}
	if err := sess.Start("git-upload-pack " + shellQuote(repoPath)); err != nil {
		_ = sess.Close()
		return nil, nil, nil, nil, ErrRepositoryUnavailable
	}
	return sess, stdin, stdout, &stderr, nil
}

func listOnConn(ctx context.Context, conn *sshConn, cfg Config) (Refs, error) {
	sess, stdin, stdout, stderr, err := startUploadPack(conn, cfg.Endpoint.RepoPath)
	if err != nil {
		return Refs{}, err
	}
	defer sess.Close()

	br := bufio.NewReader(deadlineReader{r: stdout, conn: conn.raw, idle: cfg.idleTimeout()})
	isV2, _, v1Refs, _, err := readGreeting(br, stderr)
	if err != nil {
		return Refs{}, mapSessionErr(ctx, err)
	}
	var refs Refs
	if isV2 {
		refs, err = sendLsRefsV2(stdin, br)
		_, _ = io.WriteString(stdin, "0000")
		_ = stdin.Close()
		if err != nil {
			return Refs{}, mapSessionErr(ctx, err)
		}
	} else {
		refs = v1Refs
		_, _ = io.WriteString(stdin, "0000")
		_ = stdin.Close()
	}
	_ = sess.Wait()
	return refs, nil
}

type v2Capabilities struct {
	shallow bool
	format  string
}

// readGreeting reads the initial git-upload-pack greeting and detects whether
// the server speaks protocol v2 or protocol v1.
func readGreeting(r *bufio.Reader, _ *bytes.Buffer) (isV2 bool, v2Caps v2Capabilities, v1Refs Refs, v1Caps map[string]bool, err error) {
	v2Caps.format = "sha1"
	v1Refs = Refs{Commits: map[string]string{}}
	v1Caps = map[string]bool{}
	var keptRead, wireRead int
	first := true
	for {
		line, kind, err := readPktBounded(r, maxListPktBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if first {
					// Remote closed stdout without sending a greeting: repo missing or
					// key lacks access to this repository.
					return false, v2Caps, v1Refs, v1Caps, ErrRepositoryUnavailable
				}
				return isV2, v2Caps, v1Refs, v1Caps, nil
			}
			return false, v2Caps, v1Refs, v1Caps, err
		}
		if kind == pktFlush {
			if first {
				// Empty v1 repository sends a single flush or capabilities line.
				return false, v2Caps, v1Refs, v1Caps, nil
			}
			return isV2, v2Caps, v1Refs, v1Caps, nil
		}
		if kind != pktData {
			continue
		}
		pktLen := len(line) + 4
		wireRead += pktLen
		if wireRead > maxListWireBytes {
			return false, v2Caps, v1Refs, v1Caps, ErrTooLarge
		}
		text := strings.TrimSuffix(string(line), "\n")
		if first {
			first = false
			if text == "version 2" {
				isV2 = true
				keptRead += pktLen
				continue
			}
			if strings.HasPrefix(text, "ERR ") {
				return false, v2Caps, v1Refs, v1Caps, ErrRepositoryUnavailable
			}
			// Protocol v1 ref advertisement: "<sha> <ref>\x00<caps>"
			if i := strings.IndexByte(text, 0); i >= 0 {
				for _, c := range strings.Fields(text[i+1:]) {
					v1Caps[c] = true
					if k, _, ok := strings.Cut(c, "="); ok {
						v1Caps[k] = true
					}
					if target, ok := strings.CutPrefix(c, "symref=HEAD:refs/heads/"); ok {
						v1Refs.Head = target
					}
				}
				text = text[:i]
			}
			if kept, err := addRef(&v1Refs, text); err != nil {
				return false, v2Caps, v1Refs, v1Caps, err
			} else if kept {
				keptRead += pktLen
				if keptRead > maxListBytes {
					return false, v2Caps, v1Refs, v1Caps, ErrTooLarge
				}
			}
			continue
		}
		if isV2 {
			keptRead += pktLen
			if keptRead > maxListBytes {
				return false, v2Caps, v1Refs, v1Caps, ErrTooLarge
			}
			switch {
			case strings.HasPrefix(text, "fetch"):
				features := " " + strings.TrimPrefix(text, "fetch=") + " "
				v2Caps.shallow = strings.Contains(features, " shallow ")
			case strings.HasPrefix(text, "object-format="):
				v2Caps.format = strings.TrimPrefix(text, "object-format=")
			}
		} else {
			kept, err := addRef(&v1Refs, text)
			if err != nil {
				return false, v2Caps, v1Refs, v1Caps, err
			}
			if kept {
				keptRead += pktLen
				if keptRead > maxListBytes {
					return false, v2Caps, v1Refs, v1Caps, ErrTooLarge
				}
			}
		}
	}
}

func sendLsRefsV2(w io.Writer, r *bufio.Reader) (Refs, error) {
	var buf bytes.Buffer
	writePkt(&buf, "command=ls-refs\n")
	writePkt(&buf, "agent=dockwell\n")
	buf.WriteString("0001")
	for _, arg := range []string{"peel\n", "symrefs\n", "ref-prefix HEAD\n", "ref-prefix refs/heads/\n", "ref-prefix refs/tags/\n"} {
		writePkt(&buf, arg)
	}
	buf.WriteString("0000")
	if _, err := w.Write(buf.Bytes()); err != nil {
		return Refs{}, err
	}
	refs := Refs{Commits: map[string]string{}}
	var read int
	for {
		line, kind, err := readPktBounded(r, maxListPktBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return refs, nil
			}
			return Refs{}, err
		}
		if kind == pktFlush {
			return refs, nil
		}
		if kind != pktData {
			continue
		}
		read += len(line) + 4
		if read > maxListBytes {
			return Refs{}, ErrTooLarge
		}
		text := strings.TrimSuffix(string(line), "\n")
		if strings.HasPrefix(text, "ERR ") {
			return Refs{}, ErrRepositoryUnavailable
		}
		fields := strings.Fields(text)
		if len(fields) < 2 || !commitID.MatchString(fields[0]) {
			return Refs{}, errors.New("a line of the repository's branches can't be read")
		}
		at := fields[0]
		for _, attr := range fields[2:] {
			if peeled, ok := strings.CutPrefix(attr, "peeled:"); ok && commitID.MatchString(peeled) {
				at = peeled
			}
			if target, ok := strings.CutPrefix(attr, "symref-target:refs/heads/"); ok && fields[1] == "HEAD" {
				refs.Head = target
			}
		}
		if fields[1] == "HEAD" {
			continue
		}
		if len(refs.Commits) >= maxRefs {
			return Refs{}, fmt.Errorf("the repository has over %d branches and tags, more than Dockwell lists", maxRefs)
		}
		refs.Commits[fields[1]] = at
	}
}

func fetchV2(w io.Writer, r *bufio.Reader, f *os.File, commit string, depth int, most int64, caps v2Capabilities) (int64, error) {
	if caps.format != "sha1" {
		return 0, fmt.Errorf("the repository uses %s object IDs, where Dockwell reads SHA-1 alone", strings.ToUpper(caps.format))
	}
	var buf bytes.Buffer
	writePkt(&buf, "command=fetch\n")
	writePkt(&buf, "agent=dockwell\n")
	buf.WriteString("0001")
	writePkt(&buf, "no-progress\n")
	writePkt(&buf, "ofs-delta\n")
	if caps.shallow && depth > 0 {
		writePkt(&buf, fmt.Sprintf("deepen %d\n", depth))
	}
	writePkt(&buf, "want "+commit+"\n")
	writePkt(&buf, "done\n")
	buf.WriteString("0000")
	// Send final flush to close the v2 session cleanly after fetch completes.
	buf.WriteString("0000")
	if _, err := w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return readSidebandPack(r, f, most, true)
}

func fetchV1(w io.Writer, r *bufio.Reader, f *os.File, commit string, depth int, most int64, caps map[string]bool) (int64, error) {
	var wantCaps []string
	if caps["side-band-64k"] {
		wantCaps = append(wantCaps, "side-band-64k")
	} else if caps["side-band"] {
		wantCaps = append(wantCaps, "side-band")
	}
	if caps["ofs-delta"] {
		wantCaps = append(wantCaps, "ofs-delta")
	}
	if caps["no-progress"] {
		wantCaps = append(wantCaps, "no-progress")
	}
	var buf bytes.Buffer
	wantLine := "want " + commit
	if len(wantCaps) > 0 {
		wantLine += " " + strings.Join(wantCaps, " ")
	}
	writePkt(&buf, wantLine+"\n")
	if caps["shallow"] && depth > 0 {
		writePkt(&buf, fmt.Sprintf("deepen %d\n", depth))
	}
	buf.WriteString("0000")
	writePkt(&buf, "done\n")
	if _, err := w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	useSideband := caps["side-band-64k"] || caps["side-band"]
	if !useSideband {
		return readRawV1Pack(r, f, most)
	}
	return readSidebandPack(r, f, most, false)
}

func readRawV1Pack(r *bufio.Reader, f *os.File, most int64) (int64, error) {
	// Skip shallow / unshallow / NAK pkt-lines until raw PACK stream starts.
	for {
		peek, err := r.Peek(4)
		if err != nil {
			return 0, err
		}
		if string(peek) == "PACK" {
			break
		}
		line, kind, err := readPkt(r)
		if err != nil {
			return 0, err
		}
		if kind == pktData {
			text := strings.TrimSuffix(string(line), "\n")
			if strings.HasPrefix(text, "ERR ") {
				return 0, ErrRepositoryUnavailable
			}
			if text == "NAK" {
				break
			}
		}
	}
	n, err := io.Copy(f, io.LimitReader(r, most+1))
	if err != nil {
		return 0, err
	}
	if n > most {
		return 0, ErrTooLarge
	}
	if n == 0 {
		return 0, ErrRepositoryUnavailable
	}
	return n, nil
}

func readSidebandPack(r *bufio.Reader, f *os.File, most int64, expectPackfileHeader bool) (int64, error) {
	bw := bufio.NewWriter(f)
	var size int64
	inPack := !expectPackfileHeader
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		if kind == pktFlush {
			if inPack && size > 0 {
				break
			}
			continue
		}
		if kind != pktData {
			continue
		}
		if !inPack {
			text := strings.TrimSuffix(string(line), "\n")
			if strings.HasPrefix(text, "ERR ") {
				return 0, ErrRepositoryUnavailable
			}
			if text == "packfile" {
				inPack = true
			}
			continue
		}
		// In protocol v1, shallow/unshallow/NAK lines arrive before sideband bytes.
		if !expectPackfileHeader && size == 0 {
			text := strings.TrimSuffix(string(line), "\n")
			if text == "NAK" || strings.HasPrefix(text, "shallow ") || strings.HasPrefix(text, "unshallow ") || strings.HasPrefix(text, "ACK ") {
				continue
			}
			if strings.HasPrefix(text, "ERR ") {
				return 0, ErrRepositoryUnavailable
			}
		}
		switch line[0] {
		case 1:
			size += int64(len(line) - 1)
			if size > most {
				return 0, ErrTooLarge
			}
			if _, err := bw.Write(line[1:]); err != nil {
				return 0, err
			}
		case 2:
			// Progress channel: ignored.
		case 3:
			return 0, ErrRepositoryUnavailable
		}
	}
	if size == 0 {
		return 0, ErrRepositoryUnavailable
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return size, nil
}

func addRef(refs *Refs, text string) (bool, error) {
	commit, name, ok := strings.Cut(text, " ")
	if !ok || !commitID.MatchString(commit) || name == "capabilities^{}" {
		return false, nil
	}
	if name != "HEAD" && !strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/") {
		return false, nil
	}
	if tag, peeled := strings.CutSuffix(name, "^{}"); peeled {
		if _, exists := refs.Commits[tag]; !exists && len(refs.Commits) >= maxRefs {
			return true, fmt.Errorf("the repository has over %d branches and tags, more than Dockwell lists", maxRefs)
		}
		refs.Commits[tag] = commit
		return true, nil
	}
	if _, ok := refs.Commits[name]; !ok {
		if len(refs.Commits) >= maxRefs {
			return true, fmt.Errorf("the repository has over %d branches and tags, more than Dockwell lists", maxRefs)
		}
		refs.Commits[name] = commit
	} else if !strings.HasPrefix(name, "refs/tags/") {
		refs.Commits[name] = commit
	}
	return true, nil
}

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktEnd
)

func readPkt(r *bufio.Reader) ([]byte, pktKind, error) {
	return readPktBounded(r, 65536)
}

func readPktBounded(r *bufio.Reader, maxLen int) ([]byte, pktKind, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, 0, err
	}
	n, err := strconv.ParseUint(string(head[:]), 16, 16)
	if err != nil {
		return nil, 0, errors.New("a pkt-line's length can't be read")
	}
	switch n {
	case 0:
		return nil, pktFlush, nil
	case 1:
		return nil, pktDelim, nil
	case 2:
		return nil, pktEnd, nil
	case 3:
		return nil, 0, errors.New("a pkt-line is too short to hold anything")
	}
	if int(n-4) > maxLen {
		return nil, 0, ErrTooLarge
	}
	line := make([]byte, n-4)
	if _, err := io.ReadFull(r, line); err != nil {
		return nil, 0, err
	}
	if len(line) == 0 {
		return nil, 0, errors.New("a pkt-line holds nothing")
	}
	return line, pktData, nil
}

func writePkt(w *bytes.Buffer, line string) {
	fmt.Fprintf(w, "%04x%s", len(line)+4, line)
}

func mapSessionErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || isNetTimeout(err) {
		return ErrTimeout
	}
	return err
}

type deadlineReader struct {
	r    io.Reader
	conn net.Conn
	idle time.Duration
}

func (d deadlineReader) Read(p []byte) (int, error) {
	if d.conn != nil && d.idle > 0 {
		_ = d.conn.SetReadDeadline(time.Now().Add(d.idle))
	}
	return d.r.Read(p)
}

type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return len(p), nil
	}
	n := min(len(p), l.left)
	_, _ = l.w.Write(p[:n])
	l.left -= n
	return len(p), nil
}
