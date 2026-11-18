// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A change the CLI proposes is titled in the product's words, as the rail names
// things: an app, a domain and a scheduled task, never a service, a route or a task
// (#796).
func TestAChangeIsTitledInTheProductsWords(t *testing.T) {
	t.Parallel()
	route := &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{Domain: "git.example.com", Service: "gitea"}}}
	for _, c := range []struct {
		verb string
		kind resource.Kind
		name string
		spec *resourcev1.Spec
		want string
	}{
		{"create", "service", "checkout-worker", nil, "Add the app checkout-worker"},
		{"update", "service", "web", nil, "Change the app web"},
		{"delete", "service", "web", nil, "Remove the app web"},
		{"create", "route", "code-web", route, "Add the domain git.example.com to gitea"},
		{"update", "route", "code-web", route, "Change the domain git.example.com"},
		{"delete", "route", "code-web", nil, "Remove the domain code-web"},
		{"create", "task", "backup", nil, "Add the scheduled task backup"},
		{"create", "project", "shop", nil, "Add the project shop"},
		{"create", "dns.provider", "cloudflare", nil, "Add the DNS provider cloudflare"},
	} {
		if got := changeSummary(c.verb, c.kind, c.name, c.spec); got != c.want {
			t.Errorf("%s %s %s: %q, want %q", c.verb, c.kind, c.name, got, c.want)
		}
	}
}
