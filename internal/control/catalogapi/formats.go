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
func readLeftOut(files map[string][]byte, format string) (leftOutRecord, bool) {
	var rec leftOutRecord
	raw, ok := files[leftOutFile]
	if !ok || json.Unmarshal(raw, &rec) != nil || rec.Format != format || rec.Version != convert.Version {
		return leftOutRecord{}, false
	}
	return rec, true
}

// convertOne fetches a store's commit in two rounds, its folders and then only what
// its adapter reads, converts it, and returns what's kept of it: each entry as
// <id>.yaml, and leftOutFile.
func (s *Sources) convertOne(ctx context.Context, spec *resourcev1.CatalogSource) (map[string][]byte, error) {
	store, _ := convert.StoreOf(formatOf(spec))
	ctx, cancel := context.WithTimeout(ctx, convertWait)
	defer cancel()
	snap, err := gitfetch.Trees(ctx, s.Client, spec.GetRepository(), spec.GetCommit(), gitfetch.TreeDefault)
	if err != nil {
		return nil, err
	}
	defer snap.Close()
	prefix := strings.Trim(path.Clean("/"+spec.GetPath()), "/")
	rel := func(p string) string {
		if prefix == "" {
			return p
		}
		return strings.TrimPrefix(p, prefix+"/")
	}
	listed, err := snap.Walk(prefix, func(p string, depth int) bool { return store.Descend(rel(p), depth) })
	if err != nil {
		return nil, err
	}
	l := &convert.Listing{Dirs: map[string]bool{}, Files: map[string]bool{}, Big: map[string]bool{}}
	var want []gitfetch.Entry
	for _, e := range listed {
		if e.Dir {
			l.Dirs[rel(e.Path)] = true
			continue
		}
		l.Files[rel(e.Path)] = true
		if store.Want(rel(e.Path)) {
			want = append(want, e)
		}
	}
	// The repository's own licence, at its top, which every entry keeps.
	top, err := snap.Walk("", nil)
	if err != nil {
		return nil, err
	}
	var licenseFile []gitfetch.Entry
	for _, name := range convert.LicenseFiles {
		if i := slices.IndexFunc(top, func(e gitfetch.Entry) bool { return e.Path == name && !e.Dir }); i >= 0 {
			licenseFile = append(licenseFile, top[i])
			break
		}
	}
	files, big, err := snap.Read(ctx, append(want, licenseFile...))
	if err != nil {
		return nil, err
	}
	l.Read = map[string][]byte{}
	for p, b := range files {
		l.Read[rel(p)] = b
	}
	for _, p := range big {
		l.Big[rel(p)] = true
	}
	src := convert.Source{Name: store.Name, Repository: spec.GetRepository(), Commit: spec.GetCommit(), License: "NOASSERTION"}
	if len(licenseFile) > 0 {
		text := files[licenseFile[0].Path]
		if found := convert.LicenseOf(text); found != "" {
			src.License, src.LicenseText, src.CopyrightInLicense = found, text, found == "MIT"
		}
	}
	run, err := convert.OnServer(ctx, store, l, src, snap.Time)
	if err != nil {
		return nil, fmt.Errorf("reading it as %s: %w", store.Name, err)
	}
	out := map[string][]byte{}
	rec := leftOutRecord{Format: store.Format, Version: convert.Version, License: src.License, Apps: len(run.Results)}
	date := snap.Time.UTC().Format(time.DateOnly)
	for i := range run.Results {
		r := &run.Results[i]
		if r.Entry == nil {
			rec.LeftOut = append(rec.LeftOut, leftOutApp{ID: r.ID, Path: r.Path, Kind: r.Class.Key(), Reason: r.LeftOut})
			continue
		}
		raw, err := convert.Encode(r.Entry, src, date)
		if err != nil {
			rec.LeftOut = append(rec.LeftOut, leftOutApp{ID: r.ID, Path: r.Path, Kind: convert.ClassFile.Key(), Reason: err.Error()})
			continue
		}
		out[r.ID+".yaml"] = raw
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	out[leftOutFile] = raw
	return out, nil
}

// detect says how a commit's folder is laid out, from its folders alone, the
// likeliest first, each with the folder under dir that holds it.
func (s *Sources) detect(ctx context.Context, repo, commit, dir string) ([]convert.Guess, map[convert.Guess]int, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchWait)
	defer cancel()
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

// pinImages pins each image an entry converted on the control plane names by tag, as
// a plan installs it (ADR 0078): to the digest the install runs already, from had,
// where the tag is the same, or else the one the tag points at now, as the agent of
// the server it goes on reads it. An entry pinned already is returned as it is. The
// notes say each digest that's new.
func (h *Handler) pinImages(ctx context.Context, e *catalogv1.Entry, server string, had map[string]string, ref resource.Ref) (*catalogv1.Entry, []revision.Note, error) {
	images := catalog.TaggedImages(e.GetCompose())
	if len(images) == 0 {
		return e, nil, nil
	}
	digests := map[string]string{}
	var ask []string
	for _, image := range images {
		if d := had[image]; d != "" {
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
		found, err := h.digests.Digests(ctx, server, ask)
		if err != nil {
			return nil, nil, err
		}
		var pinned []string
		for _, image := range ask {
			d := found[image]
			if d == "" {
				return nil, nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the registry of %s didn't say which build its tag points at, so Dockwell can't pin it, and the plan can't say what it installs. Try again in a moment", image), "unpinned")
			}
			digests[image] = d
			pinned = append(pinned, image+" at "+shortDigest(d))
		}
		notes = append(notes, revision.Note{
			Reason: "catalog_pins_images", Ref: ref, Names: ask,
			Message: "It pins " + strings.Join(pinned, ", ") + ", the build each tag points at now, so what runs stays put until a plan moves it.",
		})
	}
	out := proto.CloneOf(e)
	out.Compose = catalog.Pin(e.GetCompose(), digests)
	return out, notes, nil
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
