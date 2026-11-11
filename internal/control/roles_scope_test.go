// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Someone who holds one project reaches only it: what they name in another, by
// ID or by name, isn't there for them, as if it never was, and lists hold only
// their project's. A name with no project is in their one. A protected
// environment's change is an admin's to apply (ADR 0048, check 3; ADR 0049).
func TestAProjectRoleReachesItsProjectAlone(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		resourceOf := func(ch *dockwellv1.Change, kind string) string {
			t.Helper()
			for _, r := range ch.GetPlan().GetResources() {
				if r.GetResource().GetKind() == kind {
					return r.GetId()
				}
			}
			t.Fatalf("%s makes no %s: %v", ch.GetSummary(), kind, ch.GetPlan().GetResources())
			return ""
		}
		app := func(env, name string, vars map[string]string) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Scope: env, Name: name},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4", Env: vars}},
				}}},
			}
		}

		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		webID, production := resourceOf(web, "service"), web.GetPlan().GetResources()[0].GetResource().GetScope()
		blog := apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		blogID := resourceOf(blog, "project")
		staging := resourceOf(apply(t, c, propose(t, c, "Add staging to blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: blogID, Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")
		apiID := resourceOf(apply(t, c, propose(t, c, "Run api", app(staging, "api", nil))), "service")
		waiting := propose(t, c, "Change web", app(production, "web", map[string]string{"A": "1"}))
		blogPlanned := propose(t, c, "Change api", app(staging, "api", map[string]string{"A": "1"}))

		hold := aMember(t, f)
		hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper})

		services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		notHere := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodeNotFound || reasonOf(err) != "not_found" {
				t.Errorf("%s: %v, want not found", what, err)
			}
		}
		revisions := func(req *dockwellv1.ListServiceRevisionsRequest) error {
			_, err := services.ListServiceRevisions(t.Context(), connect.NewRequest(req))
			return err
		}
		_, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: web.GetId()}))
		notHere("another project's change, by ID", err)
		notHere("another project's service, by ID", revisions(&dockwellv1.ListServiceRevisionsRequest{Id: webID}))
		notHere("another project's service, by name", revisions(&dockwellv1.ListServiceRevisionsRequest{Id: "web", Project: "default"}))
		// With no project named, a name is looked for in their one project, so another's doesn't turn up.
		if err := revisions(&dockwellv1.ListServiceRevisionsRequest{Id: "web"}); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("another project's service, by name alone: %v, want not found", err)
		}
		if err := revisions(&dockwellv1.ListServiceRevisionsRequest{Id: apiID}); err != nil {
			t.Errorf("their own service, by ID: %v", err)
		}
		if err := revisions(&dockwellv1.ListServiceRevisionsRequest{Id: "api"}); err != nil {
			t.Errorf("their own service, by name, in their one project: %v", err)
		}

		listed, err := services.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range listed.Msg.GetServices() {
			names = append(names, s.GetResource().GetName())
		}
		if !slices.Equal(names, []string{"api"}) {
			t.Errorf("their services list %v, want their project's alone", names)
		}
		changes, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range changes.Msg.GetChanges() {
			if ch.GetId() == web.GetId() || ch.GetId() == waiting.GetId() {
				t.Errorf("their changes list %s, of another project", ch.GetSummary())
			}
		}

		events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if len(events.Msg.GetEvents()) == 0 {
			t.Error("their events are empty, want their project's")
		}
		for _, e := range events.Msg.GetEvents() {
			if e.GetChange().GetId() == web.GetId() || e.GetKind() == dockwellv1.EventKind_EVENT_KIND_ACCESS {
				t.Errorf("their events hold %s %s, which isn't their project's", e.GetAction(), e.GetId())
			}
		}
		// A container Dockwell didn't make is read through its server, which is the install's.
		var server string
		_ = f.store.View(t.Context(), func(tx *store.Tx) error {
			all, err := tx.ListServers(t.Context())
			if len(all) > 0 {
				server = all[0].ID
			}
			return err
		})
		_, err = logClient(b, b.origin).ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Server: server, Id: "by-hand"}))
		notHere("a container's logs, by its server", err)

		_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: waiting.GetId()}))
		notHere("applying another project's change", err)
		_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: blogPlanned.GetId(), Fingerprint: blogPlanned.GetPlan().GetFingerprint()}))
		if r := reasonOf(err); r == "needs_role" || r == "not_found" {
			t.Errorf("applying their own project's change, outside production: %v", err)
		}

		// Production is protected by its name: applying there is an admin's.
		hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper}, store.Grant{ProjectID: envProject(t, f, production), Role: store.RoleDeveloper})
		_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: waiting.GetId()}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
			t.Errorf("a developer applying in production: %v, want needs_role", err)
		}
	})
}

// envProject is the project an environment is in.
func envProject(t *testing.T, f *fullServer, env string) string {
	t.Helper()
	var project string
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(t.Context(), env)
		project = r.Scope
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// An open stream checks what its request names on each recheck, against the
// grants read then, so it ends once that leaves the caller's reach, though they
// still hold a role somewhere else (ADR 0048).
func TestAStreamEndsWhenWhatItNamesLeavesReach(t *testing.T) {
	testRecheck = 50 * time.Millisecond
	t.Cleanup(func() { testRecheck = 0 })
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		var webID string
		for _, r := range web.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "service" {
				webID = r.GetId()
			}
		}
		shop := envProject(t, f, web.GetPlan().GetResources()[0].GetResource().GetScope())
		blog := apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		}))
		hold := aMember(t, f)
		hold(store.Grant{ProjectID: shop, Role: store.RoleViewer})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stream, err := eventClient(b, b.origin).WatchEvents(ctx, connect.NewRequest(&dockwellv1.WatchEventsRequest{ResourceId: webID}))
		if err != nil {
			t.Fatal(err)
		}
		ended := make(chan error, 1)
		go func() {
			for stream.Receive() {
			}
			ended <- stream.Err()
		}()
		time.Sleep(200 * time.Millisecond)
		select {
		case err := <-ended:
			t.Fatalf("watching web ended while its project was held: %v", err)
		default:
		}
		hold(store.Grant{ProjectID: blog.GetPlan().GetResources()[0].GetId(), Role: store.RoleViewer})
		select {
		case err := <-ended:
			if reasonOf(err) != "not_found" {
				t.Errorf("watching web ended with %v, want not_found once its project isn't held", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watching web is open 5 s after its project stopped being held")
		}
	})
}

// Opening an app by its name finds it in the caller's projects alone, as every
// call that takes a name does, and keeps to a protected environment's rule: a
// developer of one project doesn't open another's, and a developer of a project
// doesn't open what runs in its protected environment, by name or by ID. What
// was opened asks the same again while it stays open (ADR 0061).
func TestOpeningAnAppKeepsToItsProjectAndItsRule(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		f.addDockerServer(t)
		c := changeClient(b, b.origin)
		resourceOf := func(ch *dockwellv1.Change, kind string) string {
			t.Helper()
			for _, r := range ch.GetPlan().GetResources() {
				if r.GetResource().GetKind() == kind {
					return r.GetId()
				}
			}
			t.Fatalf("%s makes no %s", ch.GetSummary(), kind)
			return ""
		}
		web := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
		webID, production := resourceOf(web, "service"), web.GetPlan().GetResources()[0].GetResource().GetScope()
		blogID := resourceOf(apply(t, c, propose(t, c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		staging := resourceOf(apply(t, c, propose(t, c, "Add staging to blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: blogID, Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")
		apiID := resourceOf(apply(t, c, propose(t, c, "Run api", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Scope: staging, Name: "api"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
				Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:1.4"}},
			}}},
		})), "service")
		defaultProject := envProject(t, f, production)

		hold := aMember(t, f)
		services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
		open := func(req *dockwellv1.OpenServiceRequest) error {
			_, err := services.OpenService(t.Context(), connect.NewRequest(req))
			return err
		}
		notHere := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("%s: %v, want not found", what, err)
			}
		}
		// Reaching the handler, which finds it publishes nothing to open, is what
		// getting past the role checks looks like here.
		reached := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "not_published" {
				t.Errorf("%s: %v, want not_published from the handler", what, err)
			}
		}

		hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper})
		notHere("another project's service, by name alone", open(&dockwellv1.OpenServiceRequest{Id: "web"}))
		notHere("another project's service, by name in its project", open(&dockwellv1.OpenServiceRequest{Id: "web", Project: "default"}))
		notHere("another project's service, by ID", open(&dockwellv1.OpenServiceRequest{Id: webID}))
		reached("their own service, by name", open(&dockwellv1.OpenServiceRequest{Id: "api"}))
		reached("their own service, by ID", open(&dockwellv1.OpenServiceRequest{Id: apiID}))

		// Production is protected by its name: opening there is an admin's.
		hold(store.Grant{ProjectID: blogID, Role: store.RoleDeveloper}, store.Grant{ProjectID: defaultProject, Role: store.RoleDeveloper})
		for what, req := range map[string]*dockwellv1.OpenServiceRequest{
			"by name":            {Id: "web"},
			"by name in project": {Id: "web", Project: "default"},
			"by ID":              {Id: webID},
		} {
			if err := open(req); connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "needs_role" {
				t.Errorf("a developer opening in a protected environment, %s: %v, want needs_role", what, err)
			}
		}

		// While it's open, the same is asked again: a lost role, or protection, ends it.
		var person string
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			users, err := tx.ListUsers(t.Context())
			for _, u := range users {
				if u.ID != "usr_keeper" {
					person = u.ID
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		p := auth.Principal{User: store.User{ID: person}}
		still := func(serviceID string) error {
			return authapi.Recheck(f.auth, slog.New(slog.DiscardHandler), dockwellv1connect.ServiceServiceOpenServiceProcedure, p,
				&dockwellv1.OpenServiceRequest{Id: serviceID})(t.Context())
		}
		if err := still(apiID); err != nil {
			t.Errorf("a developer of blog, rechecked on api: %v", err)
		}
		if err := still(webID); reasonOf(err) != "needs_role" {
			t.Errorf("a developer of the default project, rechecked in its protected environment: %v, want needs_role", err)
		}
		hold(store.Grant{ProjectID: blogID, Role: store.RoleViewer})
		if err := still(apiID); reasonOf(err) != "needs_role" {
			t.Errorf("a viewer of blog, rechecked on api: %v, want needs_role", err)
		}
		hold(store.Grant{ProjectID: defaultProject, Role: store.RoleDeveloper})
		if err := still(apiID); err == nil {
			t.Error("someone who no longer holds blog, rechecked on api, may still open it")
		}
	})
}
