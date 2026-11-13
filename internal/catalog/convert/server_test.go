// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// listingOf lists a folder as the control plane lists a commit: every file and
// folder, with the contents of what the store's adapter reads, and more files layered
// on top by path, empty to remove one.
func listingOf(t *testing.T, store Store, dir string, more map[string]string) *Listing {
	t.Helper()
	l := &Listing{Dirs: map[string]bool{}, Files: map[string]bool{}, Read: map[string][]byte{}, Big: map[string]bool{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
		if d.IsDir() {
			l.Dirs[rel] = true
			return nil
		}
		l.Files[rel] = true
		if store.Want(rel) {
			b, err := os.ReadFile(p)
			l.Read[rel] = b
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range more {
		if body == "" {
			delete(l.Files, p)
			delete(l.Read, p)
			continue
		}
		for d := filepath.ToSlash(filepath.Dir(p)); d != "."; d = filepath.ToSlash(filepath.Dir(d)) {
			l.Dirs[d] = true
		}
		l.Files[p] = true
		l.Read[p] = []byte(body)
	}
	return l
}

func storeOf(t *testing.T, format string) Store {
	t.Helper()
	s, ok := StoreOf(format)
	if !ok {
		t.Fatalf("no store %s", format)
	}
	return s
}

func resultsByID(run Run) map[string]Result {
	out := map[string]Result{}
	for i := range run.Results {
		out[run.Results[i].ID] = run.Results[i]
	}
	return out
}

var committed = time.Date(2026, 11, 11, 11, 34, 37, 0, time.UTC)

// On the server, an app converts with no overlay and no registry: its summary is
// its store's line, its notes say where it comes from, its images stay named by tag,
// its release is when the commit was made, and what it reaches is written for the
// plan to ask a person, rather than leaving it out (ADR 0078).
func TestOnServerConvertsWithoutAPerson(t *testing.T) {
	store := storeOf(t, FormatOnePanel)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: "GPL-3.0-only"}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/onepanel", nil), src, committed)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	notes := got["notes"]
	if notes.Entry == nil {
		t.Fatalf("notes was left out: %s", notes.LeftOut)
	}
	e := notes.Entry
	if e.GetSummary() != "A stand-in app for the converter's tests" || e.GetLicense() != "GPL-3.0-only" ||
		e.GetSource().GetRepository() != src.Repository || e.GetRelease() != uint32(committed.Unix()) ||
		!slices.Contains(e.GetNotes(), "From 1Panel's app store, under GPL-3.0-only") || e.GetAppSource() != "https://github.com/example/notes" {
		t.Errorf("notes' entry = %v", e)
	}
	if !strings.Contains(e.GetCompose(), "image: example/notes:1.2.0\n") || strings.Contains(e.GetCompose(), "@sha256:") {
		t.Errorf("an image isn't named by its tag alone:\n%s", e.GetCompose())
	}
	// The offline converter waits for a person's overlay; the server writes what the
	// app reaches, as the plan asks a person to approve it anyway.
	if agent := got["agent"]; agent.Entry == nil && agent.Class == ClassWords {
		t.Errorf("agent waits for its words on the server: %s", agent.LeftOut)
	}
	// A version that moves converts, as an install pins the build it points at (ADR 0085).
	if r := got["onlylatest"]; r.Entry == nil || r.Entry.GetVersion() != "latest" || !strings.Contains(r.Entry.GetCompose(), "image: example/a:latest\n") {
		t.Errorf("onlylatest: %v, left out %q", r.Entry, r.LeftOut)
	}
	// One that runs on amd64 alone is offered on amd64 servers (ADR 0085).
	if r := got["amd64only"]; r.Entry == nil || !slices.Equal(r.Entry.GetArchitectures(), []string{"amd64"}) {
		t.Errorf("amd64only: %v, left out %q", r.Entry.GetArchitectures(), r.LeftOut)
	}
	classes := map[string]Class{"scripted": ClassScripts, "shipped": ClassShipped}
	if !catalogMakes()[catalogv1.Engine_ENGINE_POSTGRESQL] {
		classes["withdb"] = ClassNewEngine
	}
	for id, class := range classes {
		if r := got[id]; r.Entry != nil || r.Class != class {
			t.Errorf("%s: class %q (%s), want %q", id, r.Class, r.LeftOut, class)
		}
	}
}

// What a store holds is a stranger's, so each file read on the server is bounded
// before anything parses it, and what it asks Dockwell to make or reach is bounded
// too. An app that's over a bound is left out with the reason; the rest convert.
func TestOnServerBoundsHostileFiles(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	src := Source{Name: store.Name, Repository: "https://example.com/coolify", Commit: strings.Repeat("b", 40), License: "Apache-2.0"}
	plain := "# slogan: Plain.\n# port: 80\nservices:\n  web:\n    image: nginx:1.27\n    environment:\n      - SERVICE_URL_WEB_80\n"
	deep := "# port: 80\nservices:\n  web:\n    image: nginx:1.27\n    command: " + strings.Repeat("[", 200) + strings.Repeat("]", 200) + "\n"
	big := plain + "# " + strings.Repeat("x", maxServerFile) + "\n"
	private := strings.Replace(plain, "nginx:1.27", "10.0.0.5:5000/web:1.0", 1)
	local := strings.Replace(plain, "nginx:1.27", "localhost:5000/web:1.0", 1)
	quoted := strings.Replace(plain, "image: nginx:1.27", `image: "nginx:1.27 #"`, 1)
	l := listingOf(t, store, t.TempDir(), map[string]string{
		"templates/compose/plain.yaml":   plain,
		"templates/compose/deep.yaml":    deep,
		"templates/compose/big.yaml":     big,
		"templates/compose/private.yaml": private,
		"templates/compose/local.yaml":   local,
		"templates/compose/quoted.yaml":  quoted,
	})
	run, err := OnServer(t.Context(), store, l, src, committed)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	if got["plain"].Entry == nil {
		t.Errorf("plain was left out: %s", got["plain"].LeftOut)
	}
	for id, want := range map[string]string{
		"deep":    "nests over 64 levels deep",
		"big":     "is over 256 KiB",
		"private": "where a store's app may not pull from",
		"local":   "where a store's app may not pull from",
		"quoted":  "isn't a reference Docker reads",
	} {
		if r := got[id]; r.Entry != nil || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: left out %q, want it to say %q", id, r.LeftOut, want)
		}
	}

	dokploy := storeOf(t, FormatDokploy)
	toml := "[variables]\nsecret = \"${password:100000000}\"\n\n[config]\n[[config.domains]]\nserviceName = \"web\"\nport = 80\nhost = \"${domain}\"\n\n[config.env]\nSECRET = \"${secret}\"\n"
	l = listingOf(t, dokploy, t.TempDir(), map[string]string{
		"blueprints/huge/docker-compose.yml":   "services:\n  web:\n    image: nginx:1.27\n    environment:\n      - SECRET=${SECRET}\n",
		"blueprints/huge/template.toml":        toml,
		"blueprints/nested/docker-compose.yml": "services:\n  web:\n    image: nginx:1.27\n",
		"blueprints/nested/template.toml":      "a = " + strings.Repeat("[", 100) + strings.Repeat("]", 100) + "\n",
	})
	run, err = OnServer(t.Context(), dokploy, l, Source{Name: dokploy.Name, Repository: "https://example.com/dokploy", Commit: strings.Repeat("c", 40), License: "MIT"}, committed)
	if err != nil {
		t.Fatal(err)
	}
	got = resultsByID(run)
	if r := got["huge"]; r.Entry != nil || !strings.Contains(r.LeftOut, "more than the 256 Dockwell generates") {
		t.Errorf("a password of 100,000,000 characters: %q", r.LeftOut)
	}
	if r := got["nested"]; r.Entry != nil || !strings.Contains(r.LeftOut, "nests over 64 levels deep") {
		t.Errorf("a template nested 100 deep: %q", r.LeftOut)
	}
}

func TestOnServerRefusesAStoreItCantRead(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	src := Source{Name: store.Name, Repository: "https://example.com/x", Commit: strings.Repeat("d", 40)}
	if _, err := OnServer(t.Context(), store, listingOf(t, store, t.TempDir(), map[string]string{"blueprints/a/template.toml": "x"}), src, committed); err == nil ||
		!strings.Contains(err.Error(), "it has no folder templates/compose, where Coolify's templates keeps its apps") {
		t.Errorf("a folder of another store's: %v", err)
	}
	many := map[string]string{}
	for i := range MaxApps + 1 {
		many["templates/compose/app"+strconv.Itoa(i)+".yaml"] = "x"
	}
	if _, err := OnServer(t.Context(), store, listingOf(t, store, t.TempDir(), many), src, committed); err == nil || !strings.Contains(err.Error(), "it holds over 2000 apps, more than a source reads") {
		t.Errorf("more apps than a source reads: %v", err)
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []Guess
	}{
		{"1Panel's", []string{"apps/memos/data.yml", "apps/memos/logo.png", "apps/halo/data.yml", "README.md", "renovate.yaml"}, []Guess{{FormatOnePanel, ""}}},
		{"Dokploy's", []string{"blueprints/ghost/template.toml", "blueprints/ghost/docker-compose.yml", "meta.yaml"}, []Guess{{FormatDokploy, ""}}},
		{"Coolify's", []string{"templates/compose/ghost.yaml", "templates/compose/n8n.yaml", "docker-compose.yaml", "app/Models/Server.php"}, []Guess{{FormatCoolify, ""}}},
		{"Dockwell's, at the top", []string{"memos.yaml", "notes.yaml", "README.md"}, []Guess{{FormatDockwell, ""}}},
		{"Dockwell's, in entries", []string{"entries/memos.yaml", "entries/media/memos.webp", "README.md"}, []Guess{{FormatDockwell, "entries"}}},
		{"unsure", []string{"apps/memos/data.yml", "blueprints/ghost/template.toml", "blueprints/n8n/template.toml"}, []Guess{{FormatDokploy, ""}, {FormatOnePanel, ""}}},
		{"nothing Dockwell reads", []string{"README.md", "src/main.go"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Detect(tc.files)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Detect = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLicenseOf(t *testing.T) {
	for text, want := range map[string]string{
		"                    GNU GENERAL PUBLIC LICENSE\n                       Version 3, 29 June 2007\n": "GPL-3.0-only",
		"GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007":                                   "AGPL-3.0-only",
		"GNU GENERAL PUBLIC LICENSE\nVersion 3\n\n13. Use with the GNU Affero General Public License.":     "GPL-3.0-only",
		"Apache License\n  Version 2.0, January 2004":                                                      "Apache-2.0",
		"MIT License\n\nCopyright (c) 2024 Someone\n\nPermission is hereby granted, free of charge, to":    "MIT",
		"All rights reserved.": "",
	} {
		if got := LicenseOf([]byte(text)); got != want {
			t.Errorf("LicenseOf(%.30q) = %q, want %q", text, got, want)
		}
	}
}

func TestServerSummary(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := serverSummary(long, "Coolify's templates")
	if len([]rune(got)) > maxSummary+1 || !strings.HasSuffix(got, "word…") {
		t.Errorf("a long line reads %q", got)
	}
	if got := serverSummary("  ", "Coolify's templates"); got != "An app from Coolify's templates" {
		t.Errorf("no line reads %q", got)
	}
	if got := firstSentence("Ghost is a platform. Deploy it on Dokploy in minutes."); got != "Ghost is a platform." {
		t.Errorf("firstSentence = %q", got)
	}
}

// allocated is how many bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// aliases is a list of n aliases to an anchor, in flow style.
func aliases(anchor string, n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat("*"+anchor+", ", n), ", ") + "]"
}

// An anchor of 100 KiB named through three levels of ten aliases is a few thousand
// nodes and 100 MB written out, so expanding aliases stops at the bytes it writes,
// not only at the nodes it makes (the review of #548, its H1).
func TestOnServerStopsAnAliasBombByItsBytes(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	src := Source{Name: store.Name, Repository: "https://example.com/coolify", Commit: strings.Repeat("e", 40), License: "Apache-2.0"}
	bomb := "# port: 80\nx-a: &a \"" + strings.Repeat("A", 100<<10) + "\"\n" +
		"x-b: &b " + aliases("a", 10) + "\nx-c: &c " + aliases("b", 10) + "\n" +
		"services:\n  web:\n    image: nginx:1.27\n    command: " + aliases("c", 10) + "\n"
	l := listingOf(t, store, t.TempDir(), map[string]string{"templates/compose/bomb.yaml": bomb})
	var run Run
	var err error
	used := allocated(func() { run, err = OnServer(t.Context(), store, l, src, committed) })
	if err != nil {
		t.Fatal(err)
	}
	if r := run.Results[0]; r.Entry != nil || !strings.Contains(r.LeftOut, "aliases write out over 256 KiB") {
		t.Errorf("an alias bomb: left out %q", r.LeftOut)
	}
	if used > 32<<20 {
		t.Errorf("converting an alias bomb allocated %d MiB", used>>20)
	}
}

// Dokploy's variables that each name the next ten times stand for gigabytes in a
// template of a few hundred bytes, so what a value comes to and the work of working
// it out are bounded (the review of #548, its H2).
func TestOnServerStopsAFanOutTemplate(t *testing.T) {
	store := storeOf(t, FormatDokploy)
	var vars strings.Builder
	vars.WriteString("[variables]\n")
	for i := range 5 {
		fmt.Fprintf(&vars, "v%d = \"%s\"\n", i, strings.Repeat(fmt.Sprintf("${v%d} ", i+1), 10))
	}
	vars.WriteString("v5 = \"x\"\n\n[config]\n[[config.domains]]\nserviceName = \"web\"\nport = 80\nhost = \"${domain}\"\n\n[config.env]\nFOO = \"${v0}\"\n")
	l := listingOf(t, store, t.TempDir(), map[string]string{
		"blueprints/fan/docker-compose.yml": "services:\n  web:\n    image: nginx:1.27\n    environment:\n      - FOO=${FOO}\n",
		"blueprints/fan/template.toml":      vars.String(),
	})
	src := Source{Name: store.Name, Repository: "https://example.com/dokploy", Commit: strings.Repeat("f", 40), License: "MIT"}
	run, err := OnServer(t.Context(), store, l, src, committed)
	if err != nil {
		t.Fatal(err)
	}
	r := run.Results[0]
	if r.Entry != nil || !strings.Contains(r.LeftOut, "comes to over 4 KiB") && !strings.Contains(r.LeftOut, "steps to work out") {
		t.Errorf("a fan-out template: left out %q", r.LeftOut)
	}
}

// What a commit converts to is bounded, as 2,000 apps of a file each just under what
// a file may be come to gigabytes (the review of #548, its H3).
func TestOnServerBoundsWhatACommitConvertsTo(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	src := Source{Name: store.Name, Repository: "https://example.com/coolify", Commit: strings.Repeat("a", 40), License: "Apache-2.0"}
	more := map[string]string{}
	for i := range 90 {
		more[fmt.Sprintf("templates/compose/app%d.yaml", i)] = "# port: 80\nservices:\n  web:\n    image: nginx:1.27\n    environment:\n      - NOTE=" + strings.Repeat("n", 200<<10) + "\n"
	}
	_, err := OnServer(t.Context(), store, listingOf(t, store, t.TempDir(), more), src, committed)
	if err == nil || !strings.Contains(err.Error(), "comes to over 16 MiB") {
		t.Errorf("a commit that converts to 18 MiB: %v", err)
	}
}

// A copyright line is one line of 200 characters at most in each entry, as a
// licence's first line of a megabyte would ride in every entry (the review of #548,
// its H3).
func TestACopyrightLineIsShort(t *testing.T) {
	src := Source{
		Name: "Dokploy's templates", Repository: "https://example.com/t", License: "MIT", CopyrightInLicense: true,
		LicenseText: []byte("Copyright " + strings.Repeat("x", 1<<20) + "\n\nPermission is hereby granted, free of charge"),
	}
	e := &catalogv1.Entry{Id: "x", Name: "X", Compose: "services: {}\n", Source: &catalogv1.Source{Repository: src.Repository, Path: "blueprints/x"}}
	raw, err := Encode(e, src, "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 2<<10 {
		t.Errorf("an entry with a megabyte's copyright line is %d bytes", len(raw))
	}
}

// What a store writes reaches a kept entry's header as comments alone: a carriage
// return, NEL or a line or paragraph separator, each a line break to YAML, can't end
// a comment and start a field (the review of #548, its M1).
func TestAStoresTextStaysInItsComment(t *testing.T) {
	for _, sep := range []string{"\r", "\n", "\u0085", " ", " ", "\x0b", "\x0c"} {
		r, _ := utf8.DecodeRuneInString(sep)
		t.Run(fmt.Sprintf("%U", r), func(t *testing.T) {
			src := Source{
				Name: "Dokploy's templates", Repository: "https://example.com/t", License: "MIT", CopyrightInLicense: true,
				LicenseText: []byte("Copyright 2024 Someone" + sep + "first_steps: from the copyright line\n"),
			}
			e := &catalogv1.Entry{Id: "x", Name: "X", Compose: "services: {}\n", Source: &catalogv1.Source{Repository: src.Repository, Path: "apps/x" + sep + "notes: [from the path]" + sep + "#"}}
			raw, err := Encode(e, src, "2026-11-12"+sep+"first_steps: from the date")
			if err != nil {
				t.Fatal(err)
			}
			back, err := catalog.Decode(raw)
			if err != nil {
				t.Fatalf("the kept entry doesn't read back: %v\n%s", err, raw)
			}
			if back.GetFirstSteps() != "" || len(back.GetNotes()) != 0 {
				t.Errorf("a store's text became a field: first_steps %q, notes %q\n%s", back.GetFirstSteps(), back.GetNotes(), raw)
			}
		})
	}
}

// Two apps that come to one ID keep the same one each time, the first by where it
// is, and the other says why it's left out; a commit dated in the future counts from
// now; and an entry from a repository laid out as a store says which repository it
// is, as only the store's own is the store (the review of #548, its L7 and L3).
func TestOnServerIsDeterministicAndSaysWhereItsFrom(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	app := "# slogan: An app.\n# port: 80\nservices:\n  web:\n    image: nginx:1.27\n"
	l := listingOf(t, store, t.TempDir(), map[string]string{"templates/compose/my.app.yaml": app, "templates/compose/my-app.yaml": app})
	src := Source{Name: store.SourceName("https://github.com/acme/templates"), Repository: "https://github.com/acme/templates", Commit: strings.Repeat("b", 40), License: "Apache-2.0"}
	future := time.Now().AddDate(10, 0, 0)
	for range 3 {
		run, err := OnServer(t.Context(), store, l, src, future)
		if err != nil {
			t.Fatal(err)
		}
		var kept, out []Result
		for _, r := range run.Results {
			if r.Entry != nil {
				kept = append(kept, r)
			} else {
				out = append(out, r)
			}
		}
		if len(kept) != 1 || kept[0].Path != "templates/compose/my-app.yaml" || len(out) != 1 || !strings.Contains(out[0].LeftOut, "is another app's in this store") {
			t.Fatalf("two apps of one ID: kept %v, left out %v", kept, out)
		}
		if rel := kept[0].Entry.GetRelease(); int64(rel) > time.Now().Unix() {
			t.Errorf("a commit dated %v gives release %d, after now", future, rel)
		}
		if !slices.Contains(kept[0].Entry.GetNotes(), "From github.com/acme/templates, in Coolify's format, under Apache-2.0") {
			t.Errorf("notes = %q", kept[0].Entry.GetNotes())
		}
	}
	if got := store.SourceName("https://github.com/coollabsio/coolify/"); got != "Coolify's templates" {
		t.Errorf("Coolify's own repository reads as %q", got)
	}
}

// A tag that moves, such as latest, converts on the server, as an install pins the
// build it points at when it's planned, and an image named with no tag is written as
// latest, what Docker pulls for it. The offline converter, which pins as it
// converts, still leaves such an app out (ADR 0085).
func TestOnServerTakesATagThatMoves(t *testing.T) {
	store := storeOf(t, FormatCoolify)
	src := Source{Name: store.Name, Repository: "https://example.com/coolify", Commit: strings.Repeat("c", 40), License: "Apache-2.0"}
	plain := "# slogan: Plain.\n# port: 80\nservices:\n  web:\n    image: nginx:1.27\n    environment:\n      - SERVICE_URL_WEB_80\n"
	l := listingOf(t, store, t.TempDir(), map[string]string{
		"templates/compose/latest.yaml":   strings.Replace(plain, "nginx:1.27", "nginx:latest", 1),
		"templates/compose/bare.yaml":     strings.Replace(plain, "nginx:1.27", "ghcr.io/acme/web", 1),
		"templates/compose/port.yaml":     strings.Replace(plain, "nginx:1.27", "registry.example.com:5000/web", 1),
		"templates/compose/stable.yaml":   strings.Replace(plain, "nginx:1.27", "acme/web:stable", 1),
		"templates/compose/variable.yaml": strings.Replace(plain, "nginx:1.27", "acme/web:${TAG}", 1),
		// A tag a variable gives with a default is the default (ADR 0085).
		"templates/compose/defaulted.yaml": strings.Replace(plain, "nginx:1.27", "acme/web:${TAG:-latest}", 1),
	})
	run, err := OnServer(t.Context(), store, l, src, committed)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	for id, want := range map[string]string{
		"latest": "image: nginx:latest\n",
		"bare":   "image: ghcr.io/acme/web:latest\n",
		"port":   "image: registry.example.com:5000/web:latest\n",
		"stable": "image: acme/web:stable\n",
	} {
		r := got[id]
		if r.Entry == nil {
			t.Errorf("%s was left out: %s", id, r.LeftOut)
			continue
		}
		if !strings.Contains(r.Entry.GetCompose(), want) {
			t.Errorf("%s names its image as:\n%s", id, r.Entry.GetCompose())
		}
		if err := catalog.CheckTagged(r.Entry); err != nil {
			t.Errorf("%s fails the catalog's check: %v", id, err)
		}
	}
	if v := got["latest"].Entry.GetVersion(); v != "latest" {
		t.Errorf("latest's version is %q", v)
	}
	if r := got["variable"]; r.Entry != nil || r.Class != ClassCantSay {
		t.Errorf("an image from a variable converted: %s, %q", r.Class, r.LeftOut)
	}
	if r := got["defaulted"]; r.Entry == nil || !strings.Contains(r.Entry.GetCompose(), "image: acme/web:latest\n") {
		t.Errorf("an image with a defaulted tag: %v, %q", r.Entry.GetCompose(), r.LeftOut)
	}
}

func TestUntagged(t *testing.T) {
	for image, want := range map[string]bool{
		"nginx": true, "ghcr.io/acme/web": true, "registry:5000/web": true, "localhost:5000/a/b": true,
		"nginx:1.27": false, "nginx:latest": false, "registry:5000/web:1.0": false, "nginx@sha256:" + strings.Repeat("a", 64): false,
	} {
		if got := untagged(image); got != want {
			t.Errorf("untagged(%q) = %v, want %v", image, got, want)
		}
	}
}

// What each store converted to when the repository last measured it is in the
// binary, for a panel to say before a person adds the store (ADR 0085).
func TestEachStoreIsMeasured(t *testing.T) {
	for _, s := range Stores {
		m, ok := MeasuredOf(s.Format)
		if !ok || m.Apps == 0 || m.Converted == 0 || m.Converted > m.Apps || len(m.Commit) != 40 {
			t.Errorf("%s measured %+v, %v", s.Format, m, ok)
		}
	}
	if _, ok := MeasuredOf(FormatDockwell); ok {
		t.Error("Dockwell's own format reads as a store that's measured")
	}
}

// An app whose reading panics is left out, with the reason, and the rest of the store
// converts (ADR 0085).
func TestOnServerLeavesOutAnAppThatPanics(t *testing.T) {
	c := &Converter{OnServer: true, Release: 1, Now: committed}
	r := c.convertSafely(t.Context(), App{
		ID: "bad", Path: "apps/bad/1.0", Shipped: func(string) bool { panic("a stranger's file") },
		Compose: []byte("services:\n  web:\n    image: nginx:1.27\n    volumes: [\"./conf:/etc/conf\"]\n"),
	})
	if r.Entry != nil || r.Class != ClassFile || !strings.Contains(r.LeftOut, "a stranger's file") {
		t.Errorf("an app that panics = %+v", r)
	}
}

// On the server, the files an app mounts of what its folder ships aren't in the
// rounds an adapter reads, so a round of their own reads exactly those, and the app
// converts with them; with no such round, it stays out, saying they weren't read
// (ADR 0085).
func TestOnServerReadsTheFilesAnAppMounts(t *testing.T) {
	store := storeOf(t, FormatOnePanel)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("d", 40), License: "GPL-3.0-only"}
	l := listingOf(t, store, "testdata/onepanel", nil)
	if _, read := l.Read["apps/shipped/2.0.0/conf/app.conf"]; read {
		t.Fatal("the adapter's own rounds read the shipped file, so this proves nothing")
	}
	run, err := OnServer(t.Context(), store, l, src, committed)
	if err != nil {
		t.Fatal(err)
	}
	if r := resultsByID(run)["shipped"]; r.Entry != nil || r.Class != ClassShipped || !strings.Contains(r.LeftOut, "wasn't read") {
		t.Errorf("with no round for it: %s, %q", r.Class, r.LeftOut)
	}
	var asked []string
	l = listingOf(t, store, "testdata/onepanel", nil)
	l.ReadMore = func(_ context.Context, paths []string) (map[string][]byte, error) {
		asked = append(asked, paths...)
		out := map[string][]byte{}
		for _, p := range paths {
			b, err := os.ReadFile(filepath.Join("testdata", "onepanel", p))
			if err != nil {
				return nil, err
			}
			out[p] = b
		}
		return out, nil
	}
	run, err = OnServer(t.Context(), store, l, src, committed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"apps/seeded/1.0.0/conf/app.conf", "apps/seeded/1.0.0/conf/sites/a.conf", "apps/shipped/2.0.0/conf/app.conf"}) {
		t.Errorf("the round asked for %v", asked)
	}
	if r := resultsByID(run)["seeded"]; r.Entry == nil || !strings.Contains(r.Entry.GetCompose(), "server a.example.com") {
		t.Errorf("a folder the app writes to, with a round for it: %v, %q", r.Entry, r.LeftOut)
	}
	if r := resultsByID(run)["shipped"]; r.Entry == nil || !strings.Contains(r.Entry.GetCompose(), "listen = 80") {
		t.Errorf("with a round for it: %v, %q", r.Entry, r.LeftOut)
	}
}
