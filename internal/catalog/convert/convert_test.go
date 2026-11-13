// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
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
	for id, want := range map[string]string{
		"scripted": "runs scripts on the server",
		"withdb":   "uses postgresql from another app",
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
	for id, want := range map[string]string{
		"scripted": "runs scripts on the server",
		"withdb":   "uses postgresql from another app",
	} {
		r := convertFixture(t, converter(), id)
		if r.Entry != nil || !strings.Contains(r.LeftOut, want) {
			t.Errorf("%s: %q, want it left out as %q", id, r.LeftOut, want)
		}
	}
	for name, tc := range map[string]struct {
		file, want string
	}{
		"a branch, not a version":      {"services:\n  a:\n    image: example/a:latest\n", "names no version"},
		"an image from a variable":     {"services:\n  a:\n    image: ${IMAGE:-example/a:1.0}\n", "comes from a variable"},
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
