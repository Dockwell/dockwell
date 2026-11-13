// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// fakeRegistry pins each image to a digest of its name, built 60 days ago unless
// built says otherwise.
type fakeRegistry struct {
	built     map[string]time.Time
	fail      map[string]error
	platforms map[string][]string
	asked     []string
}

func digestOf(image string) string {
	sum := sha256.Sum256([]byte(image))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *fakeRegistry) Pin(_ context.Context, image string) (string, error) {
	if err := f.fail[image]; err != nil {
		return "", err
	}
	return digestOf(image), nil
}

func (f *fakeRegistry) Platforms(_ context.Context, image, _ string) ([]string, error) {
	if p, ok := f.platforms[image]; ok {
		return p, nil
	}
	return []string{"linux/amd64", "linux/arm64"}, nil
}

func (f *fakeRegistry) Built(_ context.Context, image, _ string) (time.Time, error) {
	f.asked = append(f.asked, image)
	if t, ok := f.built[image]; ok {
		return t, nil
	}
	return now.Add(-60 * 24 * time.Hour), nil
}

func written(summary, category string) Overlay {
	return Overlay{Entry: &catalogv1.Entry{Summary: summary, Category: category}}
}

func converter() *Converter {
	return &Converter{
		Source:   OnePanelSource("0123456789abcdef0123456789abcdef01234567"),
		Registry: &fakeRegistry{},
		Overlays: map[string]Overlay{
			"notes": written("Keeps notes", "notes"), "my-app": written("Stands in", "developer tools"),
			"scripted": written("Stands in", "ai"), "shipped": written("Stands in", "ai"), "withdb": written("Stands in", "ai"),
			"agent": written("Reports to a monitor", "monitoring"), "keep": written("Stands in", "ai"), "amd64only": written("Stands in", "ai"),
			"owned": written("Stands in", "ai"), "seeded": written("Stands in", "ai"), "blog": written("Stands in", "ai"),
		},
		Now: now,
	}
}

func readFixture(t *testing.T) map[string]App {
	t.Helper()
	apps, err := OnePanel("testdata/onepanel")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]App{}
	for i := range apps {
		out[apps[i].ID] = apps[i]
	}
	return out
}

func convertFixture(t *testing.T, c *Converter, id string) Result {
	t.Helper()
	app, ok := readFixture(t)[id]
	if !ok {
		t.Fatalf("the fixture has no %s", id)
	}
	return c.Convert(t.Context(), app)
}

// 1Panel's store keeps each app's versions side by side; the converter reads the
// newest, with the one before to fall back to.
func TestOnePanelReadsEachAppAtItsNewestVersion(t *testing.T) {
	apps := readFixture(t)
	notes := apps["notes"]
	if notes.Version != "1.2.0" || notes.Path != "apps/notes/1.2.0" || notes.Older == nil || notes.Older.Version != "1.1.0" {
		t.Errorf("notes reads as %s at %s, with %v before it", notes.Version, notes.Path, notes.Older)
	}
	if notes.Name != "Notes" || notes.Homepage != "https://notes.example.com" || notes.Category != "cms" {
		t.Errorf("notes reads as %q, %q, %q", notes.Name, notes.Homepage, notes.Category)
	}
	if _, ok := apps["my-app"]; !ok {
		t.Error("my_app's ID doesn't take a dash, as an entry's must")
	}
	// A database another app runs is a need (ADR 0084).
	if w := apps["withdb"]; w.LeftOut != "" || len(w.Needs) != 1 || w.Needs[0].GetAccepts()[0].GetEngine() != catalogv1.Engine_ENGINE_POSTGRESQL {
		t.Errorf("withdb reads as %v, left out %q", w.Needs, w.LeftOut)
	}
	for id, want := range map[string]string{
		"scripted": "its init.sh runs sed on the server",
	} {
		if got := apps[id].LeftOut; !strings.Contains(got, want) {
			t.Errorf("%s is left out as %q, want %q", id, got, want)
		}
	}
}

func TestOnePanelSaysAWrongFolder(t *testing.T) {
	if _, err := OnePanel(t.TempDir()); err == nil || !strings.Contains(err.Error(), "github.com/1Panel-dev/appstore") {
		t.Errorf("OnePanel(empty) = %v", err)
	}
}

// What every converted entry says: no name for its container, no network of 1Panel's,
// every port on loopback, its folders as named volumes, its images pinned, and its
// generated password a secret Dockwell makes and seals.
func TestAConvertedEntryKeepsTheRulesEveryEntryKeeps(t *testing.T) {
	r := convertFixture(t, converter(), "notes")
	if r.Entry == nil {
		t.Fatalf("notes was left out: %s", r.LeftOut)
	}
	file := r.Entry.GetCompose()
	for _, gone := range []string{"container_name", "1panel-network", "external", "createdBy", "CONTAINER_NAME", "PANEL_", "/etc/localtime", "./data"} {
		if strings.Contains(file, gone) {
			t.Errorf("its Compose file still says %s:\n%s", gone, file)
		}
	}
	for _, want := range []string{
		`"127.0.0.1:${PORT}:8080"`,
		`"127.0.0.1:${SYNC_PORT}:9443"`,
		"- data:/app/data",
		"- uploads:/app/uploads",
		"PEER=worker:7000",
		"MAIN=notes",
		"image: example/notes:1.2.0@" + digestOf("example/notes:1.2.0"),
		"image: example/notes-worker:1.2.0@" + digestOf("example/notes-worker:1.2.0"),
	} {
		if !strings.Contains(file, want) {
			t.Errorf("its Compose file doesn't say %s:\n%s", want, file)
		}
	}
	if !slices.Contains(r.Notes, "the server's /etc/localtime, which only set the container's time zone") {
		t.Errorf("its notes don't say what it dropped: %q", r.Notes)
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range r.Entry.GetInputs() {
		inputs[in.GetName()] = in
	}
	if _, ok := inputs["NOT_USED"]; ok || len(inputs) != 5 {
		t.Errorf("its inputs are %v, want the five its file uses", slices.Sorted(func(yield func(string) bool) {
			for k := range inputs {
				if !yield(k) {
					return
				}
			}
		}))
	}
	if p := inputs["PORT"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_PORT || p.GetLabel() != "Port on this server" || p.GetDefaultValue() != "8080" {
		t.Errorf("PORT is %v", p)
	}
	if p := inputs["SYNC_PORT"]; p.GetLabel() != "Sync port on this server" || strings.Contains(p.GetHint(), "domain") {
		t.Errorf("SYNC_PORT is %v", p)
	}
	if s := inputs["ADMIN_PASSWORD"]; s.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || s.GetGenerate() != 32 || s.GetDefaultValue() != "" {
		t.Errorf("ADMIN_PASSWORD is %v, want a generated secret with no default everyone would share", s)
	}
	if l := inputs["LOG_LEVEL"]; !l.GetOptional() || l.GetHint() != "One of info, debug." || l.GetLabel() != "Log level" {
		t.Errorf("LOG_LEVEL is %v", l)
	}
	// How it's reached, on this server alone until a domain, is the panel's to show, with Open.
	if r.Entry.GetWeb().GetService() != "notes" || r.Entry.GetFirstSteps() != "" || len(r.Entry.GetNotes()) != 0 {
		t.Errorf("it opens %q, with the first steps %q and the notes %q", r.Entry.GetWeb().GetService(), r.Entry.GetFirstSteps(), r.Entry.GetNotes())
	}
	e := r.Entry
	if e.GetLicense() != "GPL-3.0-only" || e.GetSource().GetRepository() != "https://github.com/1Panel-dev/appstore" ||
		e.GetSource().GetPath() != "apps/notes/1.2.0" || e.GetRelease() != 1 || e.GetSummary() != "Keeps notes" || e.GetCategory() != "notes" {
		t.Errorf("it says %v", e)
	}
}

// What the converter writes is an entry the catalog takes, and reads as an install would.
func TestWhatItWritesTheCatalogReads(t *testing.T) {
	c := converter()
	var results []Result
	for _, app := range readFixture(t) {
		results = append(results, c.Convert(t.Context(), app))
	}
	dir := t.TempDir()
	if err := Write(dir, c.Source, results, nil, c.Pins, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "notes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# SPDX-License-Identifier: GPL-3.0-only\n", "# Converted from 1Panel's app store", "apps/notes/1.2.0", "modified by Dockwell's converter on 2026-11-07."} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("notes.yaml doesn't say %q:\n%s", want, raw)
		}
	}
	e, err := catalog.Parse(raw)
	if err != nil {
		t.Fatalf("the catalog refuses notes.yaml: %v\n%s", err, raw)
	}
	if !proto.Equal(e, results[slices.IndexFunc(results, func(r Result) bool { return r.ID == "notes" })].Entry) {
		t.Error("notes.yaml doesn't read back as the entry it was written from")
	}
	t.Setenv(catalog.DirEnv, dir)
	if _, err := catalog.Load(""); err != nil {
		t.Errorf("the catalog doesn't load what the converter wrote: %v", err)
	}
	report, err := os.ReadFile(filepath.Join(dir, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| Runs scripts on the server | 1 |", "| scripted | `` apps/scripted/1.0.0 `` |", "| notes | 1.2.0 | 1 |", "0123456789ab"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("the report doesn't say %q:\n%s", want, report)
		}
	}
}

// An app is left out, with the reason, for what an entry can't say or do.
func TestAnAppIsLeftOutWithItsReason(t *testing.T) {
	waits := map[string]string{"scripted": "its init.sh runs sed on the server"}
	if !catalogMakes()[catalogv1.Engine_ENGINE_POSTGRESQL] {
		waits["withdb"] = "it needs PostgreSQL, which Dockwell doesn't make beside an app yet"
	}
	for id, want := range waits {
		r := convertFixture(t, converter(), id)
		if r.Entry != nil || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: %q, want it left out as %q", id, r.LeftOut, want)
		}
	}
	for name, tc := range map[string]struct {
		file, want string
	}{
		"a branch, not a version":      {"services:\n  a:\n    image: example/a:latest\n", "names no version"},
		"a variable no field fills":    {"services:\n  a:\n    image: ${IMAGE}\n", "uses IMAGE, which its platform fills in"},
		"a value 1Panel fills in":      {"services:\n  a:\n    image: example/a:1.0\n    environment:\n      - DB=${PANEL_DB_PORT}\n", "PANEL_DB_PORT, which its platform fills in"},
		"a build":                      {"services:\n  a:\n    build: .\n", "has build"},
		"a folder in a home":           {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ~/data:/data\n", "home directory"},
		"a folder outside its own":     {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ../data:/data\n", "outside the app's own folder"},
		"Docker's socket":              {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n", "control of Docker, through /var/run/docker.sock"},
		"the server's own network":     {"services:\n  a:\n    image: example/a:1.0\n    network_mode: host\n", "the server's own network"},
		"a summary no one has written": {"services:\n  a:\n    image: example/a:1.0\n", "it needs a summary in our own words"},
	} {
		t.Run(name, func(t *testing.T) {
			c := converter()
			if name != "a summary no one has written" {
				c.Overlays["plain"] = written("Stands in", "ai")
			}
			r := c.Convert(t.Context(), App{ID: "plain", Name: "Plain", Version: "1.0", Compose: []byte(tc.file)})
			if r.Entry != nil || !strings.Contains(r.LeftOut, tc.want) {
				t.Errorf("%q, want it left out as %q", r.LeftOut, tc.want)
			}
		})
	}
}

// Host access converts once a person has looked, and says so in the overlay.
func TestHostAccessConvertsOnceAPersonHasLooked(t *testing.T) {
	c := converter()
	c.Overlays["dock"] = Overlay{
		Entry:   &catalogv1.Entry{Summary: "Runs containers", Category: "developer tools", HostAccess: "Docker's socket, to run containers"},
		Reaches: []string{"dock: control of Docker, through /var/run/docker.sock"},
	}
	file := "services:\n  dock:\n    image: example/dock:1.0\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"
	r := c.Convert(t.Context(), App{ID: "dock", Name: "Dock", Version: "1.0", Compose: []byte(file)})
	if r.Entry == nil || r.Entry.GetHostAccess() != "Docker's socket, to run containers" {
		t.Errorf("an app a person has looked at was left out: %s", r.LeftOut)
	}
}

// A version under 14 days old waits, and the one before converts in its place while
// it does. A rebuild of a version keeps the age it had.
func TestAYoungVersionWaits(t *testing.T) {
	c := converter()
	c.Registry = &fakeRegistry{built: map[string]time.Time{"example/notes:1.2.0": now.Add(-3 * 24 * time.Hour)}}
	r := convertFixture(t, c, "notes")
	if r.Entry == nil || r.Entry.GetVersion() != "1.1.0" || !slices.Contains(r.Notes, "took 1.1.0, as 1.2.0 is under 14 days old") {
		t.Errorf("notes converted as %v, %q, %q, want 1.1.0 in 1.2.0's place", r.Entry.GetVersion(), r.LeftOut, r.Notes)
	}

	c = converter()
	c.Registry = &fakeRegistry{built: map[string]time.Time{"example/a:2.0": now.Add(-3 * 24 * time.Hour)}}
	c.Overlays["a"] = written("Stands in", "ai")
	r = c.Convert(t.Context(), App{ID: "a", Name: "A", Version: "2.0", Compose: []byte("services:\n  a:\n    image: example/a:2.0\n")})
	if r.Entry != nil || !r.Again || !r.Young || r.LeftOut != "example/a:2.0 is 3 days old, and converts once it's 14 days old" {
		t.Errorf("a young version with none before it: %+v", r)
	}
}

// A registry that wasn't asked leaves an app for a later run, as one that's down does.
func TestARegistryNotAskedLeavesItForLater(t *testing.T) {
	c := converter()
	c.Registry = &fakeRegistry{fail: map[string]error{"example/a:1.0": ErrNotAsked}}
	c.Overlays["a"] = written("Stands in", "ai")
	r := c.Convert(t.Context(), App{ID: "a", Name: "A", Version: "1.0", Compose: []byte("services:\n  a:\n    image: example/a:1.0\n")})
	if r.Entry != nil || !r.Again || r.Young || !strings.Contains(r.LeftOut, "not asked") || r.Class != ClassPin {
		t.Errorf("%+v", r)
	}
}

// Dockwell's own entry wins, by its ID or by the image a person opens, until the
// reviewed list says two are one app.
func TestDockwellsOwnEntryWins(t *testing.T) {
	own := &catalogv1.Entry{Id: "redis", Web: &catalogv1.Web{Service: "web"}, Compose: "services:\n  web:\n    image: example/notes:1.0@sha256:" + strings.Repeat("a", 64) + "\n"}
	c := converter()
	c.Own = []*catalogv1.Entry{own}
	if r := convertFixture(t, c, "notes"); r.Entry != nil || !strings.Contains(r.LeftOut, "it runs docker.io/example/notes, as Dockwell's own entry does") {
		t.Errorf("an app that runs the image Dockwell's own entry does: %q", r.LeftOut)
	}
	c.Overlays["redis"] = written("Stands in", "databases")
	if r := c.Convert(t.Context(), App{ID: "redis", Name: "Redis", Version: "1", Compose: []byte("services:\n  r:\n    image: example/r:1\n")}); r.Entry != nil || !strings.HasPrefix(r.LeftOut, "Dockwell has its own entry of this ID, so this one stays out") || r.Class != ClassOwn {
		t.Errorf("an app of the ID of Dockwell's own entry: %q", r.LeftOut)
	}
}

// What a person wrote wins over what the converter made, field by field.
func TestAnOverlayWins(t *testing.T) {
	c := converter()
	c.Overlays["notes"] = Overlay{Entry: &catalogv1.Entry{
		Name: "Notes Server", Summary: "Keeps notes", Category: "notes", AppLicense: "MIT", FirstSteps: "Open it.",
		Description: "A place for your notes.", Icon: "/catalog-media/notes.webp",
		Documentation: "https://example.com/docs", AppSource: "https://example.com/source",
		Features:    []string{"Search your notes"},
		Screenshots: []*catalogv1.Screenshot{{Path: "/catalog-media/notes-screen.webp", Caption: "Your notes", Source: "https://example.com/notes.png"}},
		Inputs: []*catalogv1.Input{
			{Name: "ADMIN_USER", Label: "Your name", Hint: "You sign in with it."},
			{Name: "ADMIN_PASSWORD", SetOnce: true},
		},
		SignIn:           &catalogv1.SignIn{UserInput: "ADMIN_USER", PasswordInput: "ADMIN_PASSWORD"},
		FirstVisitorOwns: true,
		Signups:          &catalogv1.Signups{Service: "notes", Variable: "SIGNUPS_ALLOWED", Closed: "false"},
		AddressVariables: []*catalogv1.AddressVariable{{Service: "notes", Variable: "BASE_URL", Value: "https://{domain}"}},
		Notes:            []string{"Keeps each note as a file on its volume"},
	}}
	r := convertFixture(t, c, "notes")
	e := r.Entry
	if e == nil {
		t.Fatal(r.LeftOut)
	}
	i := slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == "ADMIN_USER" })
	if e.GetName() != "Notes Server" || e.GetAppLicense() != "MIT" || e.GetFirstSteps() != "Open it." ||
		e.GetInputs()[i].GetLabel() != "Your name" || e.GetInputs()[i].GetHint() != "You sign in with it." || e.GetInputs()[i].GetDefaultValue() != "owner" {
		t.Errorf("the overlay didn't win: %v", e)
	}
	password := e.GetInputs()[slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == "ADMIN_PASSWORD" })]
	if !password.GetSetOnce() || password.GetGenerate() != 32 || !e.GetFirstVisitorOwns() ||
		e.GetSignIn().GetPasswordInput() != "ADMIN_PASSWORD" || e.GetSignups().GetVariable() != "SIGNUPS_ALLOWED" ||
		e.GetAddressVariables()[0].GetValue() != "https://{domain}" || !slices.Equal(e.GetNotes(), []string{"Keeps each note as a file on its volume"}) {
		t.Errorf("the overlay's sign-in, sign-ups, address and notes didn't carry: %v", e)
	}
	written := c.Overlays["notes"].Entry
	if e.GetDescription() != written.GetDescription() || e.GetIcon() != written.GetIcon() || e.GetDocumentation() != written.GetDocumentation() || e.GetAppSource() != written.GetAppSource() || !slices.Equal(e.GetFeatures(), written.GetFeatures()) || !proto.Equal(e.GetScreenshots()[0], written.GetScreenshots()[0]) {
		t.Errorf("the overlay lost its presentation: %v", e)
	}
	e.Features[0] = "Changed"
	e.Screenshots[0].Caption = "Changed"
	if written.GetFeatures()[0] == "Changed" || written.GetScreenshots()[0].GetCaption() == "Changed" {
		t.Error("the converted entry shares presentation with its overlay")
	}
}

// An entry's release goes up only when it changes, so a run over a newer commit that
// didn't touch an app offers nobody an upgrade.
func TestAReleaseGoesUpOnlyWhenTheEntryChanges(t *testing.T) {
	first := convertFixture(t, converter(), "notes")
	c := converter()
	c.Source.Commit = "fedcba9876543210fedcba9876543210fedcba98"
	c.Previous = map[string]*catalogv1.Entry{"notes": first.Entry}
	c.PreviousDates = map[string]string{"notes": "2026-10-01"}
	again := convertFixture(t, c, "notes")
	if again.Entry.GetRelease() != 1 || again.Date != "2026-10-01" || again.Entry.GetSource().GetCommit() != first.Entry.GetSource().GetCommit() {
		t.Errorf("an unchanged app is release %d of %s at %s", again.Entry.GetRelease(), again.Date, again.Entry.GetSource().GetCommit())
	}
	c.Overlays["notes"] = written("Keeps your notes", "notes")
	changed := convertFixture(t, c, "notes")
	if changed.Entry.GetRelease() != 2 || changed.Date != "2026-11-07" {
		t.Errorf("a changed app is release %d of %s", changed.Entry.GetRelease(), changed.Date)
	}
}

// An entry an earlier run wrote stays while a later run may convert it again, and
// goes when it's left out for good.
func TestAnEarlierRunsEntryStaysOrGoes(t *testing.T) {
	previous := map[string]*catalogv1.Entry{"young": {}, "scripted": {}, "gone-upstream": {}, "fine": {}}
	results := []Result{{ID: "young", Again: true}, {ID: "scripted"}, {ID: "fine", Entry: &catalogv1.Entry{}}}
	stay, gone := Kept(results, previous, true)
	if !slices.Equal(stay, []string{"young"}) || !slices.Equal(gone, []string{"scripted", "gone-upstream"}) {
		t.Errorf("stay %v, gone %v", stay, gone)
	}
	// A run over some apps alone removes only what it read.
	if _, gone := Kept(results, previous, false); !slices.Equal(gone, []string{"scripted"}) {
		t.Errorf("a run over three apps removed %v", gone)
	}
}

func TestReadPreviousReadsWhatWriteWrote(t *testing.T) {
	c := converter()
	r := convertFixture(t, c, "notes")
	dir := t.TempDir()
	if err := Write(dir, c.Source, []Result{r}, nil, c.Pins, true); err != nil {
		t.Fatal(err)
	}
	entries, dates, pins, err := ReadPrevious(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(entries["notes"], r.Entry) || dates["notes"] != "2026-11-07" || pins["example/notes:1.2.0"].Digest != digestOf("example/notes:1.2.0") {
		t.Errorf("read back %v, %v, %v", entries["notes"] != nil, dates, pins)
	}
	if err := Write(dir, c.Source, []Result{{ID: "notes", LeftOut: "it runs scripts"}}, entries, pins, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an entry left out for good is still there: %v", err)
	}
}

// A service's name takes dashes, as a container's does, and what names it follows.
func TestAServiceTakesDashes(t *testing.T) {
	c := converter()
	c.Overlays["runner"] = written("Stands in", "developer tools")
	file := "services:\n  act_runner:\n    image: example/act_runner:1.0\n    depends_on:\n      - act_db\n    environment:\n      - DB=act_db:5432\n      - KEEP=act_db_backup\n  act_db:\n    image: example/db:1.0\n"
	r := c.Convert(t.Context(), App{ID: "runner", Name: "Runner", Version: "1.0", Compose: []byte(file)})
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	for _, want := range []string{"act-runner:", "act-db:", "- act-db\n", "DB=act-db:5432", "KEEP=act_db_backup", "image: example/act_runner:1.0@"} {
		if !strings.Contains(r.Entry.GetCompose(), want) {
			t.Errorf("its file doesn't say %q:\n%s", want, r.Entry.GetCompose())
		}
	}
}

// A field's name is in capitals, as an input's is.
func TestAFieldTakesCapitals(t *testing.T) {
	c := converter()
	c.Overlays["broker"] = written("Stands in", "developer tools")
	file := "services:\n  broker:\n    image: \"example/broker:1.0\"\n    environment:\n      - PASSWORD=${Dashboard_Password}\n"
	r := c.Convert(t.Context(), App{
		ID: "broker", Name: "Broker", Version: "1.0", Compose: []byte(file),
		Fields: []Field{{Name: "Dashboard_Password", Label: "Dashboard Password", Kind: Generated}},
	})
	if r.Entry == nil || r.Entry.GetInputs()[0].GetName() != "DASHBOARD_PASSWORD" || !strings.Contains(r.Entry.GetCompose(), "PASSWORD=${DASHBOARD_PASSWORD}") {
		t.Errorf("%q, %v", r.LeftOut, r.Entry.GetInputs())
	}
	if !strings.Contains(r.Entry.GetCompose(), "    image: example/broker:1.0@sha256:") {
		t.Errorf("a quoted image isn't pinned on a plain line, as the catalog reads one:\n%s", r.Entry.GetCompose())
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{"Web Port": "Web port", "MinIO Root User": "MinIO root user", "API Key": "API key", "Port": "Port"} {
		if got := sentenceCase(in); got != want {
			t.Errorf("sentenceCase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"./data": "data", "./data/db": "data-db", "./My Files/": "my-files", "./.hidden": "hidden"} {
		if got := volumeName(strings.TrimSuffix(filepath.Clean(in), "/")); got != want {
			t.Errorf("volumeName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][]string{
		"${PORT}:80":                      {"${PORT}", "80"},
		"${HOST_IP}:${PORT:-8080}:80/udp": {"${HOST_IP}", "${PORT:-8080}", "80/udp"},
		"9000":                            {"9000"},
	} {
		if got := splitPort(in); !slices.Equal(got, want) {
			t.Errorf("splitPort(%q) = %q, want %q", in, got, want)
		}
	}
	versions := []string{"1.10.0", "1.9.0", "1.9.0-alpine", "v2.0", "20250803"}
	slices.SortFunc(versions, compareVersions)
	if want := []string{"1.9.0-alpine", "1.9.0", "1.10.0", "v2.0", "20250803"}; !slices.Equal(versions, want) {
		t.Errorf("versions sort as %q, want %q", versions, want)
	}
	if got := renameVars("a=${PANEL_X} b=${PANEL_X:-${PANEL_Y}} c=$${PANEL_X}", map[string]string{"PANEL_X": "X", "PANEL_Y": "Y"}); got != "a=${X} b=${X:-${Y}} c=$${PANEL_X}" {
		t.Errorf("renameVars = %q", got)
	}
}

// A file 1Panel ships beside an app's Compose file, which the app mounts, is a file
// Dockwell writes, read-only, as 1Panel ships it (ADR 0085).
// 1Panel's scripts that make a folder the app binds and give it to the user its
// image runs as, run init.sh again on an upgrade, or remove the app's volumes, are
// steps Dockwell takes itself: the volume is made for that user (ADR 0085). Nothing in
// them runs.
func TestOnePanelsScriptsAreStepsDockwellTakes(t *testing.T) {
	r := convertFixture(t, converter(), "owned")
	if r.Entry == nil {
		t.Fatalf("owned was left out: %s", r.LeftOut)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(r.Entry.GetCompose()), Variables: map[string]string{"PORT": "8080"}})
	if err != nil {
		t.Fatalf("its file doesn't read: %v\n%s", err, r.Entry.GetCompose())
	}
	owners := map[string]string{}
	for _, m := range stack.Services[0].Spec.GetApp().GetMounts() {
		owners[m.GetTarget()] = m.GetMakeOwner()
	}
	if owners["/var/lib/owned"] != "1000:1000" || owners["/var/log/owned"] != "" {
		t.Errorf("owners = %v, want /var/lib/owned made for 1000:1000 alone\n%s", owners, r.Entry.GetCompose())
	}
}

// A folder an app writes to that its store ships files into is a volume of its own,
// which starts with them, and a script's chmod of a folder it binds is that volume's
// mode (ADR 0085).
func TestAShippedFolderIsWhatAVolumeStartsWith(t *testing.T) {
	r := convertFixture(t, converter(), "seeded")
	if r.Entry == nil {
		t.Fatalf("seeded was left out: %s", r.LeftOut)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(r.Entry.GetCompose()), Variables: map[string]string{"PORT": "8080"}})
	if err != nil {
		t.Fatalf("its file doesn't read: %v\n%s", err, r.Entry.GetCompose())
	}
	mounts := map[string]*resourcev1.Mount{}
	for _, m := range stack.Services[0].Spec.GetApp().GetMounts() {
		mounts[m.GetTarget()] = m
	}
	conf := mounts["/etc/seeded"]
	var seeds []string
	for _, s := range conf.GetSeed() {
		seeds = append(seeds, s.GetPath()+"="+s.GetContent())
	}
	if conf.GetVolume() == "" || !slices.Equal(seeds, []string{"app.conf=listen 80\nroot /srv\n", "sites/a.conf=server a.example.com\n"}) {
		t.Errorf("/etc/seeded is %v, starting with %q\n%s", conf, seeds, r.Entry.GetCompose())
	}
	if logs := mounts["/var/log/seeded"]; logs.GetVolume() == "" || logs.GetMakeMode() != 0o777 {
		t.Errorf("/var/log/seeded is %v", logs)
	}
	if len(mounts) != 2 {
		t.Errorf("it mounts %d, where a volume's files are its own, not file mounts", len(mounts))
	}
}

// What a script does that isn't one of those steps leaves its app out, naming the
// command, as Dockwell runs nothing a store holds.
func TestWhatAScriptDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		scripts map[string]string
		owners  map[string]string
		why     string
	}{
		"a folder given away": {scripts: map[string]string{"init.sh": "#!/bin/bash\n\nchown -R 1000:1000 data\n"}, owners: map[string]string{"data": "1000:1000"}},
		"folders made and given away": {
			scripts: map[string]string{"init.sh": "mkdir config\nchown -R 1000:1000 config\nmkdir database\nchown -R 1001:1001 ./database\n"},
			owners:  map[string]string{"config": "1000:1000", "database": "1001:1001"},
		},
		"by a variable, made if it isn't there": {
			scripts: map[string]string{"init.sh": "DIR=\"./data/data\"\n\nif [ -d \"$DIR\" ]; then\n  echo \"there\"\nelse\n  mkdir -p \"$DIR\"\n  if [ $? -eq 0 ]; then\n    echo \"made\"\n  fi\nfi\n\nchown -R 65534:65534 $DIR\n"},
			owners:  map[string]string{"data/data": "65534:65534"},
		},
		"quietly": {scripts: map[string]string{"init.sh": "chown -R 1000:1000 data >/dev/null 2>&1 || true\n"}, owners: map[string]string{"data": "1000:1000"}},
		"a variable an upgrade adds": {
			scripts: map[string]string{"upgrade.sh": "if [[ -f ./.env ]]; then\n  if grep -q 'PANEL_APP_PORT_S3' ./.env; then\n    echo \"there\"\n  else\n    echo 'PANEL_APP_PORT_S3=5426' >> ./.env\n  fi\nelse\n  echo \"no .env\"\nfi\n"},
			owners:  map[string]string{},
		},
		"a variable added, quoted":  {scripts: map[string]string{"upgrade.sh": "echo 'SKIP_BOOTSTRAP=\"yes\"' >> ./.env\necho \"WHITE_LIST=\" >> ./.env\n"}, owners: map[string]string{}},
		"an echo naming a variable": {scripts: map[string]string{"init.sh": "DIR=\"./data/\"\necho \"made $DIR\"\necho \"made ${DIR}\"\nchown -R 1001:1001 $DIR\n"}, owners: map[string]string{"data": "1001:1001"}},
		"an echo running a command": {scripts: map[string]string{"init.sh": "echo \"$(id -u)\"\n"}, why: "its init.sh runs echo"},
		"a value made up for .env":  {scripts: map[string]string{"init.sh": "echo \"KEY=$(head -c 9 /dev/urandom)\" >> ./.env\n"}, why: "its init.sh runs echo"},
		"a command":                 {scripts: map[string]string{"init.sh": "source ./.env\n"}, why: "its init.sh runs source on the server"},
		"a secret made up":          {scripts: map[string]string{"init.sh": "NEW=$(cat /dev/urandom | head -c 32)\n"}, why: "its init.sh runs a command on the server"},
		"a folder elsewhere":        {scripts: map[string]string{"init.sh": "chown -R 1000:1000 /etc\n"}, why: "outside the app's own folder"},
		"a folder above":            {scripts: map[string]string{"init.sh": "chown -R 1000:1000 ../other\n"}, why: "outside the app's own folder"},
		"a variable it never set":   {scripts: map[string]string{"init.sh": "chown -R 1000:1000 $DATA\n"}, why: "by a variable it doesn't set, DATA"},
		"a mode with setuid":        {scripts: map[string]string{"init.sh": "chmod 4755 data\n"}, why: "setuid, setgid or sticky"},
		"an echo into a file":       {scripts: map[string]string{"init.sh": "echo \"IMAGE=x\" >> .env\n"}, why: "its init.sh runs echo"},
	} {
		steps, err := readScripts(tc.scripts)
		switch {
		case tc.why != "" && (err == nil || !strings.Contains(err.Error(), tc.why) || classOf(err, ClassFile) != ClassScripts):
			t.Errorf("%s: %v, want it left out as %q", name, err, tc.why)
		case tc.why == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.why == "" && !maps.Equal(steps.owners, tc.owners):
			t.Errorf("%s: owners %v, want %v", name, steps.owners, tc.owners)
		}
	}
	steps := scriptSteps{owners: map[string]string{"data": "1000:1000"}}
	if steps.ownerOf("data/conf") != "1000:1000" || steps.ownerOf("data") != "1000:1000" || steps.ownerOf("logs") != "" {
		t.Errorf("chown -R gives what's inside too")
	}
	if err := ownedInside(scriptSteps{owners: map[string]string{"data/data": "65534:65534"}}, map[string]string{"data": "data"}); err == nil || !strings.Contains(err.Error(), "a folder inside the volume for data") {
		t.Errorf("a folder inside a volume = %v", err)
	}
	if err := ownedInside(scriptSteps{owners: map[string]string{"data": "1000:1000"}}, map[string]string{"data-conf": "data/conf"}); err != nil {
		t.Errorf("a folder above a volume = %v", err)
	}
}

func TestAFileOnePanelShipsIsAFileDockwellWrites(t *testing.T) {
	r := convertFixture(t, converter(), "shipped")
	if r.Entry == nil {
		t.Fatalf("shipped was left out: %s", r.LeftOut)
	}
	content, err := os.ReadFile("testdata/onepanel/apps/shipped/2.0.0/conf/app.conf")
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(r.Entry.GetCompose()), Variables: map[string]string{"PORT": "8080"}})
	if err != nil {
		t.Fatalf("its file doesn't read: %v\n%s", err, r.Entry.GetCompose())
	}
	m := stack.Services[0].Spec.GetApp().GetMounts()
	if len(m) != 1 || m[0].GetTarget() != "/etc/app.conf" || m[0].GetFile().GetContent() != string(content) || !m[0].GetReadOnly() {
		t.Errorf("it mounts %v, where 1Panel ships %q", m, content)
	}
}

// A 1Panel app that uses another app's MySQL or MariaDB needs one, which an install
// makes beside it: what 1Panel fills in is what the need sets, its host and port
// together its address, and none of it is an input (ADR 0084).
func TestAOnePanelAppNeedsTheDatabaseItUsed(t *testing.T) {
	r := convertFixture(t, converter(), "blog")
	if r.Entry == nil {
		t.Fatalf("it's left out: %s", r.LeftOut)
	}
	n := r.Entry.GetNeeds()
	if len(n) != 1 || len(n[0].GetAccepts()) != 2 || n[0].GetService() != "" || n[0].GetDatabase() != "blog" || n[0].GetUser() != "blog" ||
		!slices.Equal(setsOf(n[0]), []string{"PANEL_DB_ADDRESS=ADDRESS", "PANEL_DB_NAME=DATABASE", "PANEL_DB_USER=USER", "PANEL_DB_USER_PASSWORD=PASSWORD"}) {
		t.Fatalf("its needs are %v, setting %q", n, setsOf(n[0]))
	}
	if !strings.Contains(r.Entry.GetCompose(), "BLOG_DB_HOST: ${PANEL_DB_ADDRESS}") {
		t.Errorf("its file reads:\n%s", r.Entry.GetCompose())
	}
	for _, in := range r.Entry.GetInputs() {
		if strings.HasPrefix(in.GetName(), "PANEL_DB_") {
			t.Errorf("it asks for %s", in.GetName())
		}
	}
}
