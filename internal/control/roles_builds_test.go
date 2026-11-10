// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A build is its service's, in its project (ADR 0049): a viewer of the project
// reads it and its log, and nothing of another project's, by ID or by its
// service. Stopping one is a developer's where the environment is open and an
// admin's where it is protected, as applying is, and reading how a repository
// builds is a developer's.
func TestBuildsStayWithinTheirProjects(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		ctx := t.Context()
		g.repo.Commit("main", "Add order export endpoint", map[string]string{"Dockerfile": webDockerfile, "app.js": "1"})

		// One in the default project's production, which is protected.
		theirs := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		theirsID, production := resourceIn(t, theirs, "service"), theirs.GetPlan().GetResources()[0].GetResource().GetScope()
		defaultID := envProject(t, g.f, production)
		// And one in blog's staging, which is open.
		blogID := resourceIn(t, apply(t, g.c, propose(t, g.c, "Make blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
		})), "project")
		staging := resourceIn(t, apply(t, g.c, propose(t, g.c, "Add staging to blog", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: blogID, Name: "staging"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
		})), "environment")
		edit := g.buildEdit("main", nil)
		edit.Resource.Scope = staging
		edit.GetSpec().GetService().GetApp().Ports[0].HostPort = 8081
		oursID := resourceIn(t, g.done(t, g.propose(t, edit)), "service")

		buildOf := func(service string) *dockwellv1.Build {
			t.Helper()
			res, err := g.builds.ListBuilds(ctx, connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: service}))
			if err != nil || len(res.Msg.GetBuilds()) != 1 {
				t.Fatalf("the builds of %s read %v, %v", service, res, err)
			}
			return res.Msg.GetBuilds()[0]
		}
		ours, other := buildOf(oursID), buildOf(theirsID)
		refused := func(what string, err error) {
			t.Helper()
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s: %v, want it refused for the role", what, err)
			}
		}
		cancel := func(id string) error {
			_, err := g.builds.CancelBuild(ctx, connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: id}))
			return err
		}
		detect := func() error {
			_, err := g.builds.DetectBuild(ctx, connect.NewRequest(&dockwellv1.DetectBuildRequest{Id: oursID}))
			return err
		}

		hold := aMember(t, g.f)
		hold(in(blogID, store.RoleViewer))
		if got := buildOf(oursID); got.GetId() != ours.GetId() {
			t.Errorf("a viewer of blog lists %v for its service, not its build", got)
		}
		if res, err := g.builds.GetBuild(ctx, connect.NewRequest(&dockwellv1.GetBuildRequest{Id: ours.GetId()})); err != nil || res.Msg.GetBuild().GetId() != ours.GetId() {
			t.Errorf("a viewer of blog reads its build as %v, %v", res, err)
		}
		if res, err := g.builds.ReadBuildLog(ctx, connect.NewRequest(&dockwellv1.ReadBuildLogRequest{Id: ours.GetId()})); err != nil || len(res.Msg.GetLines()) == 0 {
			t.Errorf("a viewer of blog reads its build's log as %v, %v", res, err)
		}
		stream, err := g.builds.WatchBuild(ctx, connect.NewRequest(&dockwellv1.WatchBuildRequest{Id: ours.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if !stream.Receive() || stream.Msg().GetBuild().GetId() != ours.GetId() || len(stream.Msg().GetLines()) == 0 {
			t.Errorf("a viewer of blog watches its build as %v, %v", stream.Msg(), stream.Err())
		}
		_ = stream.Close()
		_, err = g.builds.ListBuilds(ctx, connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: theirsID}))
		notFound(t, "listing another project's service's builds", err)
		_, err = g.builds.GetBuild(ctx, connect.NewRequest(&dockwellv1.GetBuildRequest{Id: other.GetId()}))
		notFound(t, "reading another project's build", err)
		_, err = g.builds.ReadBuildLog(ctx, connect.NewRequest(&dockwellv1.ReadBuildLogRequest{Id: other.GetId()}))
		notFound(t, "reading another project's build's log", err)
		gone, err := g.builds.WatchBuild(ctx, connect.NewRequest(&dockwellv1.WatchBuildRequest{Id: other.GetId()}))
		if err == nil {
			gone.Receive()
			err = gone.Err()
			_ = gone.Close()
		}
		notFound(t, "watching another project's build", err)
		refused("a viewer stopping a build", cancel(ours.GetId()))
		refused("a viewer reading how a repository builds", detect())

		hold(in(blogID, store.RoleDeveloper))
		// It has ended, which is what a developer who may stop it is told.
		if err := cancel(ours.GetId()); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("a developer of blog stopping its ended build: %v, want that it has ended", err)
		}
		notFound(t, "a developer of blog stopping another project's build", cancel(other.GetId()))
		if err := detect(); err != nil {
			t.Errorf("a developer of blog reading how its service builds: %v", err)
		}

		hold(in(defaultID, store.RoleDeveloper))
		refused("a developer stopping a build in a protected environment", cancel(other.GetId()))
		if res, err := g.builds.GetBuild(ctx, connect.NewRequest(&dockwellv1.GetBuildRequest{Id: other.GetId()})); err != nil || res.Msg.GetBuild().GetId() != other.GetId() {
			t.Errorf("a developer of the default project reads its build as %v, %v", res, err)
		}
	})
}
