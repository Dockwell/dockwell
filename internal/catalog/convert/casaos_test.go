// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/catalog"
)

// CasaOS's app store converts on the server (ADR 0083): its data folder becomes named
// volumes, a person's media a folder they name, the port it publishes a field, its
// x-casaos the entry's words in English and Chinese, and the logo and screenshots it
// links to through jsDelivr or GitHub are read from the commit itself, never fetched,
// and only from the app's own folder.
func TestOnServerReadsCasaOSsStore(t *testing.T) {
	store := storeOf(t, FormatCasaOS)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: store.License}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/casaos", nil), src, committed)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	notes := got["notes"]
	if notes.Entry == nil {
		t.Fatalf("notes was left out: %s", notes.LeftOut)
	}
	e := notes.Entry
	if err := catalog.CheckTagged(e); err != nil {
		t.Errorf("its entry fails the catalog's check: %v", err)
	}
	compose := e.GetCompose()
	for _, want := range []string{"source: data\n", "${MEDIA_DIR}", "${TZ:-UTC}", "${PUID:-1000}", `published: "${PORT}"`, "host_ip: 127.0.0.1"} {
		if !strings.Contains(compose, want) {
			t.Errorf("its Compose file has no %q:\n%s", want, compose)
		}
	}
	for _, gone := range []string{"x-casaos", "network_mode", "/DATA", "AppID", "name: notes"} {
		if strings.Contains(compose, gone) {
			t.Errorf("its Compose file keeps %q:\n%s", gone, compose)
		}
	}
	if e.GetSummary() != "A stand-in app for the converter's tests" || e.GetReadme() != "Notes keeps your notes." || e.GetCategory() != "media" ||
		e.GetTranslations()["zh-CN"].GetSummary() != "转换器测试用的替身应用" || e.GetWeb().GetService() != "notes" || e.GetVersion() != "1.2.0" {
		t.Errorf("notes' entry = %v", e)
	}
	if notes.Icon != "Apps/Notes/icon.png" {
		t.Errorf("notes' logo is %q", notes.Icon)
	}
	want := []Shot{
		{Path: "Apps/Notes/screenshot-1.png", Source: "https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@main/Apps/Notes/screenshot-1.png"},
		{Path: "Apps/Notes/screenshot-2.png", Source: "https://raw.githubusercontent.com/IceWhaleTech/CasaOS-AppStore/main/Apps/Notes/screenshot-2.png"},
	}
	if !slices.Equal(notes.Screenshots, want) {
		t.Errorf("notes' screenshots = %v, want %v: another app's file and another host's address never count", notes.Screenshots, want)
	}
	for id, class := range map[string]Class{"amdonly": ClassArch, "other": ClassAnotherApp, "whole": ClassCantSay} {
		if r := got[id]; r.Entry != nil || r.Class != class {
			t.Errorf("%s: class %q (%s), want %q", id, r.Class, r.LeftOut, class)
		}
	}
}

// Umbrel's app store converts on the server (ADR 0083), though it's no store a person
// adds in one click, as its repository states no licence: its proxy goes and the port
// it reached is published, the seed and password it makes are generated, a container
// named as Umbrel names it is reached by its service, and an app with hooks or an
// exports.sh, scripts, or that needs another app, is left out.
func TestOnServerReadsUmbrelsStore(t *testing.T) {
	store := storeOf(t, FormatUmbrel)
	if !store.Unlisted {
		t.Error("Umbrel's store is one a person adds in one click")
	}
	src := Source{Name: store.Name, Repository: "https://example.com/umbrel-apps", Commit: strings.Repeat("a", 40), License: "NOASSERTION"}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/umbrel", nil), src, committed)
	if err != nil {
		t.Fatal(err)
	}
	got := resultsByID(run)
	planner := got["planner"]
	if planner.Entry == nil {
		t.Fatalf("planner was left out: %s", planner.LeftOut)
	}
	e := planner.Entry
	if err := catalog.CheckTagged(e); err != nil {
		t.Errorf("its entry fails the catalog's check: %v", err)
	}
	compose := e.GetCompose()
	for _, want := range []string{`"127.0.0.1:${PORT}:1337"`, "postgresql://postgres@db/planner", "BASE_URL: http://localhost:8546", "data-uploads:/uploads", "${DOWNLOADS_DIR}:/downloads"} {
		if !strings.Contains(compose, want) {
			t.Errorf("its Compose file has no %q:\n%s", want, compose)
		}
	}
	for _, gone := range []string{"app_proxy", "APP_DATA_DIR", "UMBREL_ROOT", "planner_db_1", "DEVICE_DOMAIN_NAME"} {
		if strings.Contains(compose, gone) {
			t.Errorf("its Compose file keeps %q:\n%s", gone, compose)
		}
	}
	// A repository that states no licence grants no rights to its files, and the entry says so as it is.
	if !slices.Contains(e.GetNotes(), "From Umbrel's app store, whose repository states no licence") {
		t.Errorf("planner's notes = %q", e.GetNotes())
	}
	generated := map[string]uint32{}
	for _, in := range e.GetInputs() {
		generated[in.GetName()] = in.GetGenerate()
	}
	if generated["APP_SEED"] != 64 || generated["APP_PASSWORD"] != 32 {
		t.Errorf("its generated values = %v", generated)
	}
	for id, class := range map[string]Class{"hooked": ClassScripts, "exporter": ClassScripts, "needs": ClassAnotherApp} {
		if r := got[id]; r.Entry != nil || r.Class != class {
			t.Errorf("%s: class %q (%s), want %q", id, r.Class, r.LeftOut, class)
		}
	}
}

// A store laid out as CasaOS's, or as Umbrel's, reads as that store's.
func TestDetectCasaOSAndUmbrel(t *testing.T) {
	for files, want := range map[string]string{
		"Apps/Notes/docker-compose.yml Apps/Other/docker-compose.yml README.md":  FormatCasaOS,
		"planner/umbrel-app.yml planner/docker-compose.yml notes/umbrel-app.yml": FormatUmbrel,
	} {
		guesses, _ := Detect(strings.Fields(files))
		if len(guesses) != 1 || guesses[0].Format != want {
			t.Errorf("Detect(%s) = %v, want %s", files, guesses, want)
		}
	}
}
