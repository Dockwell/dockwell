// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
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
	// slot holds the one fetch the control plane makes at a time, as a pack's index
	// and what's made of it take memory while it's read, and reads at once add up.
	slot chan struct{}
}

// fetching is a fetch under way, which a read of the same commit waits for.
type fetching struct {
	done  chan struct{}
	files map[string][]byte
	err   error
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
	sum := sha256.Sum256([]byte(spec.GetRepository() + "\x00" + spec.GetPath() + formatKey(spec)))
	key := hex.EncodeToString(sum[:8])
	return filepath.Join(s.Dir, "kept", key, spec.GetCommit()), filepath.Join(s.Dir, "plans", key, spec.GetCommit())
}

// sourceRead is what came of reading a source: the entries the catalog shows, and
// what it leaves out.
type sourceRead struct {
	entries []*catalogv1.Entry
	refused []*dockwellv1.CatalogSourceRefusal
	// apps and license are how many apps another store's commit holds, and the
	// licence its repository says, for a source converted as it's read.
	apps    int
	license string
}

// files returns a commit's files in a source's folder: as kept, or fetched and kept
// for a plan, until tidy keeps it for the source it's applied to. A read of a commit
// that's being fetched waits for that fetch, and fetches run one at a time.
func (s *Sources) files(ctx context.Context, spec *resourcev1.CatalogSource) (map[string][]byte, error) {
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
			return nil, ctx.Err()
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

// kept is a commit's files as kept, if they are.
func (s *Sources) kept(spec *resourcev1.CatalogSource) (map[string][]byte, bool) {
	kept, plan := s.folders(spec)
	if kept == "" {
		return nil, false
	}
	for _, dir := range []string{kept, plan} {
		files, err := readKept(dir)
		if err != nil {
			continue
		}
		// What an older converter wrote is converted again, as this one may write
		// more of it, or less, and goes, so what it converts now is kept in its place.
		if _, ok := readLeftOut(files, formatOf(spec)); converted(spec) && !ok {
			_ = os.RemoveAll(dir)
			continue
		}
		return files, true
	}
	return nil, false
}

// fetchOne fetches a commit's files once the slot is free, within fetchWait of
// asking, and keeps them for a plan.
func (s *Sources) fetchOne(ctx context.Context, slot chan struct{}, spec *resourcev1.CatalogSource) (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for another source's fetch: %w", ctx.Err())
	}
	// Kept by a fetch this one waited behind, of the same commit by another name.
	if files, ok := s.kept(spec); ok {
		return files, nil
	}
	_, plan := s.folders(spec)
	if converted(spec) {
		files, err := s.convertOne(ctx, spec)
		if err != nil {
			return nil, err
		}
		if plan != "" {
			keep(plan, files)
			s.prunePlans()
		}
		return files, nil
	}
	fetch := s.Fetch
	if fetch == nil {
		fetch = gitfetch.Fetch
	}
	fetched, err := fetch(ctx, s.Client, spec.GetRepository(), spec.GetCommit(), spec.GetPath(), gitfetch.Default)
	if err != nil {
		return nil, err
	}
	// Only what could be an entry, <id>.yaml, is read on or kept: a source is a
	// stranger's, and nothing else of it is any use.
	files := map[string][]byte{}
	for name, data := range fetched {
		if entryFile(name) {
			files[name] = data
		}
	}
	if plan != "" {
		keep(plan, files)
		s.prunePlans()
	}
	return files, nil
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

// keep keeps a commit's files in dir, whole or not at all, so a read that stopped
// halfway isn't taken for one. Each read writes a folder of its own, through os.Root
// so no name reaches out of it, and the first to finish keeps it.
func keep(dir string, files map[string][]byte) {
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
	for name, data := range files {
		if !entryFile(name) && name != leftOutFile || root.WriteFile(name, data, 0o600) != nil {
			return
		}
	}
	_ = os.Rename(tmp, dir)
}

func readKept(dir string) (map[string][]byte, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // what this kept, under the control plane's own folder
		if err != nil {
			return nil, err
		}
		out[e.Name()] = data
	}
	return out, nil
}

// readSource reads a source and checks its entries, once for each commit and each
// set of IDs before it.
func (s *Sources) readSource(ctx context.Context, name string, spec *resourcev1.CatalogSource, taken map[string]string) (sourceRead, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s%s", name, spec.GetRepository(), spec.GetPath(), spec.GetCommit(), formatKey(spec))
	for _, id := range slices.Sorted(maps.Keys(taken)) {
		fmt.Fprintf(h, "\x00%s=%s", id, taken[id])
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
	got = checkSource(ctx, name, spec, files, taken)
	s.mu.Lock()
	if s.read == nil {
		s.read = map[string]sourceRead{}
	}
	s.read[key] = got
	s.mu.Unlock()
	return got, nil
}

// checkSource checks each entry of a source as Dockwell's own are: as catalog.Parse
// reads one, in a file named by its ID, with its Compose file read as an install
// would read it. One whose ID taken has is left out, saying whose it is. A file
// that isn't an entry, such as a README, is no entry to leave out. An entry it
// keeps loses its icon and screenshots, which are the panel's own files.
//
// Another store's commit was converted as it was read, so its entries name their
// images by tag, for an install to pin, and what the converter left out comes first,
// each app with its reason.
func checkSource(ctx context.Context, name string, spec *resourcev1.CatalogSource, files map[string][]byte, taken map[string]string) sourceRead {
	var out sourceRead
	byTag := converted(spec)
	refuse := func(file, why string) {
		r := &dockwellv1.CatalogSourceRefusal{File: file, Reason: why}
		if byTag {
			r.Id, r.Kind = strings.TrimSuffix(file, ".yaml"), "check"
		}
		out.refused = append(out.refused, r)
	}
	parse := catalog.Parse
	if byTag {
		parse = catalog.ParseTagged
		rec, _ := readLeftOut(files, formatOf(spec))
		out.apps = rec.Apps
		out.license = rec.License
		for _, a := range rec.LeftOut {
			out.refused = append(out.refused, &dockwellv1.CatalogSourceRefusal{File: a.Path, Reason: a.Reason, Kind: a.Kind, Id: a.ID})
		}
	}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		id, ok := strings.CutSuffix(file, ".yaml")
		if !ok {
			continue
		}
		e, err := parse(files[file])
		switch {
		case err != nil:
			refuse(file, err.Error())
			continue
		case e.GetId() != id:
			refuse(file, fmt.Sprintf("it's the entry %s, in a file named for %s. Name the file %s.yaml", e.GetId(), id, e.GetId()))
			continue
		case taken[e.GetId()] != "":
			refuse(file, fmt.Sprintf("%s has an entry of the ID %s, which comes first. Give it another ID", taken[e.GetId()], e.GetId()))
			if byTag {
				out.refused[len(out.refused)-1].Kind = "taken"
			}
			continue
		}
		if err := readsAsInstall(ctx, e, byTag); err != nil {
			refuse(file, err.Error())
			continue
		}
		// The artwork an entry may name ships with the panel, for Dockwell's own
		// apps. A source's entry shows none of it, so a stranger's app can't wear
		// the logo and screens of one Dockwell tests.
		e.Icon, e.Screenshots = "", nil
		out.entries = append(out.entries, e)
		taken[e.GetId()] = "the catalog " + name
	}
	return out
}

// readsAsInstall reads an entry's Compose file as an install would, with stand-ins
// for what a person types. byTag takes an image named by a tag its file writes
// plainly, as an entry converted on the control plane names one until an install
// pins it.
func readsAsInstall(ctx context.Context, e *catalogv1.Entry, byTag bool) error {
	given := map[string]string{}
	for _, in := range e.GetInputs() {
		if catalog.Asks(in) && in.GetDefaultValue() == "" && !in.GetOptional() {
			given[in.GetName()] = standIn(in)
		}
	}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		return fmt.Errorf("its form can't be filled in: %w", err)
	}
	stack, err := compose.Read(ctx, compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		return fmt.Errorf("its Compose file, as an install reads it: %w", err)
	}
	// Each service's image as an install reads it, however the file writes it: a
	// flow mapping or a quoted key gets past a check of lines (its M3).
	tagged := catalog.TaggedImages(e.GetCompose())
	for _, s := range stack.Services {
		image := s.Spec.GetApp().GetImage()
		if byTag && slices.Contains(tagged, image) {
			continue
		}
		if !pinnedImage.MatchString(image) {
			return fmt.Errorf("its service %s runs %s, which isn't pinned by its digest, as image@sha256:<digest>, so what it runs could change under it. Pin it", s.Name, image)
		}
	}
	return nil
}

// pinnedImage is an image named by its digest.
var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// stored is a source as the store holds it.
type stored struct {
	name string
	spec *resourcev1.CatalogSource
}

// storedSources reads the sources, in their names' order, which is the order their
// entries come in.
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
			out = append(out, stored{name: r.Name, spec: spec.(*resourcev1.CatalogSource)})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b stored) int { return strings.Compare(a.name, b.name) })
	return out, err
}

// origin is where an entry comes from, as the control plane knows it: Dockwell's own
// catalog, at its build's commit, or a source, at the commit it's read at. What an
// entry's own source field says, which a source's file writes itself, never counts.
type origin struct{ catalog, repository, path, commit string }

// originOf is where a pin says its install came from.
func originOf(pin *resourcev1.CatalogPin) origin {
	return origin{catalog: pin.GetCatalog(), repository: pin.GetRepository(), path: pin.GetPath(), commit: pin.GetCommit()}
}

func (o origin) words() string {
	if o.catalog == "" {
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
	reads   map[string]sourceRead
	unread  map[string]error
}

// entryFor is the catalog's entry for an install: its ID's, where it comes from the
// catalog the install came from.
func (s sourced) entryFor(pin *resourcev1.CatalogPin) (*catalogv1.Entry, bool) {
	e, ok := s.catalog.Entry(pin.GetEntry())
	return e, ok && s.origins[pin.GetEntry()].same(pin)
}

// withSources adds each source's entries to the catalog, in their names' order. With
// instead, it's what a plan would make of them: a source of each name in place of
// what's stored, or none for nil. A source that can't be read now adds nothing, and
// says why.
func (h *Handler) withSources(ctx context.Context, c *catalog.Catalog, instead map[string]*resourcev1.CatalogSource) (sourced, error) {
	list, err := h.storedSources(ctx)
	if err != nil {
		return sourced{}, err
	}
	if instead != nil {
		list = slices.DeleteFunc(list, func(s stored) bool { _, ok := instead[s.name]; return ok })
		for name, spec := range instead {
			if spec != nil {
				list = append(list, stored{name: name, spec: spec})
			}
		}
		slices.SortFunc(list, func(a, b stored) int { return strings.Compare(a.name, b.name) })
	}
	if instead == nil {
		h.sources.tidy(list)
	}
	out := sourced{catalog: c, origins: map[string]origin{}, reads: map[string]sourceRead{}, unread: map[string]error{}}
	taken := map[string]string{}
	for _, e := range c.All() {
		taken[e.GetId()] = "Dockwell's own catalog"
		out.origins[e.GetId()] = origin{repository: e.GetSource().GetRepository(), commit: e.GetSource().GetCommit()}
	}
	for _, s := range list {
		got, err := h.sources.readSource(ctx, s.name, s.spec, maps.Clone(taken))
		if err != nil {
			out.unread[s.name] = err
			h.logger.WarnContext(ctx, "a catalog source can't be read, so its entries are left out until it can", "source", s.name, "url", s.spec.GetRepository(), "commit", s.spec.GetCommit(), "error", err)
			continue
		}
		out.reads[s.name] = got
		out.catalog = out.catalog.With(got.entries)
		// Taken whether the read was kept or made, so a later source's clash is the
		// same either way (its M4).
		for _, e := range got.entries {
			taken[e.GetId()] = "the catalog " + s.name
			out.origins[e.GetId()] = origin{catalog: s.name, repository: s.spec.GetRepository(), path: s.spec.GetPath(), commit: s.spec.GetCommit()}
		}
	}
	return out, nil
}

// handsNotes says what a plan for a source changes of where entries come from: an ID
// it takes from another catalog, or that another gets from it, and each install that
// would have no upgrade after it, or would have one again.
func (h *Handler) handsNotes(ctx context.Context, ref resource.Ref, name string, instead map[string]*resourcev1.CatalogSource) ([]revision.Note, error) {
	own, err := catalog.Load(h.commit)
	if err != nil {
		return nil, err
	}
	before, err := h.withSources(ctx, own, nil)
	if err != nil {
		return nil, err
	}
	after, err := h.withSources(ctx, own, instead)
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

// SetCatalogSource plans adding a source, or moving one to another commit, with what
// that commit holds and what the catalog leaves out of it in the plan's notes.
func (h *Handler) SetCatalogSource(ctx context.Context, req *connect.Request[dockwellv1.SetCatalogSourceRequest]) (*connect.Response[dockwellv1.SetCatalogSourceResponse], error) {
	name := req.Msg.GetName()
	if err := (catalogsource.Type{}).CheckName(name); err != nil {
		return nil, authapi.Invalid(err)
	}
	spec := &resourcev1.CatalogSource{Repository: strings.TrimSpace(req.Msg.GetRepository()), Path: req.Msg.GetPath(), Commit: strings.ToLower(strings.TrimSpace(req.Msg.GetCommit())), Format: req.Msg.GetFormat()}
	if err := (catalogsource.Type{}).Validate(spec); err != nil {
		return nil, authapi.Invalid(err)
	}
	if err := h.sources.chooseFormat(ctx, spec, req.Msg.GetFormat()); err != nil {
		return nil, err
	}
	own, err := catalog.Load(h.commit)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	// What comes before it: Dockwell's own, then each other source before it by name.
	others, err := h.storedSources(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	taken := map[string]string{}
	for _, e := range own.All() {
		taken[e.GetId()] = "Dockwell's own catalog"
	}
	var was *resourcev1.CatalogSource
	for _, o := range others {
		switch {
		case o.name == name:
			was = o.spec
		case o.name < name:
			if got, err := h.sources.readSource(ctx, o.name, o.spec, maps.Clone(taken)); err == nil {
				for _, e := range got.entries {
					taken[e.GetId()] = "the catalog " + o.name
				}
			}
		}
	}
	files, err := h.sources.files(ctx, spec)
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("Dockwell can't read %s at %s: %w", spec.GetRepository(), spec.GetCommit(), err), "unreadable") //nolint:staticcheck // it starts with a name
	}
	h.sources.forget(spec)
	got := checkSource(ctx, name, spec, files, maps.Clone(taken))
	ref := resource.Ref{Kind: catalogsource.Kind, Name: name}
	notes := []revision.Note{{
		Reason:  "catalog_source_reads",
		Message: readsWords(spec, got),
		Ref:     ref,
		Names:   ids(got.entries),
	}, h.sources.commitNote(ctx, ref, spec)}
	if converted(spec) {
		// One note, counted by kind: a store leaves out hundreds of apps, and each
		// one's reason is a step away on the source's page.
		notes = append(notes, leftOutNote(ref, got)...)
	} else {
		for _, r := range got.refused {
			notes = append(notes, revision.Note{Reason: "catalog_source_refuses", Message: "It leaves out " + r.GetFile() + ": " + r.GetReason(), Ref: ref, Names: []string{r.GetFile()}})
		}
	}
	summary := "Add the catalog " + name + " from " + spec.GetRepository()
	if was != nil {
		// Said as a move from one commit to the other, so its undo reads right too.
		summary = "Move the catalog " + name + " from " + was.GetCommit()[:12] + " to " + spec.GetCommit()[:12]
		if before, err := h.sources.files(ctx, was); err == nil {
			notes = append(notes, movesNote(ref, name, checkSource(ctx, name, was, before, maps.Clone(taken)).entries, got.entries)...)
		}
	}
	hands, err := h.handsNotes(ctx, ref, name, map[string]*resourcev1.CatalogSource{name: spec})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	notes = append(notes, hands...)
	change, err := h.changes.Propose(ctx, summary, []*dockwellv1.Edit{{
		Resource: resourcepb.Ref(ref),
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_CatalogSource{CatalogSource: spec}},
	}}, nil, changes.Also{Notes: notes})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetCatalogSourceResponse{Change: change}), nil
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
	own, err := catalog.Load(h.commit)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	list, err := h.storedSources(ctx)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	s, err := h.withSources(ctx, own, nil)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	reads, unread := s.reads, s.unread
	out := &dockwellv1.ListCatalogSourcesResponse{}
	for _, s := range list {
		info := &dockwellv1.CatalogSourceInfo{Name: s.name, Source: s.spec}
		if err := unread[s.name]; err != nil {
			info.Unread = err.Error()
		}
		if got, ok := reads[s.name]; ok {
			info.Entries, info.Refused = ids(got.entries), got.refused
		}
		out.Sources = append(out.Sources, info)
	}
	return connect.NewResponse(out), nil
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
	hands, err := h.handsNotes(ctx, ref, name, map[string]*resourcev1.CatalogSource{name: nil})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	change, err := h.changes.Propose(ctx, "Stop reading the catalog "+name, []*dockwellv1.Edit{{Resource: resourcepb.Ref(ref), Delete: true}}, nil, changes.Also{Notes: append(notes, hands...)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RemoveCatalogSourceResponse{Change: change}), nil
}
