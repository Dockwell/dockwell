// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person approves a folder for the code that writes it (#252). A built service's
// code is its repository and how it builds, so a token that points it at another
// repository, or changes how it builds, as a build argument can name any image,
// reaches further than what was approved, and is asked. A new commit of its branch
// is the same code, as a new tag of an image is, and goes on as the token's.
func TestATokenChangingHowAnAppBuildsOverAFolderIsAsked(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile, "Dockerfile.debug": webDockerfile})
		stranger := g.git.Repo("ops")
		stranger.Commit("main", "Theirs", map[string]string{"Dockerfile": webDockerfile})
		data := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		edit := func(change func(*resourcev1.ImageBuild)) *dockwellv1.Edit {
			e := g.buildEdit("main", nil)
			app := e.GetSpec().GetService().GetApp()
			app.User = "1000"
			app.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: data}, Target: "/var/www"}}
			if change != nil {
				change(e.GetSpec().GetService().GetBuild())
			}
			return e
		}
		approved := g.propose(t, edit(nil))
		applyConfirmed(t, g.c, approved)
		writer := writtenBy(approved.GetPlan())
		if !strings.HasPrefix(writer, strings.TrimSuffix(strings.TrimPrefix(g.repo.URL, "http://"), ".git")+"@build:") {
			t.Fatalf("the folder is written by %q, not the repository's build", writer)
		}

		token := createToken(t, g.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		script := changeClient(bearer{token: token, rt: g.b.client.Transport}, g.b.origin)
		for what, change := range map[string]func(*resourcev1.ImageBuild){
			"another repository": func(b *resourcev1.ImageBuild) { b.GetGit().Url = stranger.URL },
			"a build argument": func(b *resourcev1.ImageBuild) {
				b.Args = map[string]string{"NODE_ENV": "production", "BASE": "ghcr.io/stranger/ops:1.0"}
			},
			"another Dockerfile": func(b *resourcev1.ImageBuild) { b.Dockerfile = "Dockerfile.debug" },
		} {
			planned := proposeEdit(t, script, edit(change))
			tokenRefused(t, script, planned, "web")
			if got := writtenBy(planned.GetPlan()); got == writer || !strings.Contains(got, "@build:") {
				t.Errorf("%s: the plan says the folder is written by %q, which was approved as %q", what, got, writer)
			}
			if _, err := g.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.GetId()})); err != nil {
				t.Fatal(err)
			}
		}

		// A new commit of the branch goes on as the token's.
		g.repo.Commit("main", "Second", map[string]string{"Dockerfile": webDockerfile, "Dockerfile.debug": webDockerfile, "app.js": "2"})
		services := dockwellv1connect.NewServiceServiceClient(bearer{token: token, rt: g.b.client.Transport}, g.b.origin, connect.WithProtoJSON())
		res, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "web"}))
		if err != nil {
			t.Fatal(err)
		}
		if planned := res.Msg.GetChange(); planned.GetPlan().GetHostAccess() {
			t.Errorf("a new commit of the branch needs a person: %v", planned.GetPlan().GetResources())
		} else if got := apply(t, script, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Errorf("a token's deploy of a new commit ended %v: %s", got.GetState(), got.GetError())
		}
	})
}

// writtenBy is who a plan says writes a read-write server path.
func writtenBy(plan *dockwellv1.Plan) string {
	for _, r := range plan.GetResources() {
		if i := slices.IndexFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH && !a.GetReadOnly()
		}); i >= 0 {
			return r.GetHostAccess()[i].GetWrittenBy()
		}
	}
	return ""
}
