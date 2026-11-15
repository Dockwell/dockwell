// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func redirectIn(env, name, domain, to string, dropPath bool, servers ...string) Edit {
	return Edit{
		Ref:     resource.Ref{Kind: route.Kind, Scope: env, Name: name},
		Spec:    &resourcev1.Route{Domain: domain, Redirect: &resourcev1.RouteRedirect{To: to, DropPath: dropPath}},
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
		apply(t, e, propose(t, e, "Root to the blog", redirectIn(prod, "root", "example.com", "https://example.com/blog", true, "srv_A")))

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
		apply(t, e, propose(t, e, "www to the site", redirectIn(prod, "www", "www.example.com", "https://example.com", false, "srv_A")))

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

// refusesRedirects is an agent too old to serve a redirect: it refuses to read one
// back, as the agent API's session does (ADR 0070).
type refusesRedirects struct{ resource.Local }

func (refusesRedirects) CheckRead(desired []resource.Desired) error {
	for i := range desired {
		if r, ok := desired[i].Spec.(*resourcev1.Route); ok && r.GetRedirect() != nil {
			return errors.New("srv_A's agent is older than this release, so it can't serve www.example.com's redirect. Update its agent, then plan again")
		}
	}
	return nil
}

// Drift is never read back from an agent that can't serve what a server should run:
// the engine asks it first, and says why it can't check (ADR 0070).
func TestDriftIsntReadFromAnAgentThatCantServeARedirect(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "srv_A", CreatedAt: now, CertSerial: "srv_A", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	ing := &memIngress{routes: map[resource.Ref]*resourcev1.Route{}}
	local := resource.Local{Drivers: resource.NewRegistry(&route.Driver{Ingress: ing, Reserved: panelDomain})}
	agents := onDocker{"srv_A": local}
	kinds := resource.NewRegistry(&route.Driver{Reserved: panelDomain})
	e := New(db, kinds, agents, secretstest.Keeper(db, kinds), logger)
	prod := production(t, db)
	apply(t, e, propose(t, e, "www", redirectIn(prod, "www", "www.example.com", "https://example.com", false, "srv_A")))

	agents["srv_A"] = refusesRedirects{local}
	_, err := e.Drift(t.Context(), "srv_A")
	if err == nil || !strings.Contains(err.Error(), "can't serve www.example.com's redirect. Update its agent") {
		t.Errorf("drift from an agent that can't serve a redirect = %v", err)
	}
	agents["srv_A"] = local
	if _, err := e.Drift(t.Context(), "srv_A"); err != nil {
		t.Errorf("drift from an agent that serves it = %v", err)
	}
}

// A redirect for www under a name of its own is refused where another route of the
// install serves www, never taken as an update of that route, and to a caller who
// can't see that route's project in words that name nothing of it (ADR 0070).
func TestAWwwRedirectNeverTakesOverAServedWww(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, prod, _, ingresses := setupRoutes(t, b, "srv_A")
		apply(t, e, propose(t, e, "www to web", routeIn(prod, "www-example-org", "www.example.org", "", "srv_A")))
		_, err := e.Propose(t.Context(), Proposal{Summary: "Send example.org to api", Actor: "usr_test", Edits: []Edit{
			redirectIn(prod, "www-example-org-2", "www.example.org", "https://example.org", false, "srv_A"),
		}})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "www.example.org") {
			t.Errorf("a second route for www = %v", err)
		}
		if got := ingresses["srv_A"].routes[resource.Ref{Kind: route.Kind, Scope: prod, Name: "www-example-org"}]; got.GetService() != "web" {
			t.Errorf("www now %v", got)
		}
	})
}
