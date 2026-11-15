// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// A redirect is a route through the API like any other (ADR 0070): it plans, it
// applies, and an edit that writes the route whole without it keeps it. A loop
// across two routes is refused as it's proposed, by which route would answer where
// it lands, across the install.
func TestARedirectThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	ing := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.addServerWith(t, func() []resource.Type { return []resource.Type{&route.Driver{Ingress: ing}} })
	c := changeClient(b, b.origin)
	first := importCompose(t, dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()), "services:\n  web:\n    image: ghcr.io/acme/shop:1.4\n", nil, false)
	if got := apply(t, c, first); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the service: %v", got)
	}
	env := first.GetPlan().GetResources()[0].GetResource().GetScope()
	edit := func(name string, r *resourcev1.Route) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: env, Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: r}},
		}
	}
	site := &resourcev1.Route{Domain: "example.com", Service: "web", Port: 80}
	www := &resourcev1.Route{Domain: "www.example.com", Redirect: &resourcev1.RouteRedirect{To: "https://example.com/"}}
	ch := propose(t, c, "Answer example.com, and send www there", edit("site", site), edit("www", www))
	if !strings.Contains(ch.GetPlan().String(), "redirects:") {
		t.Errorf("the plan says nothing of the redirect: %v", ch.GetPlan())
	}
	if got := apply(t, c, ch); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("applying = %v", got)
	}
	got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}]
	if got.GetRedirect().GetTo() != "https://example.com" {
		t.Errorf("the server has %v, want it sent to https://example.com, the / it doesn't need trimmed", got)
	}

	// Written whole without the field, as "Move behind what's in front" does, it keeps it.
	ch = propose(t, c, "Move www behind a proxy", edit("www", &resourcev1.Route{Domain: "www.example.com", TrustedProxies: []string{"cloudflare"}}))
	applyConfirmed(t, c, ch)
	if got := ing.routes[resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}]; got.GetRedirect().GetTo() != "https://example.com" {
		t.Errorf("after a whole-route edit, the server has %v", got)
	}

	// example.com sending back to www would go round in a circle.
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Send example.com to www",
		Edits:   []*dockwellv1.Edit{edit("site", &resourcev1.Route{Domain: "example.com", Redirect: &resourcev1.RouteRedirect{To: "https://www.example.com"}})},
	}))
	if err == nil || !strings.Contains(err.Error(), "round in a circle") {
		t.Errorf("a loop across two routes = %v", err)
	}
}
