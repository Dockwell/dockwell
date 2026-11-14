// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// fetchWait bounds reading a source's commit, so a host that doesn't answer
	// leaves a request with an answer.
	fetchWait = 60 * time.Second
	// unreadWait is how long a source that couldn't be read stays unread before
	// the catalog tries it again, so a host that's down doesn't make every view of
	// the catalog wait for it.
	unreadWait = 5 * time.Minute
	// plansKept is how many commits read for plans are kept, the newest, so a plan
	// that's applied finds its commit read.
	plansKept = 4
	// mostReads is how many reads of sources are kept in memory, and mostGuesses how
	// many layouts of commits.
	mostReads   = 64
	mostGuesses = 64
)

// Sources reads the catalogs Dockwell reads beside its own, each at its commit
// (docs/adr/0055-catalogs-from-a-git-url.md). What it fetches it keeps in Dir, by
// commit, as a commit never changes: in plans/ for a plan, the newest plansKept of
// them, and in kept/ for each source that's applied, and nothing else. Empty keeps
// nothing between runs.
type Sources struct {
	Dir    string
	Client *http.Client
	// Fetch reads a commit's folder, gitfetch.Fetch unless a test sets it.
	Fetch func(ctx context.Context, client *http.Client, url, commit, dir string, lim gitfetch.Limits) (map[string][]byte, error)
	// Refs lists a repository's branches and tags, and its default branch,
	// gitfetch.List unless a test sets it.
	Refs func(ctx context.Context, client *http.Client, url string) (gitfetch.Listing, error)
	// Now is the time, time.Now unless a test sets it.
	Now func() time.Time

	mu       sync.Mutex
	read     map[string]sourceRead
	unread   map[string]unread
	fetching map[string]*fetching
	// checking are the checks of sources' entries under way, by what they read, which
	// a read of the same waits for, so readers at once share one check.
	checking map[string]*checkingRead
	// beforeCheck, where a test sets it, runs as a check starts, before it reads.
	beforeCheck func()
	// images are images as served, by what each file holds, and imageBytes what they
	// come to (served).
	images     map[[32]byte]servedImage
	imageBytes int
	// guessed are the layouts read of commits, by repository, folder and commit, as a
	// form asks again and again of the one a person is typing, and detecting is the
	// one read of a commit's folders that's made at a time for it.
	guessed   map[string]guessed
	detecting chan struct{}
	// slot holds the one fetch the control plane makes at a time, as a pack's index
	// and what's made of it take memory while it's read, and reads at once add up.
	slot chan struct{}
}

// checkWait is the most checking a commit's entries may take, each read as an install
// would read it, apart from whoever asked: a commit of a store's 2,000 apps checks in
// well under it. A test shortens it.
var checkWait = 3 * time.Minute

// settled says why a check's read can't be kept, if it can't: the check ran out of
// its time, cut, or an entry's Compose file wasn't read as the reader was busy.
func settled(got sourceRead, cut error) error {
	switch {
	case cut != nil:
		return fmt.Errorf("checking its entries took over %s, so Dockwell checks them again at the next read: %w", checkWait, cut)
	case got.unsettled:
		return errors.New("Dockwell was too busy reading other Compose files to check all of its apps, so it checks them again at the next read") //nolint:staticcheck // it starts with a name
	}
	return nil
}

// checkingRead is a check of a source's entries under way, which a read of the same
// commit and IDs before it waits for.
type checkingRead struct {
	done chan struct{}
	got  sourceRead
	err  error
}

// fetching is a fetch under way, which a read of the same commit waits for.
type fetching struct {
	done  chan struct{}
	files commitFiles
	err   error
}

// commitFiles is what the catalog reads of a source's commit: its entries' files,
// by name, and the images in its folder media, by name, with whether each passed
// its check of what its first bytes say it is, and why not. For another store's
// commit, converted as it was read, leftOut is the record of what the converter left
// out (leftOutFile). mediaKept says the commit's images were kept with it, as a
// commit kept before sources held images has none.
type commitFiles struct {
	entries   map[string][]byte
	media     map[string]error
	leftOut   []byte
	mediaKept bool
}

// images are the names of the images that passed their checks.
func (f commitFiles) images() map[string]bool {
	out := map[string]bool{}
	for name, err := range f.media {
		out[name] = err == nil
	}
	return out
}

// unread is a source's commit that couldn't be read, and when.
type unread struct {
	err error
	at  time.Time
}

func (s *Sources) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// folders are where a commit of a source is kept: for a source that's applied, and
// for a plan.
func (s *Sources) folders(spec *resourcev1.CatalogSource) (kept, plan string) {
	if s.Dir == "" {
		return "", ""
	}
	key := keyOf(spec)
	return filepath.Join(s.Dir, "kept", key, spec.GetCommit()), filepath.Join(s.Dir, "plans", key, spec.GetCommit())
}

// keyOf names a source's repository and folder, and the format it's read as when it's
// another store's, in where its commits are kept, and in the paths its images are
// served at.
func keyOf(spec *resourcev1.CatalogSource) string {
	sum := sha256.Sum256([]byte(spec.GetRepository() + "\x00" + spec.GetPath() + formatKey(spec)))
	return hex.EncodeToString(sum[:8])
}

// sourceRead is what came of reading a source: the entries the catalog shows, and
// what it leaves out.
type sourceRead struct {
	entries []*catalogv1.Entry
	// also are its entries of apps a catalog before it has, as another choice of
	// each (ADR 0078).
	also    []*catalogv1.Entry
	refused []*dockwellv1.CatalogSourceRefusal
	// apps and license are how many apps another store's commit holds, and the
	// licence its repository says, for a source converted as it's read.
	apps    int
	license string
	// unsettled says an entry's Compose file wasn't read for a reason of the server's
	// (folder.Result.Unsettled), so the read is kept for nobody.
	unsettled bool
}

// files returns a commit's files in a source's folder: as kept, or fetched and kept
// for a plan, until tidy keeps it for the source it's applied to. A read of a commit
// that's being fetched waits for that fetch, and fetches run one at a time.
func (s *Sources) files(ctx context.Context, spec *resourcev1.CatalogSource) (commitFiles, error) {
	if files, ok := s.kept(spec); ok {
		return files, nil
	}
	key := unreadKey(spec)
	s.mu.Lock()
	if f, ok := s.fetching[key]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.files, f.err
		case <-ctx.Done():
			return commitFiles{}, ctx.Err()
		}
	}
	if s.fetching == nil {
		s.fetching = map[string]*fetching{}
		s.slot = make(chan struct{}, 1)
	}
	f := &fetching{done: make(chan struct{})}
	s.fetching[key] = f
	slot := s.slot
	s.mu.Unlock()
	// The fetch is every waiting read's, so one that gives up doesn't end it.
	f.files, f.err = s.fetchOne(context.WithoutCancel(ctx), slot, spec)
	s.mu.Lock()
	delete(s.fetching, key)
	s.mu.Unlock()
	close(f.done)
	return f.files, f.err
}

// kept is a commit's files as kept, if they are: Dockwell/catalog's commit this build
// holds, from the snapshot, or one read before, from where it's kept. What an older
// converter kept of another store's commit isn't, as this one may convert more of it,
// or less: it's converted again, and what was kept stays until that succeeds
// (staleKept). A commit of Dockwell's format kept before sources held images is read
// again, as a whole.
func (s *Sources) kept(spec *resourcev1.CatalogSource) (commitFiles, bool) {
	if files, ok := snapshotFiles(spec); ok {
		return files, true
	}
	kept, plan := s.folders(spec)
	if kept == "" {
		return commitFiles{}, false
	}
	for _, dir := range []string{kept, plan} {
		files, err := readKept(dir)
		switch {
		case err != nil, stale(spec, files):
			continue
		case !converted(spec) && !files.mediaKept:
			// Kept before sources held images, so read again, as a whole.
			_ = os.RemoveAll(dir)
			continue
		}
		return files, true
	}
	return commitFiles{}, false
}

// stale says what's kept of another store's commit was converted by an older
// converter, or for another format.
func stale(spec *resourcev1.CatalogSource, files commitFiles) bool {
	_, ok := readLeftOut(files.leftOut, formatOf(spec))
	return converted(spec) && !ok
}

// staleKept is what an older converter kept of the commit a source applied names, to
// read while the commit can't be converted again, as when its repository is gone, so
// what's installed from it keeps its upgrades.
func (s *Sources) staleKept(spec *resourcev1.CatalogSource) (commitFiles, bool) {
	kept, _ := s.folders(spec)
	if kept == "" {
		return commitFiles{}, false
	}
	files, err := readKept(kept)
	return files, err == nil
}

// releaseBefore is the highest release of the commits of spec's repository, folder
// and format kept, for a source or a plan, other than spec's: what a commit that
// counts from now follows (#640).
func (s *Sources) releaseBefore(spec *resourcev1.CatalogSource) uint32 {
	if s.Dir == "" {
		return 0
	}
	var most uint32
	for _, set := range []string{"kept", "plans"} {
		dir := filepath.Join(s.Dir, set, keyOf(spec))
		commits, _ := os.ReadDir(dir)
		for _, c := range commits {
			if !c.IsDir() || c.Name() == spec.GetCommit() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, c.Name(), leftOutFile)) //nolint:gosec // what this kept, under the control plane's own folder
			if err != nil {
				continue
			}
			if rec, ok := readLeftOut(raw, formatOf(spec)); ok {
				most = max(most, rec.Release)
			}
		}
	}
	return most
}

// fetchOne fetches a commit's files once the slot is free, within fetchWait of
// asking, and keeps them for a plan. Another store's commit is fetched and converted
// within budgets of their own (convertOne).
func (s *Sources) fetchOne(parent context.Context, slot chan struct{}, spec *resourcev1.CatalogSource) (commitFiles, error) {
	ctx, cancel := context.WithTimeout(parent, fetchWait)
	defer cancel()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return commitFiles{}, fmt.Errorf("waiting for another source's fetch: %w", ctx.Err())
	}
	// Kept by a fetch this one waited behind, of the same commit by another name.
	if files, ok := s.kept(spec); ok {
		return files, nil
	}
	_, plan := s.folders(spec)
	if converted(spec) {
		out, images, err := s.convertOne(parent, spec)
		if err != nil {
			// Converted by an older converter, it still reads, until this one can.
			if old, ok := s.staleKept(spec); ok {
				return old, nil
			}
			return commitFiles{}, err
		}
		files := commitFiles{entries: map[string][]byte{}, media: map[string]error{}, leftOut: out[leftOutFile], mediaKept: true}
		for name, data := range out {
			if entryFile(name) {
				files.entries[name] = data
			}
		}
		// Each logo is kept as folder.Prepare wrote it, which passed its checks.
		for name := range images {
			files.media[name] = nil
		}
		if plan != "" {
			keep(plan, files, images)
			s.prunePlans()
		}
		return files, nil
	}
	fetch := s.Fetch
	if fetch == nil {
		fetch = gitfetch.Fetch
	}
	fetched, err := fetch(ctx, s.Client, spec.GetRepository(), spec.GetCommit(), spec.GetPath(), folder.Limits)
	if err != nil {
		return commitFiles{}, err
	}
	// Only what could be an entry, <id>.yaml, and an image named as one in the
	// folder media, is read on or kept: a source is a stranger's, and nothing else
	// of it is any use.
	files := commitFiles{entries: map[string][]byte{}, media: map[string]error{}, mediaKept: true}
	images := map[string][]byte{}
	for name, data := range fetched {
		if entryFile(name) {
			files.entries[name] = data
		} else if image, ok := mediaName(name); ok {
			// Kept as it's served: an SVG image as svgclean writes it anew. One that
			// fails its checks is kept as it came, to say why each time it's read,
			// and is never served.
			prepared, err := folder.Prepare(image, data)
			if files.media[image] = err; err != nil {
				prepared = data
			}
			images[image] = prepared
		}
	}
	if plan != "" {
		keep(plan, files, images)
		s.prunePlans()
	}
	return files, nil
}

// mediaName is the name of an image in the folder media, from its name in a commit's
// folder, as media/logo.png, and whether it's named as one may be.
func mediaName(name string) (string, bool) {
	image, ok := strings.CutPrefix(name, folder.Media+"/")
	return image, ok && folder.ImageName(image)
}

// tidy keeps what the sources applied read, and nothing else: a commit a plan read
// is kept once a source applied names it, and one no source names goes. It's called
// with the sources applied, as the catalog reads them.
func (s *Sources) tidy(sources []stored) {
	if s.Dir == "" {
		return
	}
	want := map[string]bool{}
	for _, st := range sources {
		kept, plan := s.folders(st.spec)
		want[kept] = true
		// What an older converter kept gives way to what this one converted, once it has.
		if files, err := readKept(kept); err == nil && stale(st.spec, files) {
			if fresh, err := readKept(plan); err == nil && !stale(st.spec, fresh) && os.RemoveAll(kept) == nil {
				_ = os.Rename(plan, kept)
			}
			continue
		}
		if _, err := os.Stat(kept); err == nil {
			continue
		}
		if _, err := os.Stat(plan); err == nil && os.MkdirAll(filepath.Dir(kept), 0o700) == nil {
			_ = os.Rename(plan, kept)
		}
	}
	root := filepath.Join(s.Dir, "kept")
	keys, _ := os.ReadDir(root)
	for _, k := range keys {
		commits, _ := os.ReadDir(filepath.Join(root, k.Name()))
		for _, c := range commits {
			if dir := filepath.Join(root, k.Name(), c.Name()); !want[dir] {
				_ = os.RemoveAll(dir)
			}
		}
		_ = os.Remove(filepath.Join(root, k.Name())) // gone once it's empty
	}
}

// prunePlans keeps the newest plansKept commits read for plans, and drops the rest,
// with anything a read that stopped left.
func (s *Sources) prunePlans() {
	type read struct {
		dir string
		at  time.Time
	}
	var reads []read
	root := filepath.Join(s.Dir, "plans")
	keys, _ := os.ReadDir(root)
	for _, k := range keys {
		commits, _ := os.ReadDir(filepath.Join(root, k.Name()))
		for _, c := range commits {
			info, err := c.Info()
			if err != nil {
				continue
			}
			dir := filepath.Join(root, k.Name(), c.Name())
			if strings.Contains(c.Name(), ".part-") {
				if s.now().Sub(info.ModTime()) > time.Hour {
					_ = os.RemoveAll(dir)
				}
				continue
			}
			reads = append(reads, read{dir, info.ModTime()})
		}
	}
	slices.SortFunc(reads, func(a, b read) int { return b.at.Compare(a.at) })
	for _, r := range reads[min(len(reads), plansKept):] {
		_ = os.RemoveAll(r.dir)
		_ = os.Remove(filepath.Dir(r.dir)) // gone once it's empty
	}
}

// commitNote says which branches and tags of a source's repository are at its commit,
// or that none is, as a commit a host serves through a repository's URL may be a
// fork's: GitHub serves a fork's commits through the repository it was forked from.
func (s *Sources) commitNote(ctx context.Context, ref resource.Ref, spec *resourcev1.CatalogSource) revision.Note {
	list := s.Refs
	if list == nil {
		list = gitfetch.List
	}
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
	listing, err := list(ctx, s.Client, spec.GetRepository())
	refs := listing.Refs
	if err != nil {
		return revision.Note{
			Reason: "catalog_source_unlisted", Ref: ref,
			Message: fmt.Sprintf("Dockwell couldn't list the branches and tags of %s, so it can't say which this commit is: %v. Check it's the commit you mean", spec.GetRepository(), err),
		}
	}
	// Names holds each branch and tag at it, as refs/heads/main or refs/tags/v1.
	note := revision.Note{Reason: "catalog_source_commit", Ref: ref, Names: []string{}}
	var at []string
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		if refs[name] != spec.GetCommit() {
			continue
		}
		note.Names = append(note.Names, name)
		if branch, ok := strings.CutPrefix(name, "refs/heads/"); ok {
			at = append(at, "the branch "+branch)
		} else if tag, ok := strings.CutPrefix(name, "refs/tags/"); ok {
			at = append(at, "the tag "+tag)
		}
	}
	switch len(at) {
	case 0:
		note.Reason = "catalog_source_unnamed"
		note.Message = fmt.Sprintf("No branch or tag of %s is at this commit now. It may be an older commit of one, or a fork's, which a host such as GitHub serves through the repository it was forked from. Check it's the commit you mean.", spec.GetRepository())
	case 1:
		note.Message = "It's the commit " + at[0] + " is at."
	default:
		note.Message = "It's the commit " + strings.Join(at[:len(at)-1], ", ") + " and " + at[len(at)-1] + " are at."
	}
	return note
}

// forget forgets that a source's commit couldn't be read, as a person's plan just
// read it.
func (s *Sources) forget(spec *resourcev1.CatalogSource) {
	s.mu.Lock()
	delete(s.unread, unreadKey(spec))
	s.mu.Unlock()
}

func unreadKey(spec *resourcev1.CatalogSource) string {
	return spec.GetRepository() + "\x00" + spec.GetPath() + "\x00" + spec.GetCommit() + formatKey(spec)
}

// formatKey sets apart what's kept of a commit read as another store's, which is
// what was converted of it, from the same folder read as Dockwell's entries. It's
// empty for Dockwell's own, so what a source kept before formats is still its.
func formatKey(spec *resourcev1.CatalogSource) string {
	if !converted(spec) {
		return ""
	}
	return "\x00" + formatOf(spec)
}

// entryFile says a file's name is an entry's, <id>.yaml, and so one name in its folder.
func entryFile(name string) bool {
	id, ok := strings.CutSuffix(name, ".yaml")
	return ok && resource.CheckLabel("catalog entry", id, "postgres") == nil
}

// keep keeps a commit's entries, the converter's record for another store's, and
// its images in dir, the images in its folder media, whole or not at all, so a read
// that stopped halfway isn't taken for one. Each read writes a folder of its own,
// through os.Root so no name reaches out of it, and the first to finish keeps it.
func keep(dir string, files commitFiles, images map[string][]byte) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".part-")
	if err != nil {
		return
	}
	// Gone once renamed, or left by a read that failed.
	defer os.RemoveAll(tmp)
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return
	}
	defer root.Close()
	for name, data := range files.entries {
		if !entryFile(name) || root.WriteFile(name, data, 0o600) != nil {
			return
		}
	}
	if files.leftOut != nil && root.WriteFile(leftOutFile, files.leftOut, 0o600) != nil {
		return
	}
	// Its folder media, even empty, says the commit's images were kept with it.
	if root.Mkdir(folder.Media, 0o700) != nil {
		return
	}
	for name, data := range images {
		if !folder.ImageName(name) || root.WriteFile(folder.Media+"/"+name, data, 0o600) != nil {
			return
		}
	}
	_ = os.Rename(tmp, dir)
}

// readKept reads a commit's entries as kept, and what each image kept is by its
// first bytes alone, as the images are served from where they're kept and never
// held in memory.
func readKept(dir string) (commitFiles, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return commitFiles{}, err
	}
	out := commitFiles{entries: map[string][]byte{}, media: map[string]error{}}
	for _, e := range ents {
		if !e.Type().IsRegular() || !entryFile(e.Name()) && e.Name() != leftOutFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // what this kept, under the control plane's own folder
		if err != nil {
			return commitFiles{}, err
		}
		if e.Name() == leftOutFile {
			out.leftOut = data
			continue
		}
		out.entries[e.Name()] = data
	}
	images, err := os.ReadDir(filepath.Join(dir, folder.Media))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return commitFiles{}, err
	}
	out.mediaKept = true
	for _, e := range images {
		if !e.Type().IsRegular() || !folder.ImageName(e.Name()) {
			continue
		}
		head, err := readHead(filepath.Join(dir, folder.Media, e.Name()))
		if err != nil {
			return commitFiles{}, err
		}
		_, out.media[e.Name()] = folder.Sniff(e.Name(), head)
		if folder.Vector(e.Name()) {
			// An SVG image is told by the whole of it, as svgclean wrote it.
			out.media[e.Name()] = vectorKept(filepath.Join(dir, folder.Media, e.Name()), e.Name())
		}
	}
	return out, nil
}

// snapshotSpec is Dockwell's own catalog at the commit this build holds.
func snapshotSpec() *resourcev1.CatalogSource {
	return &resourcev1.CatalogSource{Repository: snapshot.Repository, Path: snapshot.Path, Commit: snapshot.Commit(), Follow: snapshot.Branch}
}

// atSnapshot says a source reads Dockwell/catalog's entries at the commit this build
// holds, named as the snapshot names it.
func atSnapshot(spec *resourcev1.CatalogSource) bool {
	return spec.GetRepository() == snapshot.Repository && spec.GetPath() == snapshot.Path && spec.GetCommit() == snapshot.Commit() && !converted(spec)
}

// snapshotFiles are the files of Dockwell/catalog's commit this build holds, for a
// source at that commit, as a read of it would keep them: no network, as on a first
// boot offline, or an install that can't reach GitHub (ADR 0077).
func snapshotFiles(spec *resourcev1.CatalogSource) (commitFiles, bool) {
	if !atSnapshot(spec) {
		return commitFiles{}, false
	}
	return readSnapshot()
}

// readSnapshot reads the snapshot's files once a process, as they're the build's:
// each entry, and what each image's first bytes say it is. Every read of the
// snapshot shares what it returns, so nothing writes to it.
var readSnapshot = sync.OnceValues(func() (commitFiles, bool) {
	root := snapshot.Entries()
	out := commitFiles{entries: map[string][]byte{}, media: map[string]error{}, mediaKept: true}
	list, err := fs.ReadDir(root, ".")
	if err != nil {
		return commitFiles{}, false
	}
	for _, e := range list {
		if e.Type().IsRegular() && entryFile(e.Name()) {
			if out.entries[e.Name()], err = fs.ReadFile(root, e.Name()); err != nil {
				return commitFiles{}, false
			}
		}
	}
	images, _ := fs.ReadDir(root, folder.Media)
	for _, e := range images {
		if !e.Type().IsRegular() || !folder.ImageName(e.Name()) {
			continue
		}
		f, err := root.Open(folder.Media + "/" + e.Name())
		if err != nil {
			return commitFiles{}, false
		}
		// A raster image is told by its first bytes, an SVG image by the whole of it.
		most := int64(folder.Sniffed)
		if folder.Vector(e.Name()) {
			most = folder.MediaLimits.File
		}
		head, err := io.ReadAll(io.LimitReader(f, most))
		_ = f.Close()
		if err != nil {
			return commitFiles{}, false
		}
		_, out.media[e.Name()] = folder.Sniff(e.Name(), head)
	}
	return out, true
})

// checkedSnapshot is the check of the snapshot, made once a process and shared by
// every control plane that starts and every read of Dockwell's own catalog. What the
// build holds never changes, and the build proved it passes the whole check
// (TestTheSnapshotPassesTheWholeCheck), so its entries are parsed and its images
// checked, but no Compose file is read again: an install reads each in a process of
// its own, which took a first start a second. A source at the snapshot names the
// same repository, folder and commit, so the paths of its images are the same too,
// and what the check says of a file never names the source.
var checkedSnapshot = sync.OnceValue(func() sourceRead {
	files, _ := readSnapshot()
	return checkFiles(context.Background(), DefaultName, snapshotSpec(), files, claims{}, true)
})

// vectorKept says whether an SVG image kept is one folder.Image takes, reading the
// whole of it, within the most an image may be.
func vectorKept(file, name string) error {
	f, err := os.Open(file) //nolint:gosec // what this kept, under the control plane's own folder
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, folder.MediaLimits.File+1))
	switch {
	case err != nil:
		return err
	case int64(len(data)) > folder.MediaLimits.File:
		return fmt.Errorf("it's over %d MiB, more than an image may be", folder.MediaLimits.File>>20)
	}
	_, err = folder.Image(name, data)
	return err
}

// readHead reads the first bytes of a file, as many as folder.Sniff looks at.
func readHead(file string) ([]byte, error) {
	f, err := os.Open(file) //nolint:gosec // what this kept, under the control plane's own folder
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, folder.Sniffed)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:n], nil
}

// readSource reads a source and checks its entries, once for each commit and each
// set of IDs before it.
func (s *Sources) readSource(ctx context.Context, name string, spec *resourcev1.CatalogSource, taken claims) (sourceRead, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s%s", name, spec.GetRepository(), spec.GetPath(), spec.GetCommit(), formatKey(spec))
	for _, id := range slices.Sorted(maps.Keys(taken)) {
		fmt.Fprintf(h, "\x00%s=%s=%s", id, taken[id].By, appKey(taken[id].Entry))
	}
	key := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	got, ok := s.read[key]
	failed, wasUnread := s.unread[unreadKey(spec)]
	s.mu.Unlock()
	switch {
	case ok:
		return got, nil
	case wasUnread && s.now().Sub(failed.at) < unreadWait:
		return sourceRead{}, failed.err
	}
	files, err := s.files(ctx, spec)
	if err != nil {
		s.mu.Lock()
		if s.unread == nil {
			s.unread = map[string]unread{}
		}
		s.unread[unreadKey(spec)] = unread{err: err, at: s.now()}
		s.mu.Unlock()
		return sourceRead{}, err
	}
	s.forget(spec)
	return s.check(ctx, key, name, spec, files, taken)
}

// check checks a source's entries once for every reader of the same commit and IDs
// before it: a read that comes while one is under way waits for it, as reads of a
// commit wait for its fetch. The check is every reader's, so one who leaves the page
// doesn't cut it short, within checkWait of its own. A check cut short, or one where
// an entry's Compose file wasn't read as the reader was busy, is kept for nobody, and
// the next read checks again.
func (s *Sources) check(ctx context.Context, key, name string, spec *resourcev1.CatalogSource, files commitFiles, taken claims) (sourceRead, error) {
	s.mu.Lock()
	if got, ok := s.read[key]; ok {
		s.mu.Unlock()
		return got, nil
	}
	if c, ok := s.checking[key]; ok {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.got, c.err
		case <-ctx.Done():
			return sourceRead{}, ctx.Err()
		}
	}
	if s.checking == nil {
		s.checking = map[string]*checkingRead{}
	}
	c := &checkingRead{done: make(chan struct{})}
	s.checking[key] = c
	hook := s.beforeCheck
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	checking, done := context.WithTimeout(context.WithoutCancel(ctx), checkWait)
	c.got = checkSource(checking, name, spec, files, taken)
	if c.err = settled(c.got, checking.Err()); c.err != nil {
		c.got = sourceRead{}
	}
	done()
	s.mu.Lock()
	delete(s.checking, key)
	if c.err == nil {
		// What's read is kept for each commit and the IDs before it, which moves as
		// sources do, so it's let go of whole once it holds mostReads.
		if s.read == nil || len(s.read) >= mostReads {
			s.read = map[string]sourceRead{}
		}
		s.read[key] = c.got
	}
	s.mu.Unlock()
	close(c.done)
	return c.got, c.err
}

// checkSource checks each entry of a source as Dockwell's own are, through package
// folder: as catalog.Parse reads one, in a file named by its ID, with its Compose
// file read as an install would read it, and each image it names one its commit
// holds. One whose ID taken has is left out, saying whose it is. An image of the
// commit's that isn't what its name says is left out, saying why, and an entry
// that names it, or the artwork the panel ships for Dockwell's own apps, shows
// none in its place. Each image it keeps names the path the control plane serves it
// at: that commit's, from where it's kept.
//
// Another store's commit was converted as it was read, so its entries name their
// images by tag, for an install to pin, and what the converter left out comes first,
// each app with its reason.
//
// Dockwell's own at the commit this build holds, with nothing taken before it, as it
// comes first, is checkedSnapshot's, shared, so nothing writes to what it returns.
func checkSource(ctx context.Context, name string, spec *resourcev1.CatalogSource, files commitFiles, taken claims) sourceRead {
	if len(taken) == 0 && atSnapshot(spec) {
		return checkedSnapshot()
	}
	return checkFiles(ctx, name, spec, files, taken, false)
}

// checkFiles is checkSource, made each time. proved reads no Compose file again,
// for files the build proved pass the whole check.
func checkFiles(ctx context.Context, name string, spec *resourcev1.CatalogSource, files commitFiles, taken claims, proved bool) sourceRead {
	var out sourceRead
	byTag := converted(spec)
	if byTag {
		rec, _ := readLeftOut(files.leftOut, formatOf(spec))
		out.apps, out.license = rec.Apps, rec.License
		for _, a := range rec.LeftOut {
			out.refused = append(out.refused, &dockwellv1.CatalogSourceRefusal{File: a.Path, Reason: a.Reason, Kind: a.Kind, Id: a.ID})
		}
	}
	for _, image := range slices.Sorted(maps.Keys(files.media)) {
		if err := files.media[image]; err != nil {
			out.refused = append(out.refused, &dockwellv1.CatalogSourceRefusal{File: folder.Media + "/" + image, Reason: err.Error()})
		}
	}
	got := folder.Check(ctx, name, files.entries, files.images(), taken, folder.Options{Tagged: byTag, Same: sameApp, Proved: proved})
	for _, r := range got.Refused {
		refusal := &dockwellv1.CatalogSourceRefusal{File: r.File, Reason: r.Reason}
		// A converted entry's refusal names its app and kind, as the converter's do.
		if byTag && r.Kind != folder.KindImage {
			refusal.Id, refusal.Kind = strings.TrimSuffix(r.File, ".yaml"), r.Kind
		}
		out.refused = append(out.refused, refusal)
	}
	at := func(path string) string {
		image, _ := folder.MediaFile(path)
		return MediaPath(spec, image)
	}
	for _, e := range slices.Concat(got.Entries, got.Also) {
		if e.GetIcon() != "" {
			e.Icon = at(e.GetIcon())
		}
		for _, shot := range e.GetScreenshots() {
			shot.Path = at(shot.GetPath())
		}
	}
	out.entries, out.also, out.unsettled = got.Entries, got.Also, got.Unsettled
	return out
}

// MediaPrefix is where the control plane serves a source's images.
const MediaPrefix = "/catalog-media/"

// MediaPath is where the control plane serves an image of a source's commit: under
// the key of its repository and folder, and its commit, as what a commit holds never
// changes.
func MediaPath(spec *resourcev1.CatalogSource, image string) string {
	return MediaPrefix + keyOf(spec) + "/" + spec.GetCommit() + "/" + image
}

// stored is a source as the store holds it, at its revision, in its organization.
type stored struct {
	name     string
	spec     *resourcev1.CatalogSource
	revision int
	scope    string
}

// storedSources reads the sources in the order their entries come in: Dockwell's
// own first, then the rest in their names' order.
func (h *Handler) storedSources(ctx context.Context) ([]stored, error) {
	var out []stored
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListKind(ctx, string(catalogsource.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			r := &list[i]
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("reading the catalog source %s: %w", r.Name, err)
			}
			out = append(out, stored{name: r.Name, spec: spec.(*resourcev1.CatalogSource), revision: r.Revision, scope: r.Scope})
		}
		return nil
	})
	order(out)
	return out, err
}

// order sorts sources as their entries come: Dockwell's own first, as its entries
// came first when they were built in, then the rest by name.
func order(list []stored) {
	slices.SortFunc(list, func(a, b stored) int {
		if a, b := isOwn(a.spec), isOwn(b.spec); a != b {
			if a {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
}

// isOwn says a source reads Dockwell's own catalog: Dockwell/catalog's entries, in
// Dockwell's format, whatever its name, and whether it follows main or stands at a
// commit.
func isOwn(spec *resourcev1.CatalogSource) bool {
	repo := strings.TrimSuffix(strings.TrimSuffix(spec.GetRepository(), "/"), ".git")
	return strings.EqualFold(repo, snapshot.Repository) && spec.GetPath() == snapshot.Path && !converted(spec)
}

// origin is where an entry comes from, as the control plane knows it: a source, at the
// commit it's read at, or for an install made before Dockwell/catalog, the catalog
// built into Dockwell. own says the source is Dockwell's own catalog. What an entry's
// own source field says, which a source's file writes itself, never counts.
type origin struct {
	catalog, repository, path, commit string
	own                               bool
}

// originOf is where a pin says its install came from.
func originOf(pin *resourcev1.CatalogPin) origin {
	return origin{catalog: pin.GetCatalog(), repository: pin.GetRepository(), path: pin.GetPath(), commit: pin.GetCommit()}
}

func (o origin) words() string {
	switch {
	case o.catalog == "":
		return "the catalog built into Dockwell"
	case o.own || isOwn(&resourcev1.CatalogSource{Repository: o.repository, Path: o.path}):
		return "Dockwell's own catalog"
	}
	return "the catalog " + o.catalog
}

// same says an install came from this catalog, at whatever commit: Dockwell's own,
// or a source of the same name, repository and folder.
func (o origin) same(pin *resourcev1.CatalogPin) bool {
	if o.catalog == "" {
		return pin.GetCatalog() == ""
	}
	return pin.GetCatalog() == o.catalog && pin.GetRepository() == o.repository && pin.GetPath() == o.path
}

// sourced is the catalog with each source's entries, where each comes from, and
// what came of reading each source.
type sourced struct {
	catalog *catalog.Catalog
	origins map[string]origin
	// also are the same apps from catalogs after the one each is listed from, by ID.
	also   map[string][]alternative
	reads  map[string]sourceRead
	unread map[string]error
}

// entryFor is the catalog's entry for an install: its ID's, where it comes from the
// catalog the install came from, or the same app's from that catalog, where another
// lists it.
func (s sourced) entryFor(pin *resourcev1.CatalogPin) (*catalogv1.Entry, bool) {
	e, ok := s.catalog.Entry(pin.GetEntry())
	if ok && s.origins[pin.GetEntry()].same(pin) {
		return e, true
	}
	for _, a := range s.also[pin.GetEntry()] {
		if a.from.same(pin) {
			return a.entry, true
		}
	}
	return nil, false
}

// alternative is an app's entry in a catalog after the one it's listed from.
type alternative struct {
	from  origin
	entry *catalogv1.Entry
}

// planned is what a plan would make of the sources: a source of each name in place
// of what's stored, or none for nil.
type planned map[string]*resourcev1.CatalogSource

// apply is the sources with a plan's in place: a source of each name instead's
// in place of what's stored, or none for nil, in the order their entries come.
func (instead planned) apply(list []stored) []stored {
	if instead == nil {
		return list
	}
	list = slices.DeleteFunc(slices.Clone(list), func(s stored) bool { _, ok := instead[s.name]; return ok })
	for name, spec := range instead {
		if spec != nil {
			list = append(list, stored{name: name, spec: spec})
		}
	}
	order(list)
	return list
}

// withSources is the catalog: each source's entries, Dockwell's own first, then the
// rest by name, with DOCKWELL_CATALOG_DIR's entries in place of Dockwell's own of
// the same ID. With instead, it's what a plan would make of them. A source that
// can't be read now adds nothing, and says why.
func (h *Handler) withSources(ctx context.Context, instead planned) (sourced, error) {
	list, err := h.storedSources(ctx)
	if err != nil {
		return sourced{}, err
	}
	if instead == nil {
		h.sources.tidy(list)
	}
	list = instead.apply(list)
	dir, err := catalog.FromDir()
	if err != nil {
		return sourced{}, err
	}
	out := sourced{catalog: &catalog.Catalog{}, origins: map[string]origin{}, also: map[string][]alternative{}, reads: map[string]sourceRead{}, unread: map[string]error{}}
	taken := claims{}
	// What DOCKWELL_CATALOG_DIR adds takes over Dockwell's own, or with none, stands
	// for the catalog built into Dockwell, as it did before Dockwell/catalog.
	// Its entries name images as Dockwell's own do, as media/<name>, which are its
	// source's, at the commit the source is at.
	overlay := func(o origin, spec *resourcev1.CatalogSource) {
		for _, e := range dir.All() {
			at := func(path string) string {
				if image, ok := folder.MediaFile(path); ok && spec != nil {
					return MediaPath(spec, image)
				}
				return path
			}
			e.Icon = at(e.GetIcon())
			for _, shot := range e.GetScreenshots() {
				shot.Path = at(shot.GetPath())
			}
			taken[e.GetId()] = folder.Taken{By: o.words(), Entry: e}
			out.origins[e.GetId()] = o
		}
		out.catalog = out.catalog.Over(dir)
		dir = &catalog.Catalog{}
	}
	if !slices.ContainsFunc(list, func(s stored) bool { return isOwn(s.spec) }) {
		overlay(origin{}, nil)
	}
	for _, s := range list {
		o := origin{catalog: s.name, repository: s.spec.GetRepository(), path: s.spec.GetPath(), commit: s.spec.GetCommit(), own: isOwn(s.spec)}
		got, err := h.sources.readSource(ctx, s.name, s.spec, maps.Clone(taken))
		if err != nil {
			out.unread[s.name] = err
			h.logger.WarnContext(ctx, "a catalog source can't be read, so its entries are left out until it can", "source", s.name, "url", s.spec.GetRepository(), "commit", s.spec.GetCommit(), "error", err)
		} else {
			out.reads[s.name] = got
			out.catalog = out.catalog.With(got.entries)
			// Taken whether the read was kept or made, so a later source's clash is the
			// same either way (its M4).
			for _, e := range got.entries {
				taken[e.GetId()] = folder.Taken{By: o.words(), Entry: e}
				out.origins[e.GetId()] = o
			}
			for _, e := range got.also {
				out.also[e.GetId()] = append(out.also[e.GetId()], alternative{from: o, entry: e})
			}
		}
		if o.own {
			overlay(o, s.spec)
		}
	}
	return out, nil
}

// before is what the entries of the sources that come before a source say of each
// ID, by whose it is, with spec in place of what's stored of name.
func (h *Handler) before(ctx context.Context, name string, spec *resourcev1.CatalogSource) (claims, error) {
	got, err := h.withSources(ctx, planned{name: nil})
	if err != nil {
		return nil, err
	}
	list, err := h.storedSources(ctx)
	if err != nil {
		return nil, err
	}
	list = planned{name: spec}.apply(list)
	earlier := map[string]bool{}
	for _, s := range list {
		if s.name == name {
			break
		}
		earlier[s.name] = true
	}
	taken := claims{}
	for id, o := range got.origins {
		// What DOCKWELL_CATALOG_DIR adds comes first with no source of Dockwell's own,
		// and takes over that source's entries once there is one.
		if earlier[o.catalog] || o.catalog == "" && !isOwn(spec) {
			e, _ := got.catalog.Entry(id)
			taken[id] = folder.Taken{By: o.words(), Entry: e}
		}
	}
	return taken, nil
}

// handsNotes says what a plan for a source changes of where entries come from: an ID
// it takes from another catalog, or that another gets from it, and each install that
// would have no upgrade after it, or would have one again.
func (h *Handler) handsNotes(ctx context.Context, ref resource.Ref, name string, instead planned) ([]revision.Note, error) {
	before, err := h.withSources(ctx, nil)
	if err != nil {
		return nil, err
	}
	after, err := h.withSources(ctx, instead)
	if err != nil {
		return nil, err
	}
	var took, gave, tookIDs, gaveIDs []string
	for _, id := range slices.Sorted(maps.Keys(after.origins)) {
		a, b := after.origins[id], before.origins[id]
		if _, was := before.origins[id]; !was || a.catalog == b.catalog {
			continue
		}
		switch {
		case a.catalog == name:
			took, tookIDs = append(took, id+" from "+b.words()), append(tookIDs, id)
		case b.catalog == name:
			gave, gaveIDs = append(gave, id+" comes from "+a.words()), append(gaveIDs, id)
		}
	}
	var notes []revision.Note
	if len(took) > 0 {
		notes = append(notes, revision.Note{
			Reason: "catalog_source_takes", Ref: ref, Names: tookIDs,
			Message: "It takes " + strings.Join(took, ", ") + ", as it comes first. What's installed from that catalog keeps upgrading only from it.",
		})
	}
	if len(gave) > 0 {
		notes = append(notes, revision.Note{
			Reason: "catalog_source_gives", Ref: ref, Names: gaveIDs,
			Message: "In its place, " + strings.Join(gave, ", ") + ": another catalog's app, which nothing installed from " + name + " upgrades to.",
		})
	}
	var stranded, strandedNames, restored []string
	err = h.db.View(ctx, func(tx *store.Tx) error {
		all, err := h.installs(ctx, tx, before)
		for _, in := range all {
			pin := in.GetPin()
			_, was := before.entryFor(pin)
			_, now := after.entryFor(pin)
			switch {
			case was && !now:
				stranded = append(stranded, fmt.Sprintf("%s, from %s", in.GetProject(), originOf(pin).words()))
				strandedNames = append(strandedNames, in.GetProject())
			case !was && now:
				restored = append(restored, in.GetProject())
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(stranded) > 0 {
		notes = append(notes, revision.Note{
			Reason: "catalog_source_strands", Ref: ref, Names: strandedNames,
			Message: "What's installed keeps running, with no upgrade until the catalog it came from has its app again: " + strings.Join(stranded, "; ") + ".",
		})
	}
	if len(restored) > 0 {
		notes = append(notes, revision.Note{
			Reason: "catalog_source_restores", Ref: ref, Names: restored,
			Message: "It gives back the upgrades of " + strings.Join(restored, ", ") + ", from the catalog each came from.",
		})
	}
	return notes, nil
}

// SetCatalogSource plans adding a source, or moving one to another commit, or having
// it follow a branch or stay where it is, with what that commit holds and what the
// catalog leaves out of it in the plan's notes.
func (h *Handler) SetCatalogSource(ctx context.Context, req *connect.Request[dockwellv1.SetCatalogSourceRequest]) (*connect.Response[dockwellv1.SetCatalogSourceResponse], error) {
	name := req.Msg.GetName()
	if err := (catalogsource.Type{}).CheckName(name); err != nil {
		return nil, authapi.Invalid(err)
	}
	spec := &resourcev1.CatalogSource{
		Repository: strings.TrimSpace(req.Msg.GetRepository()), Path: req.Msg.GetPath(),
		Commit: strings.ToLower(strings.TrimSpace(req.Msg.GetCommit())), Follow: strings.TrimSpace(req.Msg.GetFollow()),
		Format: req.Msg.GetFormat(),
	}
	// A branch to follow with no commit is the commit it's at now.
	if spec.GetCommit() == "" && spec.GetFollow() != "" {
		if err := catalogsource.CheckBranch(spec); err != nil {
			return nil, authapi.Invalid(err)
		}
		at, err := h.sources.branchAt(ctx, spec)
		switch {
		case err != nil && spec.GetRepository() == snapshot.Repository && spec.GetPath() == snapshot.Path:
			// Dockwell's own catalog, with GitHub out of reach: the commit this build
			// holds, which reads with no network, as a first boot's does.
			at = snapshot.Commit()
		case err != nil:
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "unreadable")
		}
		spec.Commit = at
	}
	if err := (catalogsource.Type{}).Validate(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	// Dockwell's own catalog at the commit this build holds is in Dockwell's format,
	// with nothing to read to tell, as when it's added back with GitHub out of reach.
	if req.Msg.GetFormat() != resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED || !atSnapshot(spec) {
		if err := h.sources.chooseFormat(ctx, spec, req.Msg.GetFormat()); err != nil {
			return nil, err
		}
	}
	plan, err := h.planSource(ctx, name, spec, "")
	var cant unreadable
	switch {
	case errors.As(err, &cant):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "unreadable")
	case err != nil:
		return nil, h.internal(ctx, err)
	}
	change, err := h.changes.Propose(ctx, plan.summary, []*dockwellv1.Edit{{
		Resource: resourcepb.Ref(plan.ref),
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_CatalogSource{CatalogSource: spec}},
	}}, nil, changes.Also{Notes: plan.notes})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogSourceResponse{Change: change}), nil
}

// unreadable is a source's commit Dockwell couldn't read.
type unreadable struct{ err error }

func (u unreadable) Error() string { return u.err.Error() }
func (u unreadable) Unwrap() error { return u.err }

// sourcePlan is what a change that sets a source says, and what it read.
type sourcePlan struct {
	ref     resource.Ref
	summary string
	notes   []revision.Note
	// read is what the catalog would show of the commit, and was what it shows of
	// the commit before, for a source that's there already.
	read, was sourceRead
	// before is the source as it is, or nil for one that isn't yet.
	before *resourcev1.CatalogSource
}

// planSource reads a source's commit and says what setting it to spec would do: what
// the commit holds and what the catalog leaves out of it, which branch or tag it is,
// what it adds, changes and takes away beside the commit before, and what changes
// hands. branch, for a move Dockwell makes as the branch spec follows moved, is the
// branch's name, and the plan says so in place of listing the repository's refs.
func (h *Handler) planSource(ctx context.Context, name string, spec *resourcev1.CatalogSource, branch string) (sourcePlan, error) {
	// What comes before it: Dockwell's own, then each other source before it by name.
	taken, err := h.before(ctx, name, spec)
	if err != nil {
		return sourcePlan{}, err
	}
	others, err := h.storedSources(ctx)
	if err != nil {
		return sourcePlan{}, err
	}
	out := sourcePlan{ref: resource.Ref{Kind: catalogsource.Kind, Name: name}}
	for _, o := range others {
		if o.name == name {
			out.before = o.spec
		}
	}
	files, err := h.sources.files(ctx, spec)
	if err != nil {
		return sourcePlan{}, unreadable{fmt.Errorf("Dockwell can't read %s at %s: %w", spec.GetRepository(), spec.GetCommit(), err)} //nolint:staticcheck // it starts with a name
	}
	h.sources.forget(spec)
	out.read = checkSource(ctx, name, spec, files, maps.Clone(taken))
	ref := out.ref
	// A move along a branch, or Dockwell's own at a first start, says its branch, and
	// asks the repository nothing: offline, a listing would hold the plan up.
	commit := revision.Note{Reason: "catalog_source_commit", Ref: ref, Names: []string{"refs/heads/" + branch}, Message: "It's the commit the branch " + branch + " is at."}
	if branch == "" {
		commit = h.sources.commitNote(ctx, ref, spec)
	}
	out.notes = []revision.Note{{
		Reason:  "catalog_source_reads",
		Message: readsWords(spec, out.read),
		Ref:     ref,
		Names:   ids(out.read.entries),
	}, commit}
	if also := out.read.also; len(also) > 0 {
		out.notes = append(out.notes, revision.Note{
			Reason: "catalog_source_also", Ref: ref, Names: ids(also),
			Message: fmt.Sprintf("It offers %d %s the catalog has from a catalog before it, as another choice at install: %s.", len(also), plural(len(also), "app", "apps"), strings.Join(ids(also), ", ")),
		})
	}
	if converted(spec) {
		// One note, counted by kind: a store leaves out hundreds of apps, and each
		// one's reason is a step away on the source's page.
		out.notes = append(out.notes, leftOutNote(ref, out.read)...)
	} else {
		for _, r := range out.read.refused {
			out.notes = append(out.notes, revision.Note{Reason: "catalog_source_refuses", Message: "It leaves out " + r.GetFile() + ": " + r.GetReason(), Ref: ref, Names: []string{r.GetFile()}})
		}
	}
	was := out.before
	switch {
	case was == nil:
		out.summary = "Add the catalog " + name + " from " + spec.GetRepository()
	case was.GetCommit() != spec.GetCommit():
		// Said as a move from one commit to the other, so its undo reads right too.
		out.summary = "Move the catalog " + name + " from " + was.GetCommit()[:12] + " to " + spec.GetCommit()[:12]
	case spec.GetFollow() != "":
		out.summary = "Have the catalog " + name + " follow " + spec.GetFollow()
	default:
		out.summary = "Pin the catalog " + name + " at " + spec.GetCommit()[:12]
	}
	if was != nil {
		if before, err := h.sources.files(ctx, was); err == nil {
			out.was = checkSource(ctx, name, was, before, maps.Clone(taken))
			out.notes = append(out.notes, movesNote(ref, name, out.was.entries, out.read.entries)...)
		}
	}
	if f := spec.GetFollow(); f != "" && (was == nil || was.GetFollow() != f) {
		out.notes = append(out.notes, revision.Note{
			Reason: "catalog_source_follows", Ref: ref, Names: []string{"refs/heads/" + f},
			Message: "It follows the branch " + f + ": as it moves, Dockwell moves the catalog along it by a change of its own, which you can undo. What's installed doesn't change with it.",
		})
	} else if f == "" && was.GetFollow() != "" {
		out.notes = append(out.notes, revision.Note{
			Reason: "catalog_source_pinned", Ref: ref,
			Message: "It stops following " + was.GetFollow() + ", and stays at " + spec.GetCommit()[:12] + " until you move it.",
		})
	}
	hands, err := h.handsNotes(ctx, ref, name, planned{name: spec})
	if err != nil {
		return sourcePlan{}, err
	}
	out.notes = append(out.notes, hands...)
	return out, nil
}

func ids(entries []*catalogv1.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.GetId()
	}
	return out
}

func readsWords(spec *resourcev1.CatalogSource, got sourceRead) string {
	entries := got.entries
	if converted(spec) {
		return convertedWords(spec, entries, got.license)
	}
	where := spec.GetRepository()
	if spec.GetPath() != "" {
		where = "the folder " + spec.GetPath() + " in " + where
	}
	if len(entries) == 0 {
		return fmt.Sprintf("It reads no entries from %s, at %s.", where, spec.GetCommit()[:12])
	}
	return fmt.Sprintf("It reads %d %s from %s, at %s: %s.", len(entries), plural(len(entries), "entry", "entries"), where, spec.GetCommit()[:12], strings.Join(ids(entries), ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// movesNote says what a move to another commit adds, changes and takes away.
func movesNote(ref resource.Ref, name string, was, now []*catalogv1.Entry) []revision.Note {
	before := map[string]*catalogv1.Entry{}
	for _, e := range was {
		before[e.GetId()] = e
	}
	var added, changed, moved, gone []string
	for _, e := range now {
		switch b, ok := before[e.GetId()]; {
		case !ok:
			added = append(added, e.GetId())
		case b.GetRelease() != e.GetRelease() || b.GetVersion() != e.GetVersion():
			changed = append(changed, fmt.Sprintf("%s to %s", e.GetId(), cmp.Or(e.GetVersion(), "its new release")))
			moved = append(moved, e.GetId())
		}
		delete(before, e.GetId())
	}
	gone = slices.Sorted(maps.Keys(before))
	var out []revision.Note
	if len(added) > 0 {
		out = append(out, revision.Note{Reason: "catalog_source_adds", Message: "It adds " + strings.Join(added, ", ") + ".", Ref: ref, Names: added})
	}
	if len(changed) > 0 {
		out = append(out, revision.Note{Reason: "catalog_source_changes", Message: "It moves " + strings.Join(changed, ", ") + ", which an install of each can upgrade to.", Ref: ref, Names: moved})
	}
	if len(gone) > 0 {
		out = append(out, revision.Note{Reason: "catalog_source_takes_away", Message: "It takes away " + strings.Join(gone, ", ") + ". What's installed from the catalog " + name + " of them keeps running, with no upgrade until it has them again.", Ref: ref, Names: gone})
	}
	return out
}

// ListCatalogSources returns the sources, with what the catalog shows of each.
func (h *Handler) ListCatalogSources(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogSourcesRequest]) (*connect.Response[dockwellv1.ListCatalogSourcesResponse], error) {
	infos, err := h.sourceInfos(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ListCatalogSourcesResponse{Sources: infos}), nil
}

// sourceInfos says of each source, in the order their entries come, what the catalog shows of
// it and leaves out, and for one that follows a branch, what the last look found.
func (h *Handler) sourceInfos(ctx context.Context) ([]*dockwellv1.CatalogSourceInfo, error) {
	list, err := h.storedSources(ctx)
	if err != nil {
		return nil, err
	}
	s, err := h.withSources(ctx, nil)
	if err != nil {
		return nil, err
	}
	reads, unread := s.reads, s.unread
	var out []*dockwellv1.CatalogSourceInfo
	for _, s := range list {
		info := &dockwellv1.CatalogSourceInfo{Name: s.name, Source: s.spec, Own: isOwn(s.spec), Store: storeOf(s.spec).ID}
		if err := unread[s.name]; err != nil {
			info.Unread = err.Error()
		}
		if got, ok := reads[s.name]; ok {
			info.Entries, info.Refused, info.Also, info.License = ids(got.entries), got.refused, ids(got.also), got.license
		}
		h.follower.info(info)
		out = append(out, info)
	}
	return out, nil
}

// storeOf is the store a source reads, at the store's own repository and in its
// format, if it reads one.
func storeOf(spec *resourcev1.CatalogSource) convert.Store {
	known, ok := convert.StoreOf(formatOf(spec))
	if !ok || !known.At(spec.GetRepository()) || spec.GetPath() != "" {
		return convert.Store{}
	}
	return known
}

// ListCatalogStores lists the stores Dockwell reads directly, each with what this
// build measured of it, or once a source reads it, what that source's read found,
// and the source's name: the one with the most apps that install first.
func (h *Handler) ListCatalogStores(ctx context.Context, _ *connect.Request[dockwellv1.ListCatalogStoresRequest]) (*connect.Response[dockwellv1.ListCatalogStoresResponse], error) {
	list, err := h.storedSources(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	read, err := h.withSources(ctx, nil)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	var out []*dockwellv1.CatalogStore
	for i := range convert.Stores {
		st := &convert.Stores[i]
		if st.Unlisted {
			continue
		}
		info := &dockwellv1.CatalogStore{
			Id: st.ID, Name: st.Name, Label: st.Label, Repository: st.Upstream, Format: formatEnum(st.Format),
			Branch: st.Branch, License: st.License, Homepage: st.Homepage,
		}
		// What it converted to at the commit the repository pins, as `make
		// store-rates` records it (ADR 0085), for a person to weigh it by.
		if m, ok := convert.MeasuredOf(st.Format); ok {
			info.Apps, info.Installs, info.Commit = uint32(m.Apps), uint32(m.Converted), m.Commit //nolint:gosec // a store holds 2,000 apps at most
		}
		for _, src := range list {
			if storeOf(src.spec).ID != st.ID {
				continue
			}
			info.Source = src.name
			if got, ok := read.reads[src.name]; ok && got.apps > 0 {
				info.Apps, info.Installs = uint32(min(got.apps, convert.MaxApps)), uint32(min(len(got.entries)+len(got.also), convert.MaxApps))
				info.Commit, info.Live = src.spec.GetCommit(), true
			}
			break
		}
		out = append(out, info)
	}
	slices.SortStableFunc(out, func(a, b *dockwellv1.CatalogStore) int { return cmp.Compare(b.GetInstalls(), a.GetInstalls()) })
	return connect.NewResponse(&dockwellv1.ListCatalogStoresResponse{Stores: out}), nil
}

// mostRefs is how many branches and tags a listing answers with: a form offers
// them to choose from, and a repository can hold thousands.
const mostRefs = 500

// ListCatalogSourceRefs lists a repository's default branch, then its other
// branches, then its tags, each with the commit it's at, read as a source is: at
// a public address, within a wait.
func (h *Handler) ListCatalogSourceRefs(ctx context.Context, req *connect.Request[dockwellv1.ListCatalogSourceRefsRequest]) (*connect.Response[dockwellv1.ListCatalogSourceRefsResponse], error) {
	repo := strings.TrimSpace(req.Msg.GetRepository())
	if _, err := gitfetch.CheckURL(repo); err != nil {
		return nil, authapi.Invalid(err)
	}
	list := h.sources.Refs
	if list == nil {
		list = gitfetch.List
	}
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
	listing, err := list(ctx, h.sources.Client, repo)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("Dockwell can't read %s: %w", repo, err), "unreadable") //nolint:staticcheck // it starts with a name
	}
	refs := listing.Refs
	out := &dockwellv1.ListCatalogSourceRefsResponse{}
	// The default branch leads, so it's listed however many others there are.
	if at, ok := refs[listing.Head]; ok {
		out.Head = listing.Head
		out.Refs = append(out.Refs, &dockwellv1.CatalogSourceRef{Name: listing.Head, Commit: at})
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		for _, name := range slices.Sorted(maps.Keys(refs)) {
			if !strings.HasPrefix(name, prefix) || name == out.Head {
				continue
			}
			if len(out.Refs) == mostRefs {
				out.More = true
				break
			}
			out.Refs = append(out.Refs, &dockwellv1.CatalogSourceRef{Name: name, Commit: refs[name]})
		}
	}
	// How the default branch's commit is laid out, so a form that was pasted a link
	// can say what it is, and offer a choice only where Dockwell can't tell.
	if at, ok := refs[listing.Head]; ok {
		out.Formats = h.sources.guessesOf(ctx, repo, at)
	}
	return connect.NewResponse(out), nil
}

// RemoveCatalogSource plans no longer reading a source.
func (h *Handler) RemoveCatalogSource(ctx context.Context, req *connect.Request[dockwellv1.RemoveCatalogSourceRequest]) (*connect.Response[dockwellv1.RemoveCatalogSourceResponse], error) {
	name := req.Msg.GetName()
	list, err := h.storedSources(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	if !slices.ContainsFunc(list, func(s stored) bool { return s.name == name }) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no catalog source %s. List them to find its name", name), "not_found")
	}
	ref := resource.Ref{Kind: catalogsource.Kind, Name: name}
	notes := []revision.Note{{
		Reason:  "catalog_source_removed",
		Message: "Its entries leave the catalog. What's installed from them keeps running, with no upgrade until a source of the same name, repository and folder has them again.",
		Ref:     ref,
	}}
	hands, err := h.handsNotes(ctx, ref, name, planned{name: nil})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	change, err := h.changes.Propose(ctx, "Stop reading the catalog "+name, []*dockwellv1.Edit{{Resource: resourcepb.Ref(ref), Delete: true}}, nil, changes.Also{Notes: append(notes, hands...)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RemoveCatalogSourceResponse{Change: change}), nil
}
