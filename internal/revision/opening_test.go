// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const passwordForCI = "correct horse battery staple"

func privateIn(env, name, domain, path string, access *resourcev1.RouteAccess) Edit {
	e := routeIn(env, name, domain, path)
	e.Spec.(*resourcev1.Route).Access = access
	return e
}

func ci(password string) *resourcev1.RouteUser {
	return &resourcev1.RouteUser{Name: "ci", Password: password}
}

// opened is what a plan says it opens, by where, in its words.
func opened(p *Plan) map[string]string {
	out := map[string]string{}
	for i := range p.Resources {
		for j := range p.Resources[i].HostAccess {
			if a := &p.Resources[i].HostAccess[j]; a.Kind == resource.AccessOpened {
				out[a.Path] = a.Detail
			}
		}
	}
	return out
}

// Each way of letting in someone a private route kept out needs a person: in place,
// by deleting it and making another in one change or two, by a path of its own under
// it, and by deleting a private path under a public domain. Each way of keeping out
// more is a token's, and so is editing what a person opened (ADR 0087).
func TestOpeningAPrivateRouteNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, staging, ings := setupRoutes(t, b, "srv_A")
		ref := resource.Ref{Kind: route.Kind, Scope: prod, Name: "www"}
		access := func(allow []string, users ...*resourcev1.RouteUser) *resourcev1.RouteAccess {
			return &resourcev1.RouteAccess{Allow: allow, Users: users}
		}
		office := []string{"203.0.113.0/24"}
		first := propose(t, e, "Private", privateIn(prod, "www", "app.example.com", "", access(office, ci(passwordForCI))))
		if first.HostAccess() {
			t.Errorf("a new private route plans %+v", first.Resources)
		}
		apply(t, e, first)
		served := ings["srv_A"].routes[ref].GetAccess().GetUsers()[0]
		if served.GetPassword() != "" || bcrypt.CompareHashAndPassword([]byte(served.GetPasswordHash()), []byte(passwordForCI)) != nil {
			t.Fatalf("the server got %+v, want the password's hash alone", served)
		}

		for _, tc := range []struct {
			name  string
			edits []Edit
			want  map[string]string
		}{
			{"a narrower list, the user kept", []Edit{privateIn(prod, "www", "app.example.com", "", access([]string{"203.0.113.7/32"}, &resourcev1.RouteUser{Name: "ci"}))}, map[string]string{}},
			{
				"a wider list",
				[]Edit{privateIn(prod, "www", "app.example.com", "", access([]string{"203.0.113.0/24", "198.51.100.0/24"}, &resourcev1.RouteUser{Name: "ci"}))},
				map[string]string{"app.example.com": "allow:198.51.100.0/24"},
			},
			{
				"the list off",
				[]Edit{privateIn(prod, "www", "app.example.com", "", access(nil, &resourcev1.RouteUser{Name: "ci"}))},
				map[string]string{"app.example.com": "list-off"},
			},
			{
				"a user added",
				[]Edit{privateIn(prod, "www", "app.example.com", "", access(office, &resourcev1.RouteUser{Name: "ci"}, &resourcev1.RouteUser{Name: "eve", Password: "another long password"}))},
				map[string]string{"app.example.com": "user:eve"},
			},
			{
				"a password changed",
				[]Edit{privateIn(prod, "www", "app.example.com", "", access(office, ci("another long password")))},
				map[string]string{"app.example.com": "password:ci"},
			},
			{
				"public again",
				[]Edit{routeIn(prod, "www", "app.example.com", "")},
				map[string]string{"app.example.com": "list-off users-off"},
			},
			{
				"swapped for a public route",
				[]Edit{{Ref: ref}, routeIn(prod, "home", "app.example.com", "")},
				map[string]string{"app.example.com": "list-off users-off"},
			},
			{
				"a public path under it",
				[]Edit{routeIn(prod, "admin", "app.example.com", "/admin")},
				map[string]string{"app.example.com/admin": "list-off users-off"},
			},
			{
				"a private path under it",
				[]Edit{privateIn(prod, "admin", "app.example.com", "/admin", access([]string{"203.0.113.7/32"}, ci(passwordForCI)))},
				map[string]string{"app.example.com/admin": "password:ci"},
			},
			{"deleted", []Edit{{Ref: ref}}, map[string]string{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := propose(t, e, tc.name, tc.edits...)
				got := opened(p)
				if len(got) != len(tc.want) {
					t.Fatalf("it opens %v, want %v", got, tc.want)
				}
				for where, words := range tc.want {
					if got[where] != words {
						t.Errorf("at %s it says %q, want %q", where, got[where], words)
					}
				}
				if p.HostAccess() != (len(tc.want) > 0) {
					t.Errorf("it needs a person: %v, want %v", p.HostAccess(), len(tc.want) > 0)
				}
			})
		}

		// Deleted, then made again open, in this environment or another: the history
		// counts, as for going off HTTPS.
		apply(t, e, propose(t, e, "Gone", Edit{Ref: ref}))
		for _, again := range []Edit{routeIn(prod, "www", "app.example.com", ""), routeIn(staging, "www", "app.example.com", "")} {
			if got := opened(propose(t, e, "Back", again)); !strings.Contains(got["app.example.com"], "users-off") {
				t.Errorf("made again after a delete, it opens %v", got)
			}
		}
		// Once a person has opened it, editing it is a token's.
		apply(t, e, propose(t, e, "Back open", routeIn(prod, "www", "app.example.com", "")))
		if p := propose(t, e, "Another port", Edit{Ref: ref, Spec: &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 9090}}); p.HostAccess() {
			t.Errorf("an edit of what a person opened plans %+v", p.Resources)
		}

		// A private path under a public domain: deleting it leaves the path to the domain.
		apply(t, e, propose(t, e, "Shop", routeIn(prod, "shop", "shop.example.com", ""),
			privateIn(prod, "shop-admin", "shop.example.com", "/admin", access(office))))
		p := propose(t, e, "Admin gone", Edit{Ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "shop-admin"}})
		if got := opened(p); got["shop.example.com/admin"] != "list-off" || !p.HostAccess() {
			t.Errorf("deleting a private path under a public domain opens %v", got)
		}
		// Under nothing, a delete keeps everyone out.
		if got := opened(propose(t, e, "Both gone", Edit{Ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "shop-admin"}}, Edit{Ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "shop"}})); len(got) != 0 {
			t.Errorf("deleting the domain with its path opens %v", got)
		}
		if !slices.Contains(e.kinds.Claimers(), route.Kind) {
			t.Fatal("routes aren't claimers")
		}
	})
}

// A private path and its domain's other routes run on one server. Moving the path
// alone to another server would leave it to the public root on the first, which
// compares as nothing changed, spec by spec; placing them apart from the start leaves
// it open there already. Moving them together is a token's, as nothing opens, and a
// domain that keeps no one out may run where it likes (#606's review, H1).
func TestAPrivateRouteRunsBesideItsDomain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, ings := setupRoutes(t, b, "srv_A", "srv_B")
		admin := func(servers ...string) Edit {
			ed := privateIn(prod, "admin", "shop.example.com", "/admin", &resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}})
			ed.Servers = servers
			return ed
		}
		apply(t, e, propose(t, e, "Shop", routeIn(prod, "shop", "shop.example.com", "", "srv_A"), admin("srv_A")))
		if ings["srv_A"].routes[resource.Ref{Kind: route.Kind, Scope: prod, Name: "admin"}] == nil {
			t.Fatal("the private path isn't on srv_A")
		}
		refuseProposal(t, e, "the private path moved alone", ErrInvalid,
			"shop.example.com keeps strangers out of some of its routes, so all its routes run on one server", admin("srv_B"))
		refuseProposal(t, e, "the public root moved alone", ErrInvalid,
			"so all its routes run on one server", routeIn(prod, "shop", "shop.example.com", "", "srv_B"))
		refuseProposal(t, e, "a public path apart from a private one", ErrInvalid,
			"so all its routes run on one server", routeIn(prod, "api", "shop.example.com", "/api", "srv_B"))
		together := propose(t, e, "Together", routeIn(prod, "shop", "shop.example.com", "", "srv_B"), admin("srv_B"))
		if got := opened(together); len(got) != 0 || together.HostAccess() {
			t.Errorf("moving the domain's routes together opens %v", got)
		}
		// A domain that keeps no one out runs where it likes, as before.
		apply(t, e, propose(t, e, "Split", routeIn(prod, "blog", "blog.example.com", "", "srv_A"), routeIn(prod, "blog-api", "blog.example.com", "/api", "srv_B")))
		// And making one of its routes private brings that rule with it.
		refuseProposal(t, e, "a private path on a split domain", ErrInvalid, "so all its routes run on one server",
			privateIn(prod, "blog-api", "blog.example.com", "/api", &resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}}))
	})
}

// A route that keeps its users' passwords stays where they are, with why, as a plan
// would send them to the new server on a proposal alone; new passwords move it (#606's
// review, L1).
func TestAPrivateRouteMovesWithNewPasswords(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A", "srv_B")
		at := func(user *resourcev1.RouteUser, servers ...string) Edit {
			ed := privateIn(prod, "www", "app.example.com", "", &resourcev1.RouteAccess{Users: []*resourcev1.RouteUser{user}})
			ed.Servers = servers
			return ed
		}
		apply(t, e, propose(t, e, "Private", at(ci(passwordForCI), "srv_A")))
		refuseProposal(t, e, "moved, keeping ci's password", ErrInvalid,
			"www can't go on srv_B: its users' passwords stay on the server it runs on. Give ci a new password to move it",
			at(&resourcev1.RouteUser{Name: "ci"}, "srv_B"))
		propose(t, e, "Moved, with a new password", at(ci("another long password"), "srv_B"))
	})
}

// Only the domains a change touches are its to answer for: a change elsewhere in the
// environment goes, whatever another domain's routes are (#606's review, L1). The
// refusal names every route and where it runs (L3).
func TestTogetherAnswersForWhatTheChangeTouches(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A", "srv_B")
		private := &resourcev1.Route{Domain: "shop.example.com", Path: "/admin", Service: "web", Port: 8080, Access: &resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}}}
		public := &resourcev1.Route{Domain: "shop.example.com", Service: "web", Port: 8080}
		other := &resourcev1.Route{Domain: "blog.example.com", Service: "web", Port: 8080}
		split := []served{
			{name: "admin", spec: private, servers: []string{"srv_B"}},
			{name: "shop", spec: public, servers: []string{"srv_A"}},
			{name: "api", spec: &resourcev1.Route{Domain: "shop.example.com", Path: "/api", Service: "web", Port: 8080}, servers: []string{"srv_B"}},
			{name: "blog", spec: other, servers: []string{"srv_A"}},
		}
		var opener resource.Opener = &route.Driver{}
		blog := &item{ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "blog"}, spec: other}
		if err := e.togetherIn(t.Context(), route.Kind, split, opener, []*item{blog}); err != nil {
			t.Errorf("a change to another domain is refused: %v", err)
		}
		shop := &item{ref: resource.Ref{Kind: route.Kind, Scope: prod, Name: "shop"}, spec: public}
		err := e.togetherIn(t.Context(), route.Kind, split, opener, []*item{shop})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "This would run admin on srv_B, api on srv_B and shop on srv_A. Move the whole domain to one server") {
			t.Errorf("a change to the split domain = %v", err)
		}
	})
}

// A domain moved to another server together never serves its private path open on
// either while the change runs: on the server it goes to, the private path is made
// before the public root, and on the one it leaves, taken down after it (#606's
// review, L4).
func TestAPrivatePathIsMadeFirstAndTakenDownLast(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A", "srv_B")
		admin := func(servers ...string) Edit {
			ed := privateIn(prod, "admin", "shop.example.com", "/admin", &resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}})
			ed.Servers = servers
			return ed
		}
		// The public root comes first in the proposal, as a person may well write it.
		apply(t, e, propose(t, e, "Shop", routeIn(prod, "shop", "shop.example.com", "", "srv_A"), admin("srv_A")))
		p := propose(t, e, "Move", routeIn(prod, "shop", "shop.example.com", "", "srv_B"), admin("srv_B"))
		steps := func(server string) []string {
			for _, sp := range p.Servers {
				if sp.Server == server {
					var out []string
					for _, st := range sp.Steps {
						out = append(out, st.Ref.Name+" "+st.Action.String())
					}
					return out
				}
			}
			return nil
		}
		if got := strings.Join(steps("srv_B"), ", "); !strings.HasPrefix(got, "admin create") {
			t.Errorf("on the server it goes to, the steps run %s, want the private path first", got)
		}
		if got := strings.Join(steps("srv_A"), ", "); !strings.HasSuffix(got, "admin delete") {
			t.Errorf("on the server it leaves, the steps run %s, want the private path last", got)
		}
	})
}
