// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func redirectIn(env, name, domain, path, to string, dropPath bool, servers ...string) Edit {
	return Edit{
		Ref:     resource.Ref{Kind: route.Kind, Scope: env, Name: name},
		Spec:    &resourcev1.Route{Domain: domain, Path: path, Redirect: &resourcev1.RouteRedirect{To: to, DropPath: dropPath}},
		Servers: servers,
	}
}

// Deleting or moving the route a redirect lands on changes which route answers it,
// so a change that would leave another redirect in a loop is refused, naming it,
// though the change never names the redirect (ADR 0070). A loop that was there
// before holds up nothing else.
func TestAChangeThatLeavesARedirectInALoopIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "Blog", routeIn(prod, "blog", "example.com", "/blog", "srv_A")))
		apply(t, e, propose(t, e, "Root to the blog", redirectIn(prod, "root", "example.com", "", "https://example.com/blog", true, "srv_A")))

		_, err := e.Propose(t.Context(), Proposal{Summary: "Drop the blog", Actor: "usr_test", Edits: []Edit{del(route.Kind, prod, "blog")}})
		if err == nil || !strings.Contains(err.Error(), "route root, which this change leaves as it is, would then send visitors round in a circle") {
			t.Errorf("deleting /blog = %v, and example.com/blog would redirect to itself", err)
		}
		moved := routeIn(prod, "blog", "example.com", "/articles", "srv_A")
		_, err = e.Propose(t.Context(), Proposal{Summary: "Move the blog", Actor: "usr_test", Edits: []Edit{moved}})
		if err == nil || !strings.Contains(err.Error(), "route root") {
			t.Errorf("moving /blog = %v", err)
		}
		// Deleting both together is fine, as is another domain's change.
		apply(t, e, propose(t, e, "Drop both", del(route.Kind, prod, "blog"), del(route.Kind, prod, "root")))
		apply(t, e, propose(t, e, "Another site", routeIn(prod, "shop", "shop.example.com", "", "srv_A")))
	})
}

// A change that deletes or moves the route a redirect lands on, leaving the redirect
// as it is, says so in its plan, naming the redirect, as its visitors would land on
// nothing (ADR 0070). A change that leaves somewhere for it to land says nothing.
func TestAChangeSaysARedirectWouldLandOnNothing(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, _ := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "The site", routeIn(prod, "site", "example.com", "", "srv_A")))
		apply(t, e, propose(t, e, "www to the site", redirectIn(prod, "www", "www.example.com", "", "https://example.com", false, "srv_A")))

		p := propose(t, e, "Drop the site", del(route.Kind, prod, "site"))
		i := slices.IndexFunc(p.Notes, func(n Note) bool { return n.Reason == "redirect_stranded" })
		if i < 0 || p.Notes[i].Ref.Name != "www" || !strings.Contains(p.Notes[i].Message, "www.example.com still redirects to example.com, so its visitors would land on nothing") {
			t.Errorf("deleting what www lands on notes %+v", p.Notes)
		}
		moved := propose(t, e, "Move the site", routeIn(prod, "site", "example.org", "", "srv_A"))
		if !slices.ContainsFunc(moved.Notes, func(n Note) bool { return n.Reason == "redirect_stranded" }) {
			t.Errorf("moving what www lands on notes %+v", moved.Notes)
		}
		both := propose(t, e, "Drop both", del(route.Kind, prod, "site"), del(route.Kind, prod, "www"))
		if slices.ContainsFunc(both.Notes, func(n Note) bool { return n.Reason == "redirect_stranded" }) {
			t.Errorf("deleting both notes %+v", both.Notes)
		}
		other := propose(t, e, "Another site", routeIn(prod, "shop", "shop.example.com", "", "srv_A"))
		if slices.ContainsFunc(other.Notes, func(n Note) bool { return n.Reason == "redirect_stranded" }) {
			t.Errorf("another site's change notes %+v", other.Notes)
		}
	})
}
