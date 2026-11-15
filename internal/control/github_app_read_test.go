// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// The new app sheet reads a private repository as the project's builds fetch it:
// its branches and how it builds, through the installation that reaches the
// project, with nothing to pick. A project the installation doesn't reach reads
// nothing private through it, named or not, and a developer reads only for their
// own projects.
func TestAProjectReadsAPrivateRepositoryThroughTheGitHubApp(t *testing.T) {
	t.Parallel()
	a := newAppBench(t)
	ctx := t.Context()
	defaultID := envProject(t, a.f, a.web.GetResource().GetScope())
	url := a.repo.URL

	// The installation reaches the default project alone.
	narrow, err := a.c.ProposeChange(ctx, connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Narrow acme", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "github.app", Name: a.app.GetName()},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_GithubApp{GithubApp: &resourcev1.GitHubApp{
			Reaches: []*resourcev1.GitHubInstallationReach{{InstallationId: 7, Projects: []string{defaultID}}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	a.done(t, narrow.Msg.GetChange())
	blogID := resourceIn(t, apply(t, a.c, propose(t, a.c, "Make blog", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "project", Name: "blog"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}},
	})), "project")

	branches := func(project string) (*dockwellv1.ListRepositoryBranchesResponse, error) {
		res, err := a.builds.ListRepositoryBranches(ctx, connect.NewRequest(&dockwellv1.ListRepositoryBranchesRequest{Url: url, Project: project}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	detect := func(project, login string) (*dockwellv1.DetectBuildResponse, error) {
		b := a.buildEdit("main", nil).GetSpec().GetService().GetBuild()
		b.GitLogin = login
		res, err := a.builds.DetectBuild(ctx, connect.NewRequest(&dockwellv1.DetectBuildRequest{Build: b, Project: project}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	// For no project, it reads as the public do, and a private repository refuses.
	if _, err := branches(""); err == nil {
		t.Errorf("the private repository's branches read for no project")
	}
	// For the default project, through the installation, with nothing named.
	got, err := branches(defaultID)
	if err != nil || got.GetGitLogin() != "dockwell-deploy/7" || !slices.Contains(got.GetBranches(), "main") {
		t.Fatalf("the default project reads the repository's branches as %v, %v", got, err)
	}
	if d, err := detect("default", ""); err != nil || d.GetDetection() == nil {
		t.Errorf("the default project reads how the private repository builds as %v, %v", d, err)
	}
	if res, err := a.builds.DetectBuild(ctx, connect.NewRequest(&dockwellv1.DetectBuildRequest{Id: a.web.GetId()})); err != nil || res.Msg.GetDetection() == nil {
		t.Errorf("web's build reads as %v, %v", res, err)
	}
	// A login named for no project is refused before anything is read.
	if _, err := detect("", "dockwell-deploy/7"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a reading through an installation for no project = %v", err)
	}

	// blog isn't the installation's: nothing private reads for it, named or not.
	if _, err := branches(blogID); err == nil {
		t.Errorf("blog reads the private repository's branches")
	}
	if _, err := detect(blogID, "dockwell-deploy/7"); err == nil || !strings.Contains(err.Error(), "doesn't reach this project") {
		t.Errorf("blog reads how the private repository builds through acme: %v", err)
	}

	// A developer of blog reads for blog alone.
	hold := aMember(t, a.f)
	hold(in(blogID, store.RoleDeveloper))
	// Another project is out of reach, as if there were none.
	if _, err := branches(defaultID); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a developer of blog lists branches for the default project: %v", err)
	}
	if _, err := detect(defaultID, ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a developer of blog reads a build for the default project: %v", err)
	}
	if _, err := branches(blogID); err == nil {
		t.Errorf("a developer of blog reads the private repository's branches")
	}
	// The default project's developer reads it through the installation.
	hold(in(defaultID, store.RoleDeveloper))
	if got, err := branches(defaultID); err != nil || got.GetGitLogin() != "dockwell-deploy/7" {
		t.Errorf("a developer of the default project reads the branches as %v, %v", got, err)
	}
	// They see the App by name and state: not its key, its settings, or which
	// projects its installations reach.
	listed, err := a.gp.ListGitProviders(ctx, connect.NewRequest(&dockwellv1.ListGitProvidersRequest{}))
	if err != nil || len(listed.Msg.GetApps()) != 1 {
		t.Fatalf("a developer lists the Apps as %v, %v", listed, err)
	}
	app := listed.Msg.GetApps()[0]
	if app.GetName() != a.app.GetName() || app.GetDelivery() == nil || len(app.GetInstallations()) != 1 || app.GetInstallations()[0].GetAccount() != "acme" {
		t.Errorf("a developer sees too little of the App: %v", app)
	}
	if app.GetKeyFingerprint() != "" || app.GetSettingsUrl() != "" || len(app.GetInstallations()[0].GetProjects()) != 0 {
		t.Errorf("a developer sees the App's key, settings or the projects it reaches: %v", app)
	}
}
