// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	for id, class := range map[string]Class{"scripted": ClassScripts, "amd64only": ClassArch, "onlylatest": ClassNoVersion, "withdb": ClassAnotherApp, "shipped": ClassShipped} {
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
