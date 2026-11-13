// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What a catalog install's own values hold that can't be a secret shows as it is, in
// a read and in a plan, and its passwords stay secrets, used by reference. An install
// from before shows the same once, by Dockwell's own change, but for what a person
// changed since. Keeping a value without reading it never shows it (ADR 0081).
func TestAnInstallShowsItsPlainValues(t *testing.T) {
	storetest.Each(t, testAnInstallShowsItsPlainValues)
}

func testAnInstallShowsItsPlainValues(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	pullsAnything(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress"}))

	read := func(name string) *dockwellv1.ServiceInfo {
		t.Helper()
		res, err := svc.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range res.Msg.GetServices() {
			if s.GetResource().GetName() == name {
				return s
			}
		}
		t.Fatalf("no service %s", name)
		return nil
	}
	wp := read("wordpress")
	env := wp.GetSpec().GetApp().GetEnv()
	if env["WORDPRESS_DB_HOST"] != "db" || env["WORDPRESS_DB_USER"] != "wordpress" || env["WORDPRESS_DB_NAME"] != "wordpress" {
		t.Errorf("wordpress reads as %v, not with what its entry set", env)
	}
	if want := []string{"WORDPRESS_DB_HOST", "WORDPRESS_DB_NAME", "WORDPRESS_DB_USER"}; !slices.Equal(wp.GetSpec().GetShownEnv(), want) {
		t.Errorf("wordpress shows %v, want %v", wp.GetSpec().GetShownEnv(), want)
	}
	if _, ok := wp.GetSpec().GetSecrets()["WORDPRESS_DB_PASSWORD"]; !ok || env["WORDPRESS_DB_PASSWORD"] != "" {
		t.Errorf("wordpress's password isn't a secret it uses: secrets %v, env %v", wp.GetSpec().GetSecrets(), env)
	}
	db := read("db")
	if got := db.GetSpec().GetApp().GetEnv(); got["MARIADB_DATABASE"] != "wordpress" || got["MARIADB_AUTO_UPGRADE"] != "1" {
		t.Errorf("db reads as %v", got)
	}

	// A change of a value it shows says what it was and is, as both show it.
	spec := proto.CloneOf(wp.GetSpec())
	keep := slices.DeleteFunc(slices.Sorted(maps.Keys(spec.GetApp().GetEnv())), func(k string) bool { return k == "WORDPRESS_DB_NAME" })
	spec.GetApp().Env = map[string]string{"WORDPRESS_DB_NAME": "blog"}
	res, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Rename the database",
		Edits: []*dockwellv1.Edit{{
			Resource: wp.GetResource(), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
			BaseRevision: wp.GetRevision(), KeepEnv: keep,
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, s := range res.Msg.GetChange().GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			for _, fc := range step.GetChanges() {
				if fc.GetPath() == "app.env[WORDPRESS_DB_NAME]" {
					saw = true
					if fc.GetSensitive() || fc.GetFrom() != "wordpress" || fc.GetTo() != "blog" {
						t.Errorf("the plan says %v", fc)
					}
				}
			}
		}
	}
	if !saw {
		t.Errorf("the plan doesn't change WORDPRESS_DB_NAME: %v", res.Msg.GetChange().GetPlan())
	}
	apply(t, c, res.Msg.GetChange())

	// As an install from before Dockwell showed anything: nothing shown, and a value a
	// person changed since.
	asUnshown(t, f, []string{"wordpress", "db"})
	if got := read("wordpress").GetSpec().GetApp().GetEnv()["WORDPRESS_DB_HOST"]; got != "" {
		t.Fatalf("an install from before shows WORDPRESS_DB_HOST as %q", got)
	}
	// Showing a value it keeps without reading it is refused.
	wp = read("wordpress")
	spec = proto.CloneOf(wp.GetSpec())
	spec.GetApp().Env = nil
	spec.ShownEnv = []string{"WORDPRESS_DB_HOST"}
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Show what nobody saw",
		Edits: []*dockwellv1.Edit{{
			Resource: wp.GetResource(), Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
			BaseRevision: wp.GetRevision(), KeepEnv: slices.Sorted(maps.Keys(wp.GetSpec().GetApp().GetEnv())),
		}},
	}))
	if err == nil || !strings.Contains(err.Error(), "shows WORDPRESS_DB_HOST") {
		t.Errorf("showing a value it keeps: %v", err)
	}

	// It waits while its server is away, and looking again before it's back plans
	// nothing, so Activity doesn't fill with changes it discards.
	countChanges := func() int {
		t.Helper()
		res, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Msg.GetChanges())
	}
	before := countChanges()
	for range 3 {
		done, err := f.server.catalogs.ShowPlainValues(t.Context(), func(string) bool { return false })
		if err != nil || done {
			t.Fatalf("ShowPlainValues with the server away: done %t, %v", done, err)
		}
	}
	if got := countChanges(); got != before {
		t.Fatalf("looking while the server is away made %d changes", got-before)
	}
	done, err := f.server.catalogs.ShowPlainValues(t.Context(), func(string) bool { return true })
	if err != nil || !done {
		t.Fatalf("ShowPlainValues: done %t, %v", done, err)
	}
	wp = read("wordpress")
	if want := []string{"WORDPRESS_DB_HOST", "WORDPRESS_DB_USER"}; !slices.Equal(wp.GetSpec().GetShownEnv(), want) {
		t.Errorf("after the change, wordpress shows %v, want %v: the name a person changed stays hidden", wp.GetSpec().GetShownEnv(), want)
	}
	if got := wp.GetSpec().GetApp().GetEnv(); got["WORDPRESS_DB_NAME"] != "" || got["WORDPRESS_DB_HOST"] != "db" {
		t.Errorf("wordpress reads as %v", got)
	}
	// Nothing its server runs changed, so it reads as live on the revision that shows them.
	if d := wp.GetDeployments(); len(d) != 1 || d[0].GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
		t.Errorf("wordpress's deployments after the change: %v", d)
	}
	if want := []string{"MARIADB_AUTO_UPGRADE", "MARIADB_DATABASE", "MARIADB_USER"}; !slices.Equal(read("db").GetSpec().GetShownEnv(), want) {
		t.Errorf("db shows %v, want %v", read("db").GetSpec().GetShownEnv(), want)
	}
	list, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var made []*dockwellv1.Change
	for _, ch := range list.Msg.GetChanges() {
		if strings.HasPrefix(ch.GetSummary(), "Show the values installed apps set") {
			made = append(made, ch)
		}
	}
	if len(made) != 1 || made[0].GetProposedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_DOCKWELL || made[0].GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the changes that showed them: %v", made)
	}
	// Once only.
	asUnshown(t, f, []string{"db"})
	if done, err := f.server.catalogs.ShowPlainValues(t.Context(), func(string) bool { return true }); err != nil || !done {
		t.Fatalf("ShowPlainValues again: done %t, %v", done, err)
	}
	if got := read("db").GetSpec().GetShownEnv(); got != nil {
		t.Errorf("a second look showed %v", got)
	}
}

// asUnshown takes away what each service of the install shows, as an install from
// before Dockwell showed values was.
func asUnshown(t *testing.T, f *fullServer, services []string) {
	t.Helper()
	var edits []revision.Edit
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			for _, env := range p.Environments {
				list, err := tx.ListResources(t.Context(), env.ID, string(service.Kind))
				if err != nil {
					return err
				}
				for _, r := range list {
					if !slices.Contains(services, r.Name) {
						continue
					}
					spec, err := f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
					if err != nil {
						return err
					}
					s := proto.CloneOf(spec.(*resourcev1.Service))
					s.ShownEnv = nil
					edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: service.Kind, Scope: r.Scope, Name: r.Name}, Spec: s, Base: r.Revision})
				}
			}
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

// While an app waits for a server that's away, looking again plans nothing until
// that server is back, and one whose plan would change what its server runs, as
// after drift, is left hidden once and never proposed again: Activity doesn't fill
// with changes Dockwell discards (ADR 0081).
func TestShowingInstallsLeavesNoTrailWhileItWaits(t *testing.T) {
	storetest.Each(t, testShowingInstallsLeavesNoTrailWhileItWaits)
}

func testShowingInstallsLeavesNoTrailWhileItWaits(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	near := f.addDockerServer(t)
	far := f.addServerNamed(t, "hz-2", nil, nil)
	pullsAnything(near)
	pullsAnything(far)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	servers, err := dockwellv1connect.NewServerServiceClient(b, b.origin).ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	id := map[string]string{}
	for _, s := range servers.Msg.GetServers() {
		id[s.GetName()] = s.GetId()
	}
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: "blog", Servers: []string{id["hz-1"]}}))
	apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "wordpress", Project: "away", Servers: []string{id["hz-2"]}}))
	asUnshown(t, f, []string{"wordpress", "db"})
	// hz-1's database drifted: showing its values would put it back, which is no part of showing.
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	list, err := svc.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Msg.GetServices() {
		if s.GetResource().GetName() == "db" && s.GetDeployments()[0].GetServerId() == id["hz-1"] {
			near.Edit(docker.Project(s.GetResource().GetScope())+"-db-1", func(ct *docker.Container) {
				ct.Config.Env = append(ct.Config.Env, "BY_HAND=1")
			})
		}
	}
	countChanges := func() int {
		t.Helper()
		res, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Msg.GetChanges())
	}
	nearOnly := func(s string) bool { return s == id["hz-1"] }
	if done, err := f.server.catalogs.ShowPlainValues(t.Context(), nearOnly); err != nil || done {
		t.Fatalf("the first look: done %t, %v", done, err)
	}
	after := countChanges()
	for range 3 {
		if done, err := f.server.catalogs.ShowPlainValues(t.Context(), nearOnly); err != nil || done {
			t.Fatalf("a look while hz-2 is away: done %t, %v", done, err)
		}
	}
	if got := countChanges(); got != after {
		t.Fatalf("looking again while hz-2 is away made %d changes", got-after)
	}
	// hz-2 is back: what waited for it shows, and the drifted database stays hidden.
	if done, err := f.server.catalogs.ShowPlainValues(t.Context(), func(string) bool { return true }); err != nil || !done {
		t.Fatalf("the look once hz-2 is back: done %t, %v", done, err)
	}
	list, err = svc.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Msg.GetServices() {
		on := s.GetDeployments()[0].GetServerId()
		shows := len(s.GetSpec().GetShownEnv()) > 0
		switch {
		case s.GetResource().GetName() == "db" && on == id["hz-1"] && shows:
			t.Errorf("the drifted database shows %v", s.GetSpec().GetShownEnv())
		case (s.GetResource().GetName() != "db" || on != id["hz-1"]) && !shows:
			t.Errorf("%s on %s shows nothing", s.GetResource().GetName(), on)
		}
	}
}
