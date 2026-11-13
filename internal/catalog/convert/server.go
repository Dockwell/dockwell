// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"cmp"
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

	"google.golang.org/protobuf/proto"

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
	// changes, so a commit converted by an older one is converted again. 2 reads a
	// store's logos and words (ADR 0083), 3 takes a tag that moves, and 4 a container's
	// own settings and a secret inside a command's word (ADR 0085).
	Version = 4
	// MaxConverted is the most a commit may convert to, as a source of Dockwell's own
	// entries reads 16 MiB of a commit at most.
	MaxConverted = 16 << 20
)

// The formats the control plane reads, beside Dockwell's own.
const (
	FormatOnePanel = "1panel"
	FormatDokploy  = "dokploy"
	FormatCoolify  = "coolify"
	FormatRuntipi  = "runtipi"
	FormatCasaOS   = "casaos"
	FormatUmbrel   = "umbrel"
	FormatDockwell = "dockwell"
)

// A Store is a format of catalog the control plane converts: how its folder is laid
// out, what of it an adapter reads, and the adapter.
type Store struct {
	Format string
	// ID is what a source of it is named, unless that's taken, such as "onepanel",
	// and Label how a person says a source of it, short, such as "1Panel".
	ID, Label string
	// Name is how its entries say where they come from, such as "1Panel's app store".
	Name string
	// Homepage is the store's own project site, and Branch the branch a source of it
	// follows, its repository's default.
	Homepage, Branch string
	// Unlisted keeps it out of the stores a person adds in one click, though a link to
	// it reads as it: Umbrel's repository states no licence, and a repository with no
	// licence grants no rights to use its files, so Dockwell doesn't promote it
	// (ADR 0083).
	Unlisted bool
	// License is what its upstream publishes under.
	License string
	// Root is the folder it keeps its apps in, such as apps.
	Root string
	// Upstream is the store's own repository, and Layout how another repository laid
	// out as it is says so, such as "1Panel's format", as any repository can copy a
	// store's layout, and an entry says where it comes from as it is.
	Upstream, Layout string
	// Descend says whether to list a folder's contents, by its path in the store's
	// folder and how deep it is, from 1.
	Descend func(p string, depth int) bool
	// Want says whether to read a file's contents, by its path.
	Want func(p string) bool
	read func(ctx context.Context, t *tree, root string) ([]App, error)
}

// Stores are the formats the control plane converts.
var Stores = []Store{
	{
		Format: FormatOnePanel, ID: "onepanel", Label: "1Panel", Name: "1Panel's app store", License: "GPL-3.0-only", Root: "apps",
		Upstream: "https://github.com/1Panel-dev/appstore", Layout: "1Panel's format",
		Homepage: "https://1panel.pro", Branch: "dev",
		// apps/<key>/<version>/..., where a version's own folders are read for what
		// they hold, as a script or a shipped file leaves its app out.
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "apps/") && depth <= 6 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			switch {
			case len(parts) == 3 && parts[0] == "apps" && slices.Contains([]string{"data.yml", "README.md", "README_en.md"}, parts[2]):
				return true
			case len(parts) == 4 && parts[0] == "apps" && (parts[3] == "data.yml" || parts[3] == "docker-compose.yml"):
				return true
			}
			return false
		},
		read: onePanelApps,
	},
	{
		Format: FormatDokploy, ID: "dokploy", Label: "Dokploy", Name: "Dokploy's templates", License: "MIT", Root: "blueprints",
		Upstream: "https://github.com/Dokploy/templates", Layout: "Dokploy's format",
		Homepage: "https://dokploy.com", Branch: "canary",
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "blueprints/") && depth <= 6 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			return len(parts) == 3 && parts[0] == "blueprints" && slices.Contains([]string{"docker-compose.yml", "template.toml", "meta.json"}, parts[2])
		},
		read: dokployApps,
	},
	{
		Format: FormatCoolify, ID: "coolify", Label: "Coolify", Name: "Coolify's templates", License: "Apache-2.0", Root: "templates/compose",
		Upstream: "https://github.com/coollabsio/coolify", Layout: "Coolify's format",
		Homepage: "https://coolify.io", Branch: "main",
		// Its templates, and the folder of logos they name, as # logo: svgs/<name>.
		Descend: func(p string, depth int) bool {
			return p == "templates" || p == "templates/compose" || p == "public" || p == "public/svgs"
		},
		Want: func(p string) bool {
			dir, file := path.Split(p)
			return dir == "templates/compose/" && strings.HasSuffix(file, ".yaml")
		},
		read: coolifyApps,
	},
	{
		Format: FormatRuntipi, ID: "runtipi", Label: "Runtipi", Name: "Runtipi's app store", License: "GPL-3.0-only", Root: "apps",
		Upstream: "https://github.com/runtipi/runtipi-appstore", Layout: "Runtipi's format",
		Homepage: "https://runtipi.io", Branch: "master",
		// apps/<id>/ with its config.json, its Compose file, and metadata/ with its
		// logo and description.
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "apps/") && depth <= 3 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			switch {
			case len(parts) == 3 && parts[0] == "apps" && (parts[2] == "config.json" || parts[2] == "docker-compose.yml"):
				return true
			case len(parts) == 4 && parts[0] == "apps" && parts[2] == "metadata" && parts[3] == "description.md":
				return true
			}
			return false
		},
		read: runtipiApps,
	},
	{
		Format: FormatCasaOS, ID: "casaos", Label: "CasaOS", Name: "CasaOS's app store", License: "Apache-2.0", Root: "Apps",
		Upstream: "https://github.com/IceWhaleTech/CasaOS-AppStore", Layout: "CasaOS's format",
		Homepage: "https://casaos.zimaspace.com", Branch: "main",
		// Apps/<name>/ with its docker-compose.yml, whose x-casaos says the rest, and the
		// logo and screenshots beside it that it links to.
		Descend: func(p string, depth int) bool { return strings.HasPrefix(p+"/", "Apps/") && depth <= 2 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			return len(parts) == 3 && parts[0] == "Apps" && parts[2] == "docker-compose.yml"
		},
		read: casaosApps,
	},
	{
		Format: FormatUmbrel, ID: "umbrel", Label: "Umbrel", Name: "Umbrel's app store", License: "NONE", Root: ".",
		Upstream: "https://github.com/getumbrel/umbrel-apps", Layout: "Umbrel's format",
		Homepage: "https://umbrel.com", Branch: "master", Unlisted: true,
		// <id>/ at its top, with umbrel-app.yml and its Compose file, and whether it has
		// hooks or exports.sh, which leave it out.
		Descend: func(_ string, depth int) bool { return depth <= 2 },
		Want: func(p string) bool {
			parts := strings.Split(p, "/")
			return len(parts) == 2 && (parts[1] == "umbrel-app.yml" || parts[1] == "docker-compose.yml")
		},
		read: umbrelApps,
	},
}

// SourceName is how an entry says where it comes from: the store's name for its own
// repository, and for any other, the repository and the store's format, such as
// "github.com/acme/appstore, in 1Panel's format".
func (s Store) SourceName(repository string) string {
	if s.At(repository) {
		return s.Name
	}
	clean := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(repository)), "/")
	clean = strings.TrimSuffix(strings.TrimSuffix(clean, ".git"), "/")
	where := strings.TrimPrefix(strings.TrimPrefix(clean, "https://"), "http://")
	return CommentText(where) + ", in " + s.Layout
}

// At says a repository is the store's own, as a source names it: the same URL, in
// any case, with or without a trailing slash or .git.
func (s Store) At(repository string) bool {
	clean := func(u string) string {
		u = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(u)), "/")
		return strings.TrimSuffix(strings.TrimSuffix(u, ".git"), "/")
	}
	return clean(repository) == clean(s.Upstream)
}

// StoreOf is the store of a format, if the control plane converts it.
func StoreOf(format string) (Store, bool) {
	for i := range Stores {
		if Stores[i].Format == format {
			return Stores[i], true
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
		return slices.Contains([]string{"apps", "Apps", "blueprints", "templates", "entries"}, top)
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
		case len(parts) == 3 && parts[0] == "apps" && parts[2] == "config.json":
			counts[Guess{FormatRuntipi, ""}]++
		case len(parts) == 3 && parts[0] == "Apps" && parts[2] == "docker-compose.yml":
			counts[Guess{FormatCasaOS, ""}]++
		case len(parts) == 2 && parts[1] == "umbrel-app.yml":
			counts[Guess{FormatUmbrel, ""}]++
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
	apps, err := store.read(ctx, t, src.Repository)
	if errors.Is(err, ErrNoApps) {
		return Run{}, fmt.Errorf("it holds no apps where %s keeps them", store.Name)
	}
	if err != nil {
		return Run{}, err
	}
	if len(apps) > MaxApps {
		return Run{}, fmt.Errorf("it holds %d apps, more than the %d a source reads", len(apps), MaxApps)
	}
	// A commit dated after now, as a fork can date one, would outrank every later
	// commit's releases, so it counts from now.
	if now := time.Now(); committed.After(now) {
		committed = now
	}
	release := uint32(1)
	if secs := committed.Unix(); secs > 0 && secs <= int64(^uint32(0)) {
		release = uint32(secs)
	}
	c := &Converter{Source: src, OnServer: true, Release: release, Now: committed}
	var run Run
	size := 0
	for i := range apps {
		if err := ctx.Err(); err != nil {
			return Run{}, err
		}
		r := c.convertSafely(ctx, apps[i])
		if r.Entry != nil {
			r.Icon, r.Screenshots = apps[i].Icon, apps[i].Screenshots
			if size += proto.Size(r.Entry); size > MaxConverted {
				return Run{}, fmt.Errorf("what it converts to comes to over %d MiB, more than a source keeps of one commit", MaxConverted>>20)
			}
		}
		run.Results = append(run.Results, r)
	}
	// By ID, then by where each is, so two apps that come to one ID, such as my_app and
	// my-app, always keep the same one, and the other says why it's left out.
	slices.SortStableFunc(run.Results, func(a, b Result) int { return cmp.Or(strings.Compare(a.ID, b.ID), strings.Compare(a.Path, b.Path)) })
	for i := 1; i < len(run.Results); i++ {
		r, before := &run.Results[i], &run.Results[i-1]
		if r.ID != before.ID || r.Entry == nil {
			continue
		}
		if kept := slices.IndexFunc(run.Results, func(o Result) bool { return o.ID == r.ID && o.Entry != nil }); kept != i {
			r.Entry, r.LeftOut, r.Class = nil, fmt.Sprintf("its ID, %s, is another app's in this store, %s, which comes first", r.ID, run.Results[kept].Path), ClassCantSay
		}
	}
	return run, nil
}

// convertSafely converts one app, and leaves it out, saying so, if anything reading
// it panics: what's read is a stranger's, read with nobody watching once a source
// follows a branch, and one app's files must never stop the rest (ADR 0085).
func (c *Converter) convertSafely(ctx context.Context, app App) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			r = Result{ID: app.ID, Path: app.Path, LeftOut: fmt.Sprintf("reading its files stopped on what Dockwell didn't expect (%v)", p), Class: ClassFile}
		}
	}()
	return c.Convert(ctx, app)
}

// PrivateRegistry says an image names a registry that's this server, a private
// network or a metadata address, by its name, which a store's app may not pull from,
// or "" when it doesn't. Where a name leads is checked again as the agent connects.
func PrivateRegistry(image string) string {
	r, err := parseRef(image)
	if err != nil {
		return ""
	}
	return privateRegistry(r)
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
	storeWords(e, app)
	if e.GetCategory() == "" {
		e.Category = "other"
	}
	if catalog.Link(app.Docs) {
		e.Documentation = app.Docs
	}
	if catalog.Link(app.Code) {
		e.AppSource = app.Code
	}
	e.Notes = append([]string{fromNote(c.Source.Name, e.GetLicense())}, e.GetNotes()...)
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

// fromNote says where an entry comes from, and under which licence, or that its
// repository states none, or one Dockwell doesn't recognise, as an honest state
// rather than an identifier.
func fromNote(source, license string) string {
	switch license {
	case "", "NONE":
		return "From " + source + ", whose repository states no licence"
	case "NOASSERTION":
		return "From " + source + ", under a licence Dockwell doesn't recognise"
	}
	return "From " + source + ", under " + license
}

// hostAccessWords says what an app reaches on its server, as an entry's host_access.
func hostAccessWords(reaches []string) string {
	return "It reaches " + strings.Join(reaches, "; ") + "."
}

// tagged checks each image of a file the server converts is one an install can pin:
// named by a tag, from a registry that isn't this server's or a private network's,
// and written plainly, as the catalog reads an image by its line. A tag that moves,
// such as latest, is pinned when an install is planned, as a version's tag is, and
// an image named with no tag is written as latest, which is what Docker pulls for it,
// so the plan names the tag it pins (ADR 0085).
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
		if untagged(image) {
			image += ":latest"
		}
		r, err := parseRef(image)
		if err != nil {
			return nil, leave(ClassNoVersion, "%s", err)
		}
		if why := privateRegistry(r); why != "" {
			return nil, leave(ClassCantSay, "its service %s's image is on %s, which is %s, where a store's app may not pull from", name, r.host, why)
		}
		img.Value, img.Style, img.LineComment = image, 0, ""
	}
	return encodeFile(doc)
}

// untagged says an image names neither a tag nor a digest, as nginx and
// ghcr.io/acme/app do, where Docker pulls latest. A registry's port, as in
// registry:5000/app, is no tag.
func untagged(image string) bool {
	if strings.Contains(image, "@") {
		return false
	}
	return strings.LastIndex(image, ":") <= strings.LastIndex(image, "/")
}
