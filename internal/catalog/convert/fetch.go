// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/gitfetch"
)

// A Fetched commit is what the control plane reads of a store's commit: the
// listing its adapter reads, the store's source as its entries say it, with the
// licence read from the repository's own LICENSE, and when the commit was made.
type Fetched struct {
	Listing *Listing
	Source  Source
	Time    time.Time
	// Snapshot is the commit, still open, and ByPath each file its folders list,
	// by its path in the store's folder, for a round of reads after the adapter's,
	// such as the logos of the apps that convert (ADR 0083). Close closes it.
	Snapshot *gitfetch.Snapshot
	ByPath   map[string]gitfetch.Entry
}

// Close closes the commit Fetch read.
func (f Fetched) Close() {
	if f.Snapshot != nil {
		f.Snapshot.Close()
	}
}

// RepositoryLicense is the licence a repository's licence file states, as an SPDX
// identifier: NONE where it has no licence file, and NOASSERTION where its file is
// none Dockwell recognises, as SPDX says the two (ADR 0083).
func RepositoryLicense(hasFile bool, file []byte) string {
	if !hasFile {
		return "NONE"
	}
	if found := LicenseOf(file); found != "" {
		return found
	}
	return "NOASSERTION"
}

// Fetch reads a store's commit in two rounds, as the control plane does (ADR 0078):
// its folders, with no file's contents, and then only the files its adapter reads,
// and the repository's licence at its top. dir is the folder in the repository the
// store is laid out in, empty for its top. A nil client is netguard's, as for
// gitfetch.
func Fetch(ctx context.Context, client *http.Client, store Store, repo, commit, dir string) (Fetched, error) {
	snap, err := gitfetch.Trees(ctx, client, repo, commit, gitfetch.TreeDefault)
	if err != nil {
		return Fetched{}, err
	}
	kept := false
	defer func() {
		if !kept {
			snap.Close()
		}
	}()
	prefix := strings.Trim(path.Clean("/"+dir), "/")
	rel := func(p string) string {
		if prefix == "" {
			return p
		}
		return strings.TrimPrefix(p, prefix+"/")
	}
	listed, err := snap.Walk(prefix, func(p string, depth int) bool { return store.Descend(rel(p), depth) })
	if err != nil {
		return Fetched{}, err
	}
	l := &Listing{Dirs: map[string]bool{}, Files: map[string]bool{}, Big: map[string]bool{}}
	var want []gitfetch.Entry
	byPath := map[string]gitfetch.Entry{}
	for _, e := range listed {
		if e.Dir {
			l.Dirs[rel(e.Path)] = true
			continue
		}
		l.Files[rel(e.Path)] = true
		byPath[rel(e.Path)] = e
		if store.Want(rel(e.Path)) {
			want = append(want, e)
		}
	}
	// The repository's own licence, at its top, which every entry keeps.
	top, err := snap.Walk("", nil)
	if err != nil {
		return Fetched{}, err
	}
	var licenseFile []gitfetch.Entry
	for _, name := range LicenseFiles {
		if i := slices.IndexFunc(top, func(e gitfetch.Entry) bool { return e.Path == name && !e.Dir }); i >= 0 {
			licenseFile = append(licenseFile, top[i])
			break
		}
	}
	files, big, err := snap.Read(ctx, append(want, licenseFile...))
	if err != nil {
		return Fetched{}, err
	}
	l.Read = map[string][]byte{}
	for p, b := range files {
		l.Read[rel(p)] = b
	}
	for _, p := range big {
		l.Big[rel(p)] = true
	}
	src := Source{Name: store.SourceName(repo), Repository: repo, Commit: commit}
	var text []byte
	if len(licenseFile) > 0 {
		text = files[licenseFile[0].Path]
	}
	if src.License = RepositoryLicense(len(licenseFile) > 0, text); src.License != "NONE" && src.License != "NOASSERTION" {
		src.LicenseText, src.CopyrightInLicense = text, src.License == "MIT"
	}
	kept = true
	return Fetched{Listing: l, Source: src, Time: snap.Time, Snapshot: snap, ByPath: byPath}, nil
}
