// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
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
