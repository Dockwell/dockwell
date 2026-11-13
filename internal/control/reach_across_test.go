// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Someone who may change one project can't reach into another through what a link
// says (ADR 0084): a link to another environment, a value read from it, a network
// that takes in its service and a database copied from it each need a role there,
// and each says only what its link does, whoever proposes it. Data runs PostgreSQL
// with two apps' users, shop and blog; the blog, in the default project, links to it
// as blog. A developer who holds the default project alone tries each way in.
func TestOneProjectCantReachIntoAnother(t *testing.T) {
	storetest.Each(t, testOneProjectCantReachIntoAnother)
}

func testOneProjectCantReachIntoAnother(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServerWith(t, "hz-1", &usersInMemory{made: map[resource.Ref]proto.Message{}}, &networksInMemory{made: map[resource.Ref]proto.Message{}})
	c := changeClient(b, b.origin)
	blog := listedEnv(t, b)
	apply(t, c, propose(t, c, "Add the data project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	var data string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		org, _ := tx.Organization(t.Context())
		p, err := tx.GetResource(t.Context(), "project", org.ID, "data")
		if err != nil {
			return err
		}
		e, err := tx.GetResource(t.Context(), "environment", p.ID, "production")
		data = e.ID
		return err
	})
	in := func(scope string, e *dockwellv1.Edit) *dockwellv1.Edit {
		e.Resource.Scope = scope
		return e
	}
	user := func(name, password string, copyFrom *resourcev1.DatabaseCopy) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "database.user", Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_DatabaseUser{DatabaseUser: &resourcev1.DatabaseUser{Service: "db", Password: password, CopyFrom: copyFrom}}},
		}
	}
	link := func(name, env, userName string) *dockwellv1.Edit {
		return in(blog, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Database", Service: "db", MeetsEnvironment: env, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, Version: "17",
				Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
				DatabaseName: userName, User: userName,
				Sets: []*resourcev1.LinkSet{{Name: strings.ToUpper(name) + "_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD}},
			}}},
		})
	}
	from := func(name, secret, linkName string) *dockwellv1.Edit {
		return in(blog, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: data, Secret: true, Name: secret, Link: linkName}},
			}}}},
		})
	}
	reach := func(name, service string) *dockwellv1.Edit {
		return in(blog, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "network", Name: name},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Network{Network: &resourcev1.Network{
				Internal: true, Members: []*resourcev1.NetworkMember{{Environment: data, Service: service, Alias: "db"}},
			}}},
		})
	}
	apply(t, c, propose(t, c, "Run the shared database",
		in(data, secretSpec("ADMIN_PASSWORD", "the-administrators")),
		in(data, secretSpec("SHOP_DB_PASSWORD", "shops-own")),
		in(data, secretSpec("BLOG_DB_PASSWORD", "blogs-own")),
		in(data, serviceSpec("db", map[string]string{"POSTGRES_PASSWORD": "ADMIN_PASSWORD"}, hz1.id.ServerID)),
		in(data, user("shop", "SHOP_DB_PASSWORD", nil)),
		in(data, user("blog", "BLOG_DB_PASSWORD", nil)),
	))
	apply(t, c, propose(t, c, "Link the blog", link("db", data, "blog"), from("DB_PASSWORD", "BLOG_DB_PASSWORD", "db"),
		in(blog, serviceSpec("web", map[string]string{"WORDPRESS_DB_PASSWORD": "DB_PASSWORD"}, hz1.id.ServerID))))

	refused := func(code connect.Code, why string, edits ...*dockwellv1.Edit) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Try", Edits: edits}))
		if connect.CodeOf(err) != code || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: got %v", why, err)
		}
	}
	// Whatever anyone holds, each says only what its link does.
	refused(connect.CodeInvalidArgument, "SHOP_DB_PASSWORD isn't the password of blog, the user link db made there",
		from("STOLEN", "SHOP_DB_PASSWORD", "db"), in(blog, serviceSpec("thief", map[string]string{"P": "STOLEN"}, hz1.id.ServerID)))
	refused(connect.CodeInvalidArgument, "isn't the password of shop, the user link mine made there",
		link("mine", data, "shop"), from("STOLEN", "ADMIN_PASSWORD", "mine"))
	refused(connect.CodeInvalidArgument, "network reach takes in other of another environment, which no link of its environment meets",
		reach("reach", "other"))
	refused(connect.CodeInvalidArgument, "no link in this change moves from it to mine",
		in(blog, secretSpec("MINE_PASSWORD", "mine")), in(blog, appEdit(blog, "db", nil)), in(blog, user("mine", "MINE_PASSWORD", &resourcev1.DatabaseCopy{Environment: data, Service: "db", Database: "shop"})))
	refused(connect.CodeInvalidArgument, "postgres is a database its server keeps for itself",
		in(blog, secretSpec("MINE_PASSWORD", "mine")), in(blog, appEdit(blog, "db", nil)), in(blog, user("mine", "MINE_PASSWORD", &resourcev1.DatabaseCopy{Environment: data, Service: "db", Database: "postgres"})))

	// A developer of the blog's project alone reaches nothing in data, even as its
	// links would have it: the environment reads as not there.
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: envProject(t, f, blog), Role: store.RoleDeveloper})
	gone := "there's nothing by that ID or name here"
	refused(connect.CodeNotFound, gone, link("mine", data, "shop"), from("STOLEN", "SHOP_DB_PASSWORD", "mine"),
		in(blog, serviceSpec("thief", map[string]string{"P": "STOLEN"}, hz1.id.ServerID)))
	refused(connect.CodeNotFound, gone, from("AGAIN", "BLOG_DB_PASSWORD", "db"))
	refused(connect.CodeNotFound, gone, reach("reach", "db"))
	refused(connect.CodeNotFound, gone,
		in(blog, secretSpec("MINE_PASSWORD", "mine")), in(blog, appEdit(blog, "db", nil)), in(blog, user("mine", "MINE_PASSWORD", &resourcev1.DatabaseCopy{Environment: data, Service: "db", Database: "shop"})))

	// A reader of data still can't point anything there.
	hold(store.Grant{ProjectID: envProject(t, f, blog), Role: store.RoleDeveloper}, store.Grant{ProjectID: envProject(t, f, data), Role: store.RoleViewer})
	refused(connect.CodePermissionDenied, "network reach takes in db of another environment, where you need a role too", reach("reach", "db"))

	// The blog's own edits need nothing in data, its link's value among them.
	propose(t, c, "Restart the blog", in(blog, serviceSpec("web", map[string]string{"WORDPRESS_DB_PASSWORD": "DB_PASSWORD", "X": "DB_PASSWORD"}, hz1.id.ServerID)),
		from("DB_PASSWORD", "BLOG_DB_PASSWORD", "db"))
}

// networksInMemory makes networks as a record alone, for a test server whose fake
// runs no Docker networks.
type networksInMemory struct {
	network.Driver
	mu   sync.Mutex
	made map[resource.Ref]proto.Message
}

func (n *networksInMemory) Observe(_ context.Context, ref resource.Ref) (proto.Message, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.made[ref], nil
}

func (n *networksInMemory) Apply(_ context.Context, step resource.Step, desired proto.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if desired == nil {
		delete(n.made, step.Ref)
	} else {
		n.made[step.Ref] = proto.Clone(desired)
	}
	return nil
}

func (*networksInMemory) Ready(context.Context) error { return nil }
func (n *networksInMemory) ReadyKey() any             { return n }
