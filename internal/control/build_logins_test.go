// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// buildLogins are the registry logins a plan's build steps name.
func buildLogins(c *dockwellv1.Change) []string {
	var out []string
	for _, s := range c.GetPlan().GetServers() {
		for _, st := range s.GetSteps() {
			out = append(out, st.GetBuildLogins()...)
		}
	}
	return out
}

// named is a build of main that names the registry logins its base images pull with.
func (g *gitBench) named(logins ...string) *dockwellv1.Edit {
	e := g.buildEdit("main", nil)
	e.GetSpec().GetService().GetBuild().RegistryLogins = logins
	return e
}

// A Dockerfile whose base image its registry keeps private builds once an admin
// saves a login its project may use and the build names it: the plan names it,
// BuildKit's client is given it, the base image pulls with it, and its password is
// in no plan, change or log (ADR 0086). A build that names none is given none, as
// its Dockerfile could read any image a login can (security review of #591, H1). One
// its project may not use, or on the loopback, refuses the plan (H2).
func TestABuildsPrivateBaseImagePullsWithALogin(t *testing.T) {
	g := newGitBench(t, store.SQLite)
	g.e.Registry[privateImage] = dockertest.Image{Login: loginUser + ":" + loginPassword}
	reg := newPrivateRegistry(t)
	srv := g.f.lastDocker
	srv.stop()
	waitConnected(t, g.f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: reg.srv.URL}
	srv.run(t)
	waitConnected(t, g.f, srv.id.ServerID, true)
	g.repo.Commit("main", "Start from our own base", map[string]string{"Dockerfile": "FROM " + privateImage + " AS base\nFROM base\nRUN echo \"built on our base\"\n"})

	// Without a login, BuildKit can't pull the base image, and nothing runs.
	none := g.propose(t, g.buildEdit("main", nil))
	if got := buildLogins(none); len(got) != 0 {
		t.Errorf("with no login saved, the plan builds with %q", got)
	}
	failed := applied(t, g.c, none)
	if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(failed.GetError(), "pull access denied") && !strings.Contains(failed.GetError(), "failed to solve") {
		t.Fatalf("a build of a private base image with no login ended %v: %s", failed.GetState(), failed.GetError())
	}

	apply(t, g.c, propose(t, g.c, "Pull from GHCR", loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword})))
	// A build that names no login is given none, though its project may use one.
	if got := buildLogins(g.propose(t, g.buildEdit("main", nil))); len(got) != 0 {
		t.Errorf("a build that names no login plans with %q", got)
	}
	// One whose registry is on the loopback, which the build's steps share, is
	// refused.
	apply(t, g.c, propose(t, g.c, "A local registry", loginEdit("local", &resourcev1.RegistryLogin{Registry: "localhost:5000", Username: loginUser, Password: loginPassword})))
	if _, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Local", Edits: []*dockwellv1.Edit{g.named("local")}})); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("a build named a login on the loopback: %v", err)
	}
	planned := g.propose(t, g.named("ghcr-ci"))
	if got := buildLogins(planned); !slices.Equal(got, []string{"ghcr-ci"}) {
		t.Fatalf("the plan builds with %q, want ghcr-ci", got)
	}
	done := g.done(t, planned)
	for _, text := range []string{protojson.Format(planned), protojson.Format(done)} {
		if strings.Contains(text, loginPassword) {
			t.Errorf("a change holds the login's password: %s", text)
		}
	}
	if logins := g.e.SentLogins(); !slices.Contains(logins, privateImage+" as "+loginUser+":"+loginPassword) {
		t.Errorf("the base image didn't pull with the login: %q", logins)
	}
	web := g.service(t, "", "web")
	// A build login saved again between a plan and its apply makes the plan stale.
	g.repo.Commit("main", "Stale", map[string]string{"STALE": "1"})
	stale := g.named("ghcr-ci")
	stale.Resource.Scope = web.GetResource().GetScope()
	staled, err := g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Stale", Edits: []*dockwellv1.Edit{stale}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, g.c, propose(t, g.c, "Save it again", loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword})))
	if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: staled.Msg.GetChange().GetId(), Fingerprint: staled.Msg.GetChange().GetPlan().GetFingerprint()})); err == nil {
		if done := waitWatch(t, watch(t, g.c, staled.Msg.GetChange().GetId())).last(); done.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Error("a build applied after the login its base images pull with was saved again")
		}
	}
	b := g.revisions(t, web.GetId())[0].GetBuild()
	for _, l := range g.log(t, b.GetId()) {
		if strings.Contains(l, loginPassword) {
			t.Fatalf("the build's log holds the login's password: %q", l)
		}
	}
	running := g.e.Newest(docker.Project(web.GetResource().GetScope()), "web")
	if running != nil {
		for _, kv := range running.Config.Env {
			if strings.Contains(kv, loginPassword) || strings.HasPrefix(kv, "DOCKER_CONFIG") {
				t.Errorf("the build's login reached what runs: %s", kv)
			}
		}
	}

	// A login another project alone may use isn't this build's to name.
	project := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "project", Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{}}}}
	}
	made := apply(t, g.c, propose(t, g.c, "Another project", project("blog")))
	blog := ""
	for _, r := range made.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" {
			blog = r.GetId()
		}
	}
	narrowed := loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, TokenHost: "", Projects: []string{blog}})
	apply(t, g.c, propose(t, g.c, "Blog's alone", narrowed))
	g.repo.Commit("main", "Again", map[string]string{"README": "again"})
	edit := g.named("ghcr-ci")
	edit.Resource.Scope = web.GetResource().GetScope()
	_, err = g.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Build again", Edits: []*dockwellv1.Edit{edit}}))
	if err == nil || !strings.Contains(err.Error(), "no registry login ghcr-ci this app may use") || strings.Contains(err.Error(), blog) {
		t.Errorf("a build named a login another project alone may use: %v", err)
	}
}

// Railpack's builder and runtime images are Railpack's, so a build Railpack makes
// plans with no login, even one it names, and an apply checks none. (Security review
// of #591, L5.)
func TestARailpackBuildPlansWithNoLogin(t *testing.T) {
	g := newGitBench(t, store.SQLite)
	g.repo.Commit("main", "A Node app", nodeApp)
	apply(t, g.c, propose(t, g.c, "Pull from GHCR", loginEdit("ghcr-ci", &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: loginUser, Password: loginPassword})))
	planned := g.propose(t, g.named("ghcr-ci"))
	if d := buildStep(planned).GetDetection(); d.GetBuilder() != resourcev1.ImageBuild_BUILDER_RAILPACK {
		t.Fatalf("the plan builds with %v", d.GetBuilder())
	}
	if got := buildLogins(planned); len(got) != 0 {
		t.Errorf("a Railpack build plans with %q", got)
	}
	g.done(t, planned)
	for _, l := range g.e.GivenBuilders() {
		t.Errorf("a Railpack builder was given %q", l)
	}
}
