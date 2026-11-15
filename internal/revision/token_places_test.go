// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
)

// A link from blog to a database server in another project's protected
// environment, for the role checks of what's done there for blog's link.
type linkedBlog struct {
	x       *restoreBench
	blog    string
	blogEnv string
	forBlog resource.Ref
}

func withLinkedBlog(t *testing.T) linkedBlog {
	t.Helper()
	x := withVolume(t, store.SQLite)
	e, env, db := x.e, x.env, x.db
	e.kinds = e.kinds.With(link.Type{})
	e.sealer = secretstest.Keeper(db, e.kinds)
	withData(t, e, env)
	apply(t, e, propose(t, e, "Add blog", Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: tree(t, db).Organization.ID, Name: "blog"}, Spec: &resourcev1.Project{}}))
	_, blogEnv, _ := tree(t, db).Resolve("blog", "")
	blog := envProjectOf(t, db, blogEnv.ID)
	apply(t, e, propose(t, e, "Open blog", Edit{Ref: resource.Ref{Kind: scope.EnvironmentKind, Scope: blog, Name: "production"}, Spec: &resourcev1.Environment{Protection: resourcev1.Protection_PROTECTION_OPEN}}))
	apply(t, e, propose(t, e, "Link blog", Edit{Ref: resource.Ref{Kind: link.Kind, Scope: blogEnv.ID, Name: "db"}, Spec: &resourcev1.Link{
		Label: "Database", Service: "web", MeetsEnvironment: env, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, DatabaseName: "blog",
		Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
		Sets:    []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
	}}))
	e.RequireRoles()
	return linkedBlog{x: x, blog: blog, blogEnv: blogEnv.ID, forBlog: resource.Ref{Kind: link.Kind, Scope: blogEnv.ID, Name: "db"}}
}

// in is a caller holding role in blog alone: for admin, what Narrow gives an
// admin's token narrowed to blog (ADR 0071).
func (l linkedBlog) in(t *testing.T, role string) place.Caller {
	t.Helper()
	return place.Caller{UserID: "usr_ada", Grants: []store.Grant{{UserID: "usr_ada", ProjectID: l.blog, Role: role}}}
}

// What's done for blog's link on a server in another project's protected
// environment, a restore of its database or a dump of it first, needs an admin
// where that server is: a token narrowed to blog holds admin in blog alone, which
// never counts there, so it waits for a person, as a developer of blog's would
// (ADR 0071, the review's M1).
func TestANarrowedTokensAdminDoesntCountOnAnotherProjectsProtectedServer(t *testing.T) {
	t.Run("a restore of its database", func(t *testing.T) {
		l := withLinkedBlog(t)
		x := l.x
		by := By{Actor: "user:usr_ada", Via: ViaPanel}
		x.r.one = "blog"
		source := resource.RestoreSource{Backup: "bkp_1", Server: "srv_A"}
		dev := place.WithCaller(t.Context(), l.in(t, store.RoleDeveloper))
		plan, err := x.e.RestoreWith(dev, x.ref, source, RestoreAs{For: l.forBlog}, by, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := x.e.ApplyBy(dev, plan.Change, by); !errors.Is(err, ErrNeedsRole) {
			t.Fatalf("a developer of blog applying it: %v, want it refused", err)
		}
		narrowed := place.WithCaller(t.Context(), l.in(t, store.RoleAdmin))
		if err := x.e.ApplyBy(narrowed, plan.Change, by); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a token narrowed to blog applying a restore on another project's protected server: %v, want it to wait for a person", err)
		}
	})
	t.Run("a dump of its database first", func(t *testing.T) {
		l := withLinkedBlog(t)
		x := l.x
		client := x.engine.Client()
		x.agents["srv_A"] = twoAgents{Local: x.agents["srv_A"].(twoAgents).Local, volumes: resource.Local{Drivers: resource.NewRegistry(&volume.Driver{ //nolint:forcetypeassert // withVolume's
			Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}, Paths: &container.Paths{Client: client}}, Restores: x.r, BacksUp: &backs{},
		})}}
		narrowed := place.WithCaller(t.Context(), l.in(t, store.RoleAdmin))
		plan, err := x.e.Propose(narrowed, Proposal{
			Summary: "Rename blog's link, dumping its database first", Actor: "user:usr_ada",
			Edits: []Edit{{Ref: l.forBlog, Spec: &resourcev1.Link{
				Label: "Blog's database", Service: "web", MeetsEnvironment: x.env, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, DatabaseName: "blog",
				Accepts: []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				Sets:    []*resourcev1.LinkSet{{Name: "DB_HOST", Part: resourcev1.LinkPart_LINK_PART_HOST}},
			}}},
			BackUpOne: []OneBackup{{Volume: x.ref, Database: "blog", For: l.forBlog}},
		})
		if err != nil {
			t.Fatal(err)
		}
		by := By{Actor: "user:usr_ada", Via: ViaPanel}
		if err := x.e.ApplyBy(narrowed, plan.Change, by); !errors.Is(err, ErrNeedsRole) {
			t.Errorf("a token narrowed to blog applying a change that dumps its database first on another project's protected server: %v, want it to wait for a person", err)
		}
	})
}
