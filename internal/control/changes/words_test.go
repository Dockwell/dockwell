// SPDX-License-Identifier: AGPL-3.0-only

package changes

import (
	"testing"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
)

// A token's refusal says what about a plan a person approves, each as what it is:
// a domain isn't access to a server, and the panel's moving isn't given to anything.
func TestWhatAPersonApprovesIsSaidAsWhatItIs(t *testing.T) {
	plan := func(name string, access ...resource.Access) *revision.Plan {
		return &revision.Plan{Resources: []revision.ResourcePlan{{Ref: resource.Ref{Kind: "route", Name: name}, HostAccess: access}}}
	}
	for _, tc := range []struct {
		name string
		plan *revision.Plan
		want string
	}{
		{"a server path", plan("web", resource.Access{Kind: resource.AccessPath, Path: "/srv/app", ReadOnly: true}), "gives web access to its server: the server path /srv/app (read-only)"},
		{"a takeover", plan("www", resource.Access{Kind: resource.AccessDomain, Path: "app.example.com", Detail: "production"}), "claims the domain app.example.com, which the environment production held within the last 7 days"},
		{"the panel", plan("www", resource.Access{Kind: resource.AccessPanelDomain, Path: "panel.example.com", Detail: "Caddy"}), "moves the panel's domain, panel.example.com, behind Caddy"},
		{"a port on the server", plan("www", resource.Access{Kind: resource.AccessServerPort, Path: "127.0.0.1:8080", Detail: "app.example.com"}), "gives www access to its server: the port 127.0.0.1:8080 on the server itself, past its containers, at app.example.com"},
		{"a port on the server, plain", plan("www", resource.Access{Kind: resource.AccessServerPort, Path: "127.0.0.1:8080", Detail: "app.example.com/x", Type: "http"}), "gives www access to its server: the port 127.0.0.1:8080 on the server itself, past its containers, at app.example.com/x over plain HTTP"},
		{"plain for anyone", plan("www", resource.Access{Kind: resource.AccessPlainForAnyone, Path: "app.example.com"}), "has www serve plain HTTP on app.example.com for anyone who can reach this server, around whatever is in front of it"},
		{"off HTTPS", plan("www", resource.Access{Kind: resource.AccessHTTPSOff, Path: "app.example.com"}), "takes away HTTPS on app.example.com, which stops: https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out"},
		{"a proxy", plan("www", resource.Access{Kind: resource.AccessTrustedProxy, Path: "cloudflare"}), "has www take cloudflare as a proxy it believes on where a request came from"},
		{"nothing", plan("www"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accessWords(tc.plan); got != tc.want {
				t.Errorf("accessWords = %q, want %q", got, tc.want)
			}
		})
	}
}
