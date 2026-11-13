// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TreeLimits bound what a read of a commit's tree reads, as a repository is a
// stranger's. Another store's catalog keeps each app in a folder of its own, beside
// files a catalog never reads, such as 1Panel's archives of tens of MiB, so a tree is
// read in two rounds: its folders first, and then only the files asked for.
type TreeLimits struct {
	// Trees is the most the first round's pack may be: the commit and its folders,
	// with no file's contents where the host can leave them out.
	Trees int64
	// Pack is the most the second round's pack may be, of the files asked for. Where
	// the host can't leave contents out, the first round's pack holds every file,
	// and Pack bounds it instead.
	Pack int64
	// Entries is the most files and folders a walk may list.
	Entries int
	// Depth is the most folders deep a walk may go below where it starts.
	Depth int
	// Files is the most files one Read may ask for.
	Files int
	// File is the most one file may be. A file over it is left out of what Read
	// returns, and named in what it says was too big.
	File int64
	// Total is the most the files read together may be.
	Total int64
}

// TreeDefault are the limits another store's catalog is read within. 1Panel's app
// store, the largest of the three Dockwell reads, was 3,005 files in 1,183 folders
// on 11 Nov 2026, eight deep, and its converter reads 1,374 of them, 3.5 MiB.
var TreeDefault = TreeLimits{Trees: 16 << 20, Pack: 64 << 20, Entries: 20_000, Depth: 8, Files: 5_000, File: 1 << 20, Total: 32 << 20}

// A Snapshot is a commit's folders, read with the first round, and how to read the
// files in them. Close removes what it keeps on disk.
type Snapshot struct {
	// Commit is the commit, in full.
	Commit string
	// Time is when it was committed, as its committer line says.
	Time time.Time

	lim    TreeLimits
	via    doer
	u      *url.URL
	trees  *pack
	file   *os.File
	tree   string // the commit's tree
	whole  bool   // the first round's pack holds every file, as the host can't filter
	closed bool
}

// An Entry is a file or folder a walk lists.
type Entry struct {
	// Path is its path from the repository's top, such as apps/memos/data.yml.
	Path string
	// Dir says it's a folder. Anything else listed is a regular file: a link and a
	// submodule are left out of a walk, as no read follows one.
	Dir bool
	// sha is its object's name.
	sha string
}

// Trees reads the commit of the repository at repo with the first round: the commit
// and every folder of it, and no file's contents where the host can leave them out,
// as Git's partial clone does. Each object is checked against its name, as Fetch
// checks them.
//
// A nil client is netguard's, as for Fetch.
func Trees(ctx context.Context, client *http.Client, repo, commit string, lim TreeLimits) (*Snapshot, error) {
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
	// A filter of blob:limit=1 leaves out every file of a byte or more, so the
	// pack holds the commit, its folders, and empty files, which cost nothing.
	most, filter := lim.Pack, int64(0)
	if can.filter {
		most, filter = lim.Trees, 1
	}
	f, size, err := fetchPack(ctx, via, u, commit, most, filter)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Commit: commit, lim: lim, via: via, u: u, file: f, whole: !can.filter}
	p, err := indexPack(ctx, f, size, 4*most)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("reading what %s sent: %w", u.Host, err)
	}
	s.trees = p
	c, err := p.object(commit, "commit", maxObject)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("reading what %s sent: %w", u.Host, missing(err, "commit "+commit))
	}
	if s.tree, s.Time, err = readCommit(c); err != nil {
		s.Close()
		return nil, fmt.Errorf("the commit %s %w", commit, err)
	}
	return s, nil
}

// Close removes the first round's pack.
func (s *Snapshot) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.file != nil {
		_ = s.file.Close()
		_ = os.Remove(s.file.Name())
	}
}

// readCommit reads a commit's tree and when it was committed.
func readCommit(c []byte) (string, time.Time, error) {
	var tree string
	var at time.Time
	for line := range strings.Lines(string(c)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break // the message follows
		}
		switch {
		case strings.HasPrefix(line, "tree "):
			tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "committer "):
			// committer Name <email> 1731400000 +0100
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if secs, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					at = time.Unix(secs, 0).UTC()
				}
			}
		}
	}
	if len(tree) != 40 || !hexOnly(tree) {
		return "", time.Time{}, errors.New("names no tree")
	}
	return tree, at, nil
}

func hexOnly(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}

// Walk lists the files and folders under dir, and under each folder below it that
// descend says to go into, which it's asked of with the folder's path and how many
// folders deep it is below dir, from 1. A link and a submodule aren't listed. A dir
// the commit doesn't have, or a walk past the limits, is an error.
func (s *Snapshot) Walk(dir string, descend func(path string, depth int) bool) ([]Entry, error) {
	clean := strings.Trim(path.Clean("/"+dir), "/")
	tree := s.tree
	if clean != "" {
		for _, part := range strings.Split(clean, "/") {
			entries, err := readTree(s.trees, tree)
			if err != nil {
				return nil, err
			}
			i := slices.IndexFunc(entries, func(e treeEntry) bool { return e.name == part && e.mode == "40000" })
			if i < 0 {
				return nil, fmt.Errorf("the commit %s has no folder %s. Check the source's folder", s.Commit, clean)
			}
			tree = entries[i].sha
		}
	}
	var out []Entry
	var walk func(tree, at string, depth int) error
	walk = func(tree, at string, depth int) error {
		entries, err := readTree(s.trees, tree)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := path.Join(at, e.name)
			switch e.mode {
			case "40000":
			case "100644", "100755":
			default:
				continue // a link, 120000, or a submodule, 160000
			}
			if len(out) == s.lim.Entries {
				return fmt.Errorf("the commit %s holds over %d files and folders under %s, more than a source reads", s.Commit, s.lim.Entries, cmpOr(clean, "its top"))
			}
			out = append(out, Entry{Path: p, Dir: e.mode == "40000", sha: e.sha})
			if e.mode != "40000" || descend == nil || !descend(p, depth+1) {
				continue
			}
			if depth+1 > s.lim.Depth {
				return fmt.Errorf("the commit %s has folders over %d deep under %s, more than a source reads", s.Commit, s.lim.Depth, cmpOr(clean, "its top"))
			}
			if err := walk(e.sha, p, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree, clean, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// sizeWords says a size as people read one: 32 MiB, 512 KiB or 10 bytes.
func sizeWords(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Read returns the files listed, by path, with the second round: one fetch of
// exactly their objects, each checked against the name its folder gives it. A file
// over the limit of one is left out of what's returned, and named in big, so whoever
// reads it can say why, file by file. Entries that are folders are ignored.
func (s *Snapshot) Read(ctx context.Context, files []Entry) (out map[string][]byte, big []string, err error) {
	return s.ReadWithin(ctx, files, ReadLimits{Files: s.lim.Files, File: s.lim.File, Total: s.lim.Total})
}

// ReadLimits bound one Read: how many files it asks for, how large each may be, and
// all of them together.
type ReadLimits struct {
	Files       int
	File, Total int64
}

// ReadWithin is Read within limits of its own, such as a catalog's images', which
// count apart from the files an adapter reads. The pack it fetches is held to the
// snapshot's Pack, as Read's is.
func (s *Snapshot) ReadWithin(ctx context.Context, files []Entry, lim ReadLimits) (out map[string][]byte, big []string, err error) {
	var wanted []Entry
	for _, e := range files {
		if !e.Dir {
			wanted = append(wanted, e)
		}
	}
	if len(wanted) > lim.Files {
		return nil, nil, fmt.Errorf("it asks for %d files of the commit %s, more than the %d a source reads", len(wanted), s.Commit, lim.Files)
	}
	from := s.trees
	if !s.whole {
		// Empty files came in the first round, and asking for what's there already
		// costs a host nothing to leave out.
		var names []string
		for _, e := range wanted {
			if _, err := s.trees.object(e.sha, "blob", 0); err != nil {
				names = append(names, e.sha)
			}
		}
		slices.Sort(names)
		names = slices.Compact(names)
		if len(names) > 0 {
			p, closeIt, err := s.fetchBlobs(ctx, names)
			if err != nil {
				return nil, nil, err
			}
			defer closeIt()
			from = p
		}
	}
	out = map[string][]byte{}
	var total int64
	for _, e := range wanted {
		b, err := s.trees.object(e.sha, "blob", 0)
		if err != nil {
			b, err = from.object(e.sha, "blob", lim.File)
		}
		var tb tooBig
		switch {
		case errors.As(err, &tb):
			big = append(big, e.Path)
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("reading what %s sent: %w", s.u.Host, missing(err, e.Path))
		}
		if total += int64(len(b)); total > lim.Total {
			return nil, nil, fmt.Errorf("the files read of the commit %s come to over %s, more than a source reads", s.Commit, sizeWords(lim.Total))
		}
		out[e.Path] = b
	}
	return out, big, nil
}

// fetchBlobs asks for objects by name, as Git's partial clone fetches what its first
// round left out, and indexes what comes back.
func (s *Snapshot) fetchBlobs(ctx context.Context, names []string) (*pack, func(), error) {
	var body bytes.Buffer
	for _, line := range []string{"command=fetch\n", "agent=dockwell\n"} {
		writePkt(&body, line)
	}
	body.WriteString("0001")
	for _, line := range []string{"no-progress\n", "ofs-delta\n"} {
		writePkt(&body, line)
	}
	for _, n := range names {
		writePkt(&body, "want "+n+"\n")
	}
	writePkt(&body, "done\n")
	body.WriteString("0000")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.u.String()+"/git-upload-pack", &body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	what := fmt.Sprintf("%d files of %s", len(names), s.Commit)
	resp, err := s.via.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching %s from %s: %w", what, s.u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s answered %s when asked for %s", s.u.Host, status(resp), what)
	}
	f, err := os.CreateTemp("", "dockwell-pack-*")
	if err != nil {
		return nil, nil, fmt.Errorf("keeping what %s sends: %w", s.u.Host, err)
	}
	closeIt := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	size, err := receivePack(io.LimitReader(resp.Body, s.lim.Pack+1<<20), f, s.u, what, s.lim.Pack)
	if err != nil {
		closeIt()
		return nil, nil, err
	}
	p, err := indexPack(ctx, f, size, 4*s.lim.Pack)
	if err != nil {
		closeIt()
		return nil, nil, fmt.Errorf("reading what %s sent: %w", s.u.Host, err)
	}
	return p, closeIt, nil
}
