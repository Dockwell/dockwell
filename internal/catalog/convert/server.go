// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/netguard"
)

// What the control plane reads of another store, as a stranger's repository, which
// now converts on the server itself and refreshes with nobody watching (ADR 0078).
const (
	// MaxApps is the most apps a store may hold, as a source holds 2,000 entries at
	// most (ADR 0055).
	MaxApps = 2000
	// maxServerFile is the most a Compose file, a form or a template may be on the
	// server: the largest of the three stores' was 38 KiB on 11 Nov 2026.
	maxServerFile = 256 << 10
	// maxNesting is how deep a file's YAML, TOML or JSON may nest: a Compose file
	// nests six or seven levels, and a parser recurses for each.
	maxNesting = 64
	// maxSummary is the most characters of a source's line about an app that an
	// entry's summary keeps.
	maxSummary = 160
	// Version is the converter's version, which goes up whenever what it writes
	// changes, so a commit converted by an older one is converted again.
	Version = 1
)

// The formats the control plane reads, beside Dockwell's own.
const (
	FormatOnePanel = "1panel"
	FormatDokploy  = "dokploy"
	FormatCoolify  = "coolify"
	FormatDockwell = "dockwell"
)

// A Store is a format of catalog the control plane converts: how its folder is laid
// out, what of it an adapter reads, and the adapter.
type Store struct {
	Format string
	// Name is how its entries say where they come from, such as "1Panel's app store".
	Name string
	// License is what its upstream publishes under.
	License string
	// Root is the folder it keeps its apps in, such as apps.
	Root string
	// Descend says whether to list a folder's contents, by its path in the store's
	// folder and how deep it is, from 1.
	Descend func(p string, depth int) bool
	// Want says whether to read a file's contents, by its path.
	Want func(p string) bool
	read func(t *tree, root string) ([]App, error)
}

// Stores are the formats the control plane converts.
var Stores = []Store{
	{
		Format: FormatOnePanel, Name: "1Panel's app store", License: "GPL-3.0-only", Root: "apps",
		// apps/<key>/<version>/..., where a version's own folders are read for what
		// they hold, as a script or a shipped file leaves its app out.
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "apps/") && depth <= 6 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			switch {
			case len(parts) == 3 && parts[0] == "apps" && parts[2] == "data.yml":
				return true
			case len(parts) == 4 && parts[0] == "apps" && (parts[3] == "data.yml" || parts[3] == "docker-compose.yml"):
				return true
			}
			return false
		},
		read: onePanelApps,
	},
	{
		Format: FormatDokploy, Name: "Dokploy's templates", License: "MIT", Root: "blueprints",
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "blueprints/") && depth <= 6 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			return len(parts) == 3 && parts[0] == "blueprints" && slices.Contains([]string{"docker-compose.yml", "template.toml", "meta.json"}, parts[2])
		},
		read: dokployApps,
	},
	{
		Format: FormatCoolify, Name: "Coolify's templates", License: "Apache-2.0", Root: "templates/compose",
		Descend: func(p string, depth int) bool { return p == "templates" || p == "templates/compose" },
		Want: func(p string) bool {
			dir, file := path.Split(p)
			return dir == "templates/compose/" && strings.HasSuffix(file, ".yaml")
		},
		read: coolifyApps,
	},
}

// StoreOf is the store of a format, if the control plane converts it.
func StoreOf(format string) (Store, bool) {
	for _, s := range Stores {
		if s.Format == format {
			return s, true
		}
	}
	return Store{}, false
}

// A Guess is a format a folder looks like, and the folder in it that holds the
// catalog, as the source's folder names it.
type Guess struct {
	Format string
	Path   string
}

// DetectDescend is what Detect needs listed: the top two levels, and the folders a
// store keeps its apps in, one level further.
func DetectDescend(p string, depth int) bool {
	switch depth {
	case 1:
		return true
	case 2:
		top, _, _ := strings.Cut(p, "/")
		return slices.Contains([]string{"apps", "blueprints", "templates", "entries"}, top)
	}
	return false
}

// Detect says which formats a folder's listing looks like, by the paths in it, from
// the folder: 1Panel's apps/<key>/data.yml, Dokploy's blueprints/<id>/template.toml,
// Coolify's templates/compose/<id>.yaml, and Dockwell's <id>.yaml, at the top or in
// entries/. More than one guess means it's unsure, for a person to pick from; none
// means it's no catalog Dockwell reads.
func Detect(files []string) ([]Guess, map[Guess]int) {
	counts := map[Guess]int{}
	for _, p := range files {
		parts := strings.Split(p, "/")
		switch {
		case len(parts) == 3 && parts[0] == "apps" && parts[2] == "data.yml":
			counts[Guess{FormatOnePanel, ""}]++
		case len(parts) == 3 && parts[0] == "blueprints" && parts[2] == "template.toml":
			counts[Guess{FormatDokploy, ""}]++
		case len(parts) == 3 && parts[0] == "templates" && parts[1] == "compose" && strings.HasSuffix(parts[2], ".yaml"):
			counts[Guess{FormatCoolify, ""}]++
		case len(parts) == 1 && entryName(parts[0]):
			counts[Guess{FormatDockwell, ""}]++
		case len(parts) == 2 && parts[0] == "entries" && entryName(parts[1]):
			counts[Guess{FormatDockwell, "entries"}]++
		}
	}
	// A store's layout says more than a few YAML files beside it, as 1Panel's and
	// Coolify's repositories keep such files at their top.
	store := 0
	for g, n := range counts {
		if g.Format != FormatDockwell {
			store = max(store, n)
		}
	}
	var out []Guess
	for g, n := range counts {
		if g.Format == FormatDockwell && g.Path == "" && n <= store {
			continue
		}
		out = append(out, g)
	}
	// The store with the most apps first, then by name, so a person's picker leads
	// with the likeliest.
	slices.SortFunc(out, func(a, b Guess) int {
		if c := counts[b] - counts[a]; c != 0 {
			return c
		}
		return strings.Compare(a.Format+a.Path, b.Format+b.Path)
	})
	return out, counts
}

// entryName says a file's name is a Dockwell entry's, <id>.yaml, by its shape.
func entryName(name string) bool {
	id, ok := strings.CutSuffix(name, ".yaml")
	if !ok || id == "" || len(id) > 63 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// LicenseFiles are where a repository's licence is read from, at its top.
var LicenseFiles = []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING"}

// LicenseOf is the SPDX licence a repository's licence's text is, for the licences
// the stores publish under, or "" when it isn't plainly one of them. It reads a
// licence's title, at its top, as the GPL's own text names the AGPL further down.
func LicenseOf(text []byte) string {
	t := strings.Join(strings.Fields(strings.ToLower(string(text))), " ")
	head := t[:min(len(t), 200)]
	switch {
	case strings.HasPrefix(head, "gnu affero general public license version 3"):
		return "AGPL-3.0-only"
	case strings.HasPrefix(head, "gnu general public license version 3"):
		return "GPL-3.0-only"
	case strings.Contains(head, "apache license version 2.0"):
		return "Apache-2.0"
	case strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT"
	}
	return ""
}

// A Run is one store's commit converted on the control plane.
type Run struct {
	// Results are each app's, sorted by ID.
	Results []Result
}

// OnServer converts a store's apps from a commit the control plane fetched, held in
// memory, and runs nothing: an app that needs a script on the server is left out, as
// ever. What only the offline converter has stays there: there are no overlays, so an
// entry's summary is the store's own line about the app, and what it reaches on its
// server is written from what the converter finds, for the plan to ask a person, as
// for any install. Its images stay named by tag, and are pinned when an install is
// planned. Each entry's release is when the commit was made, so a later commit's
// entry is a higher release, and an upgrade is offered only where its file changes.
func OnServer(ctx context.Context, store Store, l *Listing, src Source, committed time.Time) (Run, error) {
	t := &tree{mem: l}
	if !t.isDir(store.Root) {
		return Run{}, fmt.Errorf("it has no folder %s, where %s keeps its apps", store.Root, store.Name)
	}
	held := 0
	for _, set := range []map[string]bool{l.Dirs, l.Files} {
		for p := range set {
			if path.Dir(p) == store.Root {
				held++
			}
		}
	}
	if held > MaxApps {
		return Run{}, fmt.Errorf("it holds over %d apps, more than a source reads", MaxApps)
	}
	apps, err := store.read(t, src.Repository)
	if errors.Is(err, ErrNoApps) {
		return Run{}, fmt.Errorf("it holds no apps where %s keeps them", store.Name)
	}
	if err != nil {
		return Run{}, err
	}
	if len(apps) > MaxApps {
		return Run{}, fmt.Errorf("it holds %d apps, more than the %d a source reads", len(apps), MaxApps)
	}
	release := uint32(1)
	if secs := committed.Unix(); secs > 0 && secs <= int64(^uint32(0)) {
		release = uint32(secs)
	}
	c := &Converter{Source: src, OnServer: true, Release: release, Now: committed}
	var run Run
	for i := range apps {
		if err := ctx.Err(); err != nil {
			return Run{}, err
		}
		run.Results = append(run.Results, c.Convert(ctx, apps[i]))
	}
	slices.SortFunc(run.Results, func(a, b Result) int { return strings.Compare(a.ID, b.ID) })
	return run, nil
}

// serverFile checks a file read on the server: within its size, and nesting no
// deeper than maxNesting, before any parser recurses through it.
func serverFile(name string, data []byte) error {
	if len(data) > maxServerFile {
		return leave(ClassFile, "%s is over %d KiB, more than a file Dockwell reads of a store", name, maxServerFile>>10)
	}
	if nesting(data) > maxNesting {
		return leave(ClassFile, "%s nests over %d levels deep, more than any Compose file or template does", name, maxNesting)
	}
	return nil
}

// nesting is how deep a file's brackets and indentation go, read roughly and
// generously: a bracket inside quotes counts too, so it never says less than a
// parser would recurse.
func nesting(data []byte) int {
	deepest, depth := 0, 0
	for _, b := range data {
		switch b {
		case '[', '{':
			depth++
			deepest = max(deepest, depth)
		case ']', '}':
			depth = max(0, depth-1)
		}
	}
	// Block YAML nests by indentation, two columns a level at least.
	for line := range bytes.Lines(data) {
		indent := len(line) - len(bytes.TrimLeft(line, " \t"))
		deepest = max(deepest, indent/2)
	}
	return deepest
}

// serverSummary is a store's line about an app, as an entry's summary: one line, of
// maxSummary characters at most, cut where a word ends. With none, it says which
// store the app is from.
func serverSummary(line, store string) string {
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "An app from " + store
	}
	if utf8.RuneCountInString(line) <= maxSummary {
		return line
	}
	r := []rune(line)[:maxSummary]
	cut := strings.LastIndexFunc(string(r), unicode.IsSpace)
	if cut < maxSummary/2 {
		cut = len(string(r))
	}
	return strings.TrimRight(string(r)[:cut], " ,;:-") + "…"
}

// firstSentence is a description's first sentence, as a template's meta.json writes
// several, the last of them about its own platform.
func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

// privateRegistry says an image names a registry this server, a private network or
// a metadata address answers on, which a store's entry may not: pinning it would
// have the server ask there before anyone has looked at the app.
func privateRegistry(r ref) string {
	if r.host == dockerHub {
		return ""
	}
	host := r.host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return netguard.Forbidden(host, netguard.Options{})
}

// serverWords writes what an overlay would on the server: the store's own words for
// the app, a category from its tags or "other", where it comes from in its notes, and
// what it reaches on its server, as the converter finds it, for the plan to ask a
// person, as for any install. The overlay then says a person looked at exactly that,
// as the plan is where they do.
func (c *Converter) serverWords(ctx context.Context, e *catalogv1.Entry, app App, overlay *Overlay, rw rewritten) error {
	e.Summary = serverSummary(app.Summary, c.Source.Name)
	if e.GetCategory() == "" {
		e.Category = "other"
	}
	if catalog.Link(app.Docs) {
		e.Documentation = app.Docs
	}
	if catalog.Link(app.Code) {
		e.AppSource = app.Code
	}
	e.Notes = append([]string{"From " + c.Source.Name + ", under " + e.GetLicense()}, e.GetNotes()...)
	reaches, err := reachesOf(ctx, e, rw.reaches, rw.typed)
	if err != nil {
		return err
	}
	if len(reaches) > 0 {
		e.HostAccess = hostAccessWords(reaches)
		overlay.Entry = &catalogv1.Entry{HostAccess: e.GetHostAccess()}
		overlay.Reaches = reaches
	}
	return nil
}

// hostAccessWords says what an app reaches on its server, as an entry's host_access.
func hostAccessWords(reaches []string) string {
	return "It reaches " + strings.Join(reaches, "; ") + "."
}

// tagged checks each image of a file the server converts is one an install can pin:
// named by a version, from a registry that isn't this server's or a private
// network's, and written plainly, as the catalog reads an image by its line.
func tagged(file []byte) ([]byte, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, err
	}
	for _, name := range serviceNames(doc) {
		img := mapGet(service(doc, name), "image")
		if img == nil {
			return nil, leave(ClassCantSay, "its service %s names no image, and Dockwell doesn't build one", name)
		}
		image := strings.TrimSpace(img.Value)
		if strings.Contains(image, "$") {
			return nil, leave(ClassCantSay, "its service %s's image comes from a variable, %s, so no digest can pin it", name, image)
		}
		if strings.ContainsAny(image, " \t\r\n#\"'") {
			return nil, leave(ClassCantSay, "its service %s's image %q isn't a reference Docker reads", name, image)
		}
		r, err := parseRef(image)
		if err != nil {
			return nil, leave(ClassNoVersion, "%s", err)
		}
		if r.digest == "" && unversioned[r.tag] {
			return nil, leave(ClassNoVersion, "its service %s's image is %s, which names no version", name, image)
		}
		if why := privateRegistry(r); why != "" {
			return nil, leave(ClassCantSay, "its service %s's image is on %s, which is %s, where a store's app may not pull from", name, r.host, why)
		}
		img.Value, img.Style, img.LineComment = image, 0, ""
	}
	return encodeFile(doc)
}
