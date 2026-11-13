// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
)

// A Coolify template that converts: nginx, which the test registry has, with a
// password Coolify makes up and a port it routes.
const boardTemplate = `# documentation: https://board.example.com
# slogan: A board for notes. It runs anywhere Coolify does.
# category: productivity
# port: 80

services:
  board:
    image: nginx:1.27
    environment:
      - SERVICE_URL_BOARD_80
      - ADMIN_PASSWORD=${SERVICE_PASSWORD_ADMIN}
    volumes:
      - board-data:/usr/share/nginx/html
`

// A repository laid out as another store's catalog is read as that store's, with
// nobody naming its format: the link alone is the form. Its plan says how many apps
// it brings, how many it leaves out and why, and the licence it carries; its entries
// keep their store's licence and source; and installing one pins its images by
// digest, through the agent of the server it goes on (ADR 0078).
func TestAStoreIsReadAsItsOwnFormat(t *testing.T) {
	repo := gitfetchtest.New(t)
	repo.Filter()
	head := repo.Commit(map[string]string{
		"LICENSE":                         "                                 Apache License\n                           Version 2.0, January 2004\n",
		"templates/compose/board.yaml":    boardTemplate,
		"templates/compose/moving.yaml":   strings.Replace(boardTemplate, "nginx:1.27", "nginx:latest", 1),
		"templates/compose/ignored.yaml":  "# ignore: true\n" + boardTemplate,
		"templates/compose/scripted.yaml": strings.Replace(boardTemplate, "services:", "services:\n  x:\n    image: example/x:1.0\n    build: .", 1),
		"docker-compose.yaml":             "services: {}\n",
		"app/Models/Server.php":           "<?php\n",
	})
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: testRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)

	refs, err := cat.ListCatalogSourceRefs(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourceRefsRequest{Repository: repo.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if g := refs.Msg.GetFormats(); len(g) != 1 || g[0].GetFormat() != resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY || g[0].GetPath() != "" || g[0].GetApps() != 4 {
		t.Errorf("the repository looks like %v, want Coolify's templates alone, with 4 apps", g)
	}

	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: head}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	planned := res.Msg.GetChange()
	if reads := notesOf(planned, "catalog_source_reads"); len(reads) != 1 || reads[0] != "It reads 1 app from "+repo.URL+", at "+head[:12]+", as Coolify's templates, under Apache-2.0." {
		t.Errorf("what it reads: %q", reads)
	}
	if left := notesOf(planned, "catalog_source_leaves_out"); len(left) != 1 ||
		!strings.HasPrefix(left[0], "It leaves out 3 of 4 apps: ") || !strings.Contains(left[0], "1 name no version that stays put") ||
		!strings.Contains(left[0], "1 are marked by their store to leave out") {
		t.Errorf("what it leaves out: %q", left)
	}
	if len(notesOf(planned, "catalog_source_refuses")) != 0 {
		t.Error("a store's plan lists each app it leaves out as a note of its own")
	}
	apply(t, c, planned)

	sources, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	// Beside Dockwell's own catalog (ADR 0077).
	s := slices.DeleteFunc(sources.Msg.GetSources(), func(s *dockwellv1.CatalogSourceInfo) bool { return s.GetOwn() })
	if len(s) != 1 || s[0].GetSource().GetFormat() != resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY || !slices.Equal(s[0].GetEntries(), []string{"board"}) || len(s[0].GetRefused()) != 3 {
		t.Fatalf("ListCatalogSources = %v", s)
	}
	kinds := map[string]string{}
	for _, r := range s[0].GetRefused() {
		kinds[r.GetId()] = r.GetKind()
	}
	if kinds["moving"] != "no-version" || kinds["ignored"] != "skipped" || kinds["scripted"] == "" {
		t.Errorf("what it left out, by kind: %v", kinds)
	}

	entries, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries.Msg.GetEntries(), func(e *catalogv1.Entry) bool { return e.GetId() == "board" })
	if i < 0 {
		t.Fatal("the catalog has no board once the source is applied")
	}
	board := entries.Msg.GetEntries()[i]
	if board.GetLicense() != "Apache-2.0" || board.GetSource().GetRepository() != repo.URL || board.GetSummary() != "A board for notes." ||
		!slices.Contains(board.GetNotes(), "From "+strings.TrimSuffix(strings.TrimPrefix(repo.URL, "https://"), "/.git")+", in Coolify's format, under Apache-2.0") || board.GetRelease() < 1_700_000_000 {
		t.Errorf("the board's entry = %v", board)
	}
	if !strings.Contains(board.GetCompose(), "image: nginx:1.27\n") {
		t.Errorf("the entry names its image by tag until an install pins it:\n%s", board.GetCompose())
	}

	installing, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "board"}))
	if err != nil {
		t.Fatalf("installing a store's app: %v", err)
	}
	const digest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if pins := notesOf(installing.Msg.GetChange(), "catalog_pins_images"); len(pins) != 1 || !strings.Contains(pins[0], "nginx:1.27 at sha256:777777777777") {
		t.Errorf("the plan doesn't say the digest it installs: %q", pins)
	}
	apply(t, c, installing.Msg.GetChange())
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil || len(installs.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs = %v, %v", installs.Msg, err)
	}
	in := installs.Msg.GetInstalls()[0]
	if !strings.Contains(string(in.GetPin().GetFile()), "image: nginx:1.27@"+digest) || in.GetPin().GetCatalog() != "coolify" {
		t.Errorf("the install's pin = %v", in.GetPin())
	}
	if in.GetUpgrade() != nil {
		t.Errorf("the digest an install pinned reads as an upgrade: %v", in.GetUpgrade())
	}

	// A format a person names that the folder isn't says where that store keeps
	// its apps.
	_, err = cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "dokploy", Repository: repo.URL, Commit: head, Format: resourcev1.CatalogFormat_CATALOG_FORMAT_DOKPLOY}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "it has no folder blueprints, where Dokploy's templates keeps its apps") {
		t.Errorf("the wrong format: %v", err)
	}
}

// The same app in two catalogs is one card: listed from the first, with the second
// as another choice, which the install picks and then upgrades from alone. An entry
// of the same ID that isn't plainly the same app is left out, as ADR 0029 asks
// (ADR 0078).
func TestTheSameAppInTwoCatalogsIsOneCard(t *testing.T) {
	first, second := gitfetchtest.New(t), gitfetchtest.New(t)
	license := "Apache License\nVersion 2.0, January 2004\n"
	firstHead := first.Commit(map[string]string{"LICENSE": license, "templates/compose/board.yaml": boardTemplate, "templates/compose/notes.yaml": boardTemplate})
	secondHead := second.Commit(map[string]string{
		"LICENSE":                      license,
		"templates/compose/board.yaml": strings.Replace(boardTemplate, "A board for notes.", "Boards, from another store.", 1),
		"templates/compose/other.yaml": strings.Replace(boardTemplate, "# port: 80", "# port: 81", 1),
		"templates/compose/notes.yaml": strings.Replace(boardTemplate, "nginx:1.27", "httpd:2.4", 1),
	})
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: testRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	for _, at := range [][3]string{{"alpha", first.URL, firstHead}, {"beta", second.URL, secondHead}} {
		name := at[0]
		res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: name, Repository: at[1], Commit: at[2]}))
		if err != nil {
			t.Fatalf("SetCatalogSource %s: %v", name, err)
		}
		apply(t, c, res.Msg.GetChange())
	}
	listed, err := cat.ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, e := range listed.Msg.GetEntries() {
		if e.GetId() == "board" {
			cards++
			if e.GetSummary() != "A board for notes." {
				t.Errorf("the card is beta's, not the first catalog's: %q", e.GetSummary())
			}
		}
	}
	also := listed.Msg.GetAlso()
	if cards != 1 || len(also) != 1 || also[0].GetId() != "board" || also[0].GetCatalog() != "beta" || also[0].GetEntry().GetSummary() != "Boards, from another store." {
		t.Fatalf("board shows %d cards, and also %v", cards, also)
	}
	sources, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources.Msg.GetSources() {
		if s.GetName() != "beta" {
			continue
		}
		kinds := map[string]string{}
		for _, r := range s.GetRefused() {
			kinds[r.GetId()] = r.GetKind()
		}
		if !slices.Equal(s.GetAlso(), []string{"board"}) || kinds["notes"] != "taken" || !slices.Equal(s.GetEntries(), []string{"other"}) {
			t.Errorf("beta = %v", s)
		}
	}

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "board", Catalog: "beta"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Msg.GetCatalogs(), []string{"alpha", "beta"}) || got.Msg.GetEntry().GetSummary() != "Boards, from another store." {
		t.Errorf("board from beta = %v, catalogs %v", got.Msg.GetEntry().GetSummary(), got.Msg.GetCatalogs())
	}
	if _, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "board", Catalog: "gamma"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a catalog that hasn't it: %v", err)
	}
	installing, err := cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "board", Catalog: "beta"}))
	if err != nil {
		t.Fatalf("installing board from beta: %v", err)
	}
	apply(t, c, installing.Msg.GetChange())
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil || len(installs.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs = %v, %v", installs.Msg, err)
	}
	if in := installs.Msg.GetInstalls()[0]; in.GetPin().GetCatalog() != "beta" || in.GetPin().GetRepository() != second.URL || in.GetName() == "" {
		t.Errorf("the install's pin = %v, name %q", in.GetPin(), in.GetName())
	}
}
