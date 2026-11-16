// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"strings"
	"testing"
)

// storeFixtures are each store's test folder, the file of an app's its reader reads
// first, and an app of the folder that reads.
var storeFixtures = []struct {
	format, dir, first, fine string
}{
	{FormatOnePanel, "testdata/onepanel", "apps/hostile/data.yml", "notes"},
	{FormatDokploy, "testdata/dokploy", "blueprints/hostile/template.toml", "wiki"},
	{FormatCoolify, "testdata/coolify", "templates/compose/hostile.yaml", "board"},
	{FormatRuntipi, "testdata/runtipi", "apps/hostile/config.json", "notes"},
	{FormatCasaOS, "testdata/casaos", "Apps/hostile/docker-compose.yml", "notes"},
	{FormatUmbrel, "testdata/umbrel", "hostile/umbrel-app.yml", "planner"},
}

// What a store holds is a stranger's, so whatever stops a reader on one app leaves
// that app out, saying so, and the rest of the store converts: no store's files can
// stop the control plane, which reads a source with nobody watching once it follows
// a branch (#570's review, H1). Each store's reader is made to panic on an app, and
// given an app of NULs, broken UTF-8 and brackets, which it reads as a file it can't
// use, with no panic.
func TestEachStoreLeavesOutAnAppWhoseReadingStops(t *testing.T) {
	hook := func(name string) {
		if name == "boom" || name == "boom.yaml" {
			panic("index out of range [7] with length 1")
		}
	}
	beforeRead.Store(&hook)
	t.Cleanup(func() { beforeRead.Store(nil) })
	hostile := "\x00\x00\xff\xfe" + strings.Repeat("[{", 40) + "\u0000inside7\u0000\n"
	for _, s := range storeFixtures {
		t.Run(s.format, func(t *testing.T) {
			store := storeOf(t, s.format)
			boom := strings.Replace(s.first, "hostile", "boom", 1)
			more := map[string]string{s.first: hostile, boom: hostile}
			src := Source{Name: store.Name, Repository: "https://example.com/" + s.format, Commit: strings.Repeat("d", 40), License: "MIT"}
			var run Run
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("reading %s's apps panicked past its recover: %v", store.Name, p)
					}
				}()
				run, err = OnServer(t.Context(), store, listingOf(t, store, s.dir, more), src, committed, 0)
			}()
			if err != nil {
				t.Fatal(err)
			}
			got := resultsByID(run)
			if r := got["boom"]; r.Entry != nil || r.Class != ClassFile || !strings.Contains(r.LeftOut, stoppedReading) || r.Panic == "" {
				t.Errorf("boom, whose reading panicked: %v, %q, panic %q", r.Class, r.LeftOut, r.Panic)
			}
			if r := got["hostile"]; r.Entry != nil || r.Panic != "" {
				t.Errorf("hostile: converts %v, or panicked %q: %q", r.Entry != nil, r.Panic, r.LeftOut)
			}
			if r, ok := got[s.fine]; !ok || r.Panic != "" {
				t.Errorf("%s, beside them, didn't read: %+v", s.fine, r)
			}
		})
	}
}

// A template's TOML string can write a NUL, as \u0000, and with it a mark of the
// converter's own past the ones it made. The app is left out, before anything's
// worked out, and the rest of the store converts (#570's review, H1). Before, the
// mark was read past the end of the names the converter made, and the panic stopped
// the control plane.
func TestATemplateThatWritesAMarkIsLeftOut(t *testing.T) {
	store := storeOf(t, FormatDokploy)
	more := map[string]string{
		"blueprints/forged/docker-compose.yml": "services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - DATABASE_URL=${DATABASE_URL}\n      - NOTE=${NOTE}\n",
		"blueprints/forged/template.toml":      "[variables]\npw = \"${password:32}\"\n\n[config]\nenv = [\n  \"DATABASE_URL=postgresql://app:${pw}@db:5432/app\",\n  \"NOTE=\\u0000inside7\\u0000\",\n]\n",
		"blueprints/fine/docker-compose.yml":   "services:\n  web:\n    image: example/web:1.0.0\n",
		"blueprints/fine/template.toml":        "[variables]\n",
	}
	src := Source{Name: store.Name, Repository: "https://example.com/templates", Commit: strings.Repeat("a", 40), License: "MIT"}
	var run Run
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("a store's template panics OnServer: %v", p)
			}
		}()
		run, err = OnServer(t.Context(), store, listingOf(t, store, t.TempDir(), more), src, committed, 0)
	}()
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	if r := got["forged"]; r.Entry != nil || r.Class != ClassFile || !strings.Contains(r.LeftOut, "holds a NUL character, which no value has") || r.Panic != "" {
		t.Errorf("forged: %v, %q, panic %q", r.Class, r.LeftOut, r.Panic)
	}
	if r := got["fine"]; r.Panic != "" {
		t.Errorf("fine panicked: %q", r.Panic)
	}
	// Past the template's own check, a mark the converter didn't make leaves the app
	// out, never a read past the names it made.
	tmpl := &dokployTemplate{Variables: map[string]any{"pw": "${password:32}"}}
	tmpl.Config.Env = []any{"DATABASE_URL=postgresql://app:${pw}@db:5432/app", "NOTE=\x00inside7\x00"}
	file := []byte("services:\n  web:\n    image: example/web:1.0.0\n    environment:\n      - DATABASE_URL=${DATABASE_URL}\n      - NOTE=${NOTE}\n")
	if _, _, _, err := dokployCompose(file, tmpl); err == nil || !strings.Contains(err.Error(), "its template's NOTE holds a NUL character") {
		t.Errorf("a mark the converter didn't make: %v", err)
	}
}
