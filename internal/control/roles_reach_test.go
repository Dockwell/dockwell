// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// reachStand is an install with two projects: default, whose production runs
// web, and blog, whose staging runs api with a volume, data. One change touched
// both, blog's first. A container runs that Dockwell didn't make.
type reachStand struct {
	f    *fullServer
	b    *browser
	c    dockwellv1connect.ChangeServiceClient
	hold func(grants ...store.Grant)

	production, staging, defaultID, blogID string
	webID, apiID, volumeID, server         string
	both                                   *dockwellv1.Change
}

func newReachStand(t *testing.T, backend store.Backend) *reachStand {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	// It serves routes too, in memory, so a domain can be held.
	e := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&route.Driver{Ingress: &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}}}
	})
	pullsAnything(e)
	s := &reachStand{f: f, b: b, c: changeClient(b, b.origin)}
	web := apply(t, s.c, propose(t, s.c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	s.webID, s.production = resourceIn(t, web, "service"), web.GetPlan().GetResources()[0].GetResource().GetScope()
	s.defaultID = envProject(t, f, s.production)
	s.blogID = resourceIn(t, apply(t, s.c, propose(t, s.c, "Make blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	})), "project")
	s.staging = resourceIn(t, apply(t, s.c, propose(t, s.c, "Add staging to blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.blogID, Name: "staging"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	})), "environment")
	s.apiID = resourceIn(t, apply(t, s.c, propose(t, s.c, "Run api", appEdit(s.staging, "api", nil))), "service")
	s.volumeID = resourceIn(t, apply(t, s.c, propose(t, s.c, "Add data", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "volume", Scope: s.staging, Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{}}},
	})), "volume")
	s.both = apply(t, s.c, propose(t, s.c, "Touch both", appEdit(s.staging, "api", map[string]string{"B": "1"}), appEdit(s.production, "web", map[string]string{"B": "1"})))
	runByHand(t, e, "by-hand", nil)
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		all, err := tx.ListServers(t.Context())
		if len(all) > 0 {
			s.server = all[0].ID
		}
		return err
	})
	s.hold = aMember(t, f)
	s.hold(onInstall(store.RoleAdmin))
	return s
}

// resourceIn is the ID of the first resource of kind ch's plan holds.
func resourceIn(t *testing.T, ch *dockwellv1.Change, kind string) string {
	t.Helper()
	for _, r := range ch.GetPlan().GetResources() {
		if r.GetResource().GetKind() == kind {
			return r.GetId()
		}
	}
	t.Fatalf("%s makes no %s: %v", ch.GetSummary(), kind, ch.GetPlan().GetResources())
	return ""
}

// appEdit runs an app called name in env, with vars.
func appEdit(env, name string, vars map[string]string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Scope: env, Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4", Env: vars}},
		}}},
	}
}

// in is a grant of role in a project.
func in(project, role string) store.Grant { return store.Grant{ProjectID: project, Role: role} }

func notFound(t *testing.T, what string, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeNotFound || reasonOf(err) != "not_found" {
		t.Errorf("%s: %v, want not found", what, err)
	}
}

func needsRole(t *testing.T, what string, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
		t.Errorf("%s: %v, want needs_role", what, err)
	}
}

// A grant on the install below admin covers every project's places, never the
// install's own: a container Dockwell didn't make, access and server events,
// a container on its own. Those are an admin's whatever the call needs (ADR
// 0048, ADR 0049).
func TestAnInstallRoleBelowAdminReachesProjectsAlone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		logs := logClient(s.b, s.b.origin)
		lonely, err := s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run lonely", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "container", Name: "lonely"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Container{Container: &resourcev1.Container{Image: "ghcr.io/acme/tool:1.0"}}},
		}}}))
		if err != nil {
			t.Fatal(err)
		}

		s.hold(onInstall(store.RoleViewer))
		_, err = logs.ReadLogs(ctx, connect.NewRequest(&dockwellv1.ReadLogsRequest{Server: s.server, Id: "by-hand"}))
		needsRole(t, "a viewer on the install reading a container Dockwell didn't make", err)
		_, err = logs.ListLogSources(ctx, connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Server: s.server, Id: "by-hand"}))
		needsRole(t, "a viewer on the install listing its log sources", err)
		events, err := eventClient(s.b, s.b.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		projects := 0
		for _, e := range events.Msg.GetEvents() {
			switch e.GetKind() {
			case dockwellv1.EventKind_EVENT_KIND_ACCESS, dockwellv1.EventKind_EVENT_KIND_SERVER:
				t.Errorf("a viewer on the install reads %s %s, which is the install's", e.GetKind(), e.GetAction())
			default:
				projects++
			}
		}
		if projects == 0 {
			t.Error("a viewer on the install reads no events, want every project's")
		}

		s.hold(onInstall(store.RoleDeveloper))
		_, err = s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: lonely.Msg.GetChange().GetId(), Fingerprint: lonely.Msg.GetChange().GetPlan().GetFingerprint()}))
		needsRole(t, "a developer on the install applying a container on its own", err)
		// Their projects' own, by contrast, are theirs.
		_, err = serviceClientOf(s).DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: s.apiID, Image: "ghcr.io/acme/shop:1.5"}))
		if err != nil {
			t.Errorf("a developer on the install deploying blog's api: %v", err)
		}
	})
}

func serviceClientOf(s *reachStand) dockwellv1connect.ServiceServiceClient {
	return dockwellv1connect.NewServiceServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
}

// What reaches past a container reaches the server every project on it shares,
// so a plan with host access is an admin's to apply, whatever a project's
// grants say (ADR 0048). Until internal/revision checks each item, ApplyChange
// holds it.
func TestHostAccessIsAnAdminsToApply(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		s.hold(in(s.blogID, store.RoleDeveloper))
		res, err := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON()).ImportCompose(ctx, connect.NewRequest(&dockwellv1.ImportComposeRequest{
			Project: "blog", Environment: "staging",
			Compose: "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    privileged: true\n    volumes: [\"/srv/other-projects:/host\"]\n",
		}))
		if err != nil {
			t.Fatal(err)
		}
		ch := res.Msg.GetChange()
		if !ch.GetPlan().GetHostAccess() {
			t.Fatalf("a privileged service with a server path planned without host access: %v", ch.GetPlan())
		}
		applyIt := func() error {
			proof := stepUpFor(t, s.b, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, ch.GetId())
			_, err := s.c.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(), Proof: proof}))
			return err
		}
		err = applyIt()
		needsRole(t, "blog's developer applying a privileged service in blog", err)
		if err != nil && !strings.Contains(err.Error(), "only an admin or the owner can apply this, as app reaches the server path /srv/other-projects") {
			t.Errorf("the refusal says %q, want who may apply it", err)
		}
		s.hold(onInstall(store.RoleAdmin))
		if err := applyIt(); err != nil {
			t.Errorf("an admin applying it: %v", err)
		}
	})
}

// A service or an environment named by name, with no project, is found among
// the caller's places, as the API finds it, and checked where it is: never in a
// place out of reach, and with the role held there (ADR 0049).
func TestANameWithoutItsProjectIsFoundInReach(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		svc := serviceClientOf(s)
		deploy := func(name string) error {
			_, err := svc.DeployService(ctx, connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: name, Image: "ghcr.io/acme/shop:1.5"}))
			return err
		}
		apply(t, s.c, propose(t, s.c, "Add qa to default", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: s.defaultID, Name: "qa"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		}))

		s.hold(onInstall(store.RoleViewer), in(s.blogID, store.RoleDeveloper))
		needsRole(t, "a viewer of default deploying its web by name alone", deploy("web"))
		if err := deploy("api"); err != nil {
			t.Errorf("blog's developer deploying its api by name alone: %v", err)
		}

		s.hold(in(s.blogID, store.RoleDeveloper))
		notFound(t, "blog's developer deploying default's web by name alone", deploy("web"))
		// An environment by name alone is theirs too: default's qa isn't.
		_, err := dockwellv1connect.NewVariableServiceClient(s.b, s.b.origin, connect.WithProtoJSON()).ListVariables(ctx, connect.NewRequest(&dockwellv1.ListVariablesRequest{Environment: "qa"}))
		notFound(t, "blog's developer listing qa's variables by its name alone", err)

		// Found in two of their projects, they're asked which of theirs.
		s.hold(onInstall(store.RoleAdmin))
		apply(t, s.c, propose(t, s.c, "Run web in blog", appEdit(s.staging, "web", nil)))
		s.hold(in(s.blogID, store.RoleDeveloper), in(s.defaultID, store.RoleDeveloper))
		err = deploy("web")
		if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "blog and default") {
			t.Errorf("a name in two of their projects: %v, want which of blog and default", err)
		}
	})
}

// Databases are read as their services are: each in its project, and one named
// by name below an admin is found among the caller's own (ADR 0049).
func TestDatabasesAreReadInReach(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		db := resourceIn(t, apply(t, s.c, propose(t, s.c, "Run db", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Scope: s.production, Name: "db"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}},
			}}},
		})), "service")
		dc := databaseClient(s.b, s.b.origin)
		read := func(req *dockwellv1.GetDatabaseRequest) (string, error) {
			got, err := dc.GetDatabase(ctx, connect.NewRequest(req))
			if err != nil {
				return "", err
			}
			return got.Msg.GetDatabase().GetId(), nil
		}

		s.hold(in(s.blogID, store.RoleDeveloper))
		listed, err := dc.ListDatabases(ctx, connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
		if err != nil || len(listed.Msg.GetDatabases()) != 0 {
			t.Errorf("blog's developer listing databases: %v, %v, want none of default's", listed.Msg.GetDatabases(), err)
		}
		for _, req := range []*dockwellv1.GetDatabaseRequest{{Id: db}, {Id: "db"}, {Id: "db", Project: "default"}} {
			_, err := read(req)
			notFound(t, "blog's developer reading default's database as "+req.String(), err)
		}

		s.hold(in(s.defaultID, store.RoleViewer))
		if got, err := read(&dockwellv1.GetDatabaseRequest{Id: "db"}); err != nil || got != db {
			t.Errorf("default's viewer reading its database by name: %q, %v", got, err)
		}
		listed, err = dc.ListDatabases(ctx, connect.NewRequest(&dockwellv1.ListDatabasesRequest{}))
		if err != nil || len(listed.Msg.GetDatabases()) != 1 {
			t.Errorf("default's viewer listing databases: %v, %v, want db", listed.Msg.GetDatabases(), err)
		}

		// A user is a plan in the database's project: out of reach it's not found,
		// a viewer plans nothing, and a developer's call goes on to planning, where
		// the engine checks each item as it does any change's.
		addUser := func(req *dockwellv1.CreateDatabaseUserRequest) error {
			req.User = "shop"
			_, err := dc.CreateDatabaseUser(ctx, connect.NewRequest(req))
			return err
		}
		needsRole(t, "default's viewer adding a user to its database", addUser(&dockwellv1.CreateDatabaseUserRequest{Id: "db"}))
		s.hold(in(s.blogID, store.RoleDeveloper))
		for _, req := range []*dockwellv1.CreateDatabaseUserRequest{{Id: db}, {Id: "db"}, {Id: "db", Project: "default"}} {
			notFound(t, "blog's developer adding a user to default's database as "+req.String(), addUser(req))
		}
		s.hold(in(s.defaultID, store.RoleDeveloper))
		// The stand's image has no psql, so planning stops on the server: past the
		// role and the scope, which is what this checks.
		err = addUser(&dockwellv1.CreateDatabaseUserRequest{Id: "db"})
		if reasonOf(err) == "needs_role" || reasonOf(err) == "not_found" || err != nil && !strings.Contains(err.Error(), "can't plan database.user shop") {
			t.Errorf("default's developer adding a user by the database's name: %v, want it planned on the server", err)
		}
	})
}

// A question of which project names only the caller's: someone who holds one
// project alone gets theirs, and someone with several is asked which of
// theirs, never told every project's name.
func TestQuestionsOfWhichProjectNameOnlyTheirs(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		shopID := resourceIn(t, apply(t, s.c, propose(t, s.c, "Make shop", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "shop"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		vars := dockwellv1connect.NewVariableServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		health := dockwellv1connect.NewHealthServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		composes := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		quiet := func(what string, err error, others ...string) {
			t.Helper()
			if err == nil {
				return
			}
			for _, name := range others {
				if strings.Contains(err.Error(), name) {
					t.Errorf("%s: %v, which names %s", what, err, name)
				}
			}
		}

		s.hold(in(s.blogID, store.RoleDeveloper))
		_, err := vars.ListVariables(ctx, connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
		quiet("ListVariables naming nothing", err, "default", "shop")
		_, err = health.ListHealth(ctx, connect.NewRequest(&dockwellv1.ListHealthRequest{Service: "web"}))
		notFound(t, "ListHealth of default's web by name alone", err)
		_, err = composes.ExportCompose(ctx, connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
		quiet("ExportCompose naming nothing", err, "default", "shop")
		_, err = s.c.RestartService(ctx, connect.NewRequest(&dockwellv1.RestartServiceRequest{Id: "web"}))
		notFound(t, "restarting default's web by name alone", err)
		pc, err := s.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Set X", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: "production", Name: "X"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "1"}}},
		}}}))
		if err != nil {
			t.Errorf("blog's developer setting X in production alone: %v, want it planned in blog's", err)
		}
		if err == nil && envProject(t, s.f, pc.Msg.GetChange().GetPlan().GetResources()[0].GetResource().GetScope()) != s.blogID {
			t.Errorf("blog's developer setting X in production alone planned it in %v, want blog's", pc.Msg.GetChange().GetPlan().GetResources()[0].GetResource())
		}
		// A plan that names nothing goes in their one project.
		imp, err := composes.ImportCompose(ctx, connect.NewRequest(&dockwellv1.ImportComposeRequest{Environment: "staging", Compose: "services:\n  cache:\n    image: ghcr.io/acme/cache:2\n"}))
		if err != nil {
			t.Errorf("blog's developer importing a file that names no project: %v", err)
		} else if got := envProject(t, s.f, imp.Msg.GetChange().GetPlan().GetResources()[0].GetResource().GetScope()); got != s.blogID {
			t.Errorf("their import landed in %s, want blog", got)
		}

		s.hold(in(s.blogID, store.RoleDeveloper), in(shopID, store.RoleDeveloper))
		_, err = vars.ListVariables(ctx, connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
		if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "blog and shop") {
			t.Errorf("ListVariables naming nothing, holding blog and shop: %v, want which of those", err)
		}
		quiet("ListVariables naming nothing, holding blog and shop", err, "default")
		_, err = composes.ImportCompose(ctx, connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "services:\n  cache:\n    image: ghcr.io/acme/cache:2\n"}))
		if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "blog and shop") {
			t.Errorf("importing a file that names no project, holding blog and shop: %v, want which of those", err)
		}
	})
}

// A project role's lists hold what's theirs, all of it: a service's log
// sources, a Compose file's services, the volumes of their project, their
// volume's backups, and each revision of their service.
func TestAProjectRoleListsAllOfItsOwn(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		logs := logClient(s.b, s.b.origin)
		composes := dockwellv1connect.NewComposeServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		backups := backupClient(s.b, s.b.origin)
		const file = "services:\n  app:\n    image: nginx:1.27\n"
		count := func() (sources, checked, revisions int) {
			t.Helper()
			src, err := logs.ListLogSources(ctx, connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: s.apiID}))
			if err != nil {
				t.Fatalf("api's log sources: %v", err)
			}
			cc, err := composes.CheckCompose(ctx, connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file}))
			if err != nil {
				t.Fatalf("checking a file: %v", err)
			}
			revs, err := serviceClientOf(s).ListServiceRevisions(ctx, connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: s.apiID}))
			if err != nil {
				t.Fatalf("api's revisions: %v", err)
			}
			return len(src.Msg.GetSources()), len(cc.Msg.GetServices()), len(revs.Msg.GetRevisions())
		}
		sources, checked, revisions := count()
		if sources == 0 || checked == 0 || revisions < 2 {
			t.Fatalf("an admin reads %d sources, %d services, %d revisions", sources, checked, revisions)
		}

		s.hold(in(s.blogID, store.RoleDeveloper))
		if a, b, c := count(); a != sources || b != checked || c != revisions {
			t.Errorf("blog's developer reads %d sources, %d services, %d revisions, want %d, %d and %d, as an admin does", a, b, c, sources, checked, revisions)
		}
		vols, err := dockwellv1connect.NewResourceServiceClient(s.b, s.b.origin, connect.WithProtoJSON()).ListResources(ctx, connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "volume"}))
		if err != nil || len(vols.Msg.GetResources()) != 1 {
			t.Errorf("blog's developer lists volumes %v, %v, want blog's data", vols, err)
		}
		if _, err := backups.PreviewBackup(ctx, connect.NewRequest(&dockwellv1.PreviewBackupRequest{VolumeId: s.volumeID})); reasonOf(err) == "not_found" {
			t.Errorf("previewing a backup of blog's data: %v", err)
		}
		if _, err := backups.ListBackups(ctx, connect.NewRequest(&dockwellv1.ListBackupsRequest{VolumeId: s.volumeID})); err != nil {
			t.Errorf("listing backups of blog's data: %v", err)
		}
		_, err = backups.ListBackups(ctx, connect.NewRequest(&dockwellv1.ListBackupsRequest{EnvironmentId: s.production}))
		notFound(t, "listing backups of default's production by its ID", err)
		// A server is seen by name: naming one is no reason to refuse.
		if _, err := eventClient(s.b, s.b.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{ServerId: s.server})); err != nil {
			t.Errorf("blog's developer reading events on their server: %v", err)
		}
		// A change across two projects is out of reach, whichever item comes first.
		_, err = s.c.GetChange(ctx, connect.NewRequest(&dockwellv1.GetChangeRequest{Id: s.both.GetId()}))
		notFound(t, "a change whose first item is blog's and second default's", err)
		// A stream naming another project's service is refused as it opens.
		watch, err := eventClient(s.b, s.b.origin).WatchEvents(ctx, connect.NewRequest(&dockwellv1.WatchEventsRequest{ResourceId: s.webID}))
		if err == nil {
			for watch.Receive() {
				t.Errorf("watching default's web sent %v", watch.Msg())
				break
			}
			err = watch.Err()
		}
		notFound(t, "watching default's web", err)
	})
}

// A narrowed list's next page token is sealed for its caller, so it says
// nothing of what's out of their reach, and works only for them.
func TestANarrowedPageTokenSaysNothing(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		s.hold(in(s.blogID, store.RoleDeveloper))
		var tokens int
		seen := map[string]bool{}
		token := ""
		for range 20 {
			page, err := s.c.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{PageSize: 2, PageToken: token}))
			if err != nil {
				t.Fatalf("following the pages: %v", err)
			}
			for _, ch := range page.Msg.GetChanges() {
				seen[ch.GetId()] = true
			}
			token = page.Msg.GetNextPageToken()
			if token == "" {
				break
			}
			tokens++
			if !strings.HasPrefix(token, "dwp1.") || strings.Contains(token, "chg_") {
				t.Errorf("a narrowed page token reads %q, want it sealed", token)
			}
		}
		if tokens == 0 || seen[s.both.GetId()] || len(seen) == 0 {
			t.Errorf("following %d tokens read %v, want blog's changes alone, over pages", tokens, seen)
		}
		_, err := s.c.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{PageSize: 2, PageToken: "dwp1.bm90LW9uZS13ZS1nYXZl"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a made-up page token: %v, want invalid", err)
		}
	})
}

// A stream narrowed to a project's places sends nothing when something happens
// elsewhere, not even an empty message, and doesn't name what left elsewhere.
// It reads the caller's grants again on each recheck, and narrows by them.
func TestANarrowedStreamSaysNothingOfElsewhere(t *testing.T) {
	testRecheck = 100 * time.Millisecond
	t.Cleanup(func() { testRecheck = 0 })
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// The owner, as the API would carry them, changes things elsewhere.
		keeper := place.WithCaller(ctx, place.Caller{UserID: "usr_keeper", Grants: []store.Grant{{UserID: "usr_keeper", ProjectID: store.Install, Role: store.RoleOwner}}})
		elsewhere := func(env, name string) string {
			t.Helper()
			plan, err := s.f.engine.Propose(keeper, revision.Proposal{Summary: "Set " + name, Actor: "user:usr_keeper", Edits: []revision.Edit{{
				Ref:  resource.Ref{Kind: "variable", Scope: env, Name: name},
				Spec: &resourcev1.Variable{Value: "1"},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			return plan.Change
		}
		s.hold(in(s.blogID, store.RoleDeveloper), in(s.defaultID, store.RoleViewer))
		stream, err := eventClient(s.b, s.b.origin).WatchEvents(ctx, connect.NewRequest(&dockwellv1.WatchEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan *dockwellv1.WatchEventsResponse, 64)
		go func() {
			for stream.Receive() {
				got <- stream.Msg()
			}
			close(got)
		}()
		if first := <-got; !first.GetSnapshot() {
			t.Fatalf("the first message isn't the snapshot: %v", first)
		}
		// Until the recheck, default is theirs to read too; then it isn't.
		s.hold(in(s.blogID, store.RoleDeveloper))
		time.Sleep(500 * time.Millisecond)
		planned := elsewhere(s.production, "X")
		time.Sleep(500 * time.Millisecond) // so its plan and its discard come apart
		if err := s.f.engine.Discard(keeper, planned, revision.By{Actor: "user:usr_keeper"}); err != nil {
			t.Fatal(err)
		}
		mine := elsewhere(s.staging, "Y")
		deadline := time.After(5 * time.Second)
		for {
			select {
			case m := <-got:
				for _, e := range m.GetEvents() {
					if e.GetChange().GetId() == planned {
						t.Errorf("the stream sent default's change %s", planned)
					}
				}
				if slices.Contains(m.GetRemoved(), planned) {
					t.Errorf("the stream said default's discarded change %s left", planned)
				}
				if len(m.GetEvents()) == 0 && len(m.GetRemoved()) == 0 && !m.GetSnapshot() {
					t.Errorf("the stream sent an empty message: %v", m)
				}
				for _, e := range m.GetEvents() {
					if e.GetChange().GetId() == mine {
						return
					}
				}
			case <-deadline:
				t.Fatalf("blog's own change %s never came", mine)
			}
		}
	})
}

// A viewer on the install holds every project, so each list of a project's
// objects reads for them as for an admin, but for what's the install's own:
// what the narrowing leaves out is only ever what's out of reach, never what
// it can't place.
func TestEveryProjectsListReadsWhole(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		backups := backupClient(s.b, s.b.origin)
		resources := dockwellv1connect.NewResourceServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		catalog := catalogClient(s.b)
		health := dockwellv1connect.NewHealthServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		vars := dockwellv1connect.NewVariableServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		count := func() map[string]int {
			t.Helper()
			n := map[string]int{}
			read := func(what string, size int, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				n[what] = size
			}
			sv, err := serviceClientOf(s).ListServices(ctx, connect.NewRequest(&dockwellv1.ListServicesRequest{}))
			read("services", len(sv.Msg.GetServices()), err)
			ch, err := s.c.ListChanges(ctx, connect.NewRequest(&dockwellv1.ListChangesRequest{}))
			read("changes", len(ch.Msg.GetChanges()), err)
			dr, err := s.c.ListDrift(ctx, connect.NewRequest(&dockwellv1.ListDriftRequest{}))
			read("drift", len(dr.Msg.GetDrift()), err)
			for _, kind := range []string{"project", "environment", "service", "volume"} {
				rs, err := resources.ListResources(ctx, connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: kind}))
				read("resources of kind "+kind, len(rs.Msg.GetResources()), err)
			}
			bk, err := backups.ListBackups(ctx, connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
			read("backups", len(bk.Msg.GetBackups()), err)
			bp, err := backups.ListBackupPolicies(ctx, connect.NewRequest(&dockwellv1.ListBackupPoliciesRequest{}))
			read("backup policies", len(bp.Msg.GetPolicies()), err)
			ci, err := catalog.ListCatalogInstalls(ctx, connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
			read("catalog installs", len(ci.Msg.GetInstalls()), err)
			hl, err := health.ListHealth(ctx, connect.NewRequest(&dockwellv1.ListHealthRequest{Project: "blog", Environment: "staging"}))
			read("blog's health", len(hl.Msg.GetServices()), err)
			vr, err := vars.ListVariables(ctx, connect.NewRequest(&dockwellv1.ListVariablesRequest{Environment: s.staging}))
			read("staging's variables", len(vr.Msg.GetVariables()), err)
			ev, err := eventClient(s.b, s.b.origin).ListEvents(ctx, connect.NewRequest(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_CHANGE}}))
			read("change events", len(ev.Msg.GetEvents()), err)
			return n
		}
		// Something of each kind to list in blog: a variable and a backup policy.
		policy := policyEdit(7)
		policy.Resource.Scope = s.staging
		apply(t, s.c, propose(t, s.c, "Back up data", policy, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: s.staging, Name: "MODE"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{Value: "fast"}}},
		}))
		admin := count()
		for what, n := range admin {
			if n == 0 && what != "drift" && what != "backups" && what != "catalog installs" {
				t.Errorf("an admin reads no %s, so the comparison says nothing of it", what)
			}
		}
		s.hold(onInstall(store.RoleViewer))
		viewer := count()
		// The install's own, which an admin reads and a viewer doesn't: the change
		// that made an install's first alert rules, which belong to no project.
		admin["changes"]--
		for what, want := range admin {
			if viewer[what] != want {
				t.Errorf("a viewer on the install reads %d %s, and an admin %d", viewer[what], what, want)
			}
		}
	})
}
