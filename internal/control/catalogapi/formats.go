// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/catalog/folder"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
)

// Other stores' formats, read on the control plane (ADR 0078): a source may be a
// 1Panel app store, Dokploy's templates or Coolify's, which Dockwell converts as it
// reads the commit, running nothing, and keeps converted, as it keeps a commit of
// entries. A person pastes the link and never sees a step of it.

const (
	// convertWait bounds converting a commit, on top of fetching it.
	convertWait = 60 * time.Second
	// leftOutFile is what a converted commit keeps of the apps it left out, beside
	// its entries.
	leftOutFile = "left-out.json"
)

// formats are the formats a source's spec names, by the converter's word for each.
var formats = map[resourcev1.CatalogFormat]string{
	resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED: convert.FormatDockwell,
	resourcev1.CatalogFormat_CATALOG_FORMAT_DOCKWELL:    convert.FormatDockwell,
	resourcev1.CatalogFormat_CATALOG_FORMAT_ONEPANEL:    convert.FormatOnePanel,
	resourcev1.CatalogFormat_CATALOG_FORMAT_DOKPLOY:     convert.FormatDokploy,
	resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY:     convert.FormatCoolify,
	resourcev1.CatalogFormat_CATALOG_FORMAT_RUNTIPI:     convert.FormatRuntipi,
}

// formatOf is how a source's folder is laid out, in the converter's word.
func formatOf(spec *resourcev1.CatalogSource) string {
	return cmp.Or(formats[spec.GetFormat()], convert.FormatDockwell)
}

// formatEnum is a format's value in the schema.
func formatEnum(word string) resourcev1.CatalogFormat {
	for f, w := range formats {
		if w == word && f != resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED {
			return f
		}
	}
	return resourcev1.CatalogFormat_CATALOG_FORMAT_DOCKWELL
}

// converted says a source is another store's, which Dockwell converts.
func converted(spec *resourcev1.CatalogSource) bool {
	_, ok := convert.StoreOf(formatOf(spec))
	return ok
}

// formatWords says a format as the plan does.
func formatWords(spec *resourcev1.CatalogSource) string {
	if s, ok := convert.StoreOf(formatOf(spec)); ok {
		return s.Name
	}
	return "Dockwell's own entries"
}

// leftOutRecord is what a converted commit keeps beside its entries: how it was read,
// and each app it left out, and why.
type leftOutRecord struct {
	Format  string       `json:"format"`
	Version int          `json:"version"`
	License string       `json:"license"`
	Apps    int          `json:"apps"`
	LeftOut []leftOutApp `json:"leftOut"`
}

type leftOutApp struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// readLeftOut reads what a converted commit keeps of the apps it left out, if it's
// what this converter wrote for this format.
func readLeftOut(raw []byte, format string) (leftOutRecord, bool) {
	var rec leftOutRecord
	if raw == nil || json.Unmarshal(raw, &rec) != nil || rec.Format != format || rec.Version != convert.Version {
		return leftOutRecord{}, false
	}
	return rec, true
}

// convertOne fetches a store's commit in two rounds, its folders and then only what
// its adapter reads, converts it, and returns what's kept of it: each entry as
// <id>.yaml, and leftOutFile; and with a third round, the logos of the apps that
// convert, by the names they're kept by in the folder media (storeIcons).
func (s *Sources) convertOne(ctx context.Context, spec *resourcev1.CatalogSource) (map[string][]byte, map[string][]byte, error) {
	store, _ := convert.StoreOf(formatOf(spec))
	// Fetching and converting each have a budget of their own, so a slow host leaves
	// converting its time, and a heavy commit can't take the fetch's.
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, fetchWait)
	defer cancel()
	got, err := convert.Fetch(ctx, s.Client, store, spec.GetRepository(), spec.GetCommit(), spec.GetPath())
	if err != nil {
		return nil, nil, err
	}
	defer got.Close()
	src, snap, byPath := got.Source, got.Snapshot, got.ByPath
	converting, done := context.WithTimeout(parent, convertWait)
	defer done()
	run, err := convert.OnServer(converting, store, got.Listing, src, got.Time)
	if err != nil {
		return nil, nil, fmt.Errorf("reading it as %s: %w", store.Name, err)
	}
	images := storeIcons(ctx, snap, run, byPath)
	out := map[string][]byte{}
	rec := leftOutRecord{Format: store.Format, Version: convert.Version, License: src.License, Apps: len(run.Results)}
	date := got.Time.UTC().Format(time.DateOnly)
	size := 0
	for i := range run.Results {
		r := &run.Results[i]
		if r.Entry == nil {
			rec.LeftOut = append(rec.LeftOut, leftOutApp{ID: cut(r.ID, maxLeftOutName), Path: cut(r.Path, maxLeftOutName), Kind: r.Class.Key(), Reason: cut(r.LeftOut, maxReason)})
			continue
		}
		raw, err := convert.Encode(r.Entry, src, date)
		if err != nil {
			rec.LeftOut = append(rec.LeftOut, leftOutApp{ID: cut(r.ID, maxLeftOutName), Path: cut(r.Path, maxLeftOutName), Kind: convert.ClassFile.Key(), Reason: cut(err.Error(), maxReason)})
			continue
		}
		if size += len(raw); size > convert.MaxConverted {
			return nil, nil, fmt.Errorf("reading it as %s: what it converts to comes to over %d MiB, more than a source keeps of one commit", store.Name, convert.MaxConverted>>20)
		}
		out[r.ID+".yaml"] = raw
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, nil, err
	}
	out[leftOutFile] = raw
	return out, images, nil
}

// storeIcons reads the logo of each app that converts, in a round of its own, within
// the limits of a catalog's images (ADR 0077): 1,000 at most, 1 MiB each, 32 MiB
// together, by the names they're kept by in the folder media, as folder.Prepare
// keeps them, and names each in its entry's icon. A logo that can't be read, or
// shown, leaves its app without one, and a round that fails leaves every app without
// one, as an app's place never hangs on its logo (ADR 0083). ctx is the fetch's.
func storeIcons(ctx context.Context, snap *gitfetch.Snapshot, run convert.Run, listed map[string]gitfetch.Entry) map[string][]byte {
	var want []gitfetch.Entry
	owners := map[string][]int{}
	for i := range run.Results {
		r := &run.Results[i]
		if r.Entry == nil || r.Icon == "" {
			continue
		}
		e, ok := listed[r.Icon]
		if !ok {
			continue
		}
		if _, seen := owners[e.Path]; !seen {
			if len(want) == folder.MediaLimits.Files {
				continue
			}
			want = append(want, e)
		}
		owners[e.Path] = append(owners[e.Path], i)
	}
	if len(want) == 0 {
		return nil
	}
	lim := folder.MediaLimits
	read, _, err := snap.ReadWithin(ctx, want, gitfetch.ReadLimits{Files: lim.Files, File: lim.File, Total: lim.Total})
	if err != nil {
		return nil
	}
	images := map[string][]byte{}
	var kept int64
	for _, e := range want {
		data, ok := read[e.Path]
		if !ok {
			continue // over 1 MiB
		}
		for _, i := range owners[e.Path] {
			r := &run.Results[i]
			name := convert.IconName(r.ID, e.Path)
			if !folder.ImageName(name) {
				continue
			}
			prepared, err := folder.Prepare(name, data)
			if err != nil || len(images) == lim.Files || kept+int64(len(prepared)) > lim.Total {
				continue
			}
			kept += int64(len(prepared))
			images[name] = prepared
			r.Entry.Icon = folder.Media + "/" + name
		}
	}
	return images
}

// What a converted commit keeps of each app it leaves out: its ID and folder, and a
// reason, each cut to a length a person reads, as a store names them, and 2,000 apps
// of a megabyte each would otherwise be kept and listed.
const (
	maxLeftOutName = 200
	maxReason      = 500
)

// cut is text of n characters at most, one line, ending in an ellipsis where it's cut.
func cut(s string, n int) string {
	s = convert.CommentText(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// guessed is a commit's layouts, as detect read them.
type guessed struct {
	guesses []convert.Guess
	counts  map[convert.Guess]int
}

// detect says how a commit's folder is laid out, from its folders alone, the
// likeliest first, each with the folder under dir that holds it. A commit never
// changes, so what it read of one is kept, mostGuesses of them, and it reads one at a
// time, as a form asks as a person types.
func (s *Sources) detect(ctx context.Context, repo, commit, dir string) ([]convert.Guess, map[convert.Guess]int, error) {
	key := repo + "\x00" + dir + "\x00" + commit
	s.mu.Lock()
	if g, ok := s.guessed[key]; ok {
		s.mu.Unlock()
		return g.guesses, g.counts, nil
	}
	if s.detecting == nil {
		s.detecting = make(chan struct{}, 1)
	}
	slot := s.detecting
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("waiting for another repository's read: %w", ctx.Err())
	}
	guesses, counts, err := s.detectOnce(ctx, repo, commit, dir)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.guessed == nil || len(s.guessed) >= mostGuesses {
		s.guessed = map[string]guessed{}
	}
	s.guessed[key] = guessed{guesses, counts}
	s.mu.Unlock()
	return guesses, counts, nil
}

func (s *Sources) detectOnce(ctx context.Context, repo, commit, dir string) ([]convert.Guess, map[convert.Guess]int, error) {
	snap, err := gitfetch.Trees(ctx, s.Client, repo, commit, gitfetch.TreeDefault)
	if err != nil {
		return nil, nil, err
	}
	defer snap.Close()
	prefix := strings.Trim(path.Clean("/"+dir), "/")
	listed, err := snap.Walk(prefix, func(p string, depth int) bool {
		if prefix != "" {
			p = strings.TrimPrefix(p, prefix+"/")
		}
		return convert.DetectDescend(p, depth)
	})
	if err != nil {
		return nil, nil, err
	}
	var paths []string
	for _, e := range listed {
		if !e.Dir {
			p := e.Path
			if prefix != "" {
				p = strings.TrimPrefix(p, prefix+"/")
			}
			paths = append(paths, p)
		}
	}
	guesses, counts := convert.Detect(paths)
	return guesses, counts, nil
}

// chooseFormat says how a source a person sets reads: as they say, or as Dockwell
// tells from the commit. A folder that holds no catalog Dockwell knows reads as
// Dockwell's own, whose plan says it reads no entries; one that could be more than
// one is refused, naming each, for the person to pick from.
func (s *Sources) chooseFormat(ctx context.Context, spec *resourcev1.CatalogSource, asked resourcev1.CatalogFormat) error {
	if asked != resourcev1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED {
		spec.Format = asked
		return nil
	}
	guesses, _, err := s.detect(ctx, spec.GetRepository(), spec.GetCommit(), spec.GetPath())
	if err != nil {
		return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("Dockwell can't read %s at %s: %w", spec.GetRepository(), spec.GetCommit(), err), "unreadable") //nolint:staticcheck // it starts with a name
	}
	switch len(guesses) {
	case 0:
		spec.Format = resourcev1.CatalogFormat_CATALOG_FORMAT_DOCKWELL
		return nil
	case 1:
		spec.Format = formatEnum(guesses[0].Format)
		spec.Path = strings.Trim(path.Join(spec.GetPath(), guesses[0].Path), "/")
		return nil
	}
	var options, words []string
	for _, g := range guesses {
		f := formatEnum(g.Format)
		options = append(options, f.String())
		words = append(words, formatWords(&resourcev1.CatalogSource{Format: f}))
	}
	return authapi.ChoiceError(&dockwellv1.Choice{Level: "format", Field: "format", Options: options},
		fmt.Errorf("the folder could be read as %s. Say which with format", strings.Join(words, " or ")))
}

// guessesOf are a commit's layouts, as ListCatalogSourceRefs says them.
func (s *Sources) guessesOf(ctx context.Context, repo, commit string) []*dockwellv1.CatalogSourceGuess {
	guesses, counts, err := s.detect(ctx, repo, commit, "")
	if err != nil {
		return nil
	}
	var out []*dockwellv1.CatalogSourceGuess
	for _, g := range guesses {
		out = append(out, &dockwellv1.CatalogSourceGuess{Format: formatEnum(g.Format), Path: g.Path, Apps: uint32(min(counts[g], 1<<31))}) //nolint:gosec // bounded
	}
	return out
}

// leftOutNote says, in one note, how many of a store's apps a commit leaves out and
// why, counted by kind, as each app's reason is a step away on the Sources page.
func leftOutNote(ref resource.Ref, got sourceRead) []revision.Note {
	if len(got.refused) == 0 {
		return nil
	}
	counts := map[string]int{}
	var names []string
	for _, r := range got.refused {
		counts[r.GetKind()]++
		names = append(names, cmp.Or(r.GetId(), r.GetFile()))
	}
	kinds := slices.Collect(maps.Keys(counts))
	slices.SortFunc(kinds, func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	var parts []string
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], cmp.Or(kindWords[k], "for other reasons")))
	}
	return []revision.Note{{
		Reason: "catalog_source_leaves_out", Ref: ref, Names: names,
		Message: fmt.Sprintf("It leaves out %d of %d apps: %s. The source's page lists each, and why.", len(got.refused), max(got.apps, len(got.refused)+len(got.entries)), strings.Join(parts, ", ")),
	}}
}

// kindWords say each kind of reason an app is left out, after a count.
var kindWords = map[string]string{
	"scripts": "run scripts on the server", "another-app": "use another app's database", "shipped": "ship files beside their Compose file",
	"no-version": "name no version that stays put", "arch": "run on amd64 alone", "platform": "use a value their platform fills in",
	"cant-say": "ask for what an entry can't say yet", "cant-run": "ask for what Dockwell can't run yet", "check": "fail the catalog's checks",
	"file": "have files that can't be read", "inside": "put a secret inside a longer value", "made-up": "ask for a value Dockwell doesn't make yet",
	"skipped": "are marked by their store to leave out", "terms": "come under terms of their own",
	"taken": "have an ID a catalog before it has",
}

// convertedWords says what a converted commit brings, for its plan.
func convertedWords(spec *resourcev1.CatalogSource, entries []*catalogv1.Entry, license string) string {
	where := spec.GetRepository()
	if spec.GetPath() != "" {
		where = "the folder " + spec.GetPath() + " in " + where
	}
	if license == "NOASSERTION" || license == "" {
		license = "a licence Dockwell can't tell from its LICENSE file"
	}
	n := len(entries)
	return fmt.Sprintf("It reads %d %s from %s, at %s, as %s, under %s.", n, plural(n, "app", "apps"), where, spec.GetCommit()[:12], formatWords(spec), license)
}

// mostDigests is how many images one call to an agent asks the digests of, as it
// reads 20 at most.
const mostDigests = 20

// pinImages pins each image an entry converted on the control plane runs by tag, as a
// plan installs it (ADR 0078): to the digest the install runs already, from had,
// where the tag is the same, or else the one the tag points at now, as the agent of
// the server it goes on reads it. Only what its services run, as an install reads
// the file, is asked about: an image: line elsewhere, such as a variable, was never
// checked as an image. Each is checked again for where its registry is, and each
// digest for its shape. An entry pinned already is returned as it is. The notes say
// each digest that's new, naming no resource, as the plan's project is what they're
// of.
func (h *Handler) pinImages(ctx context.Context, e *catalogv1.Entry, server string, had map[string]string) (*catalogv1.Entry, []revision.Note, error) {
	tagged := catalog.TaggedImages(e.GetCompose())
	if len(tagged) == 0 {
		return e, nil, nil
	}
	runs, err := runImages(ctx, e)
	if err != nil {
		return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, err, "invalid")
	}
	images := slices.DeleteFunc(slices.Clone(tagged), func(image string) bool { return !slices.Contains(runs, image) })
	digests := map[string]string{}
	var ask []string
	for _, image := range images {
		if why := convert.PrivateRegistry(image); why != "" {
			return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("%s is on a registry that's %s, where a store's app may not pull from, so it can't be installed", image, why), "private_registry")
		}
		if d := had[image]; catalog.Digest(d) {
			digests[image] = d
		} else {
			ask = append(ask, image)
		}
	}
	var notes []revision.Note
	if len(ask) > 0 {
		if h.digests == nil {
			return nil, nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s's images can't be pinned by digest here, so its install can't be planned. Try again once a server is connected", e.GetName()), authapi.ReasonUnavailable)
		}
		found := map[string]string{}
		for chunk := range slices.Chunk(ask, mostDigests) {
			got, err := h.digests.Digests(ctx, server, chunk)
			if err != nil {
				return nil, nil, err
			}
			maps.Copy(found, got)
		}
		var pinned []string
		for _, image := range ask {
			d := found[image]
			switch {
			case d == "":
				return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the registry of %s didn't say which build its tag points at, so Dockwell can't pin it, and the plan can't say what it installs. Try again in a moment", image), "unpinned")
			case !catalog.Digest(d):
				return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the registry of %s answered with a digest that isn't one, so Dockwell can't pin it. Try again later, or check the registry", image), "unpinned")
			}
			digests[image] = d
			pinned = append(pinned, image+" at "+shortDigest(d))
		}
		notes = append(notes, revision.Note{
			Reason: "catalog_pins_images", Names: ask,
			Message: "It pins " + strings.Join(pinned, ", ") + ", the build each tag points at now, so what runs stays put until a plan moves it.",
		})
	}
	out := proto.CloneOf(e)
	out.Compose = catalog.Pin(e.GetCompose(), digests)
	return out, notes, nil
}

// runImages are the images an entry's services run, as an install reads its file,
// with stand-ins for what a person types.
func runImages(ctx context.Context, e *catalogv1.Entry) ([]string, error) {
	stack, err := folder.ReadStack(ctx, e)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range stack.Services {
		out = append(out, s.Spec.GetApp().GetImage())
	}
	return out, nil
}

// shortDigest is a digest as a person reads one: its algorithm and first 12 digits.
func shortDigest(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	return d
}

// Digests reads which build each image's tag points at now, through the agent of a
// server (ADR 0062), by its ID or name, or the only one when it's empty.
type Digests interface {
	Digests(ctx context.Context, server string, images []string) (map[string]string, error)
}

// PinWith has the handler pin images by digest through d when a plan installs an
// entry converted on the control plane.
func (h *Handler) PinWith(d Digests) *Handler {
	h.digests = d
	return h
}
