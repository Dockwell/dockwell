// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The engine checks each item of a change against the role of whoever acts,
// wherever the change came from: planning needs a developer in reach, applying
// in a protected environment needs an admin, discarding someone else's plan
// needs what applying would, and what belongs to no project, a project
// included, is an admin's (ADR 0048, check 2; ADR 0049).
func TestTheEngineChecksTheRolesOfEachItem(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		e.RequireRoles()
		ctx := t.Context()
		var org, project, production string
		_ = db.View(ctx, func(tx *store.Tx) error {
			tr, err := scope.Load(ctx, tx)
			if err != nil || len(tr.Projects) == 0 {
				t.Fatalf("the default project: %v", err)
			}
			org, project, production = tr.Organization.ID, tr.Projects[0].ID, tr.Projects[0].Environments[0].ID
			return nil
		})
		as := func(user string, grants ...store.Grant) context.Context {
			return place.WithCaller(ctx, place.Caller{UserID: user, Grants: grants})
		}
		mira := as("usr_mira", store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleOwner})
		ada := as("usr_ada", store.Grant{UserID: "usr_ada", ProjectID: project, Role: store.RoleDeveloper})
		stranger := as("usr_sam", store.Grant{UserID: "usr_sam", ProjectID: "prj_01JD6Z3R8M9QF2K7T4XW5BNCAA", Role: store.RoleDeveloper})
		value := func(env, name string) Edit {
			return Edit{Ref: resource.Ref{Kind: "variable", Scope: env, Name: name}, Spec: &resourcev1.Variable{Value: "1"}}
		}
		plan := func(who context.Context, actor string, edits ...Edit) (*Plan, error) {
			return e.Propose(who, Proposal{Summary: "Change", Actor: actor, Edits: edits})
		}
		mustPlan := func(who context.Context, actor string, edits ...Edit) *Plan {
			t.Helper()
			p, err := plan(who, actor, edits...)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		staging := mustPlan(mira, "user:usr_mira", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{}})
		if err := e.ApplyBy(mira, staging.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		var stagingID string
		for _, r := range staging.Resources {
			stagingID = r.ID
		}

		// Planning in protected production, a developer's change waits: applying it is an admin's.
		waits := mustPlan(ada, "user:usr_ada", value(production, "A"))
		if err := e.ApplyBy(ada, waits.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer applying in protected production: %v, want ErrNeedsRole", err)
		}
		// In open staging, theirs applies.
		open := mustPlan(ada, "user:usr_ada", value(stagingID, "A"))
		if err := e.ApplyBy(ada, open.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer applying in open staging: %v", err)
		}
		// Someone else's plan in production a developer can't discard; their own, they can.
		miras := mustPlan(mira, "user:usr_mira", value(production, "B"))
		if err := e.Discard(ada, miras.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer discarding the owner's plan in production: %v, want ErrNeedsRole", err)
		}
		if err := e.Discard(ada, waits.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer discarding their own plan in production: %v", err)
		}
		if err := e.ApplyBy(mira, miras.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Errorf("the owner applying their own change in production: %v", err)
		}

		// A plan gone stale can still be discarded.
		stale := mustPlan(ada, "user:usr_ada", value(stagingID, "S"))
		moved := mustPlan(mira, "user:usr_mira", value(stagingID, "S"))
		if err := e.ApplyBy(mira, moved.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		if err := e.Discard(ada, stale.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer discarding their plan that went stale: %v", err)
		}

		// Out of reach is not found; a project, or a container on a server alone, is an admin's.
		if _, err := plan(stranger, "user:usr_sam", value(production, "C")); !errors.Is(err, ErrOutOfReach) {
			t.Errorf("someone of another project planning in default: %v, want ErrOutOfReach", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, Spec: &resourcev1.Project{}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer making a project: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "tool"}, Spec: &resourcev1.Container{Image: "ghcr.io/acme/tool:1.0"}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer running a container on a server alone: %v, want ErrNeedsRole", err)
		}
		// Changing whether an environment is protected is an admin's, either way.
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_PROTECTED}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer protecting staging: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ada, "user:usr_ada", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "production"}, Spec: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_OPEN}}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer opening production: %v, want ErrNeedsRole", err)
		}
		// A person's change with no caller is refused; Dockwell's own isn't checked.
		if _, err := plan(ctx, "user:usr_ada", value(stagingID, "D")); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a person's change with no caller: %v, want ErrNeedsRole", err)
		}
		if _, err := plan(ctx, SystemActor, value(stagingID, "D")); err != nil {
			t.Errorf("Dockwell's own change: %v", err)
		}
	})
}

// A plan with host access reaches the server every project on it shares, so
// it's an admin's to apply, even in an open environment of the developer's own
// project (ADR 0048).
func TestHostAccessIsAnAdminsToApplyInTheEngine(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		engines["srv_A"].AddVolume(docker.Volume{Name: "shared", Driver: "local"})
		e.RequireRoles()
		ctx := t.Context()
		project := envProjectOf(t, db, production(t, db))
		as := func(user string, grants ...store.Grant) context.Context {
			return place.WithCaller(ctx, place.Caller{UserID: user, Grants: grants})
		}
		mira := as("usr_mira", store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleOwner})
		ada := as("usr_ada", store.Grant{UserID: "usr_ada", ProjectID: project, Role: store.RoleDeveloper})
		made, err := e.Propose(mira, Proposal{Summary: "Add staging", Actor: "user:usr_mira", Edits: []Edit{{
			Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.ApplyBy(mira, made.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		staging := made.Resources[0].ID
		mounts, err := e.Propose(ada, Proposal{Summary: "Mount", Actor: "user:usr_ada", Edits: []Edit{webAt(staging, "/srv/x", "srv_A")}})
		if err != nil {
			t.Fatal(err)
		}
		if !mounts.HostAccess() {
			t.Fatalf("a server path planned without host access: %+v", mounts.Resources)
		}
		if err := e.ApplyBy(ada, mounts.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer applying a server path in open staging: %v, want ErrNeedsRole", err)
		}
		if err := e.ApplyBy(mira, mounts.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Errorf("the owner applying it: %v", err)
		}
		// A volume Dockwell didn't make may be another project's data, such as a
		// taken-on stack's, so it waits for an admin, until its environment
		// mounts it: then a developer's next service may too.
		shared := func(name string) Edit {
			return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: staging, Name: name}, Servers: []string{"srv_A"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "shared"}, External: true, Target: "/shared"}},
			}}}}
		}
		outside, err := e.Propose(ada, Proposal{Summary: "Share", Actor: "user:usr_ada", Edits: []Edit{shared("worker")}})
		if err != nil {
			t.Fatal(err)
		}
		if !outside.HostAccess() {
			t.Fatalf("a volume Dockwell didn't make planned without host access: %+v", outside.Resources)
		}
		err = e.ApplyBy(ada, outside.Change, By{Actor: "user:usr_ada"})
		if !errors.Is(err, ErrNeedsRole) || !strings.Contains(err.Error(), "mounts the volume shared, which Dockwell didn't make and nothing in its environment mounts yet") {
			t.Errorf("a developer applying a volume Dockwell didn't make that nothing in staging mounts: %v, want ErrNeedsRole", err)
		}
		if err := e.ApplyBy(mira, outside.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatalf("the owner applying it: %v", err)
		}
		another, err := e.Propose(ada, Proposal{Summary: "Share again", Actor: "user:usr_ada", Edits: []Edit{shared("reader")}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.ApplyBy(ada, another.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Errorf("a developer mounting a volume staging mounts already: %v", err)
		}
	})
}

// Whoever changes what runs under host access an admin gave gets what it
// reaches, so that's an admin's to apply too (ADR 0049): under what's as good
// as root, any change to what runs, another tag of its image included; under
// other such access, another repository, command or entrypoint. Another tag of
// the same repository there is the ordinary deploy, a developer's.
func TestWhatRunsUnderHostAccessIsAnAdminsToChange(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		e.RequireRoles()
		engines["srv_A"].Registry["busybox:1.36"] = dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}}
		ctx := t.Context()
		project := envProjectOf(t, db, production(t, db))
		as := func(user string, grants ...store.Grant) context.Context {
			return place.WithCaller(ctx, place.Caller{UserID: user, Grants: grants})
		}
		mira := as("usr_mira", store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleOwner})
		ada := as("usr_ada", store.Grant{UserID: "usr_ada", ProjectID: project, Role: store.RoleDeveloper})
		mustApply := func(who context.Context, actor string, edit Edit) *Plan {
			t.Helper()
			p, err := e.Propose(who, Proposal{Summary: "Change", Actor: actor, Edits: []Edit{edit}})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ApplyBy(who, p.Change, By{Actor: actor}); err != nil {
				t.Fatal(err)
			}
			return p
		}
		staging := mustApply(mira, "user:usr_mira", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: project, Name: "staging"}, Spec: &resourcev1.Environment{}}).Resources[0].ID
		svc := func(name string, c *resourcev1.Container) Edit {
			return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: staging, Name: name}, Servers: []string{"srv_A"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}}
		}
		path := func(p string, readOnly bool) []*resourcev1.Mount {
			return []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/data", ReadOnly: readOnly}}
		}
		for ki, k := range []struct {
			name, svc string
			given     *resourcev1.Container
			root      bool
			words     string
		}{
			{"a read-only server path", "reads", &resourcev1.Container{Image: "nginx:1.27", Mounts: path("/srv/shared", true)}, false, "reaches the server path /srv/shared"},
			{"a server path", "writes", &resourcev1.Container{Image: "nginx:1.27", Mounts: path("/srv/shared-rw", false)}, false, "reaches the server path /srv/shared-rw"},
			{"a capability", "admin-net", &resourcev1.Container{Image: "nginx:1.27", CapAdd: []string{"NET_ADMIN"}}, false, "runs with more than Docker gives a container"},
			{"Docker's socket", "socket", &resourcev1.Container{Image: "nginx:1.27", Mounts: path("/var/run/docker.sock", false)}, true, "as much as root"},
		} {
			name := k.svc
			mustApply(mira, "user:usr_mira", svc(name, k.given))
			for _, change := range []struct {
				what  string
				edit  func(c *resourcev1.Container)
				waits bool
				pull  bool
			}{
				{"another repository", func(c *resourcev1.Container) { c.Image = "busybox:1.36" }, true, false},
				{"another command", func(c *resourcev1.Container) { c.Command = []string{"sh", "-c", "id"} }, true, false},
				{"another health-check command", func(c *resourcev1.Container) {
					c.Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}
				}, true, false},
				{"another entrypoint", func(c *resourcev1.Container) { c.Entrypoint = []string{"/bin/sh"} }, true, false},
				{"another tag of its repository", func(c *resourcev1.Container) { c.Image = "nginx:1.28" }, k.root, false},
				{"an environment variable", func(c *resourcev1.Container) { c.Env = map[string]string{"MODE": "fast"} }, k.root, false},
				{"the same tag pulled again", func(*resourcev1.Container) {}, k.root, true},
			} {
				c := proto.Clone(k.given).(*resourcev1.Container)
				change.edit(c)
				edit := svc(name, c)
				edit.Pull, edit.Run = change.pull, change.pull
				before := engines["srv_A"].Newest(docker.Project(staging), name).ID
				newDigest := ""
				if change.pull {
					newDigest = "sha256:" + strings.Repeat("abcd"[ki:ki+1], 64)
					engines["srv_A"].Registry[c.Image] = dockertest.Image{Digest: newDigest, Config: docker.Config{Cmd: []string{"nginx"}}}
				}
				p, err := e.Propose(ada, Proposal{Summary: change.what, Actor: "user:usr_ada", Edits: []Edit{edit}})
				if err != nil {
					t.Fatalf("a developer planning %s under %s: %v", change.what, k.name, err)
				}
				err = e.ApplyBy(ada, p.Change, By{Actor: "user:usr_ada"})
				switch {
				case change.waits && (!errors.Is(err, ErrNeedsRole) || !strings.Contains(err.Error(), k.words)):
					t.Errorf("a developer applying %s under %s: %v, want ErrNeedsRole saying it %s", change.what, k.name, err, k.words)
				case !change.waits && err != nil:
					t.Errorf("a developer applying %s under %s: %v, want it applied", change.what, k.name, err)
				}
				if change.pull {
					running := engines["srv_A"].Newest(docker.Project(staging), name)
					if change.waits {
						if running.ID != before {
							t.Error("pulling the same tag replaced the root-equivalent container without an admin")
						}
					} else {
						img, inspectErr := engines["srv_A"].Client().InspectImage(ctx, running.Image)
						if inspectErr != nil || img.Digest(c.Image) != newDigest || running.ID == before {
							t.Errorf("the permitted pull did not run the new image under %s: %v", k.name, inspectErr)
						}
					}
				}
				switch {
				case change.waits:
					if err := e.Discard(ada, p.Change, By{Actor: "user:usr_ada"}); err != nil {
						t.Fatal(err)
					}
				default:
					mustApply(mira, "user:usr_mira", svc(name, k.given)) // back to what the owner gave it
				}
			}
			// What's kept asks no passkey and no warning, so the owner's own
			// change under it plans as before: under root, as reaching it anew
			// (ADR 0032); under the rest, as kept alone.
			c := proto.Clone(k.given).(*resourcev1.Container)
			c.Command = []string{"sh"}
			owners, err := e.Propose(mira, Proposal{Summary: "Owner's", Actor: "user:usr_mira", Edits: []Edit{svc(name, c)}})
			if err != nil {
				t.Fatal(err)
			}
			if got := owners.HostAccess(); got != k.root || owners.AsGoodAsRoot() != k.root {
				t.Errorf("a new command under %s planned with host access %v, want %v: %+v", k.name, got, k.root, owners.Resources)
			}
			if kept := owners.Resources[0].Keeps; len(kept) == 0 == !k.root {
				t.Errorf("a new command under %s keeps %v: %+v", k.name, kept, owners.Resources)
			}
			if err := e.ApplyBy(mira, owners.Change, By{Actor: "user:usr_mira"}); err != nil {
				t.Errorf("the owner applying a new command under %s: %v", k.name, err)
			}
		}
	})
}

// A built image keeps its service's name whatever it is built from, so under
// access an admin gave, a build of another Git repository, or with another
// build or start command, starts something else, as another image repository
// does (ADR 0046). Another commit, branch or Dockerfile of the same repository
// is the ordinary deploy.
func TestABuildOfAnotherRepositoryStartsSomethingElse(t *testing.T) {
	built := func(edit func(b *resourcev1.ImageBuild)) *resourcev1.Service {
		b := &resourcev1.ImageBuild{
			Source:     &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: "https://git.example.com/acme/web.git", Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}},
			Dockerfile: "Dockerfile",
		}
		if edit != nil {
			edit(b)
		}
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "dockwell.invalid/svc_web:" + b.GetGit().GetCommit()[:12]}}, Build: b}
	}
	given := built(nil)
	for _, c := range []struct {
		what  string
		now   *resourcev1.Service
		other bool
	}{
		{"another commit", built(func(b *resourcev1.ImageBuild) { b.GetGit().Commit = strings.Repeat("b", 40) }), false},
		{"another branch", built(func(b *resourcev1.ImageBuild) { b.GetGit().Ref = "refs/heads/next" }), false},
		{"another Dockerfile of the repository", built(func(b *resourcev1.ImageBuild) { b.Dockerfile = "deploy/Dockerfile" }), false},
		{"another repository", built(func(b *resourcev1.ImageBuild) { b.GetGit().Url = "https://git.example.com/someone/else.git" }), true},
		{"another start command", built(func(b *resourcev1.ImageBuild) { b.StartCommand = "sh -c id" }), true},
		{"another build command", built(func(b *resourcev1.ImageBuild) { b.BuildCommand = "sh -c id" }), true},
	} {
		if got := startsSomethingElse(given.GetApp().GetImage(), c.now.GetApp().GetImage(), given, c.now); got != c.other {
			t.Errorf("a build of %s starts something else: %v, want %v", c.what, got, c.other)
		}
	}
	// From an image to a build of the service's own, and back, is another repository already.
	image := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}
	if !startsSomethingElse("nginx:1.27", given.GetApp().GetImage(), image, given) || !startsSomethingElse(given.GetApp().GetImage(), "nginx:1.27", given, image) {
		t.Error("an image that becomes a build, or a build an image, doesn't start something else")
	}
}

// An item's environment is protected if it is so before the change or after
// it: a new production protects what's made in it, and deleting an empty
// production is protected as it was. One from before protection was written,
// brought back by an undo, reads as its name has it (ADR 0049).
func TestProtectionIsCheckedBeforeAndAfter(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setup(t, b, "srv_A")
		e.RequireRoles()
		ctx := t.Context()
		var org string
		_ = db.View(ctx, func(tx *store.Tx) error {
			tr, err := scope.Load(ctx, tx)
			org = tr.Organization.ID
			return err
		})
		as := func(user string, grants ...store.Grant) context.Context {
			return place.WithCaller(ctx, place.Caller{UserID: user, Grants: grants})
		}
		mira := as("usr_mira", store.Grant{UserID: "usr_mira", ProjectID: store.Install, Role: store.RoleOwner})
		plan := func(who context.Context, actor string, edits ...Edit) *Plan {
			t.Helper()
			p, err := e.Propose(who, Proposal{Summary: "Change", Actor: actor, Edits: edits})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		applied := func(who context.Context, actor string, edits ...Edit) *Plan {
			t.Helper()
			p := plan(who, actor, edits...)
			if err := e.ApplyBy(who, p.Change, By{Actor: actor}); err != nil {
				t.Fatal(err)
			}
			return p
		}
		// shop starts with staging alone, then gets a production from before
		// protection was written, as an undo makes one: nothing written.
		shop := "prj_01JD6Z3R8M9QF2K7T4XW5BNC5H"
		applied(mira, "user:usr_mira",
			Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: "shop"}, ID: shop, Spec: &resourcev1.Project{}},
			Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shop, Name: "staging"}, Spec: &resourcev1.Environment{}})
		old := applied(mira, "user:usr_mira", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shop, Name: "production"}, Spec: &resourcev1.Environment{}, Reverses: true})
		if p := old.Resources[0]; p.Ref.Name != "production" {
			t.Fatalf("made %v", p.Ref)
		}
		gone := applied(mira, "user:usr_mira", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shop, Name: "production"}})
		ada := as("usr_ada", store.Grant{UserID: "usr_ada", ProjectID: shop, Role: store.RoleDeveloper})

		// A developer brings it back: theirs to plan, as it reads protected by its
		// name, not made open; applying is an admin's.
		back, err := e.Undo(ada, gone.Change, By{Actor: "user:usr_ada"})
		if err != nil {
			t.Fatalf("a developer undoing the deletion of a production with no protection written: %v", err)
		}
		if err := e.ApplyBy(ada, back.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) || !strings.Contains(err.Error(), "in a protected environment") {
			t.Errorf("a developer applying it: %v, want ErrNeedsRole for a protected environment", err)
		}
		if err := e.Discard(ada, back.Change, By{Actor: "user:usr_ada"}); err != nil {
			t.Fatal(err)
		}

		// A new production protects what's made in it in the same change.
		env := old.Resources[0].ID // made again, it keeps its ID
		made := plan(ada, "user:usr_ada",
			Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shop, Name: "production"}, Spec: &resourcev1.Environment{}},
			Edit{Ref: resource.Ref{Kind: "variable", Scope: env, Name: "X"}, Spec: &resourcev1.Variable{Value: "1"}})
		if err := e.ApplyBy(ada, made.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer applying a new production with a variable in it: %v, want ErrNeedsRole", err)
		}
		if err := e.ApplyBy(mira, made.Change, By{Actor: "user:usr_mira"}); err != nil {
			t.Fatal(err)
		}
		// Deleting it is protected as it was, though nothing's left after.
		emptied := plan(ada, "user:usr_ada",
			Edit{Ref: resource.Ref{Kind: "variable", Scope: env, Name: "X"}},
			Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: shop, Name: "production"}})
		if err := e.ApplyBy(ada, emptied.Change, By{Actor: "user:usr_ada"}); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a developer deleting production and what's in it: %v, want ErrNeedsRole", err)
		}
	})
}

// envProjectOf is the project an environment is in.
func envProjectOf(t *testing.T, db *store.DB, env string) string {
	t.Helper()
	var project string
	if err := db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(t.Context(), env)
		project = r.Scope
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return project
}

// What a plan reaches on its server is an admin's to give where ADR 0048 lists
// it, and a person's to approve, in a project they may apply in, where it
// doesn't.
func TestOnlyWhatADR0048ListsIsAnAdminsToGive(t *testing.T) {
	for _, tt := range []struct {
		access resource.Access
		admins bool
	}{
		{resource.Access{Kind: resource.AccessPath, Path: "/srv/x", ReadOnly: true}, true},
		{resource.Access{Kind: resource.AccessDocker, Path: "/var/run/docker.sock"}, true},
		{resource.Access{Kind: resource.AccessNetwork}, true},
		{resource.Access{Kind: resource.AccessMount}, true},
		{resource.Access{Kind: resource.AccessNetworkDriver, Driver: "macvlan"}, true},
		{resource.Access{Kind: resource.AccessExternalNetwork, Path: "legacy"}, true},
		{resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN"}, true},
		{resource.Access{Kind: resource.AccessGroup, Path: "disk"}, true},
		{resource.Access{Kind: resource.AccessPrivileged}, true},
		{resource.Access{Kind: resource.AccessTrustedProxy, Path: "10.0.0.0/8"}, true},
		{resource.Access{Kind: resource.AccessPanelDomain, Path: "panel.example.com"}, true},
		{resource.Access{Kind: resource.AccessServerPort, Path: "127.0.0.1:8080"}, true},
		{resource.Access{Kind: resource.AccessTakesOn, Path: "legacy-db"}, true},
		{resource.Access{Kind: resource.AccessBackupOf, Path: "/etc"}, true},
		{resource.Access{Kind: resource.AccessDomain, Path: "shop.example.com", Detail: "staging"}, false},
		{resource.Access{Kind: resource.AccessPlainForAnyone, Path: "shop.example.com"}, false},
		{resource.Access{Kind: resource.AccessHTTPSOff, Path: "shop.example.com"}, false},
		{resource.Access{Kind: resource.AccessOpened, Path: "shop.example.com/admin", Detail: "lets in 198.51.100.0/24"}, false},
		{resource.Access{Kind: resource.AccessExternalVolume, Path: "shared"}, false},
	} {
		if got := installWide(tt.access) != ""; got != tt.admins {
			t.Errorf("%v: an admin's %v, want %v", tt.access, got, tt.admins)
		}
	}
}
