// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder a person names is that folder or one in it, never a way out: a store's
// path that climbs out with .. leaves the app out, and a folder whose name only
// starts as the person's does, such as /DATA/Media-x, is no folder of theirs (the
// review of #589, M1).
func TestAPersonsFolderIsNoWayOut(t *testing.T) {
	casaos := func(bind string) (string, error) {
		file := "name: board\nservices:\n  board:\n    image: example/board:1.0\n    volumes:\n      - " + bind + "\n"
		doc, err := parseFile([]byte(file))
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := casaosCompose(doc, "board", "board", "")
		return string(out), err
	}
	for _, bind := range []string{
		"/DATA/Media/../../var/run:/x",
		"/DATA/Downloads/../../etc:/x",
		"/DATA/AppData/board/../../etc:/x",
		"/DATA/Media-../../../etc:/x",
		"/DATA/Media-x:/x",
		"/DATA/Downloadsx:/x",
	} {
		if out, err := casaos(bind); err == nil {
			t.Errorf("CasaOS's %s converts:\n%s", bind, out)
		}
	}
	if out, err := casaos("/DATA/Media/films:/films"); err != nil || !strings.Contains(out, "${MEDIA_DIR}/films:/films") {
		t.Errorf("CasaOS's media folder: %v\n%s", err, out)
	}
	if out, err := casaos("/DATA/Downloads:/downloads"); err != nil || !strings.Contains(out, "${DOWNLOADS_DIR}:/downloads") {
		t.Errorf("CasaOS's downloads folder: %v\n%s", err, out)
	}

	runtipi := func(bind string) (string, error) {
		out, err := runtipiCompose([]byte("services:\n  notes:\n    image: example/notes:1.0\n    volumes:\n      - " + bind + "\n"))
		return string(out), err
	}
	for _, bind := range []string{"${ROOT_FOLDER_HOST}/media/../../var/run:/x", "${APP_DATA_DIR}/data/../../etc:/x", "${APP_DATA_DIR}/../x:/x"} {
		if out, err := runtipi(bind); err == nil {
			t.Errorf("Runtipi's %s converts:\n%s", bind, out)
		}
	}
	if out, _ := runtipi("${ROOT_FOLDER_HOST}/media-x:/x"); strings.Contains(out, "${MEDIA_DIR}") {
		t.Errorf("Runtipi's media-x reads as the media folder:\n%s", out)
	}
	if out, err := runtipi("${ROOT_FOLDER_HOST}/media/films:/films"); err != nil || !strings.Contains(out, "${MEDIA_DIR}/films:/films") {
		t.Errorf("Runtipi's media folder: %v\n%s", err, out)
	}
	if out, err := runtipi("${ROOT_FOLDER_HOST}/media:/media"); err != nil || !strings.Contains(out, "${MEDIA_DIR}:/media") {
		t.Errorf("Runtipi's media folder, whole: %v\n%s", err, out)
	}

	umbrel := func(bind string) (string, error) {
		out, _, err := umbrelCompose([]byte("services:\n  web:\n    image: example/web:1.0\n    volumes:\n      - "+bind+"\n"), "notes")
		return string(out), err
	}
	for _, bind := range []string{"${UMBREL_ROOT}/data/storage/downloads/../../x:/x", "${APP_DATA_DIR}/../x:/x", "${APP_DATA_DIR}/data/../../etc:/x"} {
		if out, err := umbrel(bind); err == nil {
			t.Errorf("Umbrel's %s converts:\n%s", bind, out)
		}
	}
	if out, _ := umbrel("${UMBREL_ROOT}/data/storage/downloads-x:/x"); strings.Contains(out, "${DOWNLOADS_DIR}") {
		t.Errorf("Umbrel's downloads-x reads as the downloads folder:\n%s", out)
	}
	if out, err := umbrel("${UMBREL_ROOT}/data/storage/downloads/films:/films"); err != nil || !strings.Contains(out, "${DOWNLOADS_DIR}/films:/films") {
		t.Errorf("Umbrel's downloads folder: %v\n%s", err, out)
	}
	if out, err := umbrel("${UMBREL_ROOT}/data/storage/downloads:/downloads"); err != nil || !strings.Contains(out, "${DOWNLOADS_DIR}:/downloads") {
		t.Errorf("Umbrel's downloads folder, whole: %v\n%s", err, out)
	}
}

// Whatever a store writes, a path a person names that climbs out of it is refused in
// the rewrite every store's file goes through (the review of #589, M1).
func TestAPathAPersonNamesClimbsNowhere(t *testing.T) {
	r := convertPlain(t, converter(), "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ${DATA_DIR:-/srv/data}/../../etc:/x\n")
	if r.Entry != nil || r.Class != ClassCantSay || !strings.Contains(r.LeftOut, "climbs out of the folder a person names") {
		t.Errorf("%s: %q", r.Class, r.LeftOut)
	}
}

// Umbrel's container names become service names only where they stand whole, in one
// pass over each value (the review of #589, L5).
func TestUmbrelsContainerNamesStandWhole(t *testing.T) {
	file := "services:\n  web:\n    image: example/web:1.0\n    environment:\n      DB: notes_db_1:5432\n      OTHER: notes_db_10\n      MINE: mynotes_db_1\n      URL: http://notes_web_1/x,notes_db_1\n  db:\n    image: example/db:1.0\n"
	out, _, err := umbrelCompose([]byte(file), "notes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DB: db:5432", "OTHER: notes_db_10", "MINE: mynotes_db_1", "URL: http://web/x,db"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// A repository with no licence file states none, and one whose licence file Dockwell
// doesn't recognise has one it can't name: an entry's note says which (the review
// of #589, L1).
func TestANoteSaysWhichLicenceStateItIs(t *testing.T) {
	for license, want := range map[string]string{
		"NONE":        "From a store, whose repository states no licence",
		"":            "From a store, whose repository states no licence",
		"NOASSERTION": "From a store, under a licence Dockwell doesn't recognise",
		"MIT":         "From a store, under MIT",
	} {
		if got := fromNote("a store", license); got != want {
			t.Errorf("%q: %q, want %q", license, got, want)
		}
	}
	if s := storeByID(t, "umbrel"); s.License != "NONE" {
		t.Errorf("Umbrel's store says %q", s.License)
	}
}

// storeByID is one of the stores Dockwell reads.
func storeByID(t *testing.T, id string) Store {
	t.Helper()
	for i := range Stores {
		if Stores[i].ID == id {
			return Stores[i]
		}
	}
	t.Fatalf("no store %s", id)
	return Store{}
}

// fuzzSeeds are the fixtures' Compose files under testdata, for a fuzz test's corpus.
func fuzzSeeds(f *testing.F, pattern string) {
	f.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", pattern))
	if err != nil {
		f.Fatal(err)
	}
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(b)
		}
	}
}

// noWayOut fails where a converted file names a person's folder, or the app's own,
// and climbs out of it.
func noWayOut(t *testing.T, out []byte) {
	t.Helper()
	for _, folder := range []string{"${MEDIA_DIR}/", "${DOWNLOADS_DIR}/", "./data/"} {
		for rest := string(out); ; {
			i := strings.Index(rest, folder)
			if i < 0 {
				break
			}
			rest = rest[i+len(folder):]
			if word, _, _ := strings.Cut(strings.TrimLeft(rest, " \t\n"), " "); climbs(word) {
				t.Fatalf("it names %s and climbs out:\n%s", folder, out)
			}
		}
	}
}

func FuzzRuntipiCompose(f *testing.F) {
	fuzzSeeds(f, "runtipi/apps/*/docker-compose.yml")
	f.Add([]byte("services:\n  a:\n    image: x\n    volumes:\n      - ${ROOT_FOLDER_HOST}/media/../x:/x\n"))
	f.Fuzz(func(t *testing.T, file []byte) {
		if out, err := runtipiCompose(file); err == nil {
			noWayOut(t, out)
		}
	})
}

func FuzzCasaOSCompose(f *testing.F) {
	fuzzSeeds(f, "casaos/Apps/*/docker-compose.yml")
	f.Add([]byte("name: a\nservices:\n  a:\n    image: x\n    volumes:\n      - /DATA/Media/../x:/x\n"))
	f.Fuzz(func(t *testing.T, file []byte) {
		doc, err := parseFile(file)
		if err != nil {
			return
		}
		if out, _, err := casaosCompose(doc, "a", "a", "80"); err == nil {
			noWayOut(t, out)
		}
	})
}

func FuzzUmbrelCompose(f *testing.F) {
	fuzzSeeds(f, "umbrel/*/docker-compose.yml")
	f.Add([]byte("services:\n  web:\n    image: x\n    volumes:\n      - ${APP_DATA_DIR}/../x:/x\n"))
	f.Fuzz(func(t *testing.T, file []byte) {
		if out, _, err := umbrelCompose(file, "a"); err == nil {
			noWayOut(t, out)
		}
	})
}
