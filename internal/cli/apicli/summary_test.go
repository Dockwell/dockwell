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
	app := &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}}}}
	for _, c := range []struct {
		verb string
		kind resource.Kind
		name string
		spec *resourcev1.Spec
		want string
	}{
		// As the panel titles the same changes: New app, Add a domain, New task, Delete.
		{"create", "service", "web", app, "Run nginx:1.27 as web"},
		{"create", "service", "checkout-worker", nil, "Add the app checkout-worker"},
		{"update", "service", "web", app, "Change the app web"},
		{"delete", "service", "web", nil, "Delete web"},
		{"create", "route", "code-web", route, "Send git.example.com to gitea"},
		{"update", "route", "code-web", route, "Change the domain git.example.com"},
		{"delete", "route", "code-web", nil, "Remove the domain code-web"},
		{"create", "task", "backup", nil, "Run backup on a schedule"},
		{"update", "task", "backup", nil, "Change the task backup"},
		{"delete", "task", "backup", nil, "Delete the task backup"},
		{"create", "variable", "LOG_LEVEL", nil, "Add the variable LOG_LEVEL"},
		{"delete", "variable", "LOG_LEVEL", nil, "Remove the variable LOG_LEVEL"},
		{"delete", "volume", "data", nil, "Delete the volume data"},
		{"create", "project", "shop", nil, "Add the project shop"},
		{"delete", "project", "shop", nil, "Delete the project shop"},
		{"create", "alert.rule", "disk-full", nil, "Add the alert rule disk-full"},
		{"delete", "alert.channel", "ops", nil, "Remove the alert channel ops"},
		{"create", "dns.provider", "cloudflare", nil, "Add the DNS provider cloudflare"},
	} {
		if got := changeSummary(c.verb, c.kind, c.name, c.spec); got != c.want {
			t.Errorf("%s %s %s: %q, want %q", c.verb, c.kind, c.name, got, c.want)
		}
	}
}
