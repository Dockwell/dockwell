// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"slices"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// Runtipi's app store converts on the server as the other stores do (ADR 0083): its
// data folder becomes named volumes, its port and address the entry's own, its form
// the entry's inputs, its network and labels go, and it keeps its logo and README.
// An app Runtipi gives a data folder to another user, runs on amd64 alone, retires,
// or that uses a value only Runtipi fills in, is left out, saying why.
func TestOnServerReadsRuntipisStore(t *testing.T) {
	store := storeOf(t, FormatRuntipi)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: store.License}
	run, err := OnServer(t.Context(), store, listingOf(t, store, "testdata/runtipi", nil), src, committed, 0)
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
	for _, want := range []string{
		"image: example/notes:1.2.0\n",
		"- data-notes:/data",
		`"127.0.0.1:${APP_PORT}:3000"`,
		"PUBLIC_URL=http://localhost:${APP_PORT}",
		"TZ=${TZ:-UTC}",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("its Compose file has no %q:\n%s", want, compose)
		}
	}
	for _, gone := range []string{"tipi_main_network", "traefik", "runtipi.managed", "container_name", "APP_DATA_DIR", "APP_DOMAIN"} {
		if strings.Contains(compose, gone) {
			t.Errorf("its Compose file keeps %q:\n%s", gone, compose)
		}
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		inputs[in.GetName()] = in
	}
	if in := inputs["APP_PORT"]; in == nil || in.GetType() != catalogv1.InputType_INPUT_TYPE_PORT || in.GetDefaultValue() != "8213" {
		t.Errorf("its port = %v", in)
	}
	if in := inputs["DB_PASSWORD"]; in == nil || in.GetGenerate() < 32 {
		t.Errorf("its generated password = %v", in)
	}
	if in := inputs["ADMIN_PASSWORD"]; in == nil || in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || in.GetGenerate() != 0 {
		t.Errorf("its typed password = %v", in)
	}
	if e.GetWeb().GetService() != "notes" || e.GetSummary() != "A stand-in app for the converter's tests" || e.GetCategory() != "cms" ||
		e.GetDescription() != "Notes keeps your notes." || !slices.Contains(e.GetNotes(), "From Runtipi's app store, under GPL-3.0-only") {
		t.Errorf("notes' entry = %v", e)
	}
	if notes.Icon != "apps/notes/metadata/logo.jpg" || !strings.Contains(e.GetReadme(), "**Notes** keeps your notes.") || strings.Contains(e.GetReadme(), "<img") {
		t.Errorf("notes' logo %q, README %q", notes.Icon, e.GetReadme())
	}
	// Runtipi's media folder is one a person names, which the plan shows as a path on the server.
	media := got["media"]
	if media.Entry == nil {
		t.Fatalf("media was left out: %s", media.LeftOut)
	}
	if !strings.Contains(media.Entry.GetCompose(), "${MEDIA_DIR}/data:/media") || !strings.Contains(media.Entry.GetHostAccess(), "MEDIA_DIR") {
		t.Errorf("media's folder:\n%s\n%s", media.Entry.GetCompose(), media.Entry.GetHostAccess())
	}
	for id, class := range map[string]Class{"amd64only": ClassArch, "chowned": ClassScripts, "retired": ClassSkipped, "netif": ClassPlatform} {
		if r := got[id]; r.Entry != nil || r.Class != class {
			t.Errorf("%s: class %q (%s), want %q", id, r.Class, r.LeftOut, class)
		}
	}
}

// A store laid out as Runtipi's reads as Runtipi's.
func TestDetectRuntipi(t *testing.T) {
	guesses, counts := Detect([]string{"apps/notes/config.json", "apps/notes/docker-compose.yml", "apps/media/config.json", "README.md"})
	if len(guesses) != 1 || guesses[0] != (Guess{FormatRuntipi, ""}) || counts[guesses[0]] != 2 {
		t.Errorf("Detect = %v, %v", guesses, counts)
	}
}
