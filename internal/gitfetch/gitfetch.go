// SPDX-License-Identifier: AGPL-3.0-only

// Package gitfetch reads one commit of a Git repository over Git's smart HTTP
// protocol, version 2, as git fetch --depth 1 does, and the regular files in one
// of its folders. It checks every object it reads against its name, from the commit
// down, so what it returns is that commit whatever the host sends. It speaks only
// what that takes, and no more of Git (docs/adr/0055-catalogs-from-a-git-url.md).
package gitfetch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

// Limits bound what a fetch reads, as a repository is a stranger's.
type Limits struct {
	// Pack is the most the commit's pack may be.
	Pack int64
	// Files is how many files the folder may hold.
	Files int
	// File is the most one file may be.
	File int64
	// Total is the most the folder's files may be together, or no more than Files
	// of File each when 0.
	Total int64
}

// Default are the limits a catalog's source reads within: entries are a few KiB
// each, so 2,000 of them come to a few MiB.
var Default = Limits{Pack: 64 << 20, Files: 2000, File: 1 << 20, Total: 16 << 20}

// Commit is a full commit ID, as a source pins one.
var Commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Fetch reads the regular files in folder dir of the repository at repo, at commit,
// by their names in it. A file in a folder under dir, and a link, are left out.
//
// A nil client is netguard's, which connects only to public addresses, takes no
// proxy and follows no redirect, as a source's URL is a person's to name. A test
// passes a client of its own to reach a repository it serves.
func Fetch(ctx context.Context, client *http.Client, repo, commit, dir string, lim Limits) (map[string][]byte, error) {
	if !Commit.MatchString(commit) {
		return nil, fmt.Errorf("%q isn't a commit. Name one in full, as 40 hex digits", commit)
	}
	u, err := CheckURL(repo)
	if err != nil {
		return nil, err
	}
	via := guarded(client, lim.Pack+1<<20)
	can, err := v2(ctx, via, u)
	if err != nil {
		return nil, err
	}
	// A host that can filter leaves out each file over the limit of one, which the
	// folder couldn't take anyway, so a catalog in a large repository fits.
	filter := int64(0)
	if can.filter {
		filter = lim.File
	}
	f, size, err := fetchPack(ctx, via, u, commit, lim.Pack, filter)
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	p, err := indexPack(ctx, f, size, 4*lim.Pack)
	if err != nil {
		return nil, fmt.Errorf("reading what %s sent: %w", u.Host, err)
	}
	p.filtered = filter > 0
	out, err := files(p, commit, dir, lim)
	if err != nil {
		return nil, fmt.Errorf("reading what %s sent: %w", u.Host, err)
	}
	return out, nil
}

// doer sends a request: an http.Client, or netguard's, which has Do alone.
type doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// backstop is the longest netguard's client waits for one answer. The caller's
// context is the deadline that counts; this ends a read whose caller set none.
const backstop = 2 * time.Minute

// guarded is the client a repository is read with: the one given, or netguard's,
// which connects only to public addresses, takes no proxy and reads answers of most
// bytes at most, and following no redirect, whatever the client, as one could lead
// anywhere: a host that moved says so in its answer, which names its status alone.
func guarded(client *http.Client, most int64) doer {
	if client == nil {
		return netguard.Client(netguard.Options{MaxBody: most, Timeout: backstop})
	}
	follows := *client
	follows.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &follows
}

// status is an answer's status in Go's words for its code, never the host's own,
// which could say anything to the person reading the error.
func status(resp *http.Response) string {
	return strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
}

// Refs lists the branches and tags of the repository at repo, by name, such as
// refs/heads/main, with the commit each is at, a tag's peeled to its commit.
func Refs(ctx context.Context, client *http.Client, repo string) (map[string]string, error) {
	l, err := List(ctx, client, repo)
	if err != nil {
		return nil, err
	}
	return l.Refs, nil
}

// A Listing is a repository's branches and tags, and the branch its HEAD names.
type Listing struct {
	// Refs are the branches and tags, by name, such as refs/heads/main, with the
	// commit each is at, a tag's peeled to its commit.
	Refs map[string]string
	// Head is the branch HEAD names, its default, such as refs/heads/main, or ""
	// when it names none.
	Head string
}

// List lists the branches and tags of the repository at repo, and which branch is
// its default.
func List(ctx context.Context, client *http.Client, repo string) (Listing, error) {
	u, err := CheckURL(repo)
	if err != nil {
		return Listing{}, err
	}
	via := guarded(client, 4<<20)
	if _, err := v2(ctx, via, u); err != nil {
		return Listing{}, err
	}
	var body bytes.Buffer
	for _, line := range []string{"command=ls-refs\n", "agent=dockwell\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0001")
	for _, line := range []string{"peel\n", "symrefs\n", "ref-prefix HEAD\n", "ref-prefix refs/heads/\n", "ref-prefix refs/tags/\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+"/git-upload-pack", &body)
	if err != nil {
		return Listing{}, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := via.Do(req)
	if err != nil {
		return Listing{}, fmt.Errorf("asking %s for its branches and tags: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Listing{}, fmt.Errorf("%s answered %s when asked for its branches and tags", u.Host, status(resp))
	}
	r := bufio.NewReader(resp.Body)
	out := Listing{Refs: map[string]string{}}
	for {
		line, kind, err := readPkt(r)
		switch {
		case errors.Is(err, io.EOF) || kind == pktFlush && err == nil:
			return out, nil
		case err != nil:
			return Listing{}, fmt.Errorf("reading %s's branches and tags: %w", u.Host, err)
		case kind != pktData:
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) < 2 || !Commit.MatchString(fields[0]) {
			return Listing{}, fmt.Errorf("reading %s's branches and tags: a line isn't one", u.Host)
		}
		at := fields[0]
		for _, attr := range fields[2:] {
			if peeled, ok := strings.CutPrefix(attr, "peeled:"); ok && Commit.MatchString(peeled) {
				at = peeled
			}
			// HEAD names the branch a clone checks out: the repository's default.
			if target, ok := strings.CutPrefix(attr, "symref-target:"); ok && fields[1] == "HEAD" && strings.HasPrefix(target, "refs/heads/") {
				out.Head = target
			}
		}
		if fields[1] == "HEAD" {
			continue
		}
		if len(out.Refs) == 10000 {
			return Listing{}, fmt.Errorf("%s has over 10,000 branches and tags, more than Dockwell lists", u.Host)
		}
		out.Refs[fields[1]] = at
	}
}

// CheckURL says whether a repository's URL is one a source may name: HTTPS, with no
// credentials in it, to no address or name netguard refuses. Where a name leads is
// checked again as each connection is made.
func CheckURL(repo string) (*url.URL, error) {
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q isn't a repository's URL. Name one such as https://github.com/example/catalog", repo)
	}
	switch {
	case u.User != nil:
		return nil, errors.New("the repository's URL holds credentials, which a source can't keep yet. Name a public repository")
	case u.Scheme != "https":
		return nil, fmt.Errorf("%q isn't over HTTPS. Name the repository's https:// URL", repo)
	}
	if why := netguard.Forbidden(u.Hostname(), netguard.Options{}); why != "" {
		return nil, fmt.Errorf("%s is %s, so Dockwell reads no source there. Name a public repository", u.Hostname(), why)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q has a query or a fragment, which a repository's URL doesn't", repo)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// capable is what a repository can do beyond what a source needs of it.
type capable struct {
	// filter is that it can leave out what a fetch filters.
	filter bool
}

// v2 asks the repository what it speaks, and refuses one that doesn't speak version 2,
// or can't fetch shallow.
func v2(ctx context.Context, client doer, u *url.URL) (capable, error) {
	var can capable
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return can, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := client.Do(req)
	if err != nil {
		return can, fmt.Errorf("asking %s for the repository: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return can, fmt.Errorf("%s answered %s for %s. Check it's a public repository's URL", u.Host, status(resp), u.Path)
	}
	r := bufio.NewReader(io.LimitReader(resp.Body, 1<<20))
	version, shallow, format := false, false, "sha1"
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return can, fmt.Errorf("reading what %s speaks: %w", u.Host, err)
		}
		if kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(line), "\n")
		switch {
		case text == "version 2":
			version = true
		case strings.HasPrefix(text, "fetch"):
			features := " " + strings.TrimPrefix(text, "fetch=") + " "
			shallow, can.filter = strings.Contains(features, " shallow "), strings.Contains(features, " filter ")
		case strings.HasPrefix(text, "object-format="):
			format = strings.TrimPrefix(text, "object-format=")
		}
	}
	switch {
	case !version:
		return can, fmt.Errorf("%s doesn't speak version 2 of Git's protocol, which a source is read with. Host the catalog where Git 2.18 or later serves it, such as GitHub, GitLab or Gitea", u.Host)
	case !shallow:
		return can, fmt.Errorf("%s can't send one commit alone, which a source is read as. Host the catalog where Git 2.18 or later serves it, such as GitHub, GitLab or Gitea", u.Host)
	case format != "sha1":
		return can, fmt.Errorf("the repository at %s names its objects by %s, where a source reads SHA-1 alone yet. Name a repository made with Git's default", u.Host, strings.ToUpper(format))
	}
	return can, nil
}

// fetchPack asks for the one commit, shallow, leaving out blobs over filter bytes
// unless it's 0, and keeps the pack it sends in a file of its own, which the caller
// removes, rather than in memory.
func fetchPack(ctx context.Context, client doer, u *url.URL, commit string, most, filter int64) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "dockwell-pack-*")
	if err != nil {
		return nil, 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
	}
	size, err := fetchPackTo(ctx, client, u, commit, most, filter, f)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, err
	}
	return f, size, nil
}

func fetchPackTo(ctx context.Context, client doer, u *url.URL, commit string, most, filter int64, f *os.File) (int64, error) {
	var body bytes.Buffer
	for _, line := range []string{"command=fetch\n", "agent=dockwell\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0001")
	lines := []string{"no-progress\n", "ofs-delta\n", "deepen 1\n"}
	if filter > 0 {
		lines = append(lines, fmt.Sprintf("filter blob:limit=%d\n", filter))
	}
	for _, line := range append(lines, "want "+commit+"\n", "done\n") {
		writePkt(&body, line)
	}
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+"/git-upload-pack", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetching %s from %s: %w", commit, u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s when asked for %s", u.Host, status(resp), commit)
	}
	r := bufio.NewReader(resp.Body)
	w := bufio.NewWriter(f)
	var size int64
	inPack := false
	for {
		line, kind, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, fmt.Errorf("reading what %s sent: %w", u.Host, err)
		}
		if kind == pktFlush && inPack {
			break
		}
		if kind != pktData {
			continue
		}
		if !inPack {
			text := strings.TrimSuffix(string(line), "\n")
			if strings.HasPrefix(text, "ERR ") {
				// What the host says isn't repeated, as it's a stranger's words.
				return 0, fmt.Errorf("%s refused to send %s. Check the commit is in the repository", u.Host, commit)
			}
			inPack = text == "packfile"
			continue
		}
		switch line[0] {
		case 1:
			if size += int64(len(line) - 1); size > most {
				return 0, fmt.Errorf("the commit %s's files are over %d MiB, more than a source reads. Keep the catalog in a repository of its own", commit, most>>20)
			}
			if _, err := w.Write(line[1:]); err != nil {
				return 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
			}
		case 3:
			return 0, fmt.Errorf("%s stopped sending %s. Try again, or check the repository", u.Host, commit)
		}
	}
	if size == 0 {
		return 0, fmt.Errorf("%s sent no objects for %s. Check the commit is in the repository", u.Host, commit)
	}
	if err := w.Flush(); err != nil {
		return 0, fmt.Errorf("keeping what %s sends: %w", u.Host, err)
	}
	return size, nil
}

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktEnd
)

// readPkt reads one pkt-line: four hex digits of length, then the line.
func readPkt(r *bufio.Reader) ([]byte, pktKind, error) {
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

// applyDelta makes an object of a delta and its base.
func applyDelta(base, delta []byte, most int64) ([]byte, error) {
	r := bytes.NewReader(delta)
	size := func() (int64, error) {
		var n int64
		for shift := 0; ; shift += 7 {
			c, err := r.ReadByte()
			if err != nil || shift > 56 {
				return 0, errors.New("a delta can't be read")
			}
			n |= int64(c&0x7f) << shift
			if c&0x80 == 0 {
				return n, nil
			}
		}
	}
	from, err := size()
	if err != nil {
		return nil, err
	}
	to, err := size()
	if err != nil {
		return nil, err
	}
	if from != int64(len(base)) || to > most {
		return nil, errors.New("a delta doesn't fit its base")
	}
	out := make([]byte, 0, to)
	for r.Len() > 0 {
		op, _ := r.ReadByte()
		if op&0x80 == 0 {
			if op == 0 {
				return nil, errors.New("a delta has an instruction Git doesn't")
			}
			chunk := make([]byte, op)
			if _, err := io.ReadFull(r, chunk); err != nil {
				return nil, errors.New("a delta ends inside what it adds")
			}
			out = append(out, chunk...)
			continue
		}
		var offset, n int64
		for i := range 4 {
			if op&(1<<i) != 0 {
				c, err := r.ReadByte()
				if err != nil {
					return nil, errors.New("a delta can't be read")
				}
				offset |= int64(c) << (8 * i)
			}
		}
		for i := range 3 {
			if op&(0x10<<i) != 0 {
				c, err := r.ReadByte()
				if err != nil {
					return nil, errors.New("a delta can't be read")
				}
				n |= int64(c) << (8 * i)
			}
		}
		if n == 0 {
			n = 0x10000
		}
		if offset+n > int64(len(base)) {
			return nil, errors.New("a delta copies past its base")
		}
		out = append(out, base[offset:offset+n]...)
	}
	if int64(len(out)) != to {
		return nil, errors.New("a delta makes other than its size says")
	}
	return out, nil
}

// files reads the commit, walks its tree to dir, and returns its regular files.
func files(p *pack, commit, dir string, lim Limits) (map[string][]byte, error) {
	c, err := p.object(commit, "commit", maxObject)
	if err != nil {
		return nil, missing(err, "commit "+commit)
	}
	tree, _, _ := strings.Cut(strings.TrimPrefix(string(c), "tree "), "\n")
	if !strings.HasPrefix(string(c), "tree ") || len(tree) != 40 {
		return nil, fmt.Errorf("the commit %s names no tree", commit)
	}
	clean := strings.Trim(path.Clean("/"+dir), "/")
	if clean != "" {
		for _, part := range strings.Split(clean, "/") {
			entries, err := readTree(p, tree)
			if err != nil {
				return nil, err
			}
			next, ok := "", false
			for _, e := range entries {
				if e.name == part && e.mode == "40000" {
					next, ok = e.sha, true
				}
			}
			if !ok {
				return nil, fmt.Errorf("the commit %s has no folder %s. Check the source's folder", commit, clean)
			}
			tree = next
		}
	}
	entries, err := readTree(p, tree)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	var total int64
	for _, e := range entries {
		// A regular file, as 100644 or 100755. A link, 120000, and a submodule,
		// 160000, aren't.
		if e.mode != "100644" && e.mode != "100755" {
			continue
		}
		if len(out) == lim.Files {
			return nil, fmt.Errorf("the folder %s holds over %d files, more than a source reads", clean, lim.Files)
		}
		b, err := p.object(e.sha, "blob", lim.File)
		var big tooBig
		switch {
		case errors.As(err, &big), errors.Is(err, errMissing) && p.filtered:
			// A filtered fetch leaves out a file over the limit of one, so one
			// that's missing is over it, unless the host left it out wrongly.
			return nil, fmt.Errorf("%s is over %d KiB, more than a source reads of one file", path.Join(clean, e.name), lim.File>>10)
		case err != nil:
			return nil, missing(err, path.Join(clean, e.name))
		}
		if total += int64(len(b)); lim.Total > 0 && total > lim.Total {
			return nil, fmt.Errorf("the folder %s holds over %d MiB of files, more than a source reads. Keep the catalog's entries alone in its folder", clean, lim.Total>>20)
		}
		out[e.name] = b
	}
	return out, nil
}

// missing says what of the commit wasn't in what was sent, or why it couldn't be read.
func missing(err error, what string) error {
	if errors.Is(err, errMissing) {
		return fmt.Errorf("what was sent holds no %s, so it isn't that commit. Check the commit is in the repository", what)
	}
	return err
}

type treeEntry struct{ mode, name, sha string }

func readTree(p *pack, sha string) ([]treeEntry, error) {
	data, err := p.object(sha, "tree", maxObject)
	if err != nil {
		return nil, missing(err, "tree "+sha)
	}
	var out []treeEntry
	seen := map[string]bool{}
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || len(data) < nul+21 {
			return nil, fmt.Errorf("the tree %s can't be read", sha)
		}
		name := string(data[sp+1 : nul])
		switch {
		case !entryName(name):
			return nil, fmt.Errorf("the tree %s has an entry named %q, which Git refuses", sha, name)
		case seen[name]:
			return nil, fmt.Errorf("the tree %s has two entries named %q, which Git refuses", sha, name)
		}
		seen[name] = true
		out = append(out, treeEntry{mode: string(data[:sp]), name: name, sha: hex.EncodeToString(data[nul+1 : nul+21])})
		data = data[nul+21:]
	}
	return out, nil
}

// entryName says a tree's entry has a name Git would check out: not empty, . or ..,
// and no .git, in any case, with no slash or backslash in it, so no name read from a
// tree reaches past its folder.
func entryName(name string) bool {
	switch {
	case name == "", name == ".", name == "..", strings.EqualFold(name, ".git"):
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}
