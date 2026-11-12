// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	logapi "github.com/dockwell/dockwell/internal/control/logs"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// An install starts with Dockwell's own catalog, the source dockwell, following
// Dockwell/catalog's main at the commit the build holds, by Dockwell's own change,
// with no network. An install from before, whose apps came from the catalog built
// into Dockwell, gets it once the same way, and each of those apps moves to it, so
// its upgrades come from there; the change says which, and the pin keeps the build's
// commit (ADR 0077).
func TestBuiltInInstallsMoveToDockwellsOwnCatalog(t *testing.T) {
	storetest.Each(t, testBuiltInInstallsMoveToDockwellsOwnCatalog)
}

func testBuiltInInstallsMoveToDockwellsOwnCatalog(t *testing.T, backend store.Backend) {
	dir := t.TempDir()
	t.Setenv(catalog.DirEnv, dir)
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	sources := func() []*dockwellv1.CatalogSourceInfo {
		t.Helper()
		res, err := cat.ListCatalogSources(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogSourcesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetSources()
	}
	made := func(summary string) []*dockwellv1.Change {
		t.Helper()
		res, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var out []*dockwellv1.Change
		for _, ch := range res.Msg.GetChanges() {
			if ch.GetSummary() == summary {
				// With its plan, which a listing leaves out.
				one, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: ch.GetId()}))
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, one.Msg.GetChange())
			}
		}
		return out
	}

	// A new install: Dockwell's own catalog, by Dockwell's own change, applied.
	s := sources()
	if len(s) != 1 || s[0].GetName() != catalogapi.DefaultName || !s[0].GetOwn() || s[0].GetSource().GetFollow() != "main" ||
		s[0].GetSource().GetCommit() != snapshot.Commit() || s[0].GetSource().GetRepository() != snapshot.Repository || s[0].GetSource().GetPath() != "entries" ||
		len(s[0].GetEntries()) < 30 || len(s[0].GetRefused()) != 0 || s[0].GetUnread() != "" {
		t.Fatalf("a new install's sources: %v", s)
	}
	first := made("Add Dockwell's own catalog, following main")
	if len(first) != 1 || first[0].GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL || first[0].GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the change that added it: %v", first)
	}
	if follows := notesOf(first[0], "catalog_source_follows"); len(follows) != 1 {
		t.Errorf("it doesn't say it follows main: %v", first[0].GetPlan().GetNotes())
	}
	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if icon := got.Msg.GetEntry().GetIcon(); !strings.HasPrefix(icon, "/catalog-media/") || !strings.HasSuffix(icon, "/"+snapshot.Commit()+"/postgres-icon.webp") {
		t.Errorf("postgres's icon is %q, not its own catalog's", icon)
	}

	// Two apps as an older Dockwell installed them, from the catalog built into it.
	for _, id := range []string{"postgres", "mariadb"} {
		apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: id}))
	}
	builtIn := "0123456789abcdef0123456789abcdef01234567"
	asBuiltIn(t, f, []string{"postgres", "mariadb"}, builtIn)
	// And no catalog of Dockwell's own yet, as before Dockwell/catalog.
	removing, err := cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: catalogapi.DefaultName}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, removing.Msg.GetChange())
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error { return tx.DeleteSetting(t.Context(), "catalog.default-source") }); err != nil {
		t.Fatal(err)
	}

	// Its first start since: Dockwell's own catalog, and each app moves to it.
	if err := f.server.catalogs.MakeDefault(t.Context()); err != nil {
		t.Fatal(err)
	}
	again := made("Add Dockwell's own catalog, following main")
	if len(again) != 2 {
		t.Fatalf("changes that add it: %v", again)
	}
	moving := again[0]
	if moving.GetId() == first[0].GetId() {
		moving = again[1]
	}
	for _, n := range moving.GetPlan().GetNotes() {
		if n.GetReason() == "catalog_source_moves_installs" && !slices.Equal(n.GetNames(), []string{"mariadb", "postgres"}) {
			t.Errorf("it moves %v", n.GetNames())
		}
	}
	if moves := notesOf(moving, "catalog_source_moves_installs"); len(moves) != 1 || !strings.Contains(moves[0], "mariadb, postgres") || !strings.Contains(moves[0], "nothing they run changes") {
		t.Errorf("what it says of the apps it moves: %q", moves)
	}
	res, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.GetInstalls()) != 2 {
		t.Fatalf("installs: %v", res.Msg.GetInstalls())
	}
	for _, in := range res.Msg.GetInstalls() {
		pin := in.GetPin()
		if pin.GetCatalog() != catalogapi.DefaultName || pin.GetRepository() != snapshot.Repository || pin.GetPath() != "entries" ||
			pin.GetCommit() != snapshot.Commit() || pin.GetBuiltInCommit() != builtIn || in.GetName() == "" {
			t.Errorf("%s's pin after the move: %v", in.GetProject(), pin)
		}
	}

	// Its upgrades come from Dockwell's own catalog: a newer postgres there is one.
	raw, err := os.ReadFile("../catalog/snapshot/entries/postgres.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newer := regexp.MustCompile(`(?m)^release: \d+$`).ReplaceAllString(string(raw), "release: 99")
	newer = regexp.MustCompile(`(image: postgres:[^@]+@sha256:)[0-9a-f]{64}`).ReplaceAllString(newer, "${1}"+strings.Repeat("e", 64))
	if err := os.WriteFile(filepath.Join(dir, "postgres.yaml"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range res.Msg.GetInstalls() {
		if up := in.GetUpgrade(); (in.GetProject() == "postgres") != (up.GetRelease() == 99) {
			t.Errorf("%s's upgrade: %v", in.GetProject(), up)
		}
	}
	// And it keeps its images, Dockwell's own catalog's.
	got, err = cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "postgres"}))
	if err != nil {
		t.Fatal(err)
	}
	if e := got.Msg.GetEntry(); e.GetRelease() != 99 || !strings.HasSuffix(e.GetIcon(), "/"+snapshot.Commit()+"/postgres-icon.webp") {
		t.Errorf("the newer postgres: release %d, icon %q", e.GetRelease(), e.GetIcon())
	}

	// Added back by hand with GitHub out of reach, as the panel's one click does,
	// it starts at the commit the build holds.
	removing, err = cat.RemoveCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.RemoveCatalogSourceRequest{Name: catalogapi.DefaultName}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, removing.Msg.GetChange())
	adding, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{Name: catalogapi.DefaultName, Repository: snapshot.Repository, Path: "entries", Follow: "main"}))
	if err != nil {
		t.Fatalf("adding Dockwell's own back offline: %v", err)
	}
	apply(t, c, adding.Msg.GetChange())
	if s := sources(); len(s) != 1 || !s[0].GetOwn() || s[0].GetSource().GetCommit() != snapshot.Commit() || len(s[0].GetEntries()) < 30 {
		t.Errorf("Dockwell's own, added back offline: %v", s)
	}

	// Once only: a later start changes nothing.
	if err := f.server.catalogs.MakeDefault(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(made("Add Dockwell's own catalog, following main")); n != 2 {
		t.Errorf("a later start made another: %d", n)
	}
}

// Dockwell's own catalog comes at a first start with no network: the build holds its
// commit, and the change says the branch it follows rather than asking GitHub which
// branches are at it, so an install that starts offline waits for nothing (ADR 0077).
func TestAFirstStartAsksTheNetworkNothing(t *testing.T) {
	var asked atomic.Int32
	client := &http.Client{Transport: askedOf(func(*http.Request) (*http.Response, error) {
		asked.Add(1)
		return nil, errors.New("this test has no network")
	})}
	f := startFullConfig(t, store.SQLite, nil, slog.New(slog.DiscardHandler), nil, logapi.Options{}, func(c *Config) { c.CatalogClient = client })
	res, err := catalogClient(signedIn(t, f)).ListCatalogEntries(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogEntriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.GetEntries()) < 30 {
		t.Errorf("the catalog lists %d apps", len(res.Msg.GetEntries()))
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("a first start asked the network %d times", n)
	}
}

// askedOf is an http.RoundTripper of a function.
type askedOf func(*http.Request) (*http.Response, error)

func (f askedOf) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// asBuiltIn writes each project's pin as an older Dockwell wrote it, from the catalog
// built into it at the build's commit, as Dockwell's own change.
func asBuiltIn(t *testing.T, f *fullServer, projects []string, commit string) {
	t.Helper()
	var edits []revision.Edit
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, name := range projects {
			r, err := tx.GetResource(t.Context(), string(scope.ProjectKind), tree.Organization.ID, name)
			if err != nil {
				return err
			}
			spec, err := f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			p := proto.CloneOf(spec.(*resourcev1.Project))
			p.Catalog.Catalog, p.Catalog.Repository, p.Catalog.Path, p.Catalog.Commit = "", "https://github.com/Dockwell/dockwell", "", commit
			edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: tree.Organization.ID, Name: name}, Spec: p})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.engine.Propose(context.WithoutCancel(t.Context()), revision.Proposal{Summary: "As an older Dockwell installed them", Actor: revision.SystemActor, Via: revision.ViaAutomatic, Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
}
