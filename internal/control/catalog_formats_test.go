// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/resource/service"
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
	if reads := notesOf(planned, "catalog_source_reads"); len(reads) != 1 || reads[0] != "It reads 2 apps from "+repo.URL+", at "+head[:12]+", as Coolify's templates, under Apache-2.0." {
		t.Errorf("what it reads: %q", reads)
	}
	if left := notesOf(planned, "catalog_source_leaves_out"); len(left) != 1 ||
		!strings.HasPrefix(left[0], "It leaves out 2 of 4 apps: ") || !strings.Contains(left[0], "1 are marked by their store to leave out") {
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
	// An app whose image follows a tag that moves is read too (ADR 0085).
	if len(s) != 1 || s[0].GetSource().GetFormat() != resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY || !slices.Equal(s[0].GetEntries(), []string{"board", "moving"}) || len(s[0].GetRefused()) != 2 {
		t.Fatalf("ListCatalogSources = %v", s)
	}
	kinds := map[string]string{}
	for _, r := range s[0].GetRefused() {
		kinds[r.GetId()] = r.GetKind()
	}
	if kinds["ignored"] != "skipped" || kinds["scripted"] == "" {
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

// movingRegistry stands in for Docker Hub, where nginx's latest points at one build
// and then, once moved, at another.
func movingRegistry(t *testing.T, first string) (string, func(digest string)) {
	t.Helper()
	config := `{"os":"linux","architecture":"amd64","config":{"ExposedPorts":{"80/tcp":{}}}}`
	var at atomic.Value
	at.Store(first)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest := at.Load().(string)
		switch p := r.URL.Path; {
		case p == "/v2/library/nginx/manifests/latest" || p == "/v2/library/nginx/manifests/"+digest:
			w.Header().Set("Docker-Content-Digest", digest)
			if r.Method == http.MethodGet {
				_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:%064d","size":%d}}`, 8, len(config))
			}
		case strings.HasPrefix(p, "/v2/library/nginx/blobs/"):
			_, _ = w.Write([]byte(config))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func(d string) { at.Store(d) }
}

// A store's app whose image follows a tag that moves, such as latest, installs: its
// plan pins the build the tag points at now and says so, and it runs that build
// until a new one's pushed, which its check then offers, and upgrading the install
// takes, by a plan that says the new build (ADR 0085).
func TestAMovingTagIsPinnedAndMovesByItsBuilds(t *testing.T) {
	const (
		buildA = "sha256:aaaa000000000000000000000000000000000000000000000000000000000000"
		buildB = "sha256:bbbb000000000000000000000000000000000000000000000000000000000000"
	)
	repo := gitfetchtest.New(t)
	head := repo.Commit(map[string]string{
		"LICENSE":                       "                                 Apache License\n                           Version 2.0, January 2004\n",
		"templates/compose/moving.yaml": strings.Replace(boardTemplate, "nginx:1.27", "nginx", 1),
	})
	registry, push := movingRegistry(t, buildA)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: registry}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: head}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "moving"}))
	if err != nil {
		t.Fatalf("a store's app on latest isn't in the catalog: %v", err)
	}
	// An image named with no tag is latest, written out, so the plan names what it pins.
	if e := got.Msg.GetEntry(); !strings.Contains(e.GetCompose(), "image: nginx:latest\n") || e.GetVersion() != "latest" {
		t.Errorf("the entry names %q at version %q", e.GetCompose(), e.GetVersion())
	}
	installing := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "moving"})
	if pins := notesOf(installing, "catalog_pins_images"); len(pins) != 1 || !strings.Contains(pins[0], "nginx:latest at sha256:aaaa00000000") {
		t.Errorf("the plan doesn't say the build it pins: %q", pins)
	}
	apply(t, c, installing)
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil || len(installs.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs = %v, %v", installs.Msg, err)
	}
	project := installs.Msg.GetInstalls()[0].GetProjectId()
	if file := string(installs.Msg.GetInstalls()[0].GetPin().GetFile()); !strings.Contains(file, "image: nginx:latest@"+buildA) {
		t.Errorf("the install runs:\n%s", file)
	}

	// With the same build, there's nothing to take.
	_, err = cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: project}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "on the newest build of nginx:latest") {
		t.Errorf("upgrading on the same build: %v", err)
	}

	// A new build of latest is offered as one, through its catalog.
	push(buildB)
	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	checked, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Server: srv.id.ServerID}))
	if err != nil {
		t.Fatal(err)
	}
	var offered *dockwellv1.AvailableUpdate
	for _, s := range checked.Msg.GetServices() {
		if s.GetResource().GetName() == "board" && len(s.GetUpdate().GetAvailable()) > 0 {
			if s.GetUpdate().GetSource() != dockwellv1.UpdateSource_UPDATE_SOURCE_CATALOG {
				t.Errorf("the install's update reads as %v", s.GetUpdate().GetSource())
			}
			offered = s.GetUpdate().GetAvailable()[0]
		}
	}
	if offered.GetKind() != dockwellv1.UpdateKind_UPDATE_KIND_DIGEST || offered.GetToDigest() != buildB || offered.GetCatalogRelease() == 0 {
		t.Fatalf("the install was offered %v", offered)
	}

	upgrading, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: project}))
	if err != nil {
		t.Fatalf("taking the new build: %v", err)
	}
	ch := upgrading.Msg.GetChange()
	if ch.GetSummary() != "Update moving to the newest build of nginx:latest" {
		t.Errorf("the plan's summary is %q", ch.GetSummary())
	}
	if pins := notesOf(ch, "catalog_pins_images"); len(pins) != 1 || !strings.Contains(pins[0], "nginx:latest at sha256:bbbb00000000") {
		t.Errorf("the plan doesn't say the new build: %q", pins)
	}
	apply(t, c, ch)
	installs, err = cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if file := string(installs.Msg.GetInstalls()[0].GetPin().GetFile()); !strings.Contains(file, "image: nginx:latest@"+buildB) {
		t.Errorf("after the update the install runs:\n%s", file)
	}

	// The store moves on to a release of its own, still on latest. Taking a new build
	// alone, as its update of the kind digest does, mustn't take that release, which
	// the person may skip: it's refused, and the release stays an update of its own.
	moved := repo.CommitLater(map[string]string{
		"LICENSE":                       "                                 Apache License\n                           Version 2.0, January 2004\n",
		"templates/compose/moving.yaml": strings.Replace(strings.Replace(boardTemplate, "nginx:1.27", "nginx", 1), "ADMIN_PASSWORD=${SERVICE_PASSWORD_ADMIN}", "ADMIN_PASSWORD=${SERVICE_PASSWORD_ADMIN}\n      - THEME=dark", 1),
	})
	res, err = cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: moved}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())
	board := ""
	for _, s := range checked.Msg.GetServices() {
		if s.GetResource().GetName() == "board" {
			board = s.GetId()
		}
	}
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	_, err = svc.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: board, Rebuild: true}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "catalog has moved on to") {
		t.Errorf("taking the new build alone once the store moved on: %v", err)
	}
	// The release itself is still a person's to take.
	if _, err := svc.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: board})); err != nil {
		t.Errorf("taking the newer release: %v", err)
	}
}

// A store's app that writes a secret Dockwell generates inside a word of its command,
// as redis-server --requirepass does, gets the secret itself there, read from its
// environment as the container is made, never the stand-in a plan reads the file with,
// and keeps it through an upgrade (ADR 0022, ADR 0085).
func TestASecretInsideACommandWordReachesTheContainer(t *testing.T) {
	template := `# slogan: A cache.
# port: 6379

services:
  cache:
    image: nginx:1.27
    command: redis-server --appendonly yes --requirepass ${SERVICE_PASSWORD_REDIS} --name=cache-${SERVICE_PASSWORD_REDIS}
    environment:
      - SERVICE_URL_CACHE_6379
`
	repo := gitfetchtest.New(t)
	license := "                                 Apache License\n                           Version 2.0, January 2004\n"
	first := repo.Commit(map[string]string{"LICENSE": license, "templates/compose/cache.yaml": template})
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: testRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: first}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())
	installing := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "cache"})
	apply(t, c, installing)

	password := func() string {
		t.Helper()
		var cmd []string
		var env []string
		for _, name := range e.Containers() {
			if strings.Contains(name, "cache") {
				got := e.Container(name)
				cmd, env = got.Config.Cmd, got.Config.Env
			}
		}
		if len(cmd) != 6 || cmd[3] != "--requirepass" {
			t.Fatalf("the container runs %q", cmd)
		}
		var given string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "REDIS_PASSWORD="); ok {
				given = v
			}
		}
		if len(cmd[4]) < 32 || cmd[4] != given || cmd[5] != "--name=cache-"+given {
			t.Fatalf("the command's words are %q and %q, where its environment holds a password of %d characters", cmd[4], cmd[5], len(given))
		}
		return cmd[4]
	}
	before := password()
	// The command names the value; the value is a secret the service uses by reference,
	// sealed, which no spec holds.
	var spec proto.Message
	installed, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil || len(installed.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs = %v, %v", installed.Msg, err)
	}
	env := installed.Msg.GetInstalls()[0].GetEnvironmentId()
	err = f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), env, "cache")
		if err != nil {
			return err
		}
		found := &r
		spec, err = f.keeper.Decode(t.Context(), tx, store.InRevision(found.ID, found.Kind, found.Revision), found.Spec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := spec.(*resourcev1.Service)
	app := svc.GetApp()
	if strings.Join(app.GetCommand(), " ") != "redis-server --appendonly yes --requirepass ${REDIS_PASSWORD} --name=cache-${REDIS_PASSWORD}" ||
		!slices.Contains(app.GetReferences(), "command") || svc.GetSecrets()["REDIS_PASSWORD"] == "" {
		t.Errorf("the service's spec = %v", svc)
	}
	if _, inEnv := app.GetEnv()["REDIS_PASSWORD"]; inEnv {
		t.Error("the service's spec holds the password as a value of its own")
	}
	for _, ch := range []*dockwellv1.Change{installing} {
		if strings.Contains(ch.GetPlan().String(), before) {
			t.Error("the install's plan holds the secret")
		}
	}

	// A new commit that changes the app upgrades it, and the secret stays.
	second := repo.Commit(map[string]string{"LICENSE": license, "templates/compose/cache.yaml": strings.Replace(template, "--appendonly yes", "--appendonly no", 1)})
	moved, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: "coolify", Repository: repo.URL, Commit: second}))
	if err != nil {
		t.Fatalf("moving the source: %v", err)
	}
	apply(t, c, moved.Msg.GetChange())
	installs, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil || len(installs.Msg.GetInstalls()) != 1 {
		t.Fatalf("installs = %v, %v", installs.Msg, err)
	}
	upgrading, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: installs.Msg.GetInstalls()[0].GetProjectId()}))
	if err != nil {
		t.Fatalf("upgrading: %v", err)
	}
	apply(t, c, upgrading.Msg.GetChange())
	if after := password(); after != before {
		t.Error("the upgrade changed the secret in the command")
	}
}
